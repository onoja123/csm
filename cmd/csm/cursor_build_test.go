package main

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

const shoutGo = "package demo\n\nimport \"strings\"\n\nfunc Shout(s string) string { return strings.ToUpper(s) }\n"

func TestBuildRoomRunsCursorTasksInTheirOwnWorktrees(t *testing.T) {
	quietFakes(t)
	h := newBuildHarness(t, map[string][]string{provider.CursorID: {"cursor-main"}, provider.CodexID: {"work-codex"}}, nil)
	t.Setenv("CSM_FAKE_WORK_MS", "1500")
	statusBefore := h.git("status", "--porcelain", "--branch")

	h.fixture("plan", nil, planFixture(
		map[string]any{"id": "greet", "title": "Greeting function", "objective": "Add Greet(name)", "acceptance": []string{"Greet returns Hello, name"}, "files": []string{"greet.go"}, "agent": "cursor"},
		map[string]any{"id": "format", "title": "Formatter", "objective": "Add Shout(s)", "files": []string{"format.go"}, "agent": "cursor"},
	))
	h.fixture("greet", map[string]string{"greet.go": greetGo}, workReportFixture("greet.go"))
	h.fixture("format", map[string]string{"format.go": shoutGo}, workReportFixture("format.go"))
	h.fixture("review-1", nil, map[string]any{"verdict": "approve", "summary": "Read the diff; both functions match the request.", "findings": []map[string]any{}})

	b := approvedRun(h, BuildPolicy{RequirePlanApproval: true, Planner: provider.CursorID, Review: true, RunBaseline: true, MaxParallel: 2})

	both := h.waitFor(b.ID, 30*time.Second, func(b Build) bool {
		return b.task("greet").Status == TaskRunning && b.task("format").Status == TaskRunning || b.Status == BuildNeedsAttention || b.task("greet").Status == TaskCompleted
	})

	if both.task("greet").Status != TaskRunning || both.task("format").Status != TaskRunning {
		t.Fatalf("two Cursor tasks must be able to run at once: greet=%s format=%s (%s)", both.task("greet").Status, both.task("format").Status, both.Attention)
	}

	final := h.waitFor(b.ID, 90*time.Second, statusIs(BuildAwaitingFinal, BuildNeedsAttention, BuildFailed))
	if final.Status != BuildAwaitingFinal {
		t.Fatalf("status %s: %s", final.Status, final.Attention)
	}

	greet, format := final.task("greet"), final.task("format")

	if greet.Provider != provider.CursorID || format.Provider != provider.CursorID {
		t.Fatalf("assignments: greet=%s format=%s", greet.Provider, format.Provider)
	}

	if greet.Worktree == format.Worktree || greet.Branch == format.Branch || strings.HasPrefix(greet.Worktree, h.repo) || strings.HasPrefix(format.Worktree, h.repo) {
		t.Fatalf("each Cursor task needs its own worktree and branch outside the repository: %s (%s), %s (%s)", greet.Worktree, greet.Branch, format.Worktree, format.Branch)
	}

	for _, a := range final.Attempts {
		if a.Role == RoleImplement && (a.Provider != provider.CursorID || a.Account != "cursor-main" || a.Dir != final.task(a.TaskID).Worktree) {
			t.Fatalf("attempt %+v did not run Cursor in its task's worktree", a)
		}
	}

	if !slices.Equal(greet.Completion.FilesChanged, []string{"greet.go"}) || !slices.Equal(format.Completion.FilesChanged, []string{"format.go"}) {
		t.Fatalf("changes collected: greet %v, format %v", greet.Completion.FilesChanged, format.Completion.FilesChanged)
	}

	profile := filepath.Join(h.home, "accounts", "cursor-main")

	for _, dir := range []string{greet.Worktree, format.Worktree} {
		if len(chatsIn(profile, dir)) != 1 {
			t.Errorf("Cursor's chat for %s must be filed under that worktree alone", dir)
		}
	}

	if len(final.Reviews) != 1 || final.Reviews[0].Provider != provider.CodexID || !final.Reviews[0].Independent {
		t.Fatalf("Cursor's work must be reviewed by a different agent: %+v", final.Reviews)
	}

	r := newReceipt(&final, time.Now())

	if r.Outcome != OutcomeVerified || r.Revision.Commit == "" || r.Revision.Commit != final.Integration.Head || !r.Review.Independent {
		t.Fatalf("receipt outcome=%s commit=%s head=%s independent=%v", r.Outcome, r.Revision.Commit, final.Integration.Head, r.Review.Independent)
	}

	for _, c := range r.Checks {
		if c.Status != EvidencePassed || c.ExitCode == nil || *c.ExitCode != 0 || c.Commit != r.Revision.Commit {
			t.Fatalf("check %+v is not measured evidence on the verified revision", c)
		}
	}

	if !slices.ContainsFunc(r.Agents, func(a ReceiptAgent) bool { return a.Agent == "Cursor CLI (cursor-main)" && a.Role == RoleImplement }) {
		t.Fatalf("the receipt must name Cursor as an implementer: %+v", r.Agents)
	}

	if !slices.ContainsFunc(r.Claims, func(c ReceiptClaim) bool {
		return strings.Contains(c.Agent, "Cursor CLI") && c.Status == ClaimConfirmed && strings.Contains(c.Evidence, "passed")
	}) {
		t.Fatalf("Cursor's test claim must be confirmed only by CSM's own run: %+v", r.Claims)
	}

	r.measureDrift(final.Integration.Worktree, time.Now())

	if r.Drift == nil || !r.Drift.Same {
		t.Fatalf("drift right after verification: %+v", r.Drift)
	}

	h.write(final.Integration.Worktree, "late.go", "package demo\n")
	r.measureDrift(final.Integration.Worktree, time.Now())

	if r.Drift.Same || !slices.ContainsFunc(r.Drift.Changes, func(c string) bool { return strings.HasSuffix(c, "late.go") }) || !strings.Contains(r.Drift.Note, "have not been verified") {
		t.Fatalf("a change after verification must make the receipt stale: %+v", r.Drift)
	}

	if after := h.git("status", "--porcelain", "--branch"); after != statusBefore {
		t.Fatalf("the user's working tree changed:\n%s\n%s", statusBefore, after)
	}
}

// Cursor files chats under the MD5 of the working directory's path.
func chatsIn(profile, dir string) []string {
	sum := md5.Sum([]byte(dir))
	matches, _ := filepath.Glob(filepath.Join(profile, "chats", hex.EncodeToString(sum[:]), "*"))

	return matches
}

func TestBuildRoomCursorReviewsAnotherAgentsWork(t *testing.T) {
	quietFakes(t)
	h := newBuildHarness(t, map[string][]string{provider.ClaudeID: {"alpha"}, provider.CursorID: {"cursor-main"}}, nil)
	h.fixture("plan", nil, singleTaskPlan("claude"))
	h.fixture("greet", map[string]string{"greet.go": greetGo}, workReportFixture("greet.go"))
	h.fixture("review-1", nil, map[string]any{"verdict": "approve", "summary": "Inspected greet.go against the criteria.", "findings": []map[string]any{{"severity": "low", "file": "greet.go", "line": 3, "title": "Add a doc comment", "blocking": false}}})

	b := approvedRun(h, BuildPolicy{RequirePlanApproval: true, Planner: provider.ClaudeID, Reviewer: provider.CursorID, Review: true})
	final := h.waitFor(b.ID, 60*time.Second, statusIs(BuildAwaitingFinal, BuildNeedsAttention, BuildFailed))

	if final.Status != BuildAwaitingFinal {
		t.Fatalf("status %s: %s", final.Status, final.Attention)
	}

	rv := final.Reviews[0]

	if rv.Provider != provider.CursorID || !rv.Independent || rv.Commit != final.Integration.Head || rv.Worktree == final.Integration.Worktree {
		t.Fatalf("review = %+v", rv)
	}

	brief, _ := os.ReadFile(strings.TrimSuffix(attemptFor(final, RoleReview).Report, ".json") + ".brief.txt")

	for _, want := range []string{"This is a review only: do not modify any files.", "Original request: Add a greeting", "git diff " + final.BaseCommit + "..HEAD", "Verification evidence CSM has measured so far", "go test ./...: passed (exit 0)"} {
		if !strings.Contains(string(brief), want) {
			t.Errorf("Cursor's review brief lacks %q:\n%s", want, brief)
		}
	}

	if len(final.Findings) != 1 || final.Findings[0].Blocking {
		t.Fatalf("findings %+v", final.Findings)
	}
}

func attemptFor(b Build, role string) Attempt {
	for _, a := range b.Attempts {
		if a.Role == role {
			return a
		}
	}

	return Attempt{}
}

func TestBuildRoomCursorFailureWaitsForAPersonAndCanMoveToAnotherAgent(t *testing.T) {
	quietFakes(t)
	h := newBuildHarness(t, map[string][]string{provider.CursorID: {"cursor-main"}, provider.CodexID: {"work-codex"}}, nil)
	t.Setenv("CSM_FAKE_LIMIT", "cursor-main")
	h.fixture("plan", nil, singleTaskPlan("cursor"))
	h.fixture("greet", map[string]string{"greet.go": greetGo}, workReportFixture("greet.go"))

	b := approvedRun(h, BuildPolicy{RequirePlanApproval: true, Planner: provider.CodexID})
	stuck := h.waitFor(b.ID, 60*time.Second, func(b Build) bool {
		return b.task("greet").Status == TaskNeedsAttention || b.Status == BuildAwaitingFinal
	})
	task := stuck.task("greet")

	if task.Status != TaskNeedsAttention || len(task.Attempts) != 1 || task.Attempts[0].Status != AttemptFailed || task.Provider != provider.CursorID {
		t.Fatalf("a Cursor exit without evidence of a limit must stop for a person, not switch agents: %s %+v", task.Status, task.Attempts)
	}

	if note := task.Attempts[0].Note; !strings.Contains(note, "ended without a completion report (exit status 1)") || strings.Contains(note, "usage limit") {
		t.Fatalf("note %q", note)
	}

	s, _ := loadState(h.home)

	if a, _ := s.resolveAccount("cursor-main"); !a.CooldownUntil.IsZero() {
		t.Fatal("the Cursor account was put on a cooldown without a confirmed limit")
	}

	h.act(b.ID, "retry-task", actionRequest{Task: "greet", Provider: provider.CodexID})
	final := h.waitFor(b.ID, 60*time.Second, statusIs(BuildAwaitingFinal, BuildNeedsAttention, BuildFailed))

	if final.Status != BuildAwaitingFinal || final.task("greet").Provider != provider.CodexID || final.task("greet").Worktree != task.Worktree {
		t.Fatalf("retry with Codex in the same worktree: %s %s %s", final.Status, final.task("greet").Provider, final.Attention)
	}

	brief, _ := os.ReadFile(strings.TrimSuffix(final.task("greet").Attempts[1].Report, ".json") + ".brief.txt")

	if !strings.Contains(string(brief), "A previous attempt by Cursor CLI (cursor-main) stopped") {
		t.Fatalf("Codex must be told about Cursor's attempt:\n%s", brief)
	}
}
