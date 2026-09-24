package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

func TestCheckpointRoundTrip(t *testing.T) {
	s := newTestState(t)
	c := Checkpoint{
		Version:        stateVersion,
		ProjectDir:     "/work/polishpad",
		Account:        "personal",
		SessionID:      "abc",
		StartedAt:      testNow,
		CheckpointedAt: testNow.Add(time.Hour),
		Branch:         "cards",
		Reason:         provider.StopReasonUsageLimit,
	}
	if err := s.saveCheckpoint(c); err != nil {
		t.Fatal(err)
	}
	got, err := s.loadCheckpoint(c.ProjectDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != c {
		t.Fatalf("got %+v, want %+v", got, c)
	}
	history, _ := filepath.Glob(filepath.Join(s.projectStateDir(c.ProjectDir), historyDir, "*.json"))
	if len(history) != 1 {
		t.Fatalf("history entries = %d, want 1", len(history))
	}
}

func TestLoadCheckpointInvalid(t *testing.T) {
	s := newTestState(t)
	dir := s.projectStateDir("/work/x")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, checkpointFile), []byte("{"), 0o600)
	if _, err := s.loadCheckpoint("/work/x"); err == nil {
		t.Fatal("expected error for invalid checkpoint")
	}
	if _, err := s.loadCheckpoint("/work/missing"); !os.IsNotExist(err) {
		t.Fatalf("got %v, want not-exist", err)
	}
}

func TestTransitionDetectsInterruptedSwitch(t *testing.T) {
	s := newTestState(t)
	if _, ok, err := s.loadTransition("/work/p"); ok || err != nil {
		t.Fatalf("unexpected transition: %v %v", ok, err)
	}
	dir := s.projectStateDir("/work/p")
	os.MkdirAll(dir, 0o700)
	tr := Transition{Version: stateVersion, ProjectDir: "/work/p", From: "personal", To: "work", CSMPID: 1 << 30}
	writeJSON(filepath.Join(dir, transitionFile), tr)
	got, ok, err := s.loadTransition("/work/p")
	if err != nil || !ok || got.To != "work" {
		t.Fatalf("got %+v %v %v", got, ok, err)
	}
	if processAlive(got.CSMPID) {
		t.Fatal("bogus pid reported alive")
	}
}

func TestLiveSessionsIgnoresDeadProcesses(t *testing.T) {
	s := newTestState(t)
	for i, pid := range []int{os.Getpid(), 1 << 30} {
		dir := s.projectStateDir(filepath.Join("/work", string(rune('a'+i))))
		os.MkdirAll(dir, 0o700)
		writeJSON(filepath.Join(dir, sessionFile), Session{CSMPID: pid, Account: "personal"})
	}
	if live := s.liveSessions(); len(live) != 1 || live[0].CSMPID != os.Getpid() {
		t.Fatalf("live = %+v", live)
	}
}

func TestProjectIDIsStableAndDistinct(t *testing.T) {
	a, b := projectID("/Users/x/polishpad"), projectID("/Users/y/polishpad")
	if a == b || a != projectID("/Users/x/polishpad") {
		t.Fatalf("ids: %s %s", a, b)
	}
}
