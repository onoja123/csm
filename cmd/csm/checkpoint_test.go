package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/handoff"
	"github.com/onoja123/csm/internal/provider"
)

// checkpointHome is a state with a Claude account "alpha" holding a recorded session and a Codex account "work-codex", over a git project with uncommitted work.
func checkpointHome(t *testing.T) (string, *State, string) {
	t.Helper()
	bin := testBinary(t)
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

	git("init", "-q", "-b", "feature/webhook")
	os.WriteFile(filepath.Join(project, "main.go"), []byte("package main\n"), 0o600)
	git("add", ".")
	git("commit", "-q", "-m", "init")
	os.WriteFile(filepath.Join(project, "main.go"), []byte("package main // retries\n"), 0o600)

	for _, id := range []string{provider.ClaudeID, provider.CodexID} {
		shim := filepath.Join(home, id)
		os.WriteFile(shim, []byte("#!/bin/sh\nexec '"+bin+"' __fake-"+id+" \"$@\"\n"), 0o700)
		t.Setenv("CSM_"+strings.ToUpper(id), shim)
	}

	for id, name := range map[string]string{provider.ClaudeID: "alpha", provider.CodexID: "work-codex"} {
		p, _ := provider.Find(id)
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

	if err := s.saveCheckpoint(Checkpoint{Version: stateVersion, Provider: provider.ClaudeID, ProjectDir: project, Account: "alpha", SessionID: "seed", CheckpointedAt: time.Now(), Reason: provider.StopReasonUserInterrupted}); err != nil {
		t.Fatal(err)
	}

	wd, _ := os.Getwd()
	os.Chdir(project)
	t.Cleanup(func() { os.Chdir(wd) })

	return home, s, project
}

func gitStatus(t *testing.T, dir string) string {
	out, _ := exec.Command("git", "-C", dir, "status", "--short", "--branch").Output()

	return string(out)
}

func TestCheckpointSaveEditListShowAndContinueWithCodex(t *testing.T) {
	home, _, project := checkpointHome(t)
	var out strings.Builder
	log := &Logger{out: &out, err: &out}

	if err := cmdCheckpoint(log, home, nil); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}

	for _, want := range []string{"CSM checkpoint", "claude/alpha", "Add retry logic to the webhook", "feature/webhook", "Continue it with another agent: csm handoff --checkpoint"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("checkpoint output missing %q:\n%s", want, out.String())
		}
	}

	reloaded, _ := loadState(home)
	cp, cpDir, err := handoff.LatestCheckpoint(reloaded.handoffRoot(project), "")

	if err != nil || cp.Kind != handoff.KindCheckpoint || cp.Source.Account != "alpha" || cp.Source.SessionID != "seed" || cp.Git.Head == "" || cp.Git.Unstaged != 1 || cp.Completeness != handoff.CompletenessComplete {
		t.Fatalf("checkpoint = %+v %v", cp, err)
	}

	if len(cp.Context.FilesModified) != 1 || cp.Context.NextSteps[0] != "Cap retries at 5" {
		t.Fatalf("context = %+v", cp.Context)
	}

	out.Reset()

	if err := cmdCheckpoint(log, home, []string{"edit", cp.ID, "--task", "Finish the webhook retries", "--next", "Cap retries at 5", "--next", "Add a jitter test", "--notes", "staging key ghp_abcdefghijklmnopqrstuvwxyz0123 is not needed"}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}

	edited, err := handoff.Load(cpDir)
	if err != nil || edited.Edits == nil || edited.Edits.Task != "Finish the webhook retries" || len(edited.Edits.NextSteps) != 2 {
		t.Fatalf("edited = %+v %v", edited.Edits, err)
	}

	if edited.Context.UserRequests[0] != "Add retry logic to the webhook" {
		t.Fatal("the edit replaced the session record")
	}

	raw, _ := os.ReadFile(filepath.Join(cpDir, handoff.FileJSON))
	md, _ := os.ReadFile(filepath.Join(cpDir, handoff.FileMD))

	if strings.Contains(string(raw)+string(md), "ghp_") {
		t.Fatal("a token reached the checkpoint files")
	}

	if fi, _ := os.Stat(filepath.Join(cpDir, handoff.FileJSON)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}

	out.Reset()

	if err := cmdCheckpoint(log, home, []string{"list"}); err != nil || !strings.Contains(out.String(), cp.ID) || !strings.Contains(out.String(), "Finish the webhook retries") {
		t.Fatalf("list: %v\n%s", err, out.String())
	}

	out.Reset()

	if err := cmdCheckpoint(log, home, []string{"show", cp.ID}); err != nil || !strings.Contains(out.String(), "# CSM Checkpoint") || !strings.Contains(out.String(), "The repository matches this checkpoint.") {
		t.Fatalf("show: %v\n%s", err, out.String())
	}

	os.WriteFile(filepath.Join(project, "later.txt"), []byte("written after the checkpoint\n"), 0o600)
	before := gitStatus(t, project)
	out.Reset()
	code, err := cmdHandoff(log, home, []string{"--checkpoint", cp.ID, "work-codex"})

	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v\n%s", code, err, out.String())
	}

	for _, want := range []string{"Task context", "from checkpoint " + cp.ID, "Checking the repository against checkpoint " + cp.ID} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("handoff output missing %q:\n%s", want, out.String())
		}
	}

	rollouts, _ := filepath.Glob(filepath.Join(reloaded.accountsDir(), "work-codex", "sessions", "*", "*", "*", "rollout-*.jsonl"))

	if len(rollouts) != 1 {
		t.Fatalf("codex rollouts = %v", rollouts)
	}

	received, _ := os.ReadFile(rollouts[0])

	for _, want := range []string{"You are continuing a coding task started by another coding agent (claude, account alpha)", "The task: Finish the webhook retries", "The working tree changed since the checkpoint", "on branch feature/webhook at"} {
		if !strings.Contains(string(received), want) {
			t.Errorf("codex brief missing %q:\n%s", want, received)
		}
	}

	if after := gitStatus(t, project); after != before {
		t.Fatalf("the handoff changed the repository:\n%s\n→\n%s", before, after)
	}

	if _, err := os.Stat(filepath.Join(project, "later.txt")); err != nil {
		t.Fatal("work done after the checkpoint was removed")
	}

	final, _ := loadState(home)
	h, hDir, err := handoff.Latest(final.handoffRoot(project))

	if err != nil || h.Checkpoint != cp.ID || h.Edits == nil || h.Edits.Task != "Finish the webhook retries" || h.Stage != handoff.StageTargetStarted {
		t.Fatalf("handoff = %+v %v", h, err)
	}

	if o := h.Outcome; o == nil || !o.Started || o.Delivery != handoff.DeliveryInstruction || o.Status != handoff.OutcomePartial || !o.Verification.TreeChanged || o.ProjectDir != project {
		t.Fatalf("outcome = %+v", h.Outcome)
	}

	hmd, _ := os.ReadFile(filepath.Join(hDir, handoff.FileMD))

	for _, want := range []string{"Continues checkpoint: " + cp.ID, "## Current Task\n\nFinish the webhook retries", "## Next Steps\n\n1. Cap retries at 5\n2. Add a jitter test", "## Changes Since the Checkpoint"} {
		if !strings.Contains(string(hmd), want) {
			t.Errorf("handoff.md missing %q:\n%s", want, hmd)
		}
	}

	if _, err := cmdHandoff(log, home, []string{"--checkpoint", "../../etc", "work-codex"}); err == nil || !strings.Contains(err.Error(), "not a checkpoint ID") {
		t.Fatalf("path accepted: %v", err)
	}

	if _, err := cmdHandoff(log, home, []string{"--checkpoint", h.ID, "work-codex"}); err == nil || !strings.Contains(err.Error(), "not a saved checkpoint") {
		t.Fatalf("handoff record accepted as checkpoint: %v", err)
	}

	if err := cmdCheckpoint(log, home, []string{"edit", h.ID, "--task", "x"}); err == nil {
		t.Fatal("a handoff record was edited")
	}

	other, _ := filepath.EvalSymlinks(t.TempDir())
	os.Chdir(other)

	if _, err := cmdHandoff(log, home, []string{"--checkpoint", cp.ID, "work-codex"}); err == nil || !strings.Contains(err.Error(), "belongs to") {
		t.Fatalf("checkpoint applied to another project: %v", err)
	}
}

func TestCheckpointWithoutSessionAndCorruptEntries(t *testing.T) {
	home := t.TempDir()

	if _, err := initState(home); err != nil {
		t.Fatal(err)
	}

	dir, _ := filepath.EvalSymlinks(t.TempDir())
	wd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(wd)

	var out strings.Builder
	log := &Logger{out: &out, err: &out}

	if err := cmdCheckpoint(log, home, []string{"--task", "Sketch the API"}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}

	if !strings.Contains(out.String(), "none recorded") || !strings.Contains(out.String(), "not a git repository") || !strings.Contains(out.String(), "partly") {
		t.Fatalf("output:\n%s", out.String())
	}

	s, _ := loadState(home)
	root := s.handoffRoot(dir)
	broken := filepath.Join(root, "20261012T000000Z-claude-checkpoint")
	os.MkdirAll(broken, 0o700)
	os.WriteFile(filepath.Join(broken, handoff.FileJSON), []byte("{"), 0o600)
	out.Reset()

	if err := cmdCheckpoint(log, home, []string{"list"}); err != nil || !strings.Contains(out.String(), "skipped 20261012T000000Z-claude-checkpoint") || !strings.Contains(out.String(), "Sketch the API") {
		t.Fatalf("list: %v\n%s", err, out.String())
	}

	if err := cmdCheckpoint(log, home, []string{"show", "20261012T000000Z-claude-checkpoint"}); err == nil {
		t.Fatal("corrupt checkpoint shown")
	}

	if err := cmdCheckpoint(log, home, []string{"edit"}); err == nil {
		t.Fatal("usage error expected")
	}

	if err := cmdCheckpoint(log, home, []string{"--task"}); err == nil {
		t.Fatal("missing flag value accepted")
	}
}

func TestFinishHandoffRecordsALaunchFailure(t *testing.T) {
	dir := t.TempDir()
	st := handoff.State{Version: handoff.Version, ID: "20261012T100000Z-claude-codex", Stage: handoff.StageMCPPrepared, Outcome: &handoff.Outcome{Status: handoff.OutcomeStarting, Delivery: handoff.DeliveryInstruction}}

	if err := handoff.Save(dir, &st); err != nil {
		t.Fatal(err)
	}

	finishHandoff(dir, dir, false, errors.New("exec: codex: not found"), time.Now())
	back, err := handoff.Load(dir)

	if err != nil || back.Stage != handoff.StageFailed || back.Outcome.Status != handoff.OutcomeFailed || back.Outcome.Started || !strings.Contains(back.Error, "not found") {
		t.Fatalf("failed launch recorded as %+v %+v %v", back, back.Outcome, err)
	}

	finishHandoff("", dir, true, nil, time.Now())
}

func TestLiveSwitchCarriesANamedCheckpoint(t *testing.T) {
	requested := make(chan error, 1)
	res, err := runFailoverScenario(testBinary(t), scenario{
		hold:    "alpha",
		crossTo: provider.CodexID,
		seed:    seedFile(t),
		during: func(home, projectDir string) {
			deadline := time.Now().Add(10 * time.Second)

			for time.Now().Before(deadline) {
				s, err := loadState(home)
				if err == nil {
					sess, live := s.loadLiveSession(provider.ClaudeID, projectDir)
					if live && transcriptWritten(filepath.Join(s.accountsDir(), "alpha")) {
						st, _, err := s.captureCheckpoint(projectDir, handoff.Edits{Task: "Ship the webhook retries", NextSteps: []string{"Write the jitter test"}}, time.Now())
						if err != nil {
							requested <- err

							return
						}

						requested <- requestSwitchWith(&Logger{out: io.Discard, err: io.Discard}, s, sess, "work-codex", st.ID)

						return
					}
				}

				time.Sleep(50 * time.Millisecond)
			}

			requested <- errors.New("session never became live")
		},
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}

	if err := <-requested; err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(res.files["target-transcript"], "The task: Ship the webhook retries") {
		t.Fatalf("codex did not get the checkpoint's task:\n%s", res.files["target-transcript"])
	}

	var st handoff.State

	if err := json.Unmarshal([]byte(res.files["handoff-json"]), &st); err != nil || st.Checkpoint == "" || st.Edits == nil || st.Edits.NextSteps[0] != "Write the jitter test" {
		t.Fatalf("handoff = %+v %v", st, err)
	}

	if !strings.Contains(res.files["handoff-md"], "## Next Steps\n\n1. Write the jitter test") {
		t.Fatalf("handoff.md:\n%s", res.files["handoff-md"])
	}

	if res.gitBefore != res.gitAfter {
		t.Fatalf("git changed: %q → %q", res.gitBefore, res.gitAfter)
	}
}
