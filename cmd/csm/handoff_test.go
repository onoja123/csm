package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/handoff"
	"github.com/onoja123/csm/internal/provider"
)

const seedTranscript = `{"type":"user","message":{"role":"user","content":"Add retry logic to the webhook"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"I'll add backoff in main.go."},{"type":"tool_use","name":"Edit","input":{"file_path":"main.go"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"export GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Backoff is in. Next steps:\n1. Cap retries at 5\n"}]}}
`

// seedFile is a transcript the fake Claude Code copies into each new session, so a switch has real content to extract.
func seedFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed.jsonl")

	if err := os.WriteFile(path, []byte(seedTranscript), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// writeAlphaTranscript records a finished session "seed" for alpha, the way a checkpoint would point at it.
func writeAlphaTranscript(s *State, projectDir string) {
	alpha, _ := s.resolveAccount("alpha")
	dir := filepath.Join(alpha.ConfigDir, "projects", strings.ReplaceAll(projectDir, "/", "-"))
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "seed.jsonl"), []byte(seedTranscript), 0o600)
}

func TestSameAgentSwitchWritesHandoffAndInjectsIt(t *testing.T) {
	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha", seed: seedFile(t)})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}

	if res.active != "beta" || !res.resumed {
		t.Fatalf("active=%s resumed=%v\n%s", res.active, res.resumed, res.output)
	}

	md := res.files["handoff-md"]

	for _, want := range []string{"# CSM Handoff", "Source: claude/alpha", "Target: claude/beta", "Reason: usage_limit", "The native session was carried over", "Branch: cards", "main.go"} {
		if !strings.Contains(md, want) {
			t.Errorf("handoff.md missing %q:\n%s", want, md)
		}
	}

	var st handoff.State

	if err := json.Unmarshal([]byte(res.files["handoff-json"]), &st); err != nil || st.Version != handoff.Version || st.Stage != handoff.StageTargetStarted || !st.Resumed {
		t.Fatalf("handoff.json = %+v %v", st, err)
	}

	for _, want := range []string{"Capturing task context", "Capturing git state", "Saving handoff"} {
		if !strings.Contains(res.output, want) {
			t.Errorf("output missing %q:\n%s", want, res.output)
		}
	}

	if !strings.Contains(res.files["beta-transcript"], `"prompt":"csm moved this session from account \"alpha\" to \"beta\" (usage_limit). The conversation above is intact.`) {
		t.Fatalf("beta did not receive the resumed-session note:\n%s", res.files["beta-transcript"])
	}
}

func TestClaudeToCodexHandoff(t *testing.T) {
	requested := make(chan error, 1)
	res, err := runFailoverScenario(testBinary(t), scenario{
		hold:    "alpha",
		crossTo: provider.CodexID,
		seed:    seedFile(t),
		during:  switchWhenLiveTo(provider.ClaudeID, "work-codex", requested),
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}

	if err := <-requested; err != nil {
		t.Fatal(err)
	}

	if res.active != "work-codex" {
		t.Fatalf("active codex account = %q\n%s", res.active, res.output)
	}

	for _, want := range []string{"Switching alpha (Claude Code) → work-codex (Codex).", "Saving checkpoint", "Capturing task context", "Capturing git state", "Handing the task to Codex", "Starting Codex as work-codex", "Codex cannot read a Claude Code conversation"} {
		if !strings.Contains(res.output, want) {
			t.Errorf("output missing %q:\n%s", want, res.output)
		}
	}

	transcript := res.files["target-transcript"]

	for _, want := range []string{`"profile":"work-codex"`, "You are continuing a coding task started by another coding agent (claude, account alpha)", "handoff.md", "Do not revert existing work"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("codex did not receive the instruction (%q):\n%s", want, transcript)
		}
	}

	md := res.files["handoff-md"]

	for _, want := range []string{"Source: claude/alpha", "Target: codex/work-codex", "Reason: manual_switch", "## Current Task\n\nAdd retry logic to the webhook", "I'll add backoff in main.go.", "## Files Modified\n\n- main.go", "Branch: cards", "## Next Steps\n\n1. Cap retries at 5"} {
		if !strings.Contains(md, want) {
			t.Errorf("handoff.md missing %q:\n%s", want, md)
		}
	}

	for _, leak := range []string{"ghp_", "GITHUB_TOKEN=gh"} {
		if strings.Contains(md, leak) || strings.Contains(res.output, leak) || strings.Contains(transcript, leak) {
			t.Fatalf("tool output leaked into the handoff (%q)", leak)
		}
	}

	if res.gitBefore != res.gitAfter {
		t.Fatalf("git changed: %q → %q", res.gitBefore, res.gitAfter)
	}

	if res.checkpoint.Account != "alpha" || res.checkpoint.Reason != provider.StopReasonManualSwitch {
		t.Fatalf("checkpoint = %+v", res.checkpoint)
	}
}

func TestCodexToClaudeHandoff(t *testing.T) {
	requested := make(chan error, 1)
	res, err := runFailoverScenario(testBinary(t), scenario{
		provider: provider.CodexID,
		hold:     "alpha",
		crossTo:  provider.ClaudeID,
		during:   switchWhenLiveTo(provider.CodexID, "work-claude", requested),
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}

	if err := <-requested; err != nil {
		t.Fatal(err)
	}

	if res.active != "work-claude" || !strings.Contains(res.output, "Switching alpha (Codex) → work-claude (Claude Code).") {
		t.Fatalf("active=%s\n%s", res.active, res.output)
	}

	transcript := res.files["target-transcript"]

	if !strings.Contains(transcript, `"profile":"work-claude"`) || !strings.Contains(transcript, "You are continuing a coding task started by another coding agent (codex, account alpha)") {
		t.Fatalf("claude did not receive the instruction:\n%s", transcript)
	}

	if md := res.files["handoff-md"]; !strings.Contains(md, "Source: codex/alpha") || !strings.Contains(md, "Target: claude/work-claude") {
		t.Fatalf("handoff.md:\n%s", md)
	}
}

func TestClaudeToGeminiAndCopilotHandoff(t *testing.T) {
	for _, target := range []string{provider.GeminiID, provider.CopilotID} {
		t.Run(target, func(t *testing.T) {
			requested := make(chan error, 1)
			res, err := runFailoverScenario(testBinary(t), scenario{hold: "alpha", crossTo: target, during: switchWhenLiveTo(provider.ClaudeID, "work-"+target, requested)})
			if err != nil {
				t.Fatalf("%v\n%s", err, res.output)
			}

			if err := <-requested; err != nil {
				t.Fatal(err)
			}

			transcript := res.files["target-transcript"]

			if res.active != "work-"+target || !strings.Contains(transcript, "You are continuing a coding task") || !strings.Contains(transcript, "handoff.md") {
				t.Fatalf("active=%s transcript=%s\n%s", res.active, transcript, res.output)
			}
		})
	}
}

// switchWhenLiveTo asks the running alpha session to move to any account once the fake agent has written its transcript.
func switchWhenLiveTo(providerID, to string, requested chan<- error) func(home, projectDir string) {
	return func(home, projectDir string) {
		deadline := time.Now().Add(10 * time.Second)

		for time.Now().Before(deadline) {
			s, err := loadState(home)
			if err == nil {
				sess, live := s.loadLiveSession(providerID, projectDir)
				if live && sess.Account == "alpha" && transcriptWritten(filepath.Join(s.accountsDir(), "alpha")) {
					requested <- requestSwitch(&Logger{out: io.Discard, err: io.Discard}, s, sess, to)

					return
				}
			}

			time.Sleep(50 * time.Millisecond)
		}

		requested <- errors.New("session never became live")
	}
}

func TestHandoffCommandStartsTargetFromLastCheckpoint(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	s, err := initState(home)
	if err != nil {
		t.Fatal(err)
	}

	project := t.TempDir()
	project, _ = filepath.EvalSymlinks(project)
	shims := map[string]string{}

	for _, id := range []string{provider.ClaudeID, provider.CodexID} {
		shim := filepath.Join(home, id)
		os.WriteFile(shim, []byte("#!/bin/sh\nexec '"+bin+"' __fake-"+id+" \"$@\"\n"), 0o700)
		shims[id] = shim
		t.Setenv("CSM_"+strings.ToUpper(id), shim)
	}

	for id, name := range map[string]string{provider.ClaudeID: "alpha", provider.CodexID: "work-codex"} {
		p, _ := provider.New(id, shims[id])
		a, err := s.addAccount(id, name, time.Now())
		if err != nil {
			t.Fatal(err)
		}

		os.MkdirAll(a.ConfigDir, 0o700)
		p.Login(a.ConfigDir)
		st, err := verifyProfile(p, a)
		if err != nil {
			t.Fatal(err)
		}

		a.Email, a.VerifiedAt = st.Email, time.Now()
	}

	if err := s.save(); err != nil {
		t.Fatal(err)
	}

	writeAlphaTranscript(s, project)
	cp := Checkpoint{Version: stateVersion, Provider: provider.ClaudeID, ProjectDir: project, Account: "alpha", SessionID: "seed", CheckpointedAt: time.Now(), Reason: provider.StopReasonUserInterrupted}

	if err := s.saveCheckpoint(cp); err != nil {
		t.Fatal(err)
	}

	wd, _ := os.Getwd()
	os.Chdir(project)
	defer os.Chdir(wd)

	var out strings.Builder
	log := &Logger{out: &out, err: &out}
	code, err := cmdHandoff(log, home, []string{"work-codex"})

	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v\n%s", code, err, out.String())
	}

	for _, want := range []string{"CSM handoff", "Current agent", "Claude Code / alpha (checkpoint", "Target agent", "Codex / work-codex", "Task context", "from the last checkpoint", "Capturing task context", "Handoff:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}

	matches, _ := filepath.Glob(filepath.Join(s.accountsDir(), "work-codex", "sessions", "*", "*", "*", "rollout-*.jsonl"))

	if len(matches) != 1 {
		t.Fatalf("codex rollouts = %v", matches)
	}

	rollout, _ := os.ReadFile(matches[0])

	if !strings.Contains(string(rollout), "You are continuing a coding task started by another coding agent (claude, account alpha)") || !strings.Contains(string(rollout), "Add retry logic to the webhook") {
		t.Fatalf("codex did not receive the handoff:\n%s", rollout)
	}

	reloaded, _ := loadState(home)

	if reloaded.active(provider.CodexID) != "work-codex" {
		t.Fatal("target not made active")
	}

	st, _, err := handoff.Latest(reloaded.handoffRoot(project))

	if err != nil || st.Source.Account != "alpha" || st.Target.Account != "work-codex" || st.Stage != handoff.StageTargetStarted || len(st.Context.UserRequests) != 1 {
		t.Fatalf("latest handoff = %+v %v", st, err)
	}

	out.Reset()

	if code, err := cmdHandoff(log, home, []string{"--provider", "codex"}); err != nil || code != 0 || !strings.Contains(out.String(), "Codex / work-codex") {
		t.Fatalf("--provider form: code=%d err=%v\n%s", code, err, out.String())
	}

	if _, err := cmdHandoff(log, home, nil); err == nil {
		t.Fatal("usage error expected")
	}

	if _, err := cmdHandoff(log, home, []string{"nobody"}); err == nil {
		t.Fatal("unknown account accepted")
	}

	out.Reset()

	if code, err := cmdHandoff(log, home, []string{"alpha"}); err != nil || code != 0 || !strings.Contains(out.String(), "Target agent") || !strings.Contains(out.String(), "Claude Code / alpha") {
		t.Fatalf("handoff back to claude: code=%d err=%v\n%s", code, err, out.String())
	}
}

func TestHandoffCommandRefusesUnusableTargets(t *testing.T) {
	s := newTestState(t, "alpha", "beta")
	beta, _ := s.resolveAccount("beta")
	beta.Enabled = false
	alpha, _ := s.resolveAccount("alpha")
	alpha.VerifiedAt = time.Time{}

	if err := s.save(); err != nil {
		t.Fatal(err)
	}

	log := &Logger{out: io.Discard, err: io.Discard}

	if _, err := cmdHandoff(log, s.Home, []string{"beta"}); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("got %v", err)
	}

	if _, err := cmdHandoff(log, s.Home, []string{"alpha"}); err == nil || !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("got %v", err)
	}
}

func TestDoctorHandoffRows(t *testing.T) {
	s := newTestState(t, "alpha")
	shim := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(shim, []byte("#!/bin/sh\nexec '"+testBinary(t)+"' __fake-claude \"$@\"\n"), 0o700)
	var out strings.Builder
	var problems []string
	doctorHandoff(&Logger{out: &out, err: &out}, s, map[string]provider.Provider{provider.ClaudeID: provider.Claude{Path: shim}}, t.TempDir(), func(label, reason string) { problems = append(problems, label) })

	for _, want := range []string{"Handoff directory writable", "Git repository here", "Claude Code task context extraction", "Claude Code handoff injection"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}

	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
}
