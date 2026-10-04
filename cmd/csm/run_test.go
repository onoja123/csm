package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

// The fake agents re-invoke this test binary as csm.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == "hook" || strings.HasPrefix(os.Args[1], "__fake-")) {
		os.Exit(run(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func testBinary(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFailoverSwitchesAndResumes(t *testing.T) {
	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha"})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}
	if res.active != "beta" {
		t.Fatalf("active = %s, want beta\n%s", res.active, res.output)
	}
	if res.checkpoint.Account != "alpha" || res.checkpoint.Reason != provider.StopReasonUsageLimit || res.checkpoint.Branch != "cards" {
		t.Fatalf("checkpoint = %+v", res.checkpoint)
	}
	if !res.resumed {
		t.Fatalf("beta did not resume the session\n%s", res.output)
	}
	if res.gitBefore != res.gitAfter {
		t.Fatalf("git changed: %q → %q", res.gitBefore, res.gitAfter)
	}
	if res.exitCode != 0 {
		t.Fatalf("exit code = %d", res.exitCode)
	}
	for _, want := range []string{"alpha account became unavailable", "known usage limit", "Restoring session"} {
		if !strings.Contains(res.output, want) {
			t.Errorf("output missing %q:\n%s", want, res.output)
		}
	}
}

func TestFailoverPausesWhenAllAccountsLimited(t *testing.T) {
	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha,beta"})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}
	if strings.Count(res.output, "became unavailable") != 1 {
		t.Fatalf("expected exactly one switch:\n%s", res.output)
	}
	if !strings.Contains(res.output, "All configured accounts are currently unavailable") {
		t.Fatalf("missing pause message:\n%s", res.output)
	}
}

func TestNetworkErrorDoesNotSwitch(t *testing.T) {
	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha", fakeError: "overloaded"})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}
	if res.active != "alpha" || strings.Contains(res.output, "became unavailable") {
		t.Fatalf("switched on a network error:\n%s", res.output)
	}
	if !strings.Contains(res.output, "network or service error") {
		t.Fatalf("error not reported:\n%s", res.output)
	}
}

// switchWhenLive asks the running alpha session to move to beta as soon as it is up.
func switchWhenLive(providerID string, requested chan<- error) func(home, projectDir string) {
	return func(home, projectDir string) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			s, err := loadState(home)
			if err == nil {
				if sess, live := s.loadLiveSession(providerID, projectDir); live && sess.Account == "alpha" {
					requested <- requestSwitch(s, sess, "beta")
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		requested <- errors.New("session never became live")
	}
}

func TestManualSwitchOfRunningSession(t *testing.T) {
	for _, providerID := range provider.IDs {
		t.Run(providerID, func(t *testing.T) {
			requested := make(chan error, 1)
			res, err := runFailoverScenario(testBinary(t), scenario{provider: providerID, hold: "alpha", during: switchWhenLive(providerID, requested)})
			if err != nil {
				t.Fatalf("%v\n%s", err, res.output)
			}
			if err := <-requested; err != nil {
				t.Fatal(err)
			}
			if res.active != "beta" || !res.resumed {
				t.Fatalf("active=%s resumed=%v\n%s", res.active, res.resumed, res.output)
			}
			if res.checkpoint.Reason != provider.StopReasonManualSwitch {
				t.Fatalf("checkpoint reason = %s", res.checkpoint.Reason)
			}
			if !strings.Contains(res.output, "Switching alpha → beta") {
				t.Fatalf("output:\n%s", res.output)
			}
		})
	}
}

func TestCodexFailoverSwitchesAndResumes(t *testing.T) {
	res, err := runFailoverScenario(testBinary(t), scenario{provider: provider.CodexID, limited: "alpha"})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}
	if res.active != "beta" || !res.resumed {
		t.Fatalf("active=%s resumed=%v\n%s", res.active, res.resumed, res.output)
	}
	if res.checkpoint.Account != "alpha" || res.checkpoint.Reason != provider.StopReasonUsageLimit || res.checkpoint.SessionID == "" {
		t.Fatalf("checkpoint = %+v", res.checkpoint)
	}
	if res.gitBefore != res.gitAfter {
		t.Fatalf("git changed: %q → %q", res.gitBefore, res.gitAfter)
	}
	if alpha := res.usage["alpha"]; !alpha.Usage.Limited || alpha.Usage.FiveHour.UsedPercent != 100 || alpha.Usage.SevenDay.UsedPercent != 17 {
		t.Fatalf("polled usage not recorded: %+v", res.usage)
	}
	for _, want := range []string{"alpha account became unavailable", "known usage limit", "Restoring session", "Starting Codex as beta"} {
		if !strings.Contains(res.output, want) {
			t.Errorf("output missing %q:\n%s", want, res.output)
		}
	}
}

func TestCodexFailoverPausesWhenAllAccountsLimited(t *testing.T) {
	res, err := runFailoverScenario(testBinary(t), scenario{provider: provider.CodexID, limited: "alpha,beta"})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}
	if strings.Count(res.output, "became unavailable") != 1 {
		t.Fatalf("expected exactly one switch:\n%s", res.output)
	}
	if !strings.Contains(res.output, "All configured accounts are currently unavailable") {
		t.Fatalf("missing pause message:\n%s", res.output)
	}
}

func TestCodexProfileAndUsage(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "codex")
	os.WriteFile(shim, []byte("#!/bin/sh\nexec '"+testBinary(t)+"' __fake-codex \"$@\"\n"), 0o700)
	p := provider.Codex{Path: shim}
	t.Setenv("CSM_FAKE_LIMIT", "limited")

	features, err := p.Features()
	if err != nil || !features.CanIsolate() || !features.CanContinue() || !features.UsageProbe || features.SessionID {
		t.Fatalf("features = %+v, %v", features, err)
	}
	s := newTestState(t)
	s.addAccount(provider.CodexID, "work", testNow)
	s.addAccount(provider.CodexID, "limited", testNow)
	work, _ := s.resolveAccount("work")
	limited, _ := s.resolveAccount("limited")
	for _, a := range []*Account{work, limited} {
		os.MkdirAll(a.ConfigDir, 0o700)
		if err := checkFreshProfileIsolated(p, a.ConfigDir); err != nil {
			t.Fatalf("fresh profile: %v", err)
		}
		if _, err := verifyProfile(p, a); err == nil || !strings.Contains(err.Error(), "not logged in") {
			t.Fatalf("got %v, want not logged in", err)
		}
		if err := p.Login(a.ConfigDir); err != nil {
			t.Fatal(err)
		}
		st, err := verifyProfile(p, a)
		if err != nil || st.Email != a.Name+"@example.test" {
			t.Fatalf("got %+v, %v", st, err)
		}
		a.Email, a.VerifiedAt = st.Email, testNow
	}
	if err := checkFreshProfileIsolated(p, work.ConfigDir); err == nil {
		t.Fatal("a logged-in directory passed the fresh-profile check")
	}

	if errs := refreshUsage(s, p, []string{"work", "limited"}, time.Now()); len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	rec, ok := s.loadUsage("work")
	if !ok || rec.Usage.FiveHour.UsedPercent != 42 || rec.Usage.SevenDay.UsedPercent != 17 || rec.Usage.Limited {
		t.Fatalf("work = %+v", rec)
	}
	if rec, _ := s.loadUsage("limited"); !rec.Usage.Limited {
		t.Fatalf("limited = %+v", rec)
	}
	if id := p.CurrentSession(work.ConfigDir, t.TempDir(), time.Now()); id != "" {
		t.Fatalf("found session %q in a profile with none", id)
	}
	if carried, err := p.CarrySession(work.ConfigDir, limited.ConfigDir, "missing"); len(carried.Args) > 0 || err == nil {
		t.Fatalf("carried a session that does not exist: %v %v", carried, err)
	}
}

func TestRecoversInterruptedSwitch(t *testing.T) {
	const sid = "11111111-2222-4333-8444-555555555555"
	before := func(s *State, projectDir string) {
		alpha, _ := s.resolveAccount("alpha")
		transcript := filepath.Join(alpha.ConfigDir, "projects", strings.ReplaceAll(projectDir, "/", "-"), sid+".jsonl")
		os.MkdirAll(filepath.Dir(transcript), 0o700)
		os.WriteFile(transcript, []byte(`{"profile":"alpha"}`+"\n"), 0o600)
		s.saveCheckpoint(Checkpoint{Version: stateVersion, ProjectDir: projectDir, Account: "alpha", SessionID: sid, CheckpointedAt: time.Now(), Reason: provider.StopReasonUsageLimit})
		writeJSON(filepath.Join(s.projectStateDir(provider.ClaudeID, projectDir), transitionFile), Transition{
			Version: stateVersion, ProjectDir: projectDir, From: "alpha", To: "beta", SessionID: sid, Reason: provider.StopReasonUsageLimit, CSMPID: 1 << 30,
		})
	}
	res, err := runFailoverScenario(testBinary(t), scenario{before: before})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}
	if res.active != "beta" || !res.resumed {
		t.Fatalf("active=%s resumed=%v\n%s", res.active, res.resumed, res.output)
	}
	if !strings.Contains(res.output, "Previous switch did not complete: alpha → beta") {
		t.Fatalf("output:\n%s", res.output)
	}
}

func TestUsageRecordedFromStatusLine(t *testing.T) {
	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha"})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}
	alpha, beta := res.usage["alpha"], res.usage["beta"]
	if alpha.Usage.FiveHour.UsedPercent != 100 || beta.Usage.FiveHour.UsedPercent != 42 || beta.Usage.SevenDay.UsedPercent != 17 {
		t.Fatalf("usage = %+v", res.usage)
	}
	if alpha.Account != "alpha" || alpha.UpdatedAt.IsZero() {
		t.Fatalf("record = %+v", alpha)
	}
}

func TestPrintUsage(t *testing.T) {
	s := newTestState(t, "personal", "work", "backup")
	s.saveUsage(UsageRecord{Version: stateVersion, Account: "personal", UpdatedAt: testNow.Add(-2 * time.Minute), Usage: provider.Usage{
		FiveHour: provider.UsageWindow{UsedPercent: 23.4, ResetsAt: testNow.Add(3 * time.Hour)},
		SevenDay: provider.UsageWindow{UsedPercent: 41, ResetsAt: testNow.Add(-time.Hour)},
	}})
	var out strings.Builder
	printUsage(&out, s, []string{"personal", "work"}, nil, testNow)
	got := out.String()
	for _, want := range []string{"● personal     updated 2m0s ago", "5-hour    23%   resets", "7-day    window reset at", "(was 41%)", "○ work         no usage seen yet", "csm usage work", "--cached"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "backup") {
		t.Errorf("printed an account that was not asked for:\n%s", got)
	}
	if summary := shortUsage(provider.Usage{FiveHour: provider.UsageWindow{UsedPercent: 50, ResetsAt: testNow.Add(time.Hour)}, SevenDay: provider.UsageWindow{UsedPercent: 9, ResetsAt: testNow.Add(-time.Hour)}}, testNow); summary != "5h 50%" {
		t.Errorf("shortUsage = %q", summary)
	}
}

func TestRefreshUsageLive(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(shim, []byte("#!/bin/sh\nexec '"+testBinary(t)+"' __fake-claude \"$@\"\n"), 0o700)
	p := provider.Claude{Path: shim}
	t.Setenv("CSM_FAKE_LIMIT", "work")

	s := newTestState(t, "personal", "work", "backup", "stranger")
	for _, name := range []string{"personal", "work"} {
		a, _ := s.resolveAccount(name)
		os.MkdirAll(a.ConfigDir, 0o700)
		p.Login(a.ConfigDir)
		a.Email = name + "@example.test"
	}
	backup, _ := s.resolveAccount("backup")
	backup.Enabled = false
	stranger, _ := s.resolveAccount("stranger")
	stranger.VerifiedAt = time.Time{}

	now := time.Now()
	errs := refreshUsage(s, p, []string{"personal", "work", "backup", "stranger"}, now)
	if len(errs) != 2 || errs["backup"] == nil || errs["stranger"] == nil {
		t.Fatalf("errs = %v", errs)
	}
	personal, ok := s.loadUsage("personal")
	if !ok || personal.Usage.FiveHour.UsedPercent != 42 || personal.Usage.SevenDay.UsedPercent != 17 || personal.Usage.Limited {
		t.Fatalf("personal = %+v", personal)
	}
	work, _ := s.loadUsage("work")
	if !work.Usage.Limited || work.Usage.FiveHour.UsedPercent != 100 {
		t.Fatalf("work = %+v", work)
	}

	var out strings.Builder
	printUsage(&out, s, []string{"personal", "work", "stranger"}, errs, now)
	for _, want := range []string{"5-hour    42%", "limit reached", "could not check usage", "csm account login stranger"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

func TestVerifyProfile(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(shim, []byte("#!/bin/sh\nexec '"+testBinary(t)+"' __fake-claude \"$@\"\n"), 0o700)
	p := provider.Claude{Path: shim}
	dir := t.TempDir()
	a := &Account{Name: "work", ConfigDir: dir}

	if err := checkFreshProfileIsolated(p, dir); err != nil {
		t.Fatalf("fresh profile: %v", err)
	}
	if _, err := verifyProfile(p, a); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("got %v, want not logged in", err)
	}
	if err := p.Login(dir); err != nil {
		t.Fatal(err)
	}
	if err := checkFreshProfileIsolated(p, dir); err == nil {
		t.Fatal("a logged-in directory passed the fresh-profile check")
	}
	st, err := verifyProfile(p, a)
	if err != nil || st.Email != filepath.Base(dir)+"@example.test" {
		t.Fatalf("got %+v, %v", st, err)
	}
	a.Email = "someone-else@example.test"
	if _, err := verifyProfile(p, a); err == nil {
		t.Fatal("identity mismatch not detected")
	}
}

func TestHookWritesEventAndIgnoresOutsideCSM(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CSM_STATE_DIR", "")
	runHook("failure", strings.NewReader(`{"error":"rate_limit"}`))
	if _, err := os.Stat(filepath.Join(dir, eventFile)); err == nil {
		t.Fatal("hook wrote outside a csm session")
	}

	t.Setenv("CSM_STATE_DIR", dir)
	t.Setenv("CSM_PID", "999999999")
	runHook("session-start", strings.NewReader(`{"session_id":"s1","hook_event_name":"SessionStart"}`))
	var cs providerSession
	if err := readJSON(filepath.Join(dir, providerSessionFile), &cs); err != nil || cs.SessionID != "s1" {
		t.Fatalf("got %+v %v", cs, err)
	}
	runHook("failure", strings.NewReader(`{"session_id":"s1","error":"rate_limit","last_assistant_message":"limit · resets 5pm","extra":{"x":1}}`))
	var f provider.Failure
	if err := readJSON(filepath.Join(dir, eventFile), &f); err != nil || f.Reason != provider.StopReasonUsageLimit {
		t.Fatalf("got %+v %v", f, err)
	}
}
