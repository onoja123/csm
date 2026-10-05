package provider

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestCopilotIdentity(t *testing.T) {
	profile := t.TempDir()
	c := Copilot{}
	config := filepath.Join(profile, "config.json")

	if id, err := c.Identity(profile); err != nil || id.LoggedIn || id.ProfileDir != profile {
		t.Fatalf("empty profile: %+v %v", id, err)
	}

	os.WriteFile(config, []byte("// User settings belong in settings.json.\n// This file is managed automatically.\n{\n  \"firstLaunchAt\": \"2026-10-03T21:44:48.182Z\"\n}\n"), 0o600)

	if id, err := c.Identity(profile); err != nil || id.LoggedIn {
		t.Fatalf("signed-out config: %+v %v", id, err)
	}

	os.WriteFile(config, []byte("// managed\n{\"lastLoggedInUser\": {\"host\": \"https://github.com\", \"login\": \"octocat\"}}\n"), 0o600)

	if id, err := c.Identity(profile); err != nil || !id.LoggedIn || id.Email != "octocat" {
		t.Fatalf("signed in: %+v %v", id, err)
	}

	os.WriteFile(config, []byte(`{"last_logged_in_user": {"host": "https://corp.ghe.com", "login": "octocat"}}`), 0o600)

	if id, _ := c.Identity(profile); id.Email != "octocat@corp.ghe.com" {
		t.Fatalf("another host must be a different identity, got %q", id.Email)
	}

	os.WriteFile(config, []byte("{not json"), 0o600)

	if _, err := c.Identity(profile); err == nil {
		t.Fatal("an unreadable config was accepted")
	}
}

func TestCopilotSessions(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	const cwd = "/Users/x/polishpad"

	start := time.Now().Add(-time.Hour)
	c := Copilot{}
	session := func(id, workspace string, modified time.Time) {
		dir := copilotSessionDir(from, id)
		os.MkdirAll(filepath.Join(dir, "checkpoints"), 0o700)
		os.WriteFile(filepath.Join(dir, "workspace.yaml"), []byte(workspace), 0o600)
		os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("{}\n"), 0o600)
		os.WriteFile(filepath.Join(dir, "checkpoints", "1.md"), []byte("checkpoint\n"), 0o600)
		os.Chtimes(filepath.Join(dir, "events.jsonl"), modified, modified)
		os.Chtimes(dir, modified, modified)
	}

	session("old", "id: old\ncwd: "+cwd+"\n", start.Add(-time.Minute))

	if id, _ := c.CurrentSession(from, cwd, start); id != "" {
		t.Fatalf("picked %q, which was last written before the agent started", id)
	}

	session("current", "id: current\ncwd: \""+cwd+"\"\n", start.Add(time.Minute))
	session("elsewhere", "id: elsewhere\ncwd: /Users/x/other\n", start.Add(2*time.Minute))
	session("unknown-layout", "something: else\n", start.Add(3*time.Minute))

	if id, err := c.CurrentSession(from, cwd, start); id != "current" || err != nil {
		t.Fatalf("current session = %q (%v)", id, err)
	}

	resumeArgs, sessionID, err := c.CarrySession(from, to, "current")

	if err != nil || !slices.Equal(resumeArgs, []string{"--resume", "current"}) || sessionID != "current" {
		t.Fatalf("resumeArgs=%v sessionID=%q err=%v", resumeArgs, sessionID, err)
	}

	for _, rel := range []string{"workspace.yaml", "events.jsonl", "checkpoints/1.md"} {
		if _, err := os.Stat(filepath.Join(copilotSessionDir(to, "current"), rel)); err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
	}

	if resumeArgs, _, _ := c.CarrySession(from, to, "missing"); len(resumeArgs) != 0 {
		t.Fatal("carried a session that does not exist")
	}

	if _, _, err := c.CarrySession(from, to, "../current"); err == nil {
		t.Fatal("a session ID with a path in it was accepted")
	}
}

func TestCopilotArgs(t *testing.T) {
	c := Copilot{}

	if got := c.NewSessionArgs(Features{SessionID: true}, "sid", "note"); !slices.Equal(got, []string{"--session-id", "sid"}) {
		t.Fatalf("new session args = %v", got)
	}

	if !c.HasSessionFlag([]string{"--model", "auto", "--continue"}) || !c.HasSessionFlag([]string{"--resume=abc"}) || c.HasSessionFlag([]string{"--model", "auto"}) {
		t.Fatal("session flag detection is wrong")
	}

	t.Setenv("COPILOT_HOME", "/somewhere/else")
	env := c.Env("/profiles/work")
	if !slices.Contains(env, "COPILOT_HOME=/profiles/work") || slices.Contains(env, "COPILOT_HOME=/somewhere/else") {
		t.Fatal("profile directory not applied to the environment")
	}

	for _, name := range copilotAuthOverrideEnv {
		t.Setenv(name, "")
	}

	if err := c.CheckEnv(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GH_TOKEN", "x")

	if err := c.CheckEnv(); err == nil {
		t.Fatal("a GitHub token in the environment was accepted")
	}
}
