package provider

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func cursorScript(t *testing.T, dir, name string, answers map[string]string) string {
	t.Helper()

	var b strings.Builder

	b.WriteString("#!/bin/sh\ncase \"$1\" in\n")

	for cmd, out := range answers {
		b.WriteString(cmd + ") cat <<'EOF'\n" + out + "\nEOF\n;;\n")
	}

	b.WriteString("*) exit 2 ;;\nesac\n")

	path := filepath.Join(dir, name)

	if err := os.WriteFile(path, []byte(b.String()), 0o700); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestFindCursor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CSM_CURSOR", "")
	t.Setenv("PATH", dir)

	if _, err := FindCursor(); err == nil || !strings.Contains(err.Error(), "curl https://cursor.com/install") {
		t.Fatalf("missing CLI: %v", err)
	}

	other := filepath.Join(dir, "agent")
	os.WriteFile(other, []byte("#!/bin/sh\n"), 0o700)

	if _, err := FindCursor(); err == nil || !strings.Contains(err.Error(), "is not Cursor's agent") {
		t.Fatalf("an unrelated agent on PATH was accepted: %v", err)
	}

	install := filepath.Join(dir, "versions", "2026.10.14-0f9edde")
	os.MkdirAll(install, 0o700)
	os.WriteFile(filepath.Join(install, "cursor-agent"), []byte("#!/bin/sh\n"), 0o700)
	os.Remove(other)
	os.Symlink(filepath.Join(install, "cursor-agent"), other)

	if p, err := FindCursor(); err != nil || p.Executable() != other {
		t.Fatalf("agent linked into a Cursor install: %v %v", p, err)
	}

	legacy := filepath.Join(dir, "cursor-agent")
	os.Symlink(filepath.Join(install, "cursor-agent"), legacy)

	if p, _ := FindCursor(); p.Executable() != legacy {
		t.Fatalf("cursor-agent is unambiguous and preferred, got %s", p.Executable())
	}

	t.Setenv("CSM_CURSOR", "/opt/fake/cursor")

	if p, _ := FindCursor(); p.Executable() != "/opt/fake/cursor" || p.ID() != CursorID {
		t.Fatalf("CSM_CURSOR override: %v", p)
	}
}

func TestCursorVersionAndFeatures(t *testing.T) {
	dir := t.TempDir()
	c := Cursor{Path: cursorScript(t, dir, "agent", map[string]string{"--version": "2026.10.14-0f9edde", "--help": fakeCursorHelp})}

	if v, err := c.Version(); err != nil || v != "2026.10.14-0f9edde" {
		t.Fatalf("version %q %v", v, err)
	}

	f, err := c.Features()
	if err != nil {
		t.Fatal(err)
	}

	if !f.Auth || !f.Supervise || !f.Resume || !f.InitialPrompt || !f.SharedLogin || f.SessionID || f.SystemPrompt || f.UsageProbe {
		t.Fatalf("features = %+v", f)
	}

	if !f.CanIsolate() || !f.CanContinue() {
		t.Fatalf("a current CLI must be usable: %+v", f)
	}

	old := Cursor{Path: cursorScript(t, dir, "old", map[string]string{"--version": "2025.01.01", "--help": fakeCursorOldHelp})}
	f, _ = old.Features()

	if f.CanIsolate() || f.Resume || f.InitialPrompt || CanStartTasks(old, f) {
		t.Fatalf("an old CLI without --resume or an opening prompt must be reported unsupported: %+v", f)
	}

	empty := Cursor{Path: cursorScript(t, dir, "empty", map[string]string{"--version": ""})}

	if _, err := empty.Version(); err == nil {
		t.Fatal("an empty version was accepted")
	}

	if _, err := (Cursor{Path: filepath.Join(dir, "missing")}).Version(); err == nil {
		t.Fatal("a missing executable reported a version")
	}
}

func TestCursorIdentity(t *testing.T) {
	dir := t.TempDir()
	profile := t.TempDir()
	signedOut := `{"status":"unauthenticated","isAuthenticated":false,"hasAccessToken":false,"hasRefreshToken":false,"message":"Not logged in"}`

	c := Cursor{Path: cursorScript(t, dir, "out", map[string]string{"status": signedOut})}

	if id, err := c.Identity(profile); err != nil || id.LoggedIn || id.ProfileDir != profile {
		t.Fatalf("signed out: %+v %v", id, err)
	}

	c = Cursor{Path: cursorScript(t, dir, "in", map[string]string{"status": `{"status":"authenticated","isAuthenticated":true}`, "about": `{"cliVersion":"x","userEmail":"dev@example.test"}`})}

	if id, err := c.Identity(profile); err != nil || !id.LoggedIn || id.Email != "dev@example.test" {
		t.Fatalf("signed in, email from about: %+v %v", id, err)
	}

	c = Cursor{Path: cursorScript(t, dir, "email", map[string]string{"status": `{"status":"authenticated","isAuthenticated":true,"hasAccessToken":true,"hasRefreshToken":true,"userInfo":{"email":"x@example.test","userId":1}}`})}

	if id, _ := c.Identity(profile); id.Email != "x@example.test" {
		t.Fatalf("email from status, in the shape the real 2026.10.14 CLI prints: %+v", id)
	}

	c = Cursor{Path: cursorScript(t, dir, "garbled", map[string]string{"status": "Logged in?"})}

	if _, err := c.Identity(profile); err == nil || !strings.Contains(err.Error(), "unexpected output") {
		t.Fatalf("malformed status was accepted: %v", err)
	}

	broken := filepath.Join(dir, "broken")
	os.WriteFile(broken, []byte("#!/bin/sh\necho 'Error: keychain unavailable' >&2\nexit 1\n"), 0o700)

	if _, err := (Cursor{Path: broken}).Identity(profile); err == nil || !strings.Contains(err.Error(), "cursor status failed") {
		t.Fatalf("a failing status was accepted: %v", err)
	}

	hang := filepath.Join(dir, "hang")
	os.WriteFile(hang, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700)

	defer func(d time.Duration) { cursorProbeTimeout = d }(cursorProbeTimeout)

	cursorProbeTimeout = 300 * time.Millisecond
	start := time.Now()

	if _, err := (Cursor{Path: hang}).Identity(profile); err == nil || !strings.Contains(err.Error(), "did not answer") || time.Since(start) > 5*time.Second {
		t.Fatalf("a hanging CLI must time out: %v after %s", err, time.Since(start))
	}
}

func TestCursorProbeEnvironment(t *testing.T) {
	dir := t.TempDir()
	profile := t.TempDir()
	out := filepath.Join(dir, "env")
	script := filepath.Join(dir, "agent")
	os.WriteFile(script, []byte("#!/bin/sh\nenv > '"+out+"'\necho '{\"isAuthenticated\":false}'\n"), 0o700)
	t.Setenv("CURSOR_CONFIG_DIR", "/somewhere/else")
	t.Setenv("CSM_PROBE_MARKER", "kept")

	if _, err := (Cursor{Path: script}).Identity(profile); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(out)
	env := strings.Split(string(data), "\n")

	for _, want := range []string{"CURSOR_CONFIG_DIR=" + profile, "NO_OPEN_BROWSER=1", "CSM_PROBE_MARKER=kept"} {
		if !slices.Contains(env, want) {
			t.Errorf("probe environment lacks %s", want)
		}
	}

	if slices.Contains(env, "CURSOR_CONFIG_DIR=/somewhere/else") {
		t.Error("the user's own CURSOR_CONFIG_DIR leaked into the profile")
	}
}

func TestCursorSessions(t *testing.T) {
	profile := t.TempDir()
	project, _ := filepath.EvalSymlinks(t.TempDir())
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(project, link)
	start := time.Now().Add(-time.Hour)
	c := Cursor{}

	chat := func(cwd, id string, modified time.Time) {
		sum := md5.Sum([]byte(cwd))
		dir := filepath.Join(profile, "chats", hex.EncodeToString(sum[:]), id)
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "store.db"), []byte("sqlite"), 0o600)
		os.Chtimes(filepath.Join(dir, "store.db"), modified, modified)
		os.Chtimes(dir, modified, modified)
	}

	if id, err := c.CurrentSession(profile, project, start); id != "" || err != nil {
		t.Fatalf("no chats yet: %q %v", id, err)
	}

	chat(project, "old", start.Add(-time.Minute))

	if id, _ := c.CurrentSession(profile, project, start); id != "" {
		t.Fatalf("picked %q, which was last written before the agent started", id)
	}

	chat(project, "current", start.Add(time.Minute))
	chat("/Users/x/other", "elsewhere", start.Add(2*time.Minute))

	if id, err := c.CurrentSession(profile, project, start); id != "current" || err != nil {
		t.Fatalf("current = %q %v", id, err)
	}

	if id, _ := c.CurrentSession(profile, link, start); id != "current" {
		t.Fatalf("a symlinked working directory must find the chat the CLI filed under the real path, got %q", id)
	}

	if args, newID, err := c.CarrySession(profile, t.TempDir(), "current"); args != nil || newID != "" || err != nil {
		t.Fatalf("Cursor chats are never copied between profiles: %v %q %v", args, newID, err)
	}

	if !c.HasSessionFlag([]string{"--resume", "current"}) || !c.HasSessionFlag([]string{"--resume=current"}) || !c.HasSessionFlag([]string{"--continue"}) || !c.HasSessionFlag([]string{"resume"}) || !c.HasSessionFlag([]string{"ls"}) {
		t.Fatal("session flags not recognised")
	}

	if c.HasSessionFlag([]string{"fix the resume button"}) || c.HasSessionFlag([]string{"--model", "gpt-5", "ls"}) {
		t.Fatal("task text or a later word was taken for a session flag")
	}

	if args := c.NewSessionArgs(Features{SessionID: true}, "x", ""); args != nil {
		t.Fatalf("the CLI cannot take a chat ID up front, got %v", args)
	}
}

func TestCursorTaskTextIsOneArgument(t *testing.T) {
	f := Features{InitialPrompt: true}
	text := "Fix the bug in 'billing svc'; then run: $(rm -rf ~) `id` && echo $HOME | tee /tmp/x\nsecond line --resume x"
	c := Cursor{}

	if args := c.TaskArgs(f, text); !slices.Equal(args, []string{text}) {
		t.Fatalf("task args = %q", args)
	}

	if args := c.HandoffArgs(f, ""); args != nil {
		t.Fatalf("an empty instruction must add nothing: %q", args)
	}

	if args := c.TaskArgs(Features{}, text); args != nil {
		t.Fatalf("a CLI without an opening prompt must get no task args: %q", args)
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "argv")
	script := filepath.Join(dir, "agent")
	os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$1\" > '"+out+"'\nprintf '%s' \"$#\" >> '"+out+"'\n"), 0o700)

	if err := exec.Command(script, c.TaskArgs(f, text)...).Run(); err != nil {
		t.Fatal(err)
	}

	if data, _ := os.ReadFile(out); string(data) != text+"1" {
		t.Fatalf("the agent received %q", data)
	}
}

func TestCursorRedactsSecrets(t *testing.T) {
	c := Cursor{}
	args := []string{"--api-key", "key_live_123", "--model", "gpt-5", "--auth-token=tok_456", "-H", "Authorization: Bearer abc", "--header=X-Key: def", "fix it"}
	got := c.RedactArgs(args)
	joined := strings.Join(got, " ")

	for _, secret := range []string{"key_live_123", "tok_456", "Bearer abc", "def"} {
		if strings.Contains(joined, secret) {
			t.Errorf("%q survived redaction: %v", secret, got)
		}
	}

	for _, kept := range []string{"--model", "gpt-5", "fix it", "--api-key", "--auth-token=[redacted]"} {
		if !slices.Contains(got, kept) {
			t.Errorf("%q was lost: %v", kept, got)
		}
	}

	if args[1] != "key_live_123" {
		t.Fatal("redaction changed the caller's arguments")
	}
}

func TestCursorEnvAndLimits(t *testing.T) {
	t.Setenv("CURSOR_CONFIG_DIR", "/elsewhere")
	env := Cursor{}.Env("/profiles/cursor-main", "CSM_X=1")

	if !slices.Contains(env, "CURSOR_CONFIG_DIR=/profiles/cursor-main") || slices.Contains(env, "CURSOR_CONFIG_DIR=/elsewhere") || !slices.Contains(env, "CSM_X=1") {
		t.Fatalf("env = %v", env)
	}

	if _, err := (Cursor{}).FetchUsage(t.Context(), "/p"); err == nil {
		t.Fatal("Cursor reports no usage; csm must not invent one")
	}

	if (Cursor{}).LimitPollInterval() != 0 || (Cursor{}).CheckEnv() != nil {
		t.Fatal("unexpected limit polling or environment block")
	}
}

func TestCursorCapabilities(t *testing.T) {
	cursor := CapabilitiesOf(Cursor{}, Features{Auth: true, Supervise: true, Resume: true, InitialPrompt: true, SharedLogin: true})
	want := Capabilities{Interactive: true, SessionTracking: true, SessionResume: true, Tasks: true, HandoffTarget: true}

	if cursor != want {
		t.Fatalf("cursor = %+v, want %+v", cursor, want)
	}

	if old := CapabilitiesOf(Cursor{}, Features{Auth: true, SharedLogin: true}); old != (Capabilities{}) {
		t.Fatalf("an old Cursor CLI must offer nothing it cannot do: %+v", old)
	}
}

func TestFakeCursorKeepsOneSignInAcrossProfiles(t *testing.T) {
	accounts := t.TempDir()
	run := func(profile string, args ...string) (string, int) {
		t.Helper()

		cmd := exec.Command(os.Args[0], append([]string{"-test.run=TestFakeCursorHelper", "--"}, args...)...)
		cmd.Env = append(Cursor{}.Env(filepath.Join(accounts, profile)), "CSM_FAKE_CURSOR_HELPER=1")
		out, _ := cmd.CombinedOutput()

		return string(out), cmd.ProcessState.ExitCode()
	}

	if out, _ := run("a", "status", "--format", "json"); !strings.Contains(out, `"isAuthenticated":false`) {
		t.Fatalf("fresh fake must be signed out: %s", out)
	}

	run("a", "login")

	if out, _ := run("b", "status", "--format", "json"); !strings.Contains(out, `"isAuthenticated":true`) {
		t.Fatalf("the fake must share its sign-in across profiles like the real CLI: %s", out)
	}
}

func TestFakeCursorHelper(t *testing.T) {
	if os.Getenv("CSM_FAKE_CURSOR_HELPER") == "" {
		return
	}

	i := slices.Index(os.Args, "--")
	os.Exit(RunFakeCursor(os.Args[i+1:]))
}
