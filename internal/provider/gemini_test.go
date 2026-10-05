package provider

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func writeGeminiSession(t *testing.T, profile, project, name, firstRecord string, modified time.Time) string {
	t.Helper()
	dir := filepath.Join(profile, ".gemini", "tmp", project, "chats")
	os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, name)

	if err := os.WriteFile(path, []byte(firstRecord+"\n{\"type\":\"user\",\"content\":\"hi\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestGeminiIdentity(t *testing.T) {
	profile := t.TempDir()
	g := Gemini{}
	id, err := g.Identity(profile)

	if err != nil || id.LoggedIn || id.ProfileDir != profile {
		t.Fatalf("empty profile: %+v %v", id, err)
	}

	os.MkdirAll(filepath.Join(profile, ".gemini"), 0o700)
	os.WriteFile(filepath.Join(profile, ".gemini", "google_accounts.json"), []byte(`{"active":"a@example.test","old":[]}`), 0o600)

	if id, _ := g.Identity(profile); id.LoggedIn {
		t.Fatal("a remembered email without a login file counted as signed in")
	}

	os.WriteFile(filepath.Join(profile, ".gemini", "oauth_creds.json"), []byte(`{}`), 0o600)
	id, err = g.Identity(profile)

	if err != nil || !id.LoggedIn || id.Email != "a@example.test" {
		t.Fatalf("signed in: %+v %v", id, err)
	}

	os.WriteFile(filepath.Join(profile, ".gemini", "google_accounts.json"), []byte(`{"active":null,"old":["a@example.test"]}`), 0o600)

	if _, err := g.Identity(profile); err == nil {
		t.Fatal("a login with no recorded account was accepted")
	}
}

func TestGeminiSessions(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	const cwd = "/Users/x/polishpad"

	start := time.Now().Add(-time.Hour)
	hash := geminiProjectHash(cwd)
	g := Gemini{}

	if id, err := g.CurrentSession(from, cwd, start); id != "" || err != nil {
		t.Fatalf("found %q in an empty profile (%v)", id, err)
	}

	writeGeminiSession(t, from, "polishpad", "session-1-aaaaaaaa.jsonl", `{"sessionId":"aaaaaaaa-old","projectHash":"`+hash+`"}`, start.Add(-time.Minute))

	if id, _ := g.CurrentSession(from, cwd, start); id != "" {
		t.Fatalf("picked %q, which was last written before the agent started", id)
	}

	want := writeGeminiSession(t, from, "polishpad", "session-2-bbbbbbbb.jsonl", `{"sessionId":"bbbbbbbb-current","projectHash":"`+hash+`"}`, start.Add(time.Minute))
	writeGeminiSession(t, from, "polishpad", "session-3-cccccccc.jsonl", `{"sessionId":"cccccccc-sub","projectHash":"`+hash+`","kind":"subagent"}`, start.Add(2*time.Minute))
	writeGeminiSession(t, from, "other", "session-4-dddddddd.jsonl", `{"sessionId":"dddddddd-other","projectHash":"`+geminiProjectHash("/Users/x/other")+`"}`, start.Add(3*time.Minute))

	if id, err := g.CurrentSession(from, cwd, start); id != "bbbbbbbb-current" || err != nil {
		t.Fatalf("current session = %q (%v)", id, err)
	}

	resumeArgs, newSessionID, err := g.CarrySession(from, to, "bbbbbbbb-current")

	if err != nil || !slices.Equal(resumeArgs, []string{"--session-file", want}) || newSessionID != "" {
		t.Fatalf("resumeArgs=%v newSessionID=%q err=%v", resumeArgs, newSessionID, err)
	}

	if entries, _ := os.ReadDir(to); len(entries) != 0 {
		t.Fatal("carrying a Gemini session must not write into the target profile")
	}

	if resumeArgs, _, _ := g.CarrySession(from, to, "missing"); len(resumeArgs) != 0 {
		t.Fatal("carried a session that does not exist")
	}

	if _, err := g.CurrentSession(filepath.Join(from, "[bad"), cwd, start); err == nil {
		t.Fatal("a profile path that breaks the file search was reported as having no session")
	}
}

func TestGeminiArgs(t *testing.T) {
	g := Gemini{}

	if got := g.NewSessionArgs(Features{SessionID: true}, "sid", "note"); !slices.Equal(got, []string{"--session-id", "sid"}) {
		t.Fatalf("new session args = %v", got)
	}

	for _, args := range [][]string{{"-r", "latest"}, {"--resume=3"}, {"--session-id", "x"}, {"--model", "pro", "--session-file", "f.json"}} {
		if !g.HasSessionFlag(args) {
			t.Errorf("missed session flag in %v", args)
		}
	}

	if g.HasSessionFlag([]string{"--model", "pro"}) {
		t.Error("false positive")
	}

	t.Setenv("GEMINI_CLI_HOME", "/somewhere/else")
	env := g.Env("/profiles/work")

	if !slices.Contains(env, "GEMINI_CLI_HOME=/profiles/work") || slices.Contains(env, "GEMINI_CLI_HOME=/somewhere/else") {
		t.Fatal("profile directory not applied to the environment")
	}

	for _, name := range []string{"GEMINI_API_KEY", "GEMINI_FORCE_ENCRYPTED_FILE_STORAGE"} {
		t.Setenv(name, "true")

		if err := g.CheckEnv(); err == nil {
			t.Fatalf("%s in the environment was accepted", name)
		}

		t.Setenv(name, "")
	}
}
