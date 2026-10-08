package handoff

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/mcp"
)

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"export GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123":      "export GITHUB_TOKEN=<redacted>",
		"use sk-live-ABCDEFGHIJKLMNOPQRSTUVWXYZ for openai":           "use <redacted> for openai",
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig":      "Authorization: <redacted>",
		"postgres://user:hunter2@db.internal/app":                     "<redacted>db.internal/app",
		"DATABASE_URL=\"postgres://u:p@h/db\" npm start":              "DATABASE_URL=<redacted> npm start",
		"aws key AKIAIOSFODNN7EXAMPLE here":                           "aws key <redacted> here",
		"-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----": "<redacted>",
		"plain text with api_key: 12345678901234567890":               "plain text with api_key=<redacted>",
		"nothing secret here, PORT=8080":                              "nothing secret here, PORT=8080",
	}

	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanBoundsAndRedacts(t *testing.T) {
	var requests []string

	for i := range 10 {
		requests = append(requests, strings.Repeat("x", 50)+string(rune('a'+i)))
	}

	requests = append(requests, "deploy with TOKEN=super-secret-value now")

	c := Clean(Context{
		UserRequests:  requests,
		LastAssistant: strings.Repeat("y", 5000),
		FilesRead:     []string{"b.go", "a.go", "a.go", "", "evil\npath"},
		Commands:      []string{"  git   status  ", "curl -H 'Authorization: Bearer abcdefghijklmnopqrstuvwxyz'"},
	})

	if len(c.UserRequests) != maxRequests || !strings.Contains(c.UserRequests[len(c.UserRequests)-1], "TOKEN=<redacted>") {
		t.Fatalf("requests = %q", c.UserRequests)
	}

	if len(c.LastAssistant) > maxAssistant+5 || !strings.HasSuffix(c.LastAssistant, "…") {
		t.Fatalf("assistant not truncated: %d", len(c.LastAssistant))
	}

	if !slices.Equal(c.FilesRead, []string{"a.go", "b.go"}) {
		t.Fatalf("files = %q", c.FilesRead)
	}

	if c.Commands[0] != "git status" || strings.Contains(c.Commands[1], "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("commands = %q", c.Commands)
	}
}

func TestNextStepsAndDecisions(t *testing.T) {
	text := "I'll keep the retry in webhook.go instead of the handler.\n\nDone so far: tests pass.\n\n## Next steps\n\n1. Profile the render path\n2. Compare with the native fallback\n\nUnrelated trailing text.\n- not a step"
	steps := NextStepsFrom(text)

	if !slices.Equal(steps, []string{"Profile the render path", "Compare with the native fallback"}) {
		t.Fatalf("steps = %q", steps)
	}

	if d := DecisionsFrom(text); len(d) != 1 || !strings.HasPrefix(d[0], "I'll keep the retry") {
		t.Fatalf("decisions = %q", d)
	}

	if got := NextStepsFrom("no headings here"); len(got) != 0 {
		t.Fatalf("got %q", got)
	}

	if got := NextStepsFrom("Backoff is in. Next steps:\n1. Cap retries at 5\n2. Add a metric\n"); !slices.Equal(got, []string{"Cap retries at 5", "Add a metric"}) {
		t.Fatalf("inline heading: %q", got)
	}

	if got := NextStepsFrom("I set the next steps field in the struct.\n- unrelated bullet\n"); len(got) != 0 {
		t.Fatalf("a sentence mentioning next steps mid-line is not a heading: %q", got)
	}
}

func sampleState() State {
	return State{
		Version:   Version,
		ID:        "20261008T100000Z-claude-codex",
		CreatedAt: time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC),
		Stage:     StageContextExtracted,
		Reason:    "manual_switch",
		Source:    Agent{Provider: "claude", Account: "personal", SessionID: "abc123"},
		Target:    Agent{Provider: "codex", Account: "work-codex"},
		Context: Context{
			UserRequests:  []string{"Add retry logic to the webhook", "Make the retries exponential"},
			LastAssistant: "Added backoff in webhook.go; tests still failing on timeout.",
			FilesModified: []string{"internal/webhook.go"},
			FilesRead:     []string{"internal/webhook_test.go"},
			NextSteps:     []string{"Fix the timeout test"},
		},
		Git: Git{Root: "/repo", WorkingDir: "/repo", Branch: "feature/retry", Unstaged: 1, Status: []string{" M internal/webhook.go"}, DiffStat: " internal/webhook.go | 12 +-"},
		MCP: []mcp.Result{{Server: "filesystem", Status: mcp.Migrated}, {Server: "linear", Status: mcp.RequiresAuth, RequiresAuth: true, Reason: "set LINEAR_API_KEY in work-codex"}},
	}
}

func TestRenderAndInstruction(t *testing.T) {
	st := sampleState()
	md := Render(st)

	for _, want := range []string{"# CSM Handoff", "Source: claude/personal", "Target: codex/work-codex", "## Current Task\n\nMake the retries exponential", "## Earlier Requests\n\n- Add retry logic", "## Last Agent Progress", "## Files Modified\n\n- internal/webhook.go", "Branch: feature/retry", " M internal/webhook.go", "## MCP\n\n- ✓ filesystem\n- ⚠ linear requires authentication", "## Next Steps\n\n1. Fix the timeout test", "See handoff.json"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}

	instr := Instruction(st, "/state/handoff.md")

	for _, want := range []string{"continuing a coding task started by another coding agent (claude, account personal)", "Read the csm handoff file: /state/handoff.md", "Do not revert existing work", "branch feature/retry with 1 changed", "most recent request was: Make the retries exponential"} {
		if !strings.Contains(instr, want) {
			t.Errorf("instruction missing %q:\n%s", want, instr)
		}
	}

	st.Resumed = true
	resumed := Instruction(st, "/state/handoff.md")

	if !strings.Contains(resumed, "The conversation above is intact") || strings.Contains(resumed, "Do not revert") {
		t.Fatalf("resumed instruction = %q", resumed)
	}

	empty := Render(State{Version: Version, Source: Agent{Provider: "claude"}, Target: Agent{Provider: "gemini"}})

	if !strings.Contains(empty, "Not recorded. Ask the user") || !strings.Contains(empty, "Not a git repository") {
		t.Fatalf("empty render:\n%s", empty)
	}
}

func TestWriteLoadLatest(t *testing.T) {
	root := t.TempDir()
	st := sampleState()
	st.Git.StatusFull = []string{" M internal/webhook.go", "?? notes.txt"}
	dir := filepath.Join(root, st.ID)

	if err := Write(dir, &st, []byte("diff --git a/x b/x\n+TOKEN=super-secret-value\n")); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{FileJSON, FileMD, FileStatus, FileDiff} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", name, fi.Mode().Perm())
		}
	}

	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %o", fi.Mode().Perm())
	}

	back, err := Load(dir)
	if err != nil || back.ID != st.ID || back.Git.DiffFile != FileDiff || len(back.Context.UserRequests) != 2 {
		t.Fatalf("load = %+v %v", back, err)
	}

	md, _ := os.ReadFile(filepath.Join(dir, FileMD))

	if strings.Contains(string(md), "super-secret-value") {
		t.Fatal("the markdown must never contain the diff body")
	}

	older := sampleState()
	older.ID, older.CreatedAt = "older", st.CreatedAt.Add(-time.Hour)

	if err := Write(filepath.Join(root, older.ID), &older, nil); err != nil {
		t.Fatal(err)
	}

	os.WriteFile(filepath.Join(root, "junk"), []byte("x"), 0o600)
	os.MkdirAll(filepath.Join(root, "broken"), 0o700)
	os.WriteFile(filepath.Join(root, "broken", FileJSON), []byte("{"), 0o600)

	latest, latestDir, err := Latest(root)
	if err != nil || latest.ID != st.ID || latestDir != dir {
		t.Fatalf("latest = %s %s %v", latest.ID, latestDir, err)
	}

	if _, _, err := Latest(t.TempDir()); err == nil {
		t.Fatal("empty root should report no handoff")
	}

	os.WriteFile(filepath.Join(dir, FileJSON), []byte(`{"version": 99}`), 0o600)

	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "version 99") {
		t.Fatalf("got %v", err)
	}
}

func TestCaptureGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	repo := t.TempDir()
	repo, _ = filepath.EvalSymlinks(repo)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}

	run("init", "-q", "-b", "feature/x")
	os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o600)
	run("add", "a.go")
	run("commit", "-q", "-m", "init")
	os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a // changed\nconst token = \"ghp_abcdefghijklmnopqrstuvwxyz0123\"\n"), 0o600)
	os.WriteFile(filepath.Join(repo, "b.go"), []byte("package b\n"), 0o600)
	run("add", "b.go")
	os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("x"), 0o600)

	g, diff := CaptureGit(repo)

	if g.Root != repo || g.Branch != "feature/x" || g.Worktree {
		t.Fatalf("git = %+v", g)
	}

	if g.Staged != 1 || g.Unstaged != 1 || g.Untracked != 1 {
		t.Fatalf("counts = staged %d unstaged %d untracked %d\n%v", g.Staged, g.Unstaged, g.Untracked, g.Status)
	}

	if !strings.Contains(g.DiffStat, "a.go") || len(g.RecentCommits) != 1 || !strings.HasSuffix(g.RecentCommits[0], "init") {
		t.Fatalf("stat %q commits %q", g.DiffStat, g.RecentCommits)
	}

	if !strings.Contains(string(diff), "changed") || !strings.Contains(string(diff), "ghp_") {
		t.Fatal("the stored diff is the real diff, kept out of the prompt rather than altered")
	}

	after, _ := exec.Command("git", "-C", repo, "status", "--short").Output()

	if !strings.Contains(string(after), " M a.go") || !strings.Contains(string(after), "A  b.go") || !strings.Contains(string(after), "?? untracked.txt") {
		t.Fatalf("capture changed the repository:\n%s", after)
	}

	wt := filepath.Join(t.TempDir(), "wt")
	run("worktree", "add", "-q", "-b", "wt-branch", wt)
	wg, _ := CaptureGit(wt)

	if !wg.Worktree || wg.Branch != "wt-branch" {
		t.Fatalf("worktree = %+v", wg)
	}

	plain, diff := CaptureGit(t.TempDir())

	if plain.Root != "" || diff != nil {
		t.Fatalf("non-repo = %+v", plain)
	}
}
