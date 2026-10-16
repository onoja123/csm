package main

import (
	"testing"

	"github.com/onoja123/csm/internal/provider"
)

func TestRunsSayACursorRequestCannotBeShown(t *testing.T) {
	m := &runManager{}
	r := &agentRun{}

	if got := m.taskFor(&State{}, r, runView{runInfo: runInfo{Provider: provider.CursorID}}); got.Note != "Cursor CLI sessions cannot be read, so the request is not shown." || got.Summary != "" {
		t.Fatalf("task = %+v", got)
	}

	if got := m.taskFor(&State{}, r, runView{runInfo: runInfo{Provider: provider.CodexID}}); got.Note != "" {
		t.Fatalf("an agent whose sessions are read must not carry the note: %+v", got)
	}
}

func TestRunsShowTheRequestCSMDeliveredToCursor(t *testing.T) {
	m := &runManager{}
	r := &agentRun{}
	v := runView{runInfo: runInfo{Provider: provider.CursorID}}

	for _, in := range []string{"y", "\r", "a", "b", "\r"} {
		r.noteInput([]byte(in))
	}

	if got := m.taskFor(&State{}, r, v); got.Summary != "" || got.Note == "" {
		t.Fatalf("keystrokes typed one by one were taken for a request: %+v", got)
	}

	r.noteInput([]byte("Add RemoveMember to team.go\nand a test"))

	if got := m.taskFor(&State{}, r, v); got.Summary != "" {
		t.Fatalf("text that was never submitted was taken for a request: %+v", got)
	}

	r.noteInput([]byte("Add RemoveMember to team.go and a test"))
	r.noteInput([]byte("\r"))

	if got := m.taskFor(&State{}, r, v); got.Summary != "Add RemoveMember to team.go and a test" || got.Note != "" {
		t.Fatalf("task = %+v", got)
	}

	r.noteInput([]byte("y"))
	r.noteInput([]byte("\r"))

	if got := m.taskFor(&State{}, r, v); got.Summary != "Add RemoveMember to team.go and a test" {
		t.Fatalf("an approval keystroke replaced the request: %+v", got)
	}
}
