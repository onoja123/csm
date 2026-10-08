package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/onoja123/csm/internal/mcp"
	"github.com/onoja123/csm/internal/provider"
)

const alphaMCPConfig = `{
  "numStartups": 1,
  "mcpServers": {
    "filesystem": {"type": "stdio", "command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]},
    "github": {"type": "stdio", "command": "npx", "args": ["github-mcp"], "env": {"GITHUB_TOKEN": "${CSM_TEST_ABSENT_TOKEN}"}},
    "memory": {"type": "stdio", "command": "npx", "args": ["@modelcontextprotocol/server-memory"], "env": {"MEMORY_FILE_PATH": "/tmp/memory.jsonl"}},
    "linear": {"type": "http", "url": "https://mcp.linear.app/mcp", "headers": {"Authorization": "Bearer super-secret-value"}},
    "postgres": {"type": "stdio", "command": "npx", "args": ["-y", "@modelcontextprotocol/server-postgres", "postgres://user:db-secret-value@localhost/db"]}
  }
}`

func writeAlphaMCP(config string) func(s *State, projectDir string) {
	return func(s *State, projectDir string) {
		alpha := filepath.Join(s.accountsDir(), "alpha")
		os.WriteFile(filepath.Join(alpha, ".claude.json"), []byte(config), 0o600)
		os.WriteFile(filepath.Join(alpha, "mcp-needs-auth-cache.json"), []byte(`{"linear": {}}`), 0o600)
	}
}

func TestFailoverCarriesMCPConfiguration(t *testing.T) {
	os.Unsetenv("CSM_TEST_ABSENT_TOKEN")
	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha", before: writeAlphaMCP(alphaMCPConfig)})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}

	if res.active != "beta" || !res.resumed {
		t.Fatalf("active=%s resumed=%v\n%s", res.active, res.resumed, res.output)
	}

	for _, want := range []string{"Carrying MCP configuration", "MCP handoff:", "✓ filesystem", "✓ memory", "runtime state stays with the server", "⚠ github requires authentication: MCP requires missing environment variable: CSM_TEST_ABSENT_TOKEN", "⚠ linear requires authentication: needs a login in beta"} {
		if !strings.Contains(res.output, want) {
			t.Errorf("output missing %q:\n%s", want, res.output)
		}
	}

	for name, content := range map[string]string{"output": res.output, "snapshot": res.files["mcp-snapshot"], "handoff": res.files["mcp-handoff"]} {
		if content == "" {
			t.Errorf("%s is empty", name)
		}

		for _, secret := range []string{"super-secret-value", "db-secret-value"} {
			if strings.Contains(content, secret) {
				t.Errorf("%s leaks %q:\n%s", name, secret, content)
			}
		}
	}

	var snap mcp.Snapshot

	if err := json.Unmarshal([]byte(res.files["mcp-snapshot"]), &snap); err != nil || len(snap.Servers) != 5 || snap.Provider != provider.ClaudeID {
		t.Fatalf("snapshot = %+v %v", snap, err)
	}

	for _, s := range snap.Servers {
		if s.Name == "postgres" && (!s.SecretArgs || !slices.Contains(s.Args, "<redacted>")) {
			t.Fatalf("DSN argument not redacted: %+v", s)
		}
	}

	var rep mcp.Report

	if err := json.Unmarshal([]byte(res.files["mcp-handoff"]), &rep); err != nil || rep.Count(mcp.Migrated) != 3 || rep.Count(mcp.RequiresAuth) != 2 {
		t.Fatalf("handoff = %+v %v", rep, err)
	}

	if !strings.Contains(res.files["beta-config"], "Bearer super-secret-value") || !strings.Contains(res.files["beta-config"], "db-secret-value") {
		t.Fatalf("a Claude to Claude switch carries the definitions as they are:\n%s", res.files["beta-config"])
	}

	var beta map[string]json.RawMessage

	if err := json.Unmarshal([]byte(res.files["beta-config"]), &beta); err != nil {
		t.Fatalf("beta config: %v\n%s", err, res.files["beta-config"])
	}

	var servers map[string]json.RawMessage
	json.Unmarshal(beta["mcpServers"], &servers)

	for _, name := range []string{"filesystem", "github", "memory", "linear", "postgres"} {
		if _, ok := servers[name]; !ok {
			t.Errorf("beta is missing %s:\n%s", name, res.files["beta-config"])
		}
	}
}

func TestFailoverSurvivesMalformedMCPConfiguration(t *testing.T) {
	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha", before: writeAlphaMCP(`{"mcpServers": {`)})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}

	if res.active != "beta" || !res.resumed {
		t.Fatalf("a broken MCP config blocked the switch: active=%s resumed=%v\n%s", res.active, res.resumed, res.output)
	}

	if !strings.Contains(res.output, "Carrying MCP configuration (unreadable)") || strings.Contains(res.output, "MCP handoff:") {
		t.Fatalf("output:\n%s", res.output)
	}

	if _, ok := res.files["beta-config"]; ok {
		t.Fatal("beta's config was written from a broken source")
	}
}

func TestFailoverWithoutMCPStaysQuiet(t *testing.T) {
	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha"})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}

	if strings.Contains(res.output, "MCP") {
		t.Fatalf("MCP mentioned with nothing configured:\n%s", res.output)
	}

	if _, ok := res.files["beta-config"]; ok {
		t.Fatal("an empty handoff created a config file")
	}
}

func TestMCPHandoffCommandAcrossProviders(t *testing.T) {
	s := newTestState(t, "alpha")
	codexAcct, err := s.addAccount(provider.CodexID, "work-codex", testNow)
	if err != nil {
		t.Fatal(err)
	}

	alpha, _ := s.resolveAccount("alpha")
	os.MkdirAll(alpha.ConfigDir, 0o700)
	os.MkdirAll(codexAcct.ConfigDir, 0o700)
	os.WriteFile(filepath.Join(alpha.ConfigDir, ".claude.json"), []byte(alphaMCPConfig+"\n"), 0o600)
	os.WriteFile(filepath.Join(codexAcct.ConfigDir, "config.toml"), []byte("model = \"gpt-5\"\n"), 0o600)
	project := t.TempDir()
	os.WriteFile(filepath.Join(project, ".mcp.json"), []byte(`{"mcpServers": {"legacy": {"type": "sse", "url": "https://x/sse"}}}`), 0o644)

	if err := s.save(); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	log := &Logger{out: &out, err: &out}

	if err := mcpHandoffCommand(log, s, project, []string{"alpha", "work-codex"}); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"MCP handoff alpha (Claude Code) → work-codex (Codex)", "✓ filesystem", "⚠ github requires authentication: MCP requires missing environment variable: GITHUB_TOKEN", "✗ legacy unsupported by work-codex: sse transport", "Nothing was written"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}

	if data, _ := os.ReadFile(filepath.Join(codexAcct.ConfigDir, "config.toml")); string(data) != "model = \"gpt-5\"\n" {
		t.Fatalf("a preview wrote to the target:\n%s", data)
	}

	out.Reset()

	if err := mcpHandoffCommand(log, s, project, []string{"alpha", "work-codex", "--apply"}); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(codexAcct.ConfigDir, "config.toml"))
	text := string(data)

	if !strings.HasPrefix(text, "model = \"gpt-5\"\n") || !strings.Contains(text, "[mcp_servers.filesystem]") || !strings.Contains(text, "[mcp_servers.github]") || !strings.Contains(text, "[mcp_servers.linear]") {
		t.Fatalf("codex config:\n%s", text)
	}

	if strings.Contains(text, "postgres") || !strings.Contains(out.String(), "⚠ postgres requires authentication: its arguments contain a credential") {
		t.Fatalf("a server whose arguments hold a credential must not move to another agent:\n%s\n%s", text, out.String())
	}

	for _, secret := range []string{"super-secret-value", "db-secret-value", "CSM_TEST_ABSENT_TOKEN", "Authorization", "Bearer"} {
		if strings.Contains(text, secret) || strings.Contains(out.String(), "super-secret-value") {
			t.Fatalf("%q leaked:\n%s\n%s", secret, text, out.String())
		}
	}

	if !strings.Contains(text, "env_vars = [\"GITHUB_TOKEN\"]") {
		t.Fatalf("Codex should read GITHUB_TOKEN from its environment:\n%s", text)
	}

	if strings.Contains(text, "legacy") || !strings.Contains(out.String(), "Written to the work-codex profile") {
		t.Fatalf("apply output:\n%s\n%s", text, out.String())
	}

	if err := mcpHandoffCommand(log, s, project, []string{"alpha", "alpha"}); err == nil {
		t.Fatal("same account accepted")
	}

	if err := mcpHandoffCommand(log, s, project, []string{"alpha"}); err == nil {
		t.Fatal("missing target accepted")
	}
}

func TestMCPCommandAndDoctorListServers(t *testing.T) {
	s := newTestState(t, "alpha", "beta")
	alpha, _ := s.resolveAccount("alpha")
	os.MkdirAll(alpha.ConfigDir, 0o700)
	os.WriteFile(filepath.Join(alpha.ConfigDir, ".claude.json"), []byte(alphaMCPConfig), 0o600)
	beta, _ := s.resolveAccount("beta")
	os.MkdirAll(beta.ConfigDir, 0o700)
	os.WriteFile(filepath.Join(beta.ConfigDir, ".claude.json"), []byte(`{"mcpServers": {`), 0o600)

	if err := s.save(); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	log := &Logger{out: &out, err: &out}
	t.Setenv("CSM_HOME", s.Home)
	wd, _ := os.Getwd()
	project := t.TempDir()
	os.Chdir(project)
	defer os.Chdir(wd)

	if err := cmdMCP(log, s.Home, nil); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"alpha (Claude Code)", "✓ filesystem", "⚠ github requires authentication", "beta (Claude Code): ✗"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}

	if strings.Contains(out.String(), "super-secret-value") {
		t.Fatalf("secret printed:\n%s", out.String())
	}

	out.Reset()
	var problems []string
	fail := func(label, reason string) { problems = append(problems, label+": "+reason) }
	doctorMCP(log, s, map[string]provider.Provider{provider.ClaudeID: provider.Claude{}}, project, fail)

	if !strings.Contains(out.String(), "MCP alpha") || !strings.Contains(out.String(), "✓ filesystem") || !strings.Contains(out.String(), "⚠ github") {
		t.Fatalf("doctor output:\n%s", out.String())
	}

	if len(problems) != 1 || !strings.HasPrefix(problems[0], "MCP beta:") {
		t.Fatalf("problems = %v", problems)
	}

	out.Reset()

	if err := cmdMCP(&Logger{out: &out, err: io.Discard}, s.Home, []string{"alpha", "extra"}); err == nil {
		t.Fatal("bad usage accepted")
	}
}
