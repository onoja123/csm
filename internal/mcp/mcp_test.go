package mcp

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeAdapter hosts stdio and http servers and records what it is asked to write.
type fakeAdapter struct {
	id        string
	envByName bool
	existing  []Server
	parseErr  error
	prepErr   error
	written   []Server
}

func (f *fakeAdapter) Provider() string { return f.id }

func (f *fakeAdapter) EnvByName() bool { return f.envByName }

func (f *fakeAdapter) Parse(profileDir, projectDir string) ([]Server, error) {
	return f.existing, f.parseErr
}

func (f *fakeAdapter) Validate(s Server) error {
	if s.Transport == Stdio || s.Transport == HTTP {
		return nil
	}

	return errors.New(string(s.Transport) + " transport is not supported")
}

func (f *fakeAdapter) Prepare(profileDir, projectDir string, servers []Server) error {
	if f.prepErr != nil {
		return f.prepErr
	}

	f.written = append(f.written, servers...)

	for _, s := range servers {
		if s.Scope == ScopeProject && s.Provider == f.id {
			continue
		}

		s.Provider = f.id
		f.existing = append(f.existing, s)
	}

	return nil
}

func noEnv(string) bool { return false }

func allEnv(string) bool { return true }

func TestSecretName(t *testing.T) {
	for _, name := range []string{"GITHUB_TOKEN", "LINEAR_API_KEY", "NOTION_TOKEN", "DATABASE_URL", "Authorization", "password", "AWS_SECRET_ACCESS_KEY", "X-Api-Key", "SENTRY_AUTH"} {
		if !SecretName(name) {
			t.Errorf("%s should look like a credential", name)
		}
	}

	for _, name := range []string{"LOG_LEVEL", "PORT", "ALLOWED_DIRS", "Content-Type"} {
		if SecretName(name) {
			t.Errorf("%s should not look like a credential", name)
		}
	}
}

func TestEnvRefs(t *testing.T) {
	refs := EnvRefs("${GITHUB_TOKEN}", "plain", "${ROOT:-/tmp}/x", "Bearer ${API_KEY} ${GITHUB_TOKEN}")

	if !slices.Equal(refs, []string{"API_KEY", "GITHUB_TOKEN"}) {
		t.Fatalf("refs = %v", refs)
	}
}

func TestRedactArgs(t *testing.T) {
	args := []string{"-y", "server-postgres", "postgres://user:pw@localhost/db", "--token", "abc", "--api-key=xyz", "GITHUB_TOKEN=ghp", "LOG_LEVEL=debug", "https://example.com/path", "--port", "5432"}
	got, hidden := RedactArgs(args)
	want := []string{"-y", "server-postgres", "<redacted>", "--token", "<redacted>", "--api-key=<redacted>", "GITHUB_TOKEN=<redacted>", "LOG_LEVEL=debug", "https://example.com/path", "--port", "5432"}

	if !hidden || !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}

	if got, hidden := RedactArgs([]string{"-y", "fs", "/tmp"}); hidden || !slices.Equal(got, []string{"-y", "fs", "/tmp"}) {
		t.Fatalf("plain args changed: %q %v", got, hidden)
	}
}

func TestClassifyState(t *testing.T) {
	tests := []struct {
		name string
		s    Server
		want State
	}{
		{"http is remote", Server{Transport: HTTP, URL: "https://x"}, Stateless},
		{"filesystem unknown", Server{Transport: Stdio, Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"}}, StateUnknown},
		{"sqlite file", Server{Transport: Stdio, Command: "uvx", Args: []string{"mcp-server-sqlite", "--db-path", "/data/app.db"}}, Stateful},
		{"memory server", Server{Transport: Stdio, Command: "npx", Args: []string{"@modelcontextprotocol/server-memory"}}, Stateful},
		{"postgres", Server{Transport: Stdio, Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-postgres"}}, Stateful},
		{"store path env", Server{Transport: Stdio, Command: "x", EnvNames: []string{"MEMORY_FILE_PATH"}}, Stateful},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyState(tt.s); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestSnapshotNeverSerializesNativeDefinition(t *testing.T) {
	native := json.RawMessage(`{"command":"x","env":{"TOKEN":"super-secret"},"headers":{"Authorization":"Bearer super-secret"}}`)
	a := &fakeAdapter{id: "claude", existing: []Server{{Name: "s", Transport: Stdio, Command: "x", EnvNames: []string{"TOKEN"}, HeaderNames: []string{"Authorization"}, Native: native}}}
	snap, err := Take(a, "/p", "/proj", time.Unix(0, 0))

	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(snap)

	if strings.Contains(string(data), "super-secret") {
		t.Fatalf("snapshot leaks a secret:\n%s", data)
	}

	if !strings.Contains(string(data), `"env":["TOKEN"]`) || !strings.Contains(string(data), `"headers":["Authorization"]`) {
		t.Fatalf("snapshot should keep names:\n%s", data)
	}

	var back Snapshot

	if err := json.Unmarshal(data, &back); err != nil || len(back.Servers) != 1 || back.Servers[0].Native != nil {
		t.Fatalf("round trip = %+v %v", back, err)
	}

	rep := Handoff(snap, Target{Adapter: &fakeAdapter{id: "codex"}, Label: "work-codex"}, noEnv, true, time.Unix(0, 0))
	out := strings.Join(rep.Lines(), "\n") + rep.Summary()

	if strings.Contains(out, "super-secret") {
		t.Fatalf("report leaks a secret:\n%s", out)
	}
}

func servers() []Server {
	return []Server{
		{Name: "filesystem", Transport: Stdio, Command: "npx", Args: []string{"-y", "fs"}, Provider: "claude", Scope: ScopeUser},
		{Name: "github", Transport: Stdio, Command: "npx", Args: []string{"github-mcp"}, EnvNames: []string{"GITHUB_TOKEN"}, Provider: "claude", Scope: ScopeUser},
		{Name: "linear", Transport: HTTP, URL: "https://mcp.linear.app/mcp", OAuth: true, Provider: "claude", Scope: ScopeUser},
		{Name: "legacy", Transport: SSE, URL: "https://example.com/sse", Provider: "claude", Scope: ScopeUser},
		{Name: "memory", Transport: Stdio, Command: "npx", Args: []string{"@modelcontextprotocol/server-memory"}, Provider: "claude", Scope: ScopeLocal},
		{Name: "shared", Transport: Stdio, Command: "npx", Args: []string{"shared"}, Provider: "claude", Scope: ScopeProject, EnvRefs: []string{"SHARED_KEY"}},
		{Name: "postgres", Transport: Stdio, Command: "npx", Args: []string{"-y", "server-postgres", "postgres://u:pw@localhost/db"}, Provider: "claude", Scope: ScopeUser},
	}
}

func snapshot(t *testing.T, list []Server) Snapshot {
	t.Helper()
	src := &fakeAdapter{id: "claude", existing: list}
	snap, err := Take(src, "/a", "/proj", time.Unix(0, 0))

	if err != nil {
		t.Fatal(err)
	}

	return snap
}

func resultsByName(rep Report) map[string]Result {
	m := map[string]Result{}

	for _, r := range rep.Results {
		m[r.Server] = r
	}

	return m
}

func TestHandoffClaudeToClaude(t *testing.T) {
	target := &fakeAdapter{id: "claude", existing: []Server{{Name: "github", Transport: Stdio, Command: "other"}}}
	rep := Handoff(snapshot(t, servers()), Target{Adapter: target, ProfileDir: "/b", ProjectDir: "/proj", Label: "beta"}, noEnv, false, time.Unix(0, 0))
	got := resultsByName(rep)

	if got["filesystem"].Status != Migrated {
		t.Errorf("filesystem = %+v", got["filesystem"])
	}

	if got["github"].Status != Available || !strings.Contains(got["github"].Reason, "already configured in beta") {
		t.Errorf("target should win for github: %+v", got["github"])
	}

	if got["linear"].Status != RequiresAuth || !got["linear"].RequiresAuth {
		t.Errorf("an OAuth server must ask for a login: %+v", got["linear"])
	}

	if got["legacy"].Status != Unsupported {
		t.Errorf("fake target has no sse: %+v", got["legacy"])
	}

	if got["memory"].Status != Migrated || !got["memory"].Stateful || !strings.Contains(got["memory"].Reason, "runtime state stays") {
		t.Errorf("memory = %+v", got["memory"])
	}

	if got["shared"].Status != RequiresAuth || !strings.Contains(got["shared"].Reason, "SHARED_KEY") || strings.Contains(got["shared"].Reason, "=") {
		t.Errorf("missing env ref must be reported by name: %+v", got["shared"])
	}

	if !strings.Contains(got["shared"].Reason, "approve it in beta") {
		t.Errorf("an unapproved project server must say so: %+v", got["shared"])
	}

	names := make([]string, 0, len(target.written))

	for _, s := range target.written {
		names = append(names, s.Name)
	}

	if !slices.Equal(names, []string{"filesystem", "linear", "memory", "postgres"}) {
		t.Fatalf("written = %v (an approval that was never recorded must not be written)", names)
	}

	if got["postgres"].Status != Migrated || !got["postgres"].Stateful {
		t.Errorf("same-agent carry keeps a server whose arguments hold a credential: %+v", got["postgres"])
	}

	rep = Handoff(snapshot(t, servers()), Target{Adapter: &fakeAdapter{id: "claude"}, Label: "beta"}, allEnv, false, time.Unix(0, 0))

	if got := resultsByName(rep)["shared"]; got.Status != Unknown || !strings.Contains(got.Reason, "approve it in beta") {
		t.Fatalf("shared, not approved anywhere = %+v", got)
	}

	approved := servers()
	approved[5].Approved = true
	target = &fakeAdapter{id: "claude", existing: []Server{{Name: "shared", Transport: Stdio, Command: "npx", Scope: ScopeProject, Approved: true}}}
	rep = Handoff(snapshot(t, approved), Target{Adapter: target, Label: "beta"}, allEnv, false, time.Unix(0, 0))

	if got := resultsByName(rep)["shared"]; got.Status != Available || got.Reason != "shared through the project configuration" {
		t.Fatalf("shared and approved in the target = %+v", got)
	}

	for _, s := range target.written {
		if s.Name == "shared" {
			t.Fatal("an approval the target already has was rewritten")
		}
	}

	target = &fakeAdapter{id: "claude", existing: []Server{{Name: "shared", Transport: Stdio, Command: "npx", Scope: ScopeProject}}}
	rep = Handoff(snapshot(t, approved), Target{Adapter: target, Label: "beta"}, allEnv, false, time.Unix(0, 0))

	if got := resultsByName(rep)["shared"]; got.Status != Unknown || !slices.ContainsFunc(target.written, func(s Server) bool { return s.Name == "shared" }) {
		t.Fatalf("a recorded approval must be offered to the target even if it cannot keep it: %+v, written %d", got, len(target.written))
	}
}

func TestHandoffClaudeToOtherProviderNeverCarriesSecrets(t *testing.T) {
	target := &fakeAdapter{id: "codex"}
	rep := Handoff(snapshot(t, servers()), Target{Adapter: target, Label: "work-codex"}, allEnv, false, time.Unix(0, 0))
	got := resultsByName(rep)

	if got["filesystem"].Status != Migrated || got["filesystem"].RequiresAuth {
		t.Errorf("filesystem = %+v", got["filesystem"])
	}

	if got["github"].Status != RequiresAuth || !strings.Contains(got["github"].Reason, "set GITHUB_TOKEN in work-codex") {
		t.Errorf("github = %+v", got["github"])
	}

	if got["linear"].Status != RequiresAuth || !strings.Contains(got["linear"].Reason, "login") {
		t.Errorf("linear = %+v", got["linear"])
	}

	if got["legacy"].Status != Unsupported || !strings.Contains(got["legacy"].Reason, "unsupported by work-codex") {
		t.Errorf("legacy = %+v", got["legacy"])
	}

	if got["shared"].Status != Migrated {
		t.Errorf("a project server moves to another agent like any other: %+v", got["shared"])
	}

	if got["postgres"].Status != RequiresAuth || !strings.Contains(got["postgres"].Reason, "arguments contain a credential") {
		t.Errorf("postgres = %+v", got["postgres"])
	}

	if rep.From != "claude" || rep.To != "codex" || rep.Count(Unsupported) != 1 || rep.Count(RequiresAuth) != 3 {
		t.Fatalf("report = %+v", rep)
	}

	for _, s := range target.written {
		if s.Name == "legacy" || s.Name == "postgres" {
			t.Fatalf("%s was handed to Prepare", s.Name)
		}
	}

	out := strings.Join(rep.Lines(), "\n")

	if strings.Contains(out, "pw@") {
		t.Fatalf("report leaks a password:\n%s", out)
	}
}

func TestHandoffToAgentThatReadsEnvByName(t *testing.T) {
	src := []Server{
		{Name: "github", Transport: Stdio, Command: "npx", Args: []string{"gh"}, EnvNames: []string{"GITHUB_TOKEN"}, EnvRefs: []string{"OLD_REF"}, Provider: "claude", Scope: ScopeUser},
		{Name: "both", Transport: Stdio, Command: "x", EnvNames: []string{"A_TOKEN", "B_KEY"}, Provider: "claude", Scope: ScopeUser},
	}
	present := func(name string) bool { return name == "GITHUB_TOKEN" || name == "A_TOKEN" }
	target := &fakeAdapter{id: "codex", envByName: true}
	rep := Handoff(snapshot(t, src), Target{Adapter: target, Label: "work-codex"}, present, false, time.Unix(0, 0))
	got := resultsByName(rep)

	if got["github"].Status != Migrated || got["github"].Reason != "reads GITHUB_TOKEN from the environment" {
		t.Errorf("a reference inside a value that does not travel must not count: %+v", got["github"])
	}

	if got["both"].Status != RequiresAuth || got["both"].Reason != "MCP requires missing environment variable: B_KEY" {
		t.Errorf("both = %+v", got["both"])
	}

	if len(target.written) != 2 {
		t.Fatalf("both servers should still be written with their variable names: %d", len(target.written))
	}
}

func TestHandoffDegradesInsteadOfFailing(t *testing.T) {
	snap := snapshot(t, servers())

	rep := Handoff(snap, Target{Adapter: &fakeAdapter{id: "claude", parseErr: errors.New("boom")}, Label: "beta"}, noEnv, false, time.Unix(0, 0))

	for _, r := range rep.Results {
		if r.Status != Failed {
			t.Fatalf("unreadable target must fail every server: %+v", r)
		}
	}

	rep = Handoff(snap, Target{Adapter: &fakeAdapter{id: "claude", prepErr: errors.New("disk full")}, Label: "beta"}, noEnv, false, time.Unix(0, 0))
	got := resultsByName(rep)

	if got["filesystem"].Status != Failed || !strings.Contains(got["filesystem"].Reason, "disk full") {
		t.Fatalf("write failure not reported: %+v", got["filesystem"])
	}

	if got["legacy"].Status != Unsupported {
		t.Fatalf("a write failure must not rewrite other verdicts: %+v", got["legacy"])
	}

	dry := &fakeAdapter{id: "claude"}
	rep = Handoff(snap, Target{Adapter: dry, Label: "beta"}, noEnv, true, time.Unix(0, 0))

	if len(dry.written) != 0 || !rep.DryRun {
		t.Fatal("a dry run wrote something")
	}

	rep = Handoff(snapshot(t, []Server{{Name: "../evil", Transport: Stdio, Command: "x", Provider: "claude"}}), Target{Adapter: dry, Label: "beta"}, noEnv, false, time.Unix(0, 0))

	if rep.Results[0].Status != Unsupported || len(dry.written) != 0 {
		t.Fatalf("a server name with a path was accepted: %+v", rep.Results[0])
	}
}

func TestInspect(t *testing.T) {
	a := &fakeAdapter{id: "claude", existing: []Server{
		{Name: "ok", Transport: Stdio, Command: "x"},
		{Name: "needs", Transport: Stdio, Command: "x", EnvRefs: []string{"LINEAR_API_KEY"}},
		{Name: "login", Transport: HTTP, URL: "https://x", OAuth: true},
		{Name: "off", Transport: Stdio, Command: "x", Disabled: true},
	}}
	results, err := Inspect(a, "/p", "/proj", noEnv)

	if err != nil {
		t.Fatal(err)
	}

	lines := make([]string, 0, len(results))

	for _, r := range results {
		lines = append(lines, r.Line())
	}

	want := []string{
		"⚠ login requires authentication: needs a login in this profile",
		"⚠ needs requires authentication: MCP requires missing environment variable: LINEAR_API_KEY",
		"✓ off disabled for this project",
		"✓ ok",
	}

	if !slices.Equal(lines, want) {
		t.Fatalf("lines = %q", lines)
	}

	if _, err := Inspect(&fakeAdapter{id: "x", parseErr: errors.New("bad json")}, "/p", "/proj", noEnv); err == nil {
		t.Fatal("parse error swallowed")
	}
}

func TestReportRendering(t *testing.T) {
	rep := Report{Results: []Result{
		{Server: "filesystem", Status: Migrated},
		{Server: "github", Status: Available, Reason: "already configured in beta"},
		{Server: "linear", Status: RequiresAuth, RequiresAuth: true},
		{Server: "custom", Status: Failed, Reason: "could not write"},
	}}

	if got := rep.Lines(); !slices.Equal(got, []string{"✓ filesystem", "✓ github already configured in beta", "⚠ linear requires authentication", "✗ custom could not write"}) {
		t.Fatalf("lines = %q", got)
	}

	if !strings.HasPrefix(rep.Summary(), "MCP handoff: filesystem migrated; github available (already configured in beta); linear requires_auth;") {
		t.Fatalf("summary = %q", rep.Summary())
	}

	if (Report{}).Summary() != "" {
		t.Fatal("an empty report should have no summary")
	}
}
