package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/handoff"
	"github.com/onoja123/csm/internal/provider"
)

func handoffList(ids ...string) func(s *State, projectDir string) {
	return func(s *State, projectDir string) {
		s.Config.setHandoffTo(ids)
		s.save()
	}
}

func TestUsageLimitOnEveryAccountHandsTheTaskToTheNextAgent(t *testing.T) {
	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha,beta", crossTo: provider.CodexID, seed: seedFile(t), before: handoffList("gemini", "codex")})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}

	if res.active != "work-codex" || !res.resumed {
		t.Fatalf("active=%s resumed=%v\n%s", res.active, res.resumed, res.output)
	}

	for _, want := range []string{"alpha account became unavailable", "Restoring session", "beta reached its usage limit and no other Claude Code account is ready", "Automatic handoff: beta (Claude Code) → work-codex (Codex).", "Capturing task context", "Saving handoff", "Handing the task to Codex", "Starting Codex as work-codex"} {
		if !strings.Contains(res.output, want) {
			t.Errorf("output missing %q:\n%s", want, res.output)
		}
	}

	transcript := res.files["target-transcript"]

	for _, want := range []string{`"profile":"work-codex"`, "You are continuing a coding task started by another coding agent (claude, account beta)", "Verify the repository", "The task: Add retry logic to the webhook"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("codex did not receive %q:\n%s", want, transcript)
		}
	}

	var st handoff.State

	if err := json.Unmarshal([]byte(res.files["handoff-json"]), &st); err != nil || st.Reason != string(provider.StopReasonUsageLimit) || st.Source.Account != "beta" || st.Target.Account != "work-codex" {
		t.Fatalf("handoff = %+v %v", st, err)
	}

	if o := st.Outcome; o == nil || !o.Started || o.Delivery != handoff.DeliveryInstruction || o.ProjectDir == "" || !o.Verification.SameRoot {
		t.Fatalf("outcome = %+v", st.Outcome)
	}

	if st.Source.SessionID == "" || st.Target.SessionID == "" || st.Source.SessionID == st.Target.SessionID {
		t.Fatalf("the handoff must link the session it left and the one it started: %+v → %+v", st.Source, st.Target)
	}

	if st.SourceCommit != "" {
		t.Fatal("csm must not commit in a repository outside Build Room")
	}

	if res.files["handoff-count"] != "2" {
		t.Fatalf("one handoff per limit expected (alpha→beta, beta→codex), got %s", res.files["handoff-count"])
	}

	if res.gitBefore != res.gitAfter {
		t.Fatalf("git changed: %q → %q", res.gitBefore, res.gitAfter)
	}

	if a := res.accounts["alpha"]; len(a.RecentFailures) != 1 {
		t.Fatalf("alpha's limit was recorded %d times", len(a.RecentFailures))
	}
}

func TestNoAutomaticHandoffWithoutAHandoffList(t *testing.T) {
	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha,beta", crossTo: provider.CodexID, seed: seedFile(t)})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}

	if res.files["target-transcript"] != "" || strings.Contains(res.output, "Automatic handoff") {
		t.Fatalf("handed off without being configured:\n%s", res.output)
	}

	if !strings.Contains(res.output, "All configured accounts are currently unavailable. Automatic failover paused.") {
		t.Fatalf("output:\n%s", res.output)
	}
}

func TestAutomaticHandoffStopsWhenTheContextCannotBeSaved(t *testing.T) {
	block := func(s *State, projectDir string) {
		handoffList("codex")(s, projectDir)
		os.RemoveAll(filepath.Join(s.Home, handoffsDir))
		os.WriteFile(filepath.Join(s.Home, handoffsDir), []byte("not a directory"), 0o600)
	}

	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha,beta", crossTo: provider.CodexID, seed: seedFile(t), before: block})

	if err == nil || !strings.Contains(err.Error(), "automatic handoff stopped") {
		t.Fatalf("err = %v\n%s", err, res.output)
	}

	for _, want := range []string{"Automatic handoff stopped: the task context could not be saved", "Codex was not started", "csm handoff work-codex"} {
		if !strings.Contains(res.output, want) {
			t.Errorf("output missing %q:\n%s", want, res.output)
		}
	}

	if res.files["target-transcript"] != "" {
		t.Fatal("codex started without the task context")
	}

	if res.gitBefore != res.gitAfter {
		t.Fatalf("git changed: %q → %q", res.gitBefore, res.gitAfter)
	}
}

func TestCrossFallbackChoosesOnlyUsableAgentsAndIsBounded(t *testing.T) {
	home, s, project := checkpointHome(t)
	r, err := newRunnerAt(&Logger{out: &strings.Builder{}, err: &strings.Builder{}}, s, provider.ClaudeID, testBinary(t), project)
	if err != nil {
		t.Fatal(err)
	}

	r.unavailable = map[string]bool{}
	now := time.Now()

	if got := r.crossFallback(now); got != "" {
		t.Fatalf("no list configured, got %q", got)
	}

	s.Config.setHandoffTo([]string{"claude", "copilot", "codex"})

	if got := r.crossFallback(now); got != "work-codex" {
		t.Fatalf("got %q; the running agent and an uninstalled one must be skipped", got)
	}

	r.autoHops = maxAutoHandoffs

	if got := r.crossFallback(now); got != "" {
		t.Fatalf("hop limit ignored: %q", got)
	}

	r.autoHops, r.tried = 0, map[string]bool{provider.CodexID: true}

	if got := r.crossFallback(now); got != "" {
		t.Fatalf("an agent already used in this run was chosen again: %q", got)
	}

	r.tried = nil
	codex, _ := s.resolveAccount("work-codex")
	codex.CooldownUntil = now.Add(time.Hour)

	if got := r.crossFallback(now); got != "" {
		t.Fatalf("an account in cooldown was chosen: %q", got)
	}

	codex.CooldownUntil = time.Time{}
	os.Remove(filepath.Join(codex.ConfigDir, "fake-auth"))

	if got := r.crossFallback(now); got != "" {
		t.Fatalf("a logged-out account was chosen: %q", got)
	}

	if err := s.Config.setHandoffTo([]string{"not-an-agent"}); err == nil {
		t.Fatal("unknown agent accepted")
	}

	if err := s.Config.setHandoffTo([]string{"cursor"}); err != nil {
		t.Fatalf("Cursor CLI must be a valid handoff target: %v", err)
	}

	_ = home
}

func TestRepeatedLimitReportsAreHandledOnce(t *testing.T) {
	_, s, project := checkpointHome(t)
	r, err := newRunnerAt(&Logger{out: &strings.Builder{}, err: &strings.Builder{}}, s, provider.ClaudeID, testBinary(t), project)
	if err != nil {
		t.Fatal(err)
	}

	r.unavailable = map[string]bool{}
	s.Config.setPolicy(policyAutomatic)
	s.Config.setHandoffTo([]string{"codex"})
	s.save()
	alpha, _ := s.resolveAccount("alpha")
	var res outcome
	stops := 0

	for i := 0; i < 3; i++ {
		r.handleFailure(alpha, &res, r.limitFailure(), false, func() { stops++ })
	}

	if stops != 1 || res.crossTo != "work-codex" || !res.autoHandoff {
		t.Fatalf("stops=%d crossTo=%q auto=%v", stops, res.crossTo, res.autoHandoff)
	}

	fresh, _ := loadState(s.Home)
	a, _ := fresh.resolveAccount("alpha")

	if len(a.RecentFailures) != 1 {
		t.Fatalf("the limit was recorded %d times", len(a.RecentFailures))
	}
}

func TestAutoHandoffSettingAndStatus(t *testing.T) {
	home, _, _ := checkpointHome(t)
	var out strings.Builder
	log := &Logger{out: &out, err: &out}

	if err := cmdAuto(log, home, []string{"handoff", "codex,gemini,codex"}); err != nil {
		t.Fatal(err)
	}

	if s, _ := loadState(home); strings.Join(s.Config.HandoffTo, ",") != "codex,gemini" {
		t.Fatalf("handoff_to = %v", s.Config.HandoffTo)
	}

	if !strings.Contains(out.String(), "Hand off to another agent: Codex → Gemini CLI") {
		t.Fatalf("status:\n%s", out.String())
	}

	if err := cmdAuto(log, home, []string{"handoff", "nope"}); err == nil {
		t.Fatal("unknown agent accepted")
	}

	out.Reset()

	if err := cmdAuto(log, home, []string{"handoff", "off"}); err != nil || !strings.Contains(out.String(), "Hand off to another agent: off") {
		t.Fatalf("off: %v\n%s", err, out.String())
	}
}

func TestAutomaticHandoffToGeminiAndCopilot(t *testing.T) {
	for _, target := range []string{provider.GeminiID, provider.CopilotID} {
		t.Run(target, func(t *testing.T) {
			res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha,beta", crossTo: target, seed: seedFile(t), before: handoffList(target)})
			if err != nil {
				t.Fatalf("%v\n%s", err, res.output)
			}

			if res.active != "work-"+target || !strings.Contains(res.files["target-transcript"], "You are continuing a coding task") || !strings.Contains(res.output, "Automatic handoff: beta (Claude Code)") {
				t.Fatalf("active=%s\n%s\n%s", res.active, res.files["target-transcript"], res.output)
			}

			if res.gitBefore != res.gitAfter {
				t.Fatalf("git changed: %q → %q", res.gitBefore, res.gitAfter)
			}
		})
	}
}

func TestAutomaticHandoffRecordsADestinationThatDidNotStart(t *testing.T) {
	dir := t.TempDir()
	st := handoff.State{Version: handoff.Version, ID: "20261012T100000Z-claude-codex", Stage: handoff.StageMCPPrepared, Outcome: &handoff.Outcome{Status: handoff.OutcomeStarting, Delivery: handoff.DeliveryInstruction}}

	if err := handoff.Save(dir, &st); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	err := autoHandoffFailed(&Logger{out: &out, err: &out}, &nextRun{auto: true, handoffDir: dir, target: "work-codex"}, os.ErrNotExist)

	if err != os.ErrNotExist {
		t.Fatalf("err = %v", err)
	}

	back, _ := handoff.Load(dir)

	if back.Stage != handoff.StageFailed || back.Outcome.Status != handoff.OutcomeFailed {
		t.Fatalf("recorded as %s/%s", back.Stage, back.Outcome.Status)
	}

	for _, want := range []string{"Automatic handoff to work-codex did not start", "The task context is kept at", "csm handoff work-codex"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}

	started := handoff.State{Version: handoff.Version, ID: "20261012T100001Z-claude-codex", Stage: handoff.StageTargetStarted, Outcome: &handoff.Outcome{Status: handoff.OutcomeComplete, Started: true}}
	dir2 := t.TempDir()
	handoff.Save(dir2, &started)
	autoHandoffFailed(&Logger{out: &out, err: &out}, &nextRun{auto: true, handoffDir: dir2, target: "work-codex"}, os.ErrClosed)

	if back, _ := handoff.Load(dir2); back.Stage != handoff.StageTargetStarted {
		t.Fatal("an agent that did start was marked failed by a later error")
	}
}
