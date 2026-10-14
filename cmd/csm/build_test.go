package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

func task(id string, deps ...string) *BuildTask {
	return &BuildTask{ID: id, Kind: TaskKindImplement, Title: id, Objective: "do " + id, DependsOn: deps, Status: TaskBlocked}
}

func TestBuildTransitionsAreValidated(t *testing.T) {
	b := &Build{Status: BuildDraft}

	for _, to := range []BuildStatus{BuildPlanning, BuildAwaitingPlan, BuildReady, BuildRunning, BuildIntegrating, BuildValidating, BuildReviewing, BuildFixing, BuildIntegrating, BuildValidating, BuildAwaitingFinal, BuildCompleted} {
		if err := b.transition(to); err != nil {
			t.Fatal(err)
		}
	}

	if b.CompletedAt.IsZero() {
		t.Fatal("a terminal state records its time")
	}

	for _, c := range []struct{ from, to BuildStatus }{
		{BuildDraft, BuildRunning},
		{BuildAwaitingPlan, BuildRunning},
		{BuildRunning, BuildCompleted},
		{BuildCompleted, BuildRunning},
		{BuildCancelled, BuildRunning},
		{BuildReviewing, BuildCompleted},
	} {
		b := &Build{Status: c.from}

		if err := b.transition(c.to); !errors.Is(err, errBadTransition) || b.Status != c.from {
			t.Errorf("%s → %s allowed", c.from, c.to)
		}
	}

	b = &Build{Status: BuildRunning, Attention: "x"}
	b.transition(BuildNeedsAttention)
	b.Attention, b.Recovery = "stuck", []string{"retry"}
	b.transition(BuildRunning)

	if b.Attention != "" || b.Recovery != nil {
		t.Fatal("leaving needs-attention clears the reason")
	}
}

func TestValidatePlanRejectsBadGraphs(t *testing.T) {
	cases := map[string][]*BuildTask{
		"cycle: a → c → b → a":   {task("a", "c"), task("b", "a"), task("c", "b")},
		"depends on itself":      {task("a", "a")},
		`"zzz", which is not in`: {task("a", "zzz")},
		"used twice":             {task("a"), task("a")},
		"lowercase letters":      {task("Bad ID")},
		"has no tasks":           {},
		"unknown agent":          {{ID: "a", Title: "a", Objective: "o", Provider: "not-an-agent"}},
		"needs a title":          {{ID: "a", Objective: "o"}},
	}

	for want, tasks := range cases {
		err := validatePlan(tasks)

		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}

	var many []*BuildTask

	for i := range maxPlanTasks + 1 {
		many = append(many, task(string(rune('a'+i))))
	}

	if err := validatePlan(many); err == nil {
		t.Fatal("an oversized plan must be rejected")
	}

	if err := validatePlan([]*BuildTask{task("api"), task("ui"), task("docs", "api", "ui")}); err != nil {
		t.Fatal(err)
	}
}

func TestTopoOrderKeepsDependenciesFirst(t *testing.T) {
	tasks := []*BuildTask{task("docs", "api", "ui"), task("ui", "api"), task("api")}
	var got []string

	for _, t := range topoOrder(tasks) {
		got = append(got, t.ID)
	}

	if !slices.Equal(got, []string{"api", "ui", "docs"}) {
		t.Fatal(got)
	}
}

func TestSchedulingRespectsDependenciesConcurrencyAndSharedFiles(t *testing.T) {
	a, b2, c, d := task("a"), task("b"), task("c"), task("d", "a")
	a.Files, b2.Files, c.Files = []string{"api.go"}, []string{"api.go"}, []string{"ui.go"}
	b := &Build{Policy: BuildPolicy{MaxParallel: 2}, Tasks: []*BuildTask{a, b2, c, d}}

	var ids []string

	for _, t := range b.schedulable() {
		ids = append(ids, t.ID)
	}

	if !slices.Equal(ids, []string{"a", "c"}) {
		t.Fatalf("b shares api.go with a and d waits for a: %v", ids)
	}

	a.Status, c.Status = TaskRunning, TaskRunning

	if got := b.schedulable(); len(got) != 0 {
		t.Fatalf("over the limit: %v", got)
	}

	a.Status = TaskCompleted

	if got := b.schedulable(); len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("one slot is free and b comes first in the plan: %v", got)
	}

	b.Paused = true

	if got := b.schedulable(); got != nil {
		t.Fatal("a paused run schedules nothing")
	}

	b.Paused, b.CancelAsked = false, true

	if got := b.schedulable(); got != nil {
		t.Fatal("a cancelled run schedules nothing")
	}
}

func TestNormalizePolicyBoundsLimits(t *testing.T) {
	p := normalizePolicy(BuildPolicy{MaxParallel: 99, MaxReviewCycles: 99, Agents: []string{"codex", "nope", "codex", "claude"}, Planner: "x", Reviewer: "gemini"})

	if p.MaxParallel != maxBuildParallel || p.MaxReviewCycles != maxReviewCycles || !slices.Equal(p.Agents, []string{"codex", "claude"}) || p.Planner != "" || p.Reviewer != "gemini" {
		t.Fatalf("%+v", p)
	}

	if p := normalizePolicy(BuildPolicy{}); p.MaxParallel != defaultParallel || p.MaxReviewCycles != defaultReviewCycles {
		t.Fatalf("%+v", p)
	}
}

func TestCheckResultsAreComparedWithTheBaseline(t *testing.T) {
	final := []CheckResult{{Name: "go test", State: CheckFailed}, {Name: "go vet", State: CheckFailed}, {Name: "go build", State: CheckPassed}}
	baseline := []CheckResult{{Name: "go test", State: CheckFailed}, {Name: "go vet", State: CheckPassed}}
	got := compareBaseline(final, baseline)

	if got[0].State != CheckBaselineFailure || got[1].State != CheckFailed || got[2].Baseline != "" {
		t.Fatalf("%+v", got)
	}

	if s := summarizeChecks(got); s.Verified() || s.Baseline != 1 || s.Failed != 1 {
		t.Fatalf("%+v", s)
	}

	if !summarizeChecks([]CheckResult{{State: CheckPassed}}).Verified() {
		t.Fatal("all passed is verified")
	}

	for _, st := range []CheckState{CheckSkipped, CheckBlocked, CheckNotRun, CheckBaselineFailure} {
		if summarizeChecks([]CheckResult{{State: CheckPassed}, {State: st}}).Verified() {
			t.Errorf("%s must not count as verified", st)
		}
	}

	if summarizeChecks(nil).Verified() {
		t.Fatal("nothing run is not verified")
	}
}

func TestRunCheckReportsActualExecution(t *testing.T) {
	dir := t.TempDir()

	if r := runCheck(dir, CheckDef{Name: "ok", Command: []string{"/bin/sh", "-c", "echo token=abc123secret; exit 0"}}, "integration"); r.State != CheckPassed || *r.ExitCode != 0 || strings.Contains(r.Output, "abc123secret") {
		t.Fatalf("%+v", r)
	}

	if r := runCheck(dir, CheckDef{Name: "bad", Command: []string{"/bin/sh", "-c", "exit 3"}}, "integration"); r.State != CheckFailed || *r.ExitCode != 3 {
		t.Fatalf("%+v", r)
	}

	if r := runCheck(dir, CheckDef{Name: "missing", Command: []string{"no-such-tool-xyz"}}, "integration"); r.State != CheckBlocked {
		t.Fatalf("%+v", r)
	}
}

func TestDetectChecksReadsTheRepository(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"test":"vitest","build":"vite build","dev":"vite"}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), nil, 0o644)

	var names []string

	for _, c := range detectChecks(dir) {
		names = append(names, c.Name)
	}

	if !slices.Equal(names, []string{"pnpm run test", "pnpm run build"}) {
		t.Fatal(names)
	}

	if c, ok := customCheck("  make lint  "); !ok || !slices.Equal(c.Command, []string{"/bin/sh", "-c", "make lint"}) || c.Source != "custom" {
		t.Fatalf("%+v", c)
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()

	dir, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(dir, "repo")
	os.MkdirAll(repo, 0o755)

	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%s", out)
		}
	}

	return repo
}

func TestWorktreesAreCreatedOnlyWhereTheyCannotHurtUserWork(t *testing.T) {
	repo := gitRepo(t)
	head, _ := gitOut(repo, "rev-parse", "HEAD")
	outsideDir := filepath.Join(filepath.Dir(repo), "trees", "a")

	if err := addWorktree(repo, filepath.Join(repo, "inside"), "csm/build-x/a", head); err == nil || !strings.Contains(err.Error(), "outside the repository") {
		t.Fatalf("inside the repo: %v", err)
	}

	if err := addWorktree(repo, "relative/path", "csm/build-x/a", head); err == nil {
		t.Fatal("a relative path must be refused")
	}

	if err := addWorktree(repo, outsideDir, "-bad", head); err == nil {
		t.Fatal("a branch name starting with a dash must be refused")
	}

	gitIn(repo, "branch", "user-branch")

	if err := addWorktree(repo, outsideDir, "user-branch", head); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("an existing branch must not be reused: %v", err)
	}

	os.MkdirAll(filepath.Join(filepath.Dir(repo), "trees", "taken"), 0o755)

	if err := addWorktree(repo, filepath.Join(filepath.Dir(repo), "trees", "taken"), "csm/build-x/b", head); err == nil {
		t.Fatal("an existing directory must not be overwritten")
	}

	if err := addWorktree(repo, outsideDir, "csm/build-x/a", head); err != nil {
		t.Fatal(err)
	}

	if err := addWorktree(repo, outsideDir, "csm/build-x/a", head); err != nil {
		t.Fatalf("recreating the same worktree after a restart must be a no-op: %v", err)
	}

	if b, _ := gitOut(repo, "branch", "--show-current"); b != "main" {
		t.Fatal("the user's checkout changed")
	}
}

func TestReportsAreBoundedAndCleaned(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.json")
	os.WriteFile(big, []byte(`{"summary":"`+strings.Repeat("x", maxReportBytes)+`"}`), 0o600)

	var r workReport

	if err := readReport(big, &r); err == nil {
		t.Fatal("an oversized report must be refused")
	}

	if err := readReport(filepath.Join(dir, "none.json"), &r); !errors.Is(err, errNoReport) {
		t.Fatal(err)
	}

	for _, bad := range []string{"../etc/passwd", "/etc/passwd", "a/../../b", ""} {
		if cleanPath(bad) != "" {
			t.Errorf("%q accepted", bad)
		}
	}

	if got := clean("token: ghp_abcdefghijklmnopqrstuvwxyz0123456789", 200); strings.Contains(got, "ghp_") {
		t.Fatal(got)
	}
}

func newTestEngine(t *testing.T) (*buildEngine, string) {
	t.Helper()

	home := filepath.Join(t.TempDir(), "csm")

	if _, err := initState(home); err != nil {
		t.Fatal(err)
	}

	bus := newEventBus()
	log := newActivityLog(home, bus)

	return newBuildEngine(home, newRunManager(home, "/bin/false", bus, log), log, bus), home
}

func deadPID(t *testing.T) int {
	cmd := exec.Command("true")
	cmd.Run()

	return cmd.Process.Pid
}

func TestRestartMarksLostAgentsInterruptedAndKeepsReportedWork(t *testing.T) {
	e, home := newTestEngine(t)
	now := time.Now()
	store := newBuildStore(home)

	lost := &Build{ID: newBuildID(now), Status: BuildRunning, Policy: normalizePolicy(BuildPolicy{}), CreatedAt: now}
	tk := task("api")
	tk.Status, tk.Provider = TaskRunning, "claude"
	lost.Tasks = []*BuildTask{tk}
	lost.Attempts = []Attempt{{ID: "a1", Role: RoleImplement, TaskID: "api", Provider: "claude", RunID: "gone", CSMPID: deadPID(t), Status: AttemptRunning, Report: filepath.Join(home, "none.json"), StartedAt: now}}
	store.save(lost)

	planned := &Build{ID: newBuildID(now.Add(time.Second)), Status: BuildPlanning, Policy: normalizePolicy(BuildPolicy{RequirePlanApproval: true}), CreatedAt: now}
	report := filepath.Join(home, "plan.json")
	data, _ := json.Marshal(planFixture(map[string]any{"id": "api", "title": "API", "objective": "x"}))
	os.WriteFile(report, data, 0o600)
	planned.Attempts = []Attempt{{ID: "a1", Role: RolePlan, Provider: "codex", RunID: "gone", Status: AttemptRunning, Report: report, StartedAt: now}}
	store.save(planned)

	e, _ = newBuildEngine(home, e.runs, e.log, e.bus), home
	e.step()

	got := e.builds[lost.ID]

	if got.Attempts[0].Status != AttemptInterrupted || !strings.Contains(got.Attempts[0].Note, "service stopped") {
		t.Fatalf("attempt %+v", got.Attempts[0])
	}

	if got.Tasks[0].Status != TaskNeedsAttention || got.Status != BuildNeedsAttention || !slices.Contains(got.Recovery, "retry_task") {
		t.Fatalf("task %s build %s %v", got.Tasks[0].Status, got.Status, got.Recovery)
	}

	if p := e.builds[planned.ID]; p.Status != BuildAwaitingPlan || len(p.Tasks) != 1 {
		t.Fatalf("a report written before the restart must still count: %s %s", p.Status, p.Attention)
	}

	e.step()

	if n := len(e.builds[lost.ID].Attempts); n != 1 {
		t.Fatalf("a repeated tick must not start anything: %d attempts", n)
	}

	reloaded, _ := store.load(lost.ID)

	if reloaded.Status != BuildNeedsAttention {
		t.Fatal("the reconciled state must be saved")
	}
}

func TestRemoteActionsNeedFinalApprovalAndPermission(t *testing.T) {
	e, _ := newTestEngine(t)
	now := time.Now()
	b := &Build{ID: newBuildID(now), Status: BuildRunning, Policy: normalizePolicy(BuildPolicy{AllowRemote: false}), CreatedAt: now, Integration: Integration{Branch: "csm/build-abcdef/integration"}}
	e.builds[b.ID] = b

	if err := e.act(b.ID, "approve-pr", actionRequest{}); !errors.Is(err, errActionState) {
		t.Fatalf("before final approval: %v", err)
	}

	b.Status, b.PR.State = BuildAwaitingFinal, "prepared"

	if err := e.act(b.ID, "approve-pr", actionRequest{}); err == nil || !strings.Contains(err.Error(), "without permission for remote") {
		t.Fatalf("without permission: %v", err)
	}

	b.Policy.AllowRemote, b.PR.Blocker = true, "the GitHub CLI (gh) is not installed"

	if err := e.act(b.ID, "approve-pr", actionRequest{}); err == nil || b.PR.Approved {
		t.Fatalf("with a blocker: %v", err)
	}

	b.Integration.Branch = "main"
	b.BaseBranch = "main"
	b.PR.Blocker = ""
	e.pushAndOpenPR(b)

	if !strings.Contains(b.PR.Error, "did not create") {
		t.Fatalf("pushing a branch Build Room did not create must be refused: %q", b.PR.Error)
	}

	if err := e.act(b.ID, "dance", actionRequest{}); err == nil {
		t.Fatal("unknown action accepted")
	}

	if err := e.act(b.ID, "assign", actionRequest{Task: "x", Provider: "cursor"}); err == nil {
		t.Fatal("unknown agent accepted")
	}
}

func TestDecideBoundsTheFixCycle(t *testing.T) {
	e, _ := newTestEngine(t)
	b := &Build{ID: newBuildID(time.Now()), Status: BuildReviewing, Policy: normalizePolicy(BuildPolicy{MaxReviewCycles: 2}), Cycle: 1, Integration: Integration{Head: "abc"}}
	b.Tasks = []*BuildTask{{ID: "api", Provider: "codex", Status: TaskCompleted, Completion: &Completion{FilesChanged: []string{"api.go"}}}}
	b.Findings = []Finding{{ID: "f1", Cycle: 1, Blocking: true, Status: findingOpen, File: "api.go", TaskID: "api", Title: "bug"}, {ID: "f2", Cycle: 1, Blocking: false, Status: findingOpen, Title: "nit"}}

	e.decide(b)

	if b.Status != BuildFixing || len(b.Tasks) != 2 || b.Tasks[1].Provider != "codex" || !slices.Equal(b.Tasks[1].Findings, []string{"f1"}) || b.Findings[0].Status != findingFixAsked {
		t.Fatalf("status %s tasks %+v", b.Status, b.Tasks)
	}

	b.Status, b.Cycle = BuildReviewing, 2
	b.Findings = append(b.Findings, Finding{ID: "f3", Cycle: 2, Blocking: true, Status: findingOpen, Title: "still"})
	e.decide(b)

	if b.Status != BuildNeedsAttention || len(b.Tasks) != 2 {
		t.Fatalf("past the limit: %s, %d tasks", b.Status, len(b.Tasks))
	}

	b.Status, b.Attention = BuildReviewing, ""
	b.Findings[2].Status = findingAccepted
	e.decide(b)

	if b.Status != BuildAwaitingFinal {
		t.Fatal(b.Status)
	}
}

func TestRetryRefusesRunningOrFinishedTasks(t *testing.T) {
	e, _ := newTestEngine(t)
	b := &Build{ID: newBuildID(time.Now()), Status: BuildRunning, Policy: normalizePolicy(BuildPolicy{})}
	running, done := task("a"), task("b")
	running.Status, done.Status = TaskRunning, TaskCompleted
	b.Tasks = []*BuildTask{running, done}
	e.builds[b.ID] = b

	for _, id := range []string{"a", "b", "missing"} {
		if err := e.act(b.ID, "retry-task", actionRequest{Task: id}); err == nil {
			t.Errorf("retry of %s accepted", id)
		}
	}

	if err := e.act(b.ID, "assign", actionRequest{Task: "a", Provider: "codex"}); err == nil {
		t.Fatal("reassigning a running task accepted")
	}
}

func TestBuildContractReachesAHandedOffAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brief.txt")
	os.WriteFile(path, []byte("CSM Build Room task brief:\ndo the thing\n"), 0o600)
	t.Setenv("CSM_BUILD_CONTRACT", path)

	if got := buildContract(); !strings.Contains(got, "do the thing") {
		t.Fatal(got)
	}

	t.Setenv("CSM_BUILD_CONTRACT", "")

	if buildContract() != "" {
		t.Fatal("no contract outside a build")
	}
}

func TestBuildEndpoints(t *testing.T) {
	_, srv := testDaemon(t)
	repo := gitRepo(t)

	if code, _, raw := call(t, srv, "GET", "/v1/builds", nil); code != 200 || strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("list: %d %s", code, raw)
	}

	code, out, _ := call(t, srv, "GET", "/v1/builds/preflight?project_dir="+repo, nil)
	if code != 200 || out["branch"] != "main" || out["repo_name"] != "repo" {
		t.Fatalf("preflight: %d %v", code, out)
	}

	if agents, _ := out["agents"].([]any); len(agents) != len(provider.IDs) {
		t.Fatalf("preflight lists every agent with its readiness: %v", out["agents"])
	}

	if code, out, _ := call(t, srv, "GET", "/v1/builds/preflight?project_dir="+t.TempDir(), nil); code != 400 || !strings.Contains(out["error"].(string), "not inside a git repository") {
		t.Fatalf("non-repo: %d %v", code, out)
	}

	code, out, _ = call(t, srv, "POST", "/v1/builds", map[string]any{"instruction": "Add invitations", "project_dir": repo})
	if code != 400 || !strings.Contains(out["error"].(string), "no coding agent is ready") {
		t.Fatalf("create without agents: %d %v", code, out)
	}

	if code, _, _ := call(t, srv, "POST", "/v1/builds", map[string]any{"instruction": "x", "project_dir": repo, "base_branch": "--upload-pack=evil"}); code != 400 {
		t.Fatal("an option-like base branch must be refused")
	}

	if code, _, _ := call(t, srv, "GET", "/v1/builds/20260101-000000-abcdef", nil); code != 404 {
		t.Fatal("unknown run")
	}

	if code, _, _ := call(t, srv, "POST", "/v1/builds/20260101-000000-abcdef/actions/cancel", map[string]any{}); code != 409 {
		t.Fatal("action on an unknown run")
	}

	if code, _, _ := call(t, srv, "GET", "/v1/builds/..%2F..%2Fetc/diff", nil); code == 200 {
		t.Fatal("path traversal in the run id")
	}
}

func TestBuildTitleUsesTheLeadOfTheInstruction(t *testing.T) {
	for in, want := range map[string]string{
		"Add team invitations: a store, endpoints and tests": "Add team invitations",
		"Fix: x":                         "Fix: x",
		"Rename the config loader\nmore": "Rename the config loader",
	} {
		if got := buildTitle(in); got != want {
			t.Errorf("%q → %q", in, got)
		}
	}
}

func TestRunnerCommitsOnlyInsideBuildWorktrees(t *testing.T) {
	home := t.TempDir()
	repo := gitRepo(t)
	r := &runner{state: &State{Home: home}, project: Project{Dir: repo}, cwd: repo}

	t.Setenv("CSM_BUILD_ID", "20260101-000000-abcdef")

	if r.inBuildWorktree() {
		t.Fatal("a repository outside ~/.csm/builds must never get a commit from csm")
	}

	tree := filepath.Join(home, buildsDir, "20260101-000000-abcdef", "worktrees", "api")
	os.MkdirAll(tree, 0o700)
	r.project.Dir = tree

	if !r.inBuildWorktree() {
		t.Fatal("a Build Room worktree is where the switch commit belongs")
	}

	t.Setenv("CSM_BUILD_ID", "")

	if r.inBuildWorktree() {
		t.Fatal("without a build, no commit")
	}
}
