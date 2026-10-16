package main

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/handoff"
	"github.com/onoja123/csm/internal/provider"
)

func quietFakes(t *testing.T) {
	t.Helper()

	for _, key := range []string{"CSM_FAKE_LIMIT", "CSM_FAKE_HOLD", "CSM_FAKE_ECHO", "CSM_FAKE_SEED", "CSM_FAKE_ERROR", "CSM_FAKE_CURSOR_OLD", "CSM_FAKE_CURSOR_EMAIL", "CSM_BUILD_ID", "CSM_BUILD_STEP", "CSM_TASK_REPORT", "CSM_BUILD_CONTRACT"} {
		t.Setenv(key, "")
	}
}

func fakeShim(t *testing.T, dir, id string) string {
	t.Helper()

	shim := filepath.Join(dir, id)
	os.WriteFile(shim, []byte("#!/bin/sh\nexec '"+testBinary(t)+"' __fake-"+id+" \"$@\"\n"), 0o700)
	t.Setenv("CSM_"+strings.ToUpper(id), shim)

	return shim
}

func cursorHome(t *testing.T, accounts map[string]string) (string, *State, string) {
	t.Helper()
	quietFakes(t)

	home := t.TempDir()
	s, err := initState(home)
	if err != nil {
		t.Fatal(err)
	}

	project, _ := filepath.EvalSymlinks(t.TempDir())
	git := func(args ...string) {
		t.Helper()

		if out, err := exec.Command("git", append([]string{"-C", project, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}

	git("init", "-q", "-b", "billing")
	os.WriteFile(filepath.Join(project, "main.go"), []byte("package main\n"), 0o600)
	git("add", ".")
	git("commit", "-q", "-m", "init")
	os.WriteFile(filepath.Join(project, "main.go"), []byte("package main // retries\n"), 0o600)

	for _, id := range provider.IDs {
		t.Setenv("CSM_"+strings.ToUpper(id), "")
	}

	for id, name := range accounts {
		p, _ := provider.New(id, fakeShim(t, home, id))
		a, err := s.addAccount(id, name, time.Now())
		if err != nil {
			t.Fatal(err)
		}

		os.MkdirAll(a.ConfigDir, 0o700)

		if err := p.Login(a.ConfigDir); err != nil {
			t.Fatal(err)
		}

		st, err := verifyProfile(p, a)
		if err != nil {
			t.Fatal(err)
		}

		a.Email, a.VerifiedAt = st.Email, time.Now()
	}

	s.Config.setPolicy(policyAutomatic)

	if err := s.save(); err != nil {
		t.Fatal(err)
	}

	wd, _ := os.Getwd()
	os.Chdir(project)
	t.Cleanup(func() { os.Chdir(wd) })

	return home, s, project
}

type cursorChat struct {
	ID      string
	Profile string `json:"profile"`
	Resumed bool   `json:"resumed"`
	Prompt  string `json:"prompt"`
}

func cursorChats(t *testing.T, profile string) []cursorChat {
	t.Helper()

	var out []cursorChat

	stores, _ := filepath.Glob(filepath.Join(profile, "chats", "*", "*", "store.db"))

	for _, store := range stores {
		data, _ := os.ReadFile(store)

		for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
			c := cursorChat{ID: filepath.Base(filepath.Dir(store))}

			if json.Unmarshal([]byte(line), &c) == nil {
				out = append(out, c)
			}
		}
	}

	return out
}

func accountNamed(t *testing.T, home, name string) *Account {
	t.Helper()

	s, err := loadState(home)
	if err != nil {
		t.Fatal(err)
	}

	a, err := s.resolveAccount(name)
	if err != nil {
		t.Fatal(err)
	}

	return a
}

func TestCursorAccountUsesTheOneSignIn(t *testing.T) {
	quietFakes(t)
	home := t.TempDir()
	s, _ := initState(home)
	fakeShim(t, home, provider.CursorID)

	var out strings.Builder
	log := &Logger{out: &out, err: &out}

	if err := accountAdd(log, s, provider.CursorID, "cursor-main", false); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}

	if a := accountNamed(t, home, "cursor-main"); a.Email != "dev@example.test" || a.VerifiedAt.IsZero() {
		t.Fatalf("account = %+v", a)
	}

	s, _ = loadState(home)
	err := accountAdd(log, s, provider.CursorID, "second", false)

	if err == nil || !strings.Contains(err.Error(), "keeps one sign-in per user") || !strings.Contains(err.Error(), "cursor-main") {
		t.Fatalf("a second Cursor account must be refused with the reason: %v", err)
	}

	if _, err := os.Stat(filepath.Join(s.accountsDir(), "second")); err == nil {
		t.Fatal("a refused account left a profile directory behind")
	}

	t.Setenv("CSM_FAKE_CURSOR_EMAIL", "someone-else@example.test")
	out.Reset()
	p, _ := provider.Find(provider.CursorID)

	if err := accountLogin(log, s, p, accountNamed(t, home, "cursor-main")); err != nil || !strings.Contains(out.String(), "already signed in") {
		t.Fatalf("logging in again must reuse the existing sign-in: %v\n%s", err, out.String())
	}

	if a := accountNamed(t, home, "cursor-main"); a.Email != "dev@example.test" {
		t.Fatalf("csm ran the login again: %s", a.Email)
	}
}

func TestCursorSetupAndDoctorExplainTheSharedSignIn(t *testing.T) {
	_, s, _ := cursorHome(t, map[string]string{provider.CursorID: "cursor-main"})

	var out strings.Builder
	log := &Logger{out: &out, err: &out}

	if installed, err := setupProvider(log, s, provider.CursorID); !installed || err != nil {
		t.Fatalf("setup: %v %v\n%s", installed, err, out.String())
	}

	for _, want := range []string{"Provider: Cursor CLI", "2026.10.14-fake", "keeps one sign-in per user", "Session resume"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("setup output missing %q:\n%s", want, out.String())
		}
	}

	var problems []string
	p, _ := provider.Find(provider.CursorID)

	if !doctorProvider(log, s, p, func(label, reason string) { problems = append(problems, label+": "+reason) }) || len(problems) > 0 {
		t.Fatalf("doctor: %v\n%s", problems, out.String())
	}

	t.Setenv("CSM_FAKE_CURSOR_OLD", "1")
	out.Reset()

	if installed, err := setupProvider(log, s, provider.CursorID); !installed || err == nil || !strings.Contains(err.Error(), "Upgrade Cursor CLI") {
		t.Fatalf("an old CLI must be reported as installed but unsupported: %v", err)
	}

	if err := accountAdd(log, s, provider.CursorID, "old", false); err == nil || !strings.Contains(err.Error(), "lacks the profile login and session supervision") {
		t.Fatalf("an old CLI must not get an account: %v", err)
	}
}

func TestCsmCursorRunsATaskAndResumesItsChat(t *testing.T) {
	home, s, project := cursorHome(t, map[string]string{provider.CursorID: "cursor-main"})
	task := "Fix 'billing svc'; then run $(id) && echo `whoami` > /tmp/x | tee -a ~/.zshrc"

	var out strings.Builder
	log := &Logger{out: &out, err: &out}
	r, err := newRunnerAt(log, s, provider.CursorID, testBinary(t), project)
	if err != nil {
		t.Fatal(err)
	}

	if code, next, err := r.run([]string{task}); code != 0 || next != nil || err != nil {
		t.Fatalf("code=%d next=%v err=%v\n%s", code, next, err, out.String())
	}

	profile := accountNamed(t, home, "cursor-main").ConfigDir
	chats := cursorChats(t, profile)

	if len(chats) != 1 || chats[0].Prompt != task || chats[0].Resumed {
		t.Fatalf("the agent must receive the task text as one unchanged argument: %+v", chats)
	}

	if id, _ := r.provider.CurrentSession(profile, project, time.Now().Add(-time.Minute)); id != chats[0].ID {
		t.Fatalf("csm found chat %q, the CLI wrote %q", id, chats[0].ID)
	}

	r, _ = newRunnerAt(log, s, provider.CursorID, testBinary(t), project)

	if code, _, err := r.run([]string{"--resume", chats[0].ID}); code != 0 || err != nil {
		t.Fatalf("resume: code=%d err=%v\n%s", code, err, out.String())
	}

	if chats = cursorChats(t, profile); len(chats) != 2 || !slices.ContainsFunc(chats, func(c cursorChat) bool { return c.Resumed && c.ID == chats[0].ID }) {
		t.Fatalf("resume did not continue the chat: %+v", chats)
	}

	r, _ = newRunnerAt(log, s, provider.CursorID, testBinary(t), project)

	if code, next, _ := r.run([]string{"--resume", "00000000-0000-4000-8000-000000000000"}); code == 0 || next != nil {
		t.Fatalf("resuming an unknown chat must fail and stop: code=%d next=%v", code, next)
	}

	if a := accountNamed(t, home, "cursor-main"); !a.CooldownUntil.IsZero() || a.LastFailure == provider.StopReasonUsageLimit {
		t.Fatalf("an ordinary failure was recorded as a usage limit: %+v", a)
	}
}

func TestLiveCursorSessionHandsOffToClaude(t *testing.T) {
	home, s, project := cursorHome(t, map[string]string{provider.CursorID: "cursor-main", provider.ClaudeID: "alpha"})
	t.Setenv("CSM_FAKE_HOLD", "cursor-main")

	var out strings.Builder
	log := &Logger{out: &out, err: &out}
	profile := accountNamed(t, home, "cursor-main").ConfigDir
	requested := make(chan error, 1)

	go func() {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			fresh, err := loadState(home)
			if err != nil {
				continue
			}

			if sess, live := fresh.loadLiveSession(provider.CursorID, project); live && len(cursorChats(t, profile)) > 0 {
				requested <- requestSwitch(&Logger{out: io.Discard, err: io.Discard}, fresh, sess, "alpha")

				return
			}
		}

		requested <- errors.New("the Cursor session never became live")
	}()

	r, _ := newRunnerAt(log, s, provider.CursorID, testBinary(t), project)
	_, next, err := r.run([]string{"Add retries to the webhook"})

	if err := <-requested; err != nil {
		t.Fatal(err)
	}

	if err != nil || next == nil || next.providerID != provider.ClaudeID {
		t.Fatalf("next=%+v err=%v\n%s", next, err, out.String())
	}

	claude, err := newRunnerAt(log, s, next.providerID, testBinary(t), project)
	if err != nil {
		t.Fatal(err)
	}

	claude.pendingHandoff, claude.autoHops, claude.tried = next.handoffDir, next.autoHops, next.tried

	if code, _, err := claude.run(next.args); code != 0 || err != nil {
		t.Fatalf("claude: code=%d err=%v\n%s", code, err, out.String())
	}

	chat := cursorChats(t, profile)[0].ID

	if cp, err := s.loadCheckpoint(provider.CursorID, project); err != nil || cp.SessionID != chat || cp.Reason != provider.StopReasonManualSwitch {
		t.Fatalf("checkpoint = %+v (%v), chat %s", cp, err, chat)
	}

	st, dir, err := handoff.Latest(s.handoffRoot(project))
	if err != nil || st.Source.Provider != provider.CursorID || st.Source.SessionID != chat || st.Target.Provider != provider.ClaudeID || st.Target.SessionID == "" {
		t.Fatalf("handoff %s: %s session %q → %s session %q (%v)", st.ID, st.Source.Provider, st.Source.SessionID, st.Target.Provider, st.Target.SessionID, err)
	}

	md, _ := os.ReadFile(filepath.Join(dir, handoff.FileMD))

	if !strings.Contains(string(md), "Cursor CLI session records cannot be read") {
		t.Fatalf("the brief must say the Cursor chat was not read:\n%s", md)
	}

	if received := receivedInstruction(t, accountNamed(t, home, "alpha").ConfigDir); !strings.Contains(received, "started by another coding agent (cursor, account cursor-main)") {
		t.Fatalf("Claude Code did not receive the brief:\n%s", received)
	}
}

func TestCursorExitIsNeverTakenForAUsageLimit(t *testing.T) {
	home, s, project := cursorHome(t, map[string]string{provider.CursorID: "cursor-main", provider.ClaudeID: "alpha"})
	s.Config.setHandoffTo([]string{provider.ClaudeID})
	s.save()
	t.Setenv("CSM_FAKE_LIMIT", "cursor-main")

	var out strings.Builder
	log := &Logger{out: &out, err: &out}
	r, _ := newRunnerAt(log, s, provider.CursorID, testBinary(t), project)
	code, next, err := r.run([]string{"Add retries"})

	if code == 0 || next != nil || err != nil {
		t.Fatalf("code=%d next=%v err=%v\n%s", code, next, err, out.String())
	}

	if strings.Contains(out.String(), "Automatic handoff") {
		t.Fatalf("csm handed off on text it cannot verify:\n%s", out.String())
	}

	if entries, _ := os.ReadDir(s.handoffRoot(project)); len(entries) != 0 {
		t.Fatalf("%d handoff(s) recorded", len(entries))
	}

	if a := accountNamed(t, home, "cursor-main"); !a.CooldownUntil.IsZero() || a.LastFailure == provider.StopReasonUsageLimit {
		t.Fatalf("the Cursor account was marked limited without evidence: %+v", a)
	}

	if s.active(provider.ClaudeID) != "alpha" || claudeTranscripts(t, accountNamed(t, home, "alpha").ConfigDir) != 0 {
		t.Fatal("Claude Code was started")
	}
}

func claudeTranscripts(t *testing.T, profile string) int {
	t.Helper()

	n := 0

	filepath.WalkDir(filepath.Join(profile, "projects"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
			n++
		}

		return nil
	})

	return n
}

func TestAutomaticHandoffFromClaudeToCursor(t *testing.T) {
	quietFakes(t)
	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha,beta", crossTo: provider.CursorID, seed: seedFile(t), before: handoffList(provider.CursorID)})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}

	if res.active != "work-cursor" || !strings.Contains(res.output, "Automatic handoff: beta (Claude Code) → work-cursor (Cursor CLI).") {
		t.Fatalf("active=%s\n%s", res.active, res.output)
	}

	for _, want := range []string{`"profile":"work-cursor"`, "You are continuing a coding task started by another coding agent (claude, account beta)", "The task: Add retry logic to the webhook"} {
		if !strings.Contains(res.files["target-transcript"], want) {
			t.Errorf("Cursor did not receive %q:\n%s", want, res.files["target-transcript"])
		}
	}

	var st handoff.State

	if err := json.Unmarshal([]byte(res.files["handoff-json"]), &st); err != nil || st.Reason != string(provider.StopReasonUsageLimit) || st.Target.Provider != provider.CursorID || st.Target.Account != "work-cursor" {
		t.Fatalf("handoff = %+v %v", st, err)
	}

	if o := st.Outcome; o == nil || !o.Started || o.Delivery != handoff.DeliveryInstruction {
		t.Fatalf("outcome = %+v", st.Outcome)
	}

	if st.Source.SessionID == "" || st.Target.SessionID == "" || st.Source.SessionID == st.Target.SessionID {
		t.Fatalf("the handoff must link Claude's session and the Cursor chat it started: %q → %q", st.Source.SessionID, st.Target.SessionID)
	}

	if res.gitBefore != res.gitAfter {
		t.Fatalf("git changed: %q → %q", res.gitBefore, res.gitAfter)
	}
}

func TestHandoffFromCursorToOtherAgents(t *testing.T) {
	for _, target := range []struct{ provider, account string }{{provider.ClaudeID, "alpha"}, {provider.CodexID, "work-codex"}} {
		t.Run(target.provider, func(t *testing.T) {
			home, s, project := cursorHome(t, map[string]string{provider.CursorID: "cursor-main", target.provider: target.account})

			if err := s.saveCheckpoint(Checkpoint{Version: stateVersion, Provider: provider.CursorID, ProjectDir: project, Account: "cursor-main", SessionID: "chat-1", CheckpointedAt: time.Now(), Reason: provider.StopReasonProcessExited}); err != nil {
				t.Fatal(err)
			}

			var out strings.Builder
			log := &Logger{out: &out, err: &out}

			if code, err := cmdHandoff(log, home, []string{target.account}); code != 0 || err != nil {
				t.Fatalf("code=%d err=%v\n%s", code, err, out.String())
			}

			for _, want := range []string{"Cursor CLI / cursor-main", "repository state only; Cursor CLI sessions cannot be read"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output missing %q:\n%s", want, out.String())
				}
			}

			st, dir, err := handoff.Latest(s.handoffRoot(project))
			if err != nil || st.Source.Provider != provider.CursorID || st.Source.SessionID != "chat-1" || st.Target.Account != target.account {
				t.Fatalf("handoff = %+v %v", st, err)
			}

			md, _ := os.ReadFile(filepath.Join(dir, handoff.FileMD))

			for _, want := range []string{"Cursor CLI session records cannot be read", "main.go", "Branch: billing"} {
				if !strings.Contains(string(md), want) {
					t.Errorf("handoff.md missing %q:\n%s", want, md)
				}
			}

			if received := receivedInstruction(t, accountNamed(t, home, target.account).ConfigDir); !strings.Contains(received, "You are continuing a coding task started by another coding agent (cursor, account cursor-main)") {
				t.Fatalf("%s did not receive the brief:\n%s", target.provider, received)
			}
		})
	}
}

func receivedInstruction(t *testing.T, profile string) string {
	t.Helper()

	found := ""

	filepath.WalkDir(profile, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), "You are continuing a coding task") {
				found = string(data)
			}
		}

		return nil
	})

	return found
}

func TestHandoffFromClaudeToCursor(t *testing.T) {
	home, s, project := cursorHome(t, map[string]string{provider.ClaudeID: "alpha", provider.CursorID: "cursor-main"})
	writeAlphaTranscript(s, project)

	if err := s.saveCheckpoint(Checkpoint{Version: stateVersion, Provider: provider.ClaudeID, ProjectDir: project, Account: "alpha", SessionID: "seed", CheckpointedAt: time.Now(), Reason: provider.StopReasonUserInterrupted}); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder

	if code, err := cmdHandoff(&Logger{out: &out, err: &out}, home, []string{"cursor-main"}); code != 0 || err != nil {
		t.Fatalf("code=%d err=%v\n%s", code, err, out.String())
	}

	chats := cursorChats(t, accountNamed(t, home, "cursor-main").ConfigDir)

	if len(chats) != 1 || !strings.Contains(chats[0].Prompt, "You are continuing a coding task started by another coding agent (claude, account alpha)") {
		t.Fatalf("Cursor did not receive the brief: %+v\n%s", chats, out.String())
	}

	st, _, err := handoff.Latest(s.handoffRoot(project))
	if err != nil || st.Target.Provider != provider.CursorID || st.Target.SessionID != chats[0].ID || st.Source.SessionID != "seed" {
		t.Fatalf("handoff %s: source session %q, target %s session %q, chat %s (%v)", st.ID, st.Source.SessionID, st.Target.Provider, st.Target.SessionID, chats[0].ID, err)
	}
}

func TestServiceReportsCursorCapabilities(t *testing.T) {
	quietFakes(t)
	_, srv := testDaemon(t)
	fakeShim(t, t.TempDir(), provider.CursorID)

	_, _, raw := call(t, srv, "GET", "/v1/status", nil)

	var status struct {
		Providers []providerInfo `json:"providers"`
	}

	json.Unmarshal(raw, &status)

	i := slices.IndexFunc(status.Providers, func(p providerInfo) bool { return p.ID == provider.CursorID })
	if i < 0 {
		t.Fatalf("cursor missing from %s", raw)
	}

	got := status.Providers[i]
	want := provider.Capabilities{Interactive: true, SessionTracking: true, SessionResume: true, Tasks: true, HandoffTarget: true}

	if !got.Installed || got.Name != "Cursor CLI" || got.Version != "2026.10.14-fake" || !got.SharedLogin || !got.Resume || got.Capabilities == nil || *got.Capabilities != want {
		t.Fatalf("cursor = %+v caps %+v", got, got.Capabilities)
	}
}

func TestBuildRoomExplainsWhyAnOldCursorCannotTakeTasks(t *testing.T) {
	_, s, _ := cursorHome(t, map[string]string{provider.CursorID: "cursor-main"})

	if c := agentChoices(s)[slices.Index(provider.IDs, provider.CursorID)]; !c.Eligible || c.Account != "cursor-main" {
		t.Fatalf("a signed-in Cursor must be eligible: %+v", c)
	}

	t.Setenv("CSM_FAKE_CURSOR_OLD", "1")

	if c := agentChoices(s)[slices.Index(provider.IDs, provider.CursorID)]; c.Eligible || !strings.Contains(c.Reason, "takes no opening instruction") {
		t.Fatalf("an old Cursor must say why it cannot take a task: %+v", c)
	}
}

func TestServiceHandoffPreviewSaysACursorChatIsNotRead(t *testing.T) {
	quietFakes(t)
	d, srv := testDaemon(t)
	fakeShim(t, t.TempDir(), provider.CursorID)
	codex := fakeShim(t, t.TempDir(), provider.CodexID)
	s, _ := loadState(d.home)
	project, _ := filepath.EvalSymlinks(t.TempDir())
	exec.Command("git", "-C", project, "init", "-q").Run()

	for id, name := range map[string]string{provider.CursorID: "cursor-main", provider.CodexID: "codex-main"} {
		p, _ := provider.New(id, map[string]string{provider.CursorID: os.Getenv("CSM_CURSOR"), provider.CodexID: codex}[id])
		a, _ := s.addAccount(id, name, time.Now())
		os.MkdirAll(a.ConfigDir, 0o700)
		p.Login(a.ConfigDir)

		st, err := verifyProfile(p, a)
		if err != nil {
			t.Fatal(err)
		}

		a.Email, a.VerifiedAt = st.Email, time.Now()
	}

	s.save()
	s.saveCheckpoint(Checkpoint{Version: stateVersion, Provider: provider.CursorID, ProjectDir: project, Account: "cursor-main", SessionID: "chat-1", CheckpointedAt: time.Now()})

	_, out, raw := call(t, srv, "POST", "/v1/handoffs", map[string]any{"project_dir": project, "target_account": "codex-main", "preview": true})

	if out["task_context"] != "none; Cursor CLI sessions cannot be read (repository state only)" || out["source"] != provider.CursorID {
		t.Fatalf("preview = %s", raw)
	}
}
