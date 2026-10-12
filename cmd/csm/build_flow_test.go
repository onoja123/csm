package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

const greetGo = "package demo\n\nfunc Greet(name string) string { return \"Hello, \" + name }\n"

func singleTaskPlan(agent string) map[string]any {
	return planFixture(map[string]any{"id": "greet", "title": "Greeting function", "objective": "Add Greet(name)", "files": []string{"greet.go"}, "agent": agent})
}

func approvedRun(h *buildHarness, policy BuildPolicy) Build {
	h.t.Helper()

	b := h.create("Add a greeting", policy)
	got := h.waitFor(b.ID, 30*time.Second, statusIs(BuildAwaitingPlan, BuildNeedsAttention))

	if got.Status != BuildAwaitingPlan {
		h.t.Fatalf("plan: %s %s", got.Status, got.Attention)
	}

	h.act(b.ID, "approve-plan", actionRequest{})

	return got
}

func TestBuildTaskContinuesThroughAutomaticHandoffAfterAUsageLimit(t *testing.T) {
	h := newBuildHarness(t, map[string][]string{provider.ClaudeID: {"alpha"}, provider.CodexID: {"work-codex"}}, func(s *State) { s.Config.setHandoffTo([]string{"codex"}) })
	t.Setenv("CSM_FAKE_LIMIT", "alpha")
	h.fixture("plan", nil, singleTaskPlan("claude"))
	h.fixture("greet", map[string]string{"greet.go": greetGo}, workReportFixture("greet.go"))

	b := approvedRun(h, BuildPolicy{RequirePlanApproval: true, Planner: "codex"})
	final := h.waitFor(b.ID, 60*time.Second, statusIs(BuildAwaitingFinal, BuildNeedsAttention))

	if final.Status != BuildAwaitingFinal {
		t.Fatalf("status %s: %s", final.Status, final.Attention)
	}

	task := final.task("greet")

	if task.Provider != provider.CodexID || len(task.Switches) != 1 || task.Switches[0].From != "claude/alpha" || task.Switches[0].To != "codex/work-codex" || task.Switches[0].Reason != "usage_limit" || !task.Switches[0].Started {
		t.Fatalf("switches %+v provider %s", task.Switches, task.Provider)
	}

	if n := len(task.Attempts); n != 1 {
		t.Fatalf("the runner's handoff must continue the same attempt, got %d attempts", n)
	}

	if !slices.Equal(task.Completion.FilesChanged, []string{"greet.go"}) {
		t.Fatalf("completion %+v", task.Completion)
	}

	if !slices.ContainsFunc(h.engine.store.events(b.ID), func(e BuildEvent) bool {
		return e.Kind == "fallback.handoff" && strings.Contains(e.Message, "usage limit detected")
	}) {
		t.Fatal("no handoff event")
	}
}

func TestBuildRetriesATaskWithAnotherAgentWhenEveryAccountIsLimited(t *testing.T) {
	h := newBuildHarness(t, map[string][]string{provider.ClaudeID: {"alpha"}, provider.CodexID: {"work-codex"}}, nil)
	t.Setenv("CSM_FAKE_LIMIT", "alpha")
	h.fixture("plan", nil, singleTaskPlan("claude"))
	h.fixture("greet", map[string]string{"greet.go": greetGo}, workReportFixture("greet.go"))

	b := approvedRun(h, BuildPolicy{RequirePlanApproval: true, Planner: "codex"})
	final := h.waitFor(b.ID, 60*time.Second, statusIs(BuildAwaitingFinal, BuildNeedsAttention))

	if final.Status != BuildAwaitingFinal {
		t.Fatalf("status %s: %s", final.Status, final.Attention)
	}

	task := final.task("greet")

	if len(task.Attempts) != 2 || task.Attempts[0].Provider != provider.ClaudeID || task.Attempts[0].Status != AttemptFailed || task.Provider != provider.CodexID {
		t.Fatalf("attempts %+v", task.Attempts)
	}

	if !strings.Contains(task.Attempts[0].Note, "no Claude Code account is ready") {
		t.Fatalf("note %q", task.Attempts[0].Note)
	}

	brief, _ := os.ReadFile(strings.TrimSuffix(task.Attempts[1].Report, ".json") + ".brief.txt")

	if !strings.Contains(string(brief), "A previous attempt by Claude Code (alpha) stopped") {
		t.Fatalf("the retry must tell the agent about the earlier attempt:\n%s", brief)
	}
}

func TestBuildStopsForAPersonWhenNoAgentCanTakeOver(t *testing.T) {
	h := newBuildHarness(t, map[string][]string{provider.ClaudeID: {"alpha"}, provider.CodexID: {"work-codex"}}, nil)
	t.Setenv("CSM_FAKE_LIMIT", "alpha")
	h.fixture("plan", nil, singleTaskPlan("claude"))

	b := approvedRun(h, BuildPolicy{RequirePlanApproval: true, Planner: "codex", Agents: []string{"claude"}})
	h.state.update(func(s *State) error {
		a, _ := s.resolveAccount("work-codex")
		a.Enabled = false

		return nil
	})

	got := h.waitFor(b.ID, 60*time.Second, statusIs(BuildNeedsAttention))
	task := got.task("greet")

	if task.Status != TaskNeedsAttention || !strings.Contains(task.Recovery, "worktree and its changes are kept") || !slices.Contains(got.Recovery, "retry_task") {
		t.Fatalf("task %s %q, recovery %v", task.Status, task.Recovery, got.Recovery)
	}

	if _, err := os.Stat(task.Worktree); err != nil {
		t.Fatal("the worktree must be kept")
	}
}

func TestBuildDetectsAConflictAndValidatesTheAgentsResolution(t *testing.T) {
	h := newBuildHarness(t, map[string][]string{provider.ClaudeID: {"alpha"}}, nil)
	h.fixture("plan", nil, planFixture(
		map[string]any{"id": "left", "title": "Left", "objective": "Add Left", "files": []string{"left.go"}},
		map[string]any{"id": "right", "title": "Right", "objective": "Add Right", "files": []string{"right.go"}},
	))
	h.fixture("left", map[string]string{"shared.go": "package demo\n\nfunc Left() int { return 1 }\n"}, workReportFixture("shared.go"))
	h.fixture("right", map[string]string{"shared.go": "package demo\n\nfunc Right() int { return 2 }\n"}, workReportFixture("shared.go"))
	h.fixture("resolve-right", map[string]string{"shared.go": "package demo\n\nfunc Left() int { return 1 }\n\nfunc Right() int { return 2 }\n"}, workReportFixture("shared.go"))

	b := approvedRun(h, BuildPolicy{RequirePlanApproval: true})
	final := h.waitFor(b.ID, 60*time.Second, statusIs(BuildAwaitingFinal, BuildNeedsAttention))

	if final.Status != BuildAwaitingFinal {
		t.Fatalf("status %s: %s", final.Status, final.Attention)
	}

	if len(final.Integration.Overlaps) != 1 || final.Integration.Overlaps[0].File != "shared.go" {
		t.Fatalf("overlaps %+v", final.Integration.Overlaps)
	}

	if len(final.Integration.Conflicts) != 1 {
		t.Fatalf("conflicts %+v", final.Integration.Conflicts)
	}

	c := final.Integration.Conflicts[0]

	if c.TaskID != "right" || !slices.Equal(c.Tasks, []string{"left"}) || c.Status != "resolved" || !slices.Equal(c.Files, []string{"shared.go"}) {
		t.Fatalf("conflict %+v", c)
	}

	data, _ := os.ReadFile(filepath.Join(final.Integration.Worktree, "shared.go"))

	if !strings.Contains(string(data), "Left") || !strings.Contains(string(data), "Right") {
		t.Fatalf("resolution lost a side:\n%s", data)
	}

	for _, task := range final.Tasks {
		if _, err := os.Stat(task.Worktree); err != nil {
			t.Fatalf("task worktree %s removed", task.ID)
		}
	}
}

func TestBuildConflictWithMarkersLeftStopsForAPerson(t *testing.T) {
	h := newBuildHarness(t, map[string][]string{provider.ClaudeID: {"alpha"}}, nil)
	h.fixture("plan", nil, planFixture(
		map[string]any{"id": "left", "title": "Left", "objective": "Add Left", "files": []string{"left.go"}},
		map[string]any{"id": "right", "title": "Right", "objective": "Add Right", "files": []string{"right.go"}},
	))
	h.fixture("left", map[string]string{"shared.go": "package demo\n\nfunc Left() int { return 1 }\n"}, workReportFixture("shared.go"))
	h.fixture("right", map[string]string{"shared.go": "package demo\n\nfunc Right() int { return 2 }\n"}, workReportFixture("shared.go"))
	h.fixture("resolve-right", nil, workReportFixture())

	b := approvedRun(h, BuildPolicy{RequirePlanApproval: true})
	got := h.waitFor(b.ID, 60*time.Second, statusIs(BuildNeedsAttention, BuildAwaitingFinal))

	if got.Status != BuildNeedsAttention || !strings.Contains(got.Attention, "conflict markers remain in shared.go") || !slices.Contains(got.Recovery, "continue_integration") {
		t.Fatalf("status %s: %s %v", got.Status, got.Attention, got.Recovery)
	}

	h.write(got.Integration.Worktree, "shared.go", "package demo\n\nfunc Left() int { return 1 }\n\nfunc Right() int { return 2 }\n")
	h.act(b.ID, "continue-integration", actionRequest{})

	final := h.waitFor(b.ID, 60*time.Second, statusIs(BuildAwaitingFinal, BuildFailed))

	if c := final.Integration.Conflicts[0]; c.Status != "resolved" || !strings.HasPrefix(c.Resolution, "Resolved by you") {
		t.Fatalf("conflict %+v", c)
	}
}

func TestBuildSendsBlockingFindingsBackAndReReviews(t *testing.T) {
	h := newBuildHarness(t, map[string][]string{provider.ClaudeID: {"alpha"}, provider.CodexID: {"work-codex"}}, nil)
	h.fixture("plan", nil, singleTaskPlan("claude"))
	h.fixture("greet", map[string]string{"greet.go": "package demo\n\nfunc Greet(name string) string { return \"Hello \" + name }\n"}, workReportFixture("greet.go"))
	h.fixture("review-1", nil, map[string]any{"verdict": "changes_requested", "findings": []map[string]any{{"severity": "high", "file": "greet.go", "line": 3, "title": "Missing comma", "explanation": "The request says Hello, name", "recommendation": "Add the comma", "blocking": true}}})
	h.fixture("fix-1", map[string]string{"greet.go": greetGo}, workReportFixture("greet.go"))
	h.fixture("review-2", nil, map[string]any{"verdict": "approve", "findings": []any{}})

	b := approvedRun(h, BuildPolicy{RequirePlanApproval: true, Review: true, Reviewer: "codex"})
	final := h.waitFor(b.ID, 90*time.Second, statusIs(BuildAwaitingFinal, BuildNeedsAttention))

	if final.Status != BuildAwaitingFinal {
		t.Fatalf("status %s: %s", final.Status, final.Attention)
	}

	fix := final.task("fix-1")

	if fix == nil || fix.Kind != TaskKindFix || fix.Provider != provider.ClaudeID || fix.Status != TaskCompleted || !fix.Integrated {
		t.Fatalf("fix task %+v", fix)
	}

	if len(final.Reviews) != 2 || !final.Reviews[0].Independent || final.Reviews[0].Provider != provider.CodexID || final.Reviews[1].BlockingOpen != 0 {
		t.Fatalf("reviews %+v", final.Reviews)
	}

	if f := final.Findings[0]; f.Status != findingRechecked || f.TaskID != "greet" {
		t.Fatalf("finding %+v", f)
	}

	data, _ := os.ReadFile(filepath.Join(final.Integration.Worktree, "greet.go"))

	if string(data) != greetGo {
		t.Fatalf("the fix was not integrated:\n%s", data)
	}

	brief, _ := os.ReadFile(strings.TrimSuffix(fix.Attempts[0].Report, ".json") + ".brief.txt")

	if !strings.Contains(string(brief), "Missing comma") || !strings.Contains(string(brief), "Add the comma") {
		t.Fatalf("the fix brief must carry the findings:\n%s", brief)
	}
}

func TestBuildReviewCycleIsBounded(t *testing.T) {
	h := newBuildHarness(t, map[string][]string{provider.ClaudeID: {"alpha"}}, nil)
	h.fixture("plan", nil, singleTaskPlan("claude"))
	h.fixture("greet", map[string]string{"greet.go": greetGo}, workReportFixture("greet.go"))
	h.fixture("review-1", nil, map[string]any{"verdict": "changes_requested", "findings": []map[string]any{{"severity": "critical", "file": "greet.go", "title": "Still wrong", "blocking": true}}})

	b := approvedRun(h, BuildPolicy{RequirePlanApproval: true, Review: true, MaxReviewCycles: 1})
	got := h.waitFor(b.ID, 60*time.Second, statusIs(BuildNeedsAttention, BuildAwaitingFinal))

	if got.Status != BuildNeedsAttention || !slices.Contains(got.Recovery, "accept_findings") || got.task("fix-1") != nil {
		t.Fatalf("status %s %s %v", got.Status, got.Attention, got.Recovery)
	}

	if got.Reviews[0].Independent || got.Reviews[0].Note == "" {
		t.Fatal("a review by the implementing agent must say it is not independent")
	}

	if err := h.engine.act(b.ID, "retry-review", actionRequest{}); err == nil {
		t.Fatal("another review round past the limit must be refused")
	}

	h.act(b.ID, "accept-findings", actionRequest{})

	final := h.snapshot(b.ID)

	if final.Status != BuildAwaitingFinal || final.Findings[0].Status != findingAccepted {
		t.Fatalf("status %s finding %+v", final.Status, final.Findings[0])
	}
}

func TestBuildFailingCheckBecomesAFixRound(t *testing.T) {
	h := newBuildHarness(t, map[string][]string{provider.ClaudeID: {"alpha"}}, nil)
	h.fixture("plan", nil, singleTaskPlan("claude"))
	h.fixture("greet", map[string]string{"greet.go": greetGo, "greet_test.go": "package demo\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif Greet(\"Ada\") != \"Hi, Ada\" {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n"}, workReportFixture("greet.go", "greet_test.go"))
	h.fixture("fix-1", map[string]string{"greet_test.go": "package demo\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif Greet(\"Ada\") != \"Hello, Ada\" {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n"}, workReportFixture("greet_test.go"))

	b := approvedRun(h, BuildPolicy{RequirePlanApproval: true, RunBaseline: true})
	final := h.waitFor(b.ID, 90*time.Second, statusIs(BuildAwaitingFinal, BuildNeedsAttention))

	if final.Status != BuildAwaitingFinal {
		t.Fatalf("status %s: %s", final.Status, final.Attention)
	}

	if f := final.Findings[0]; f.Title != "Check failed: go test" || !strings.HasPrefix(f.Source, "CSM ran") || f.Status != findingFixAsked {
		t.Fatalf("finding %+v", f)
	}

	for _, c := range final.Checks {
		if c.Stage == "integration" && c.State != CheckPassed {
			t.Fatalf("after the fix %s is %s", c.Name, c.State)
		}
	}

	if final.Cycle != 2 {
		t.Fatalf("cycle %d", final.Cycle)
	}
}

func TestBuildPauseHoldsSchedulingAndCancelKeepsWork(t *testing.T) {
	h := newBuildHarness(t, map[string][]string{provider.ClaudeID: {"alpha"}}, nil)
	h.fixture("plan", nil, planFixture(
		map[string]any{"id": "slow", "title": "Slow", "objective": "Takes a while", "files": []string{"a.go"}},
		map[string]any{"id": "later", "title": "Later", "objective": "After slow", "depends_on": []string{"slow"}},
	))

	b := h.create("Something slow", BuildPolicy{RequirePlanApproval: true})
	h.waitFor(b.ID, 30*time.Second, statusIs(BuildAwaitingPlan))
	h.act(b.ID, "pause", actionRequest{})
	h.act(b.ID, "approve-plan", actionRequest{})
	time.Sleep(2500 * time.Millisecond)

	if got := h.snapshot(b.ID); got.task("slow").Status == TaskRunning || len(got.Attempts) != 1 {
		t.Fatalf("a paused run started work: %s", got.task("slow").Status)
	}

	h.act(b.ID, "resume", actionRequest{})
	running := h.waitFor(b.ID, 20*time.Second, func(b Build) bool { return b.task("slow").Status == TaskRunning })
	h.write(running.task("slow").Worktree, "a.go", "package demo\n")

	h.act(b.ID, "cancel", actionRequest{})
	h.act(b.ID, "cancel", actionRequest{})

	final := h.waitFor(b.ID, 30*time.Second, statusIs(BuildCancelled))

	if final.task("later").Status != TaskCancelled || final.task("slow").Status != TaskCancelled {
		t.Fatalf("tasks %s %s", final.task("slow").Status, final.task("later").Status)
	}

	if data, err := os.ReadFile(filepath.Join(final.task("slow").Worktree, "a.go")); err != nil || string(data) != "package demo\n" {
		t.Fatal("cancelling must keep the worktree and its changes")
	}

	if err := h.engine.act(b.ID, "resume", actionRequest{}); err == nil {
		t.Fatal("a cancelled run must not accept actions")
	}

	time.Sleep(1500 * time.Millisecond)

	if got := h.snapshot(b.ID); got.task("later").Status != TaskCancelled || len(got.Attempts) != 2 {
		t.Fatalf("work started after cancel: %d attempts", len(got.Attempts))
	}
}
