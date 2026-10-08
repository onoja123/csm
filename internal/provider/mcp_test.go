package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/mcp"
)

// claudeConfigProbe is what claude 2.1.294 wrote for `claude mcp add` at user and local scope, with other state kept around it.
const claudeConfigProbe = `{
  "numStartups": 3,
  "hasCompletedOnboarding": true,
  "mcpServers": {
    "sentry": {"type": "http", "url": "https://mcp.sentry.dev/mcp", "headers": {"Authorization": "Bearer probe-token"}},
    "legacy": {"type": "sse", "url": "https://example.com/sse"},
    "viajson": {"type": "stdio", "command": "uvx", "args": ["mcp-server-git"], "env": {"DATABASE_URL": "postgres://u:super-secret@h/db"}}
  },
  "projects": {
    "PROJECT": {
      "allowedTools": [],
      "mcpContextUris": [],
      "mcpServers": {"localonly": {"type": "stdio", "command": "echo", "args": ["hi"], "env": {}}},
      "enabledMcpjsonServers": ["projfs"],
      "disabledMcpjsonServers": ["projoff"],
      "hasTrustDialogAccepted": true
    }
  }
}`

const projectMCPProbe = `{"mcpServers": {
  "projfs": {"type": "stdio", "command": "npx", "args": ["-y", "github-mcp"], "env": {"GITHUB_TOKEN": "${GITHUB_TOKEN}"}},
  "projoff": {"command": "npx", "args": ["off"]}
}}`

func writeClaudeProfile(t *testing.T, config string) (profile, project string) {
	t.Helper()
	profile, project = t.TempDir(), t.TempDir()

	if config != "" {
		os.WriteFile(filepath.Join(profile, claudeConfigFile), []byte(strings.ReplaceAll(config, "PROJECT", project)), 0o600)
	}

	return profile, project
}

func serverMap(list []mcp.Server) map[string]mcp.Server {
	m := map[string]mcp.Server{}

	for _, s := range list {
		m[s.Name] = s
	}

	return m
}

func TestClaudeMCPParse(t *testing.T) {
	profile, project := writeClaudeProfile(t, claudeConfigProbe)
	os.WriteFile(filepath.Join(project, claudeProjectMCP), []byte(projectMCPProbe), 0o644)
	os.WriteFile(filepath.Join(profile, claudeMCPAuthCache), []byte(`{"sentry": {"needsAuth": true}}`), 0o600)

	servers, err := Claude{}.MCP().Parse(profile, project)
	if err != nil {
		t.Fatal(err)
	}

	got := serverMap(servers)

	if len(got) != 6 {
		t.Fatalf("got %d servers: %v", len(got), got)
	}

	if s := got["sentry"]; s.Transport != mcp.HTTP || s.URL != "https://mcp.sentry.dev/mcp" || !slices.Equal(s.HeaderNames, []string{"Authorization"}) || s.Scope != mcp.ScopeUser || !s.OAuth {
		t.Errorf("sentry = %+v", s)
	}

	if s := got["legacy"]; s.Transport != mcp.SSE {
		t.Errorf("legacy = %+v", s)
	}

	if s := got["viajson"]; s.Transport != mcp.Stdio || s.Command != "uvx" || !slices.Equal(s.Args, []string{"mcp-server-git"}) || !slices.Equal(s.EnvNames, []string{"DATABASE_URL"}) || s.OAuth {
		t.Errorf("viajson = %+v", s)
	}

	if s := got["localonly"]; s.Scope != mcp.ScopeLocal || s.Command != "echo" || len(s.EnvNames) != 0 {
		t.Errorf("localonly = %+v", s)
	}

	if s := got["projfs"]; s.Scope != mcp.ScopeProject || s.Disabled || !slices.Equal(s.EnvRefs, []string{"GITHUB_TOKEN"}) || s.Source != filepath.Join(project, claudeProjectMCP) {
		t.Errorf("projfs = %+v", s)
	}

	if s := got["projoff"]; !s.Disabled || s.Transport != mcp.Stdio {
		t.Errorf("projoff = %+v", s)
	}

	snap, err := mcp.Take(Claude{}.MCP(), profile, project, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.MarshalIndent(snap, "", "  ")

	for _, secret := range []string{"super-secret", "probe-token"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("snapshot contains %q:\n%s", secret, data)
		}
	}

	if got["viajson"].State != "" {
		t.Fatal("Parse should leave classification to the snapshot")
	}

	if s := serverMap(snap.Servers)["viajson"]; s.State != mcp.Stateful {
		t.Errorf("a server with DATABASE_URL should look stateful: %+v", s)
	}
}

func TestClaudeMCPParseTolerance(t *testing.T) {
	profile, project := writeClaudeProfile(t, "")

	if servers, err := (Claude{}).MCP().Parse(profile, project); err != nil || len(servers) != 0 {
		t.Fatalf("missing files: %v %v", servers, err)
	}

	os.WriteFile(filepath.Join(profile, claudeConfigFile), []byte(`{"mcpServers": {"x": {"type": "http", "url": "https://x"}}}`), 0o600)

	if servers, err := (Claude{}).MCP().Parse(profile, project); err != nil || len(servers) != 1 || len(servers[0].EnvNames) != 0 {
		t.Fatalf("missing fields: %v %v", servers, err)
	}

	os.WriteFile(filepath.Join(profile, claudeConfigFile), []byte(`{"mcpServers": {"x": "not an object"}}`), 0o600)

	if _, err := (Claude{}).MCP().Parse(profile, project); err == nil {
		t.Fatal("a malformed server was accepted")
	}

	os.WriteFile(filepath.Join(profile, claudeConfigFile), []byte(`{"mcpServers": {`), 0o600)

	if _, err := (Claude{}).MCP().Parse(profile, project); err == nil || strings.Contains(err.Error(), "super") {
		t.Fatalf("malformed JSON: %v", err)
	}

	os.WriteFile(filepath.Join(profile, claudeConfigFile), []byte(`{}`), 0o600)
	os.WriteFile(filepath.Join(project, claudeProjectMCP), []byte(`{"mcpServers": []}`), 0o644)

	if _, err := (Claude{}).MCP().Parse(profile, project); err == nil {
		t.Fatal("a malformed .mcp.json was accepted")
	}
}

func TestClaudeMCPPrepareMergesIntoTargetProfile(t *testing.T) {
	from, project := writeClaudeProfile(t, claudeConfigProbe)
	os.WriteFile(filepath.Join(project, claudeProjectMCP), []byte(projectMCPProbe), 0o644)
	os.WriteFile(filepath.Join(from, claudeMCPAuthCache), []byte(`{"sentry": {"needsAuth": true}}`), 0o600)
	to := t.TempDir()
	targetConfig := `{"oauthAccount": {"emailAddress": "beta@example.test"}, "mcpServers": {"legacy": {"type": "sse", "url": "https://beta.example.com/sse"}}, "projects": {"PROJECT": {"hasTrustDialogAccepted": true, "allowedTools": ["Bash"]}}}`
	os.WriteFile(filepath.Join(to, claudeConfigFile), []byte(strings.ReplaceAll(targetConfig, "PROJECT", project)), 0o600)

	snap, err := mcp.Take(Claude{}.MCP(), from, project, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("GITHUB_TOKEN", "present")
	rep := mcp.Handoff(snap, mcp.Target{Adapter: Claude{}.MCP(), ProfileDir: to, ProjectDir: project, Label: "beta"}, func(name string) bool { return os.Getenv(name) != "" }, false, time.Now())
	got := map[string]mcp.Result{}

	for _, r := range rep.Results {
		got[r.Server] = r
	}

	if got["viajson"].Status != mcp.Migrated || got["localonly"].Status != mcp.Migrated {
		t.Fatalf("results = %+v", rep.Results)
	}

	if got["legacy"].Status != mcp.Available {
		t.Fatalf("target must win: %+v", got["legacy"])
	}

	if got["sentry"].Status != mcp.RequiresAuth {
		t.Fatalf("an OAuth login does not move: %+v", got["sentry"])
	}

	if got["projfs"].Status != mcp.Available || got["projoff"].Status != mcp.Available {
		t.Fatalf("project servers are shared: %+v %+v", got["projfs"], got["projoff"])
	}

	var written map[string]json.RawMessage
	data, _ := os.ReadFile(filepath.Join(to, claudeConfigFile))

	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(written["oauthAccount"]), "beta@example.test") {
		t.Fatal("unrelated keys were lost")
	}

	user, _ := rawObject(written[claudeMCPServersKey])

	if !strings.Contains(string(user["legacy"]), "beta.example.com") {
		t.Fatal("the target's own definition was overwritten")
	}

	if !strings.Contains(string(user["viajson"]), "super-secret") {
		t.Fatal("a Claude to Claude carry keeps the server's own definition so it keeps working")
	}

	if !strings.Contains(string(user["sentry"]), "probe-token") {
		t.Fatal("the static header travels with the definition; only the OAuth login stays behind")
	}

	entry, err := claudeProjectEntry(written, project)
	if err != nil {
		t.Fatal(err)
	}

	local, _ := rawObject(entry[claudeMCPServersKey])

	if _, ok := local["localonly"]; !ok {
		t.Fatalf("local scope server missing: %s", data)
	}

	if !slices.Equal(rawStrings(entry[claudeEnabledKey]), []string{"projfs"}) || !slices.Equal(rawStrings(entry[claudeDisabledKey]), []string{"projoff"}) {
		t.Fatalf("project approvals not carried into a trusted directory: %s", entry)
	}

	if !strings.Contains(string(entry["allowedTools"]), "Bash") || string(entry[claudeTrustKey]) != "true" {
		t.Fatalf("the target's own project entry was damaged: %s", entry)
	}

	fi, _ := os.Stat(filepath.Join(to, claudeConfigFile))

	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", fi.Mode().Perm())
	}

	again := mcp.Handoff(snap, mcp.Target{Adapter: Claude{}.MCP(), ProfileDir: to, ProjectDir: project, Label: "beta"}, func(string) bool { return true }, false, time.Now())

	if again.Count(mcp.Migrated) != 0 || again.Count(mcp.Available) != 6 {
		t.Fatalf("a second handoff should find everything in place: %+v", again.Results)
	}

	after, _ := os.ReadFile(filepath.Join(to, claudeConfigFile))

	if string(after) != string(data) {
		t.Fatal("a no-op handoff rewrote the file")
	}
}

func TestClaudeMCPApprovalNeedsTrustedTarget(t *testing.T) {
	from, project := writeClaudeProfile(t, claudeConfigProbe)
	os.WriteFile(filepath.Join(project, claudeProjectMCP), []byte(projectMCPProbe), 0o644)
	to := t.TempDir()
	snap, _ := mcp.Take(Claude{}.MCP(), from, project, time.Now())
	rep := mcp.Handoff(snap, mcp.Target{Adapter: Claude{}.MCP(), ProfileDir: to, ProjectDir: project, Label: "beta"}, func(string) bool { return true }, false, time.Now())

	for _, r := range rep.Results {
		if (r.Server == "projfs" || r.Server == "projoff") && (r.Status != mcp.Unknown || !strings.Contains(r.Reason, "approve it in beta")) {
			t.Errorf("%+v", r)
		}
	}

	config, _ := readJSONObject(filepath.Join(to, claudeConfigFile))
	entry, _ := claudeProjectEntry(config, project)

	if _, ok := entry[claudeEnabledKey]; ok {
		t.Fatalf("approvals written into an untrusted directory: %s", entry)
	}

	if _, ok := entry[claudeTrustKey]; ok {
		t.Fatal("csm must never grant directory trust")
	}
}

func TestClaudeMCPPrepareFromAnotherAgentDropsSecrets(t *testing.T) {
	to := t.TempDir()
	servers := []mcp.Server{
		{Name: "gh", Transport: mcp.Stdio, Command: "npx", Args: []string{"github-mcp"}, EnvNames: []string{"GITHUB_TOKEN"}, Provider: CodexID, Native: json.RawMessage(`{"command":"npx","env":{"GITHUB_TOKEN":"super-secret"}}`)},
		{Name: "remote", Transport: mcp.HTTP, URL: "https://x/mcp", HeaderNames: []string{"Authorization"}, Provider: CodexID},
	}

	if err := (Claude{}).MCP().Prepare(to, t.TempDir(), servers); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(to, claudeConfigFile))

	if strings.Contains(string(data), "super-secret") || strings.Contains(string(data), "GITHUB_TOKEN") {
		t.Fatalf("secrets or env leaked into Claude config:\n%s", data)
	}

	if !strings.Contains(string(data), `"command": "npx"`) || !strings.Contains(string(data), `"url": "https://x/mcp"`) {
		t.Fatalf("definitions missing:\n%s", data)
	}

	fi, _ := os.Stat(filepath.Join(to, claudeConfigFile))

	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode = %o", fi.Mode().Perm())
	}
}

func TestWriteConfigFileRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	os.WriteFile(real, []byte(`{"mine": true}`), 0o644)
	link := filepath.Join(dir, "link.json")
	os.Symlink(real, link)

	err := writeConfigFile(link, []byte(`{}`))

	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("wrote through a symlink: %v", err)
	}

	if data, _ := os.ReadFile(real); string(data) != `{"mine": true}` {
		t.Fatal("the linked file was modified")
	}

	if err := writeConfigFile(real, []byte(`{"changed": true}`)); err != nil {
		t.Fatal(err)
	}

	fi, _ := os.Stat(real)

	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode changed to %o", fi.Mode().Perm())
	}

	if matches, _ := filepath.Glob(filepath.Join(dir, "*.csm-tmp")); len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}
}

const codexConfigProbe = `# Codex config
model = "gpt-5"

[mcp_servers.filesystem]
command = "npx"
args = ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]

[mcp_servers.github]
command = "github-mcp"
env = { GITHUB_TOKEN = "super-secret", LOG_LEVEL = "debug" }
enabled = false

[mcp_servers.remote]
url = "https://mcp.example.com/mcp"
bearer_token_env_var = "REMOTE_TOKEN"
startup_timeout_sec = 20

[mcp_servers.remote.http_headers]
"X-Org" = "acme"

[mcp_servers.remote.env_http_headers]
"X-Api-Key" = "REMOTE_API_KEY"
`

func TestCodexMCPParseAndPrepare(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	os.WriteFile(codexConfigPath(from), []byte(codexConfigProbe), 0o600)

	servers, err := Codex{}.MCP().Parse(from, "")
	if err != nil {
		t.Fatal(err)
	}

	got := serverMap(servers)

	if s := got["filesystem"]; s.Transport != mcp.Stdio || !slices.Equal(s.Args, []string{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"}) {
		t.Errorf("filesystem = %+v", s)
	}

	if s := got["github"]; !slices.Equal(s.EnvNames, []string{"GITHUB_TOKEN", "LOG_LEVEL"}) || !s.Disabled {
		t.Errorf("github = %+v", s)
	}

	if s := got["remote"]; s.Transport != mcp.HTTP || !slices.Equal(s.EnvNames, []string{"REMOTE_API_KEY", "REMOTE_TOKEN"}) || !slices.Equal(s.HeaderNames, []string{"X-Api-Key", "X-Org"}) {
		t.Errorf("remote = %+v", s)
	}

	os.WriteFile(codexConfigPath(to), []byte("model = \"gpt-5\"\n\n[mcp_servers.github]\ncommand = \"mine\"\n"), 0o600)
	snap, _ := mcp.Take(Codex{}.MCP(), from, "", time.Now())
	rep := mcp.Handoff(snap, mcp.Target{Adapter: Codex{}.MCP(), ProfileDir: to, Label: "beta"}, func(string) bool { return false }, false, time.Now())

	for _, r := range rep.Results {
		if r.Status != mcp.Migrated && r.Server != "github" {
			t.Errorf("%+v", r)
		}
	}

	data, _ := os.ReadFile(codexConfigPath(to))
	text := string(data)

	if !strings.HasPrefix(text, "model = \"gpt-5\"\n\n[mcp_servers.github]\ncommand = \"mine\"\n") {
		t.Fatalf("existing text changed:\n%s", text)
	}

	if strings.Count(text, "[mcp_servers.github]") != 1 || !strings.Contains(text, "[mcp_servers.filesystem]") || !strings.Contains(text, "[mcp_servers.remote.env_http_headers]") {
		t.Fatalf("appended tables wrong:\n%s", text)
	}

	reparsed, err := Codex{}.MCP().Parse(to, "")
	if err != nil {
		t.Fatalf("written config does not parse: %v\n%s", err, text)
	}

	if r := serverMap(reparsed)["remote"]; !slices.Equal(r.HeaderNames, []string{"X-Api-Key", "X-Org"}) || !slices.Equal(r.EnvNames, []string{"REMOTE_API_KEY", "REMOTE_TOKEN"}) {
		t.Fatalf("round trip lost fields: %+v", r)
	}
}

func TestCodexMCPFromClaude(t *testing.T) {
	to := t.TempDir()
	servers := []mcp.Server{
		{Name: "github", Transport: mcp.Stdio, Command: "npx", Args: []string{"-y", "github-mcp"}, EnvNames: []string{"GITHUB_TOKEN"}, Provider: ClaudeID, Native: json.RawMessage(`{"env":{"GITHUB_TOKEN":"super-secret"}}`)},
		{Name: "sentry", Transport: mcp.HTTP, URL: "https://mcp.sentry.dev/mcp", Provider: ClaudeID},
		{Name: "it's odd", Transport: mcp.Stdio, Command: "x", Provider: ClaudeID},
	}

	if err := (Codex{}).MCP().Prepare(to, "", servers); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(codexConfigPath(to))
	text := string(data)

	if strings.Contains(text, "super-secret") || strings.Contains(text, "[mcp_servers.github.env]") {
		t.Fatalf("secret or env values written for another agent:\n%s", text)
	}

	for _, want := range []string{"[mcp_servers.github]\nargs = [\"-y\", \"github-mcp\"]\ncommand = \"npx\"\nenv_vars = [\"GITHUB_TOKEN\"]\n", "[mcp_servers.sentry]\nurl = \"https://mcp.sentry.dev/mcp\"\n", "[mcp_servers.\"it's odd\"]\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}

	if _, err := parseTOML(text); err != nil {
		t.Fatalf("invalid TOML written: %v", err)
	}

	back, err := Codex{}.MCP().Parse(to, "")
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(serverMap(back)["github"].EnvNames, []string{"GITHUB_TOKEN"}) {
		t.Fatalf("env_vars not read back: %+v", serverMap(back)["github"])
	}

	if err := (Codex{}).MCP().Validate(mcp.Server{Name: "sse", Transport: mcp.SSE}); err == nil {
		t.Fatal("Codex has no SSE transport")
	}
}

func TestParseTOML(t *testing.T) {
	cfg, err := parseTOML("title = 'x' # c\nn = 1_000\nf = 1.5\nb = true\nd = 2024-01-01\nm = \"\"\"\nmulti\nline\"\"\"\narr = [\n  \"a\",\n  'b', # trailing\n]\n[a.\"b c\"]\ninline = { x = \"\\u00e9\\n\", y = [1, 2] }\n[[list]]\nk = 1\n[[list]]\nk = 2\n")
	if err != nil {
		t.Fatal(err)
	}

	if cfg["title"] != "x" || cfg["n"] != int64(1000) || cfg["f"] != 1.5 || cfg["b"] != true || cfg["d"] != "2024-01-01" || cfg["m"] != "multi\nline" {
		t.Fatalf("scalars = %#v", cfg)
	}

	inline := cfg["a"].(tomlTable)["b c"].(tomlTable)["inline"].(tomlTable)

	if inline["x"] != "é\n" || len(inline["y"].([]any)) != 2 {
		t.Fatalf("inline = %#v", inline)
	}

	if list := cfg["list"].([]any); len(list) != 2 || list[1].(tomlTable)["k"] != int64(2) {
		t.Fatalf("array of tables = %#v", cfg["list"])
	}

	for _, bad := range []string{"a = ", "a = \"open", "[t\nk = 1", "a = 1\na = 2", "= 3"} {
		if _, err := parseTOML(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestCopilotMCP(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	os.WriteFile(copilotMCPPath(from), []byte(`{"mcpServers": {
	  "fs": {"type": "local", "command": "npx", "args": ["-y", "fs"], "tools": ["*"], "env": {"TOKEN": "super-secret"}},
	  "remote": {"type": "http", "url": "https://x/mcp", "headers": {"Authorization": "Bearer super-secret"}}
	}}`), 0o600)

	servers, err := Copilot{}.MCP().Parse(from, "")
	if err != nil {
		t.Fatal(err)
	}

	got := serverMap(servers)

	if got["fs"].Transport != mcp.Stdio || !slices.Equal(got["fs"].EnvNames, []string{"TOKEN"}) || got["remote"].Transport != mcp.HTTP || !slices.Equal(got["remote"].HeaderNames, []string{"Authorization"}) {
		t.Fatalf("servers = %+v", got)
	}

	snap, _ := mcp.Take(Copilot{}.MCP(), from, "", time.Now())
	os.WriteFile(copilotMCPPath(to), []byte(`{"mcpServers": {"fs": {"type": "local", "command": "mine"}}}`), 0o600)
	rep := mcp.Handoff(snap, mcp.Target{Adapter: Copilot{}.MCP(), ProfileDir: to, Label: "beta"}, func(string) bool { return false }, false, time.Now())
	data, _ := os.ReadFile(copilotMCPPath(to))

	if rep.Count(mcp.Migrated) != 1 || !strings.Contains(string(data), `"command": "mine"`) || !strings.Contains(string(data), "Bearer super-secret") {
		t.Fatalf("same-agent carry: %+v\n%s", rep.Results, data)
	}

	claude := []mcp.Server{{Name: "gh", Transport: mcp.Stdio, Command: "npx", Args: []string{"gh"}, EnvNames: []string{"GITHUB_TOKEN"}, Provider: ClaudeID, Native: json.RawMessage(`{"env":{"GITHUB_TOKEN":"super-secret"}}`)}}

	if err := (Copilot{}).MCP().Prepare(to, "", claude); err != nil {
		t.Fatal(err)
	}

	data, _ = os.ReadFile(copilotMCPPath(to))

	if strings.Contains(string(data), "GITHUB_TOKEN") || !strings.Contains(string(data), `"type": "local"`) || !strings.Contains(string(data), `"tools"`) {
		t.Fatalf("cross-agent definition:\n%s", data)
	}

	os.Remove(copilotMCPPath(to))
	os.Symlink(copilotMCPPath(from), copilotMCPPath(to))
	rep = mcp.Handoff(snap, mcp.Target{Adapter: Copilot{}.MCP(), ProfileDir: to, Label: "beta"}, func(string) bool { return false }, false, time.Now())

	if rep.Count(mcp.Available) != 2 {
		t.Fatalf("a linked config is the same config: %+v", rep.Results)
	}

	os.Remove(copilotMCPPath(to))
	os.Symlink(filepath.Join(t.TempDir(), "mcp-config.json"), copilotMCPPath(to))
	rep = mcp.Handoff(snap, mcp.Target{Adapter: Copilot{}.MCP(), ProfileDir: to, Label: "beta"}, func(string) bool { return false }, false, time.Now())

	if rep.Count(mcp.Failed) != 2 || !strings.Contains(rep.Results[0].Reason, "symlink") {
		t.Fatalf("writing through a dangling symlink: %+v", rep.Results)
	}
}

func TestGeminiMCP(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	os.MkdirAll(geminiDir(from), 0o700)
	os.WriteFile(geminiSettingsPath(from), []byte(`{"theme": "dark", "mcpServers": {
	  "fs": {"command": "npx", "args": ["fs"], "env": {"TOKEN": "$TOKEN"}},
	  "http": {"httpUrl": "https://x/mcp"},
	  "sse": {"url": "https://x/sse"},
	  "typed": {"url": "https://x/mcp", "type": "http", "headers": {"Authorization": "Bearer s"}}
	}}`), 0o600)

	servers, err := Gemini{}.MCP().Parse(from, "")
	if err != nil {
		t.Fatal(err)
	}

	got := serverMap(servers)

	if got["fs"].Transport != mcp.Stdio || got["http"].Transport != mcp.HTTP || got["http"].URL != "https://x/mcp" || got["sse"].Transport != mcp.SSE || got["typed"].Transport != mcp.HTTP {
		t.Fatalf("servers = %+v", got)
	}

	snap, _ := mcp.Take(Gemini{}.MCP(), from, "", time.Now())
	rep := mcp.Handoff(snap, mcp.Target{Adapter: Gemini{}.MCP(), ProfileDir: to, Label: "beta"}, func(string) bool { return false }, false, time.Now())

	if rep.Count(mcp.Migrated) != 4 {
		t.Fatalf("%+v", rep.Results)
	}

	reparsed, err := Gemini{}.MCP().Parse(to, "")

	if err != nil || len(reparsed) != 4 {
		t.Fatalf("%v %v", reparsed, err)
	}

	fromClaude := []mcp.Server{
		{Name: "c", Transport: mcp.HTTP, URL: "https://c/mcp", Provider: ClaudeID},
		{Name: "gh", Transport: mcp.Stdio, Command: "npx", Args: []string{"gh"}, EnvNames: []string{"GITHUB_TOKEN"}, Provider: ClaudeID, Native: json.RawMessage(`{"env":{"GITHUB_TOKEN":"super-secret"}}`)},
	}

	if err := (Gemini{}).MCP().Prepare(to, "", fromClaude); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(geminiSettingsPath(to))

	if !strings.Contains(string(data), `"type": "http"`) || !strings.Contains(string(data), `"url": "https://c/mcp"`) {
		t.Fatalf("http server should be written as gemini mcp add writes it:\n%s", data)
	}

	if !strings.Contains(string(data), `"GITHUB_TOKEN": "$GITHUB_TOKEN"`) || strings.Contains(string(data), "super-secret") {
		t.Fatalf("env should be a $NAME reference, never a value:\n%s", data)
	}

	os.WriteFile(geminiSettingsPath(from), []byte(`{"mcpServers": {"x": {"timeout": 5}}}`), 0o600)

	if _, err := (Gemini{}).MCP().Parse(from, ""); err == nil {
		t.Fatal("a server without a command or URL was accepted")
	}
}

func TestEveryProviderHasAnMCPAdapter(t *testing.T) {
	for _, id := range IDs {
		p, _ := New(id, "")
		a, ok := MCPAdapter(p)

		if !ok || a.Provider() != id {
			t.Fatalf("%s: adapter %v %v", id, a, ok)
		}
	}
}
