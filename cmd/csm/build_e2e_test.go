package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

type buildHarness struct {
	t        *testing.T
	tmp      string
	home     string
	repo     string
	fixtures string
	state    *State
	engine   *buildEngine
	runs     *runManager
}

// newBuildHarness runs the real service engine and run manager against CSM's test agents, a throwaway CSM home and a small Go repository.
func newBuildHarness(t *testing.T, accounts map[string][]string, configure func(s *State)) *buildHarness {
	t.Helper()

	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	h := &buildHarness{t: t, tmp: tmp, home: filepath.Join(tmp, "csm"), repo: filepath.Join(tmp, "repo"), fixtures: filepath.Join(tmp, "fixtures")}

	t.Setenv("CSM_TEST_AS_CLI", "1")
	t.Setenv("CSM_FAKE_WORK", h.fixtures)
	t.Setenv("CSM_FAKE_ECHO", "")
	t.Setenv("CSM_FAKE_LIMIT", "")
	t.Setenv("CSM_FAKE_HOLD", "")
	t.Setenv("CSM_FAKE_SEED", "")
	t.Setenv("CSM_FAKE_WORK_MS", "")

	for _, id := range provider.IDs {
		t.Setenv("CSM_"+strings.ToUpper(id), "")
	}

	s, err := initState(h.home)
	if err != nil {
		t.Fatal(err)
	}

	for id, names := range accounts {
		shim := filepath.Join(tmp, "bin", id)
		os.MkdirAll(filepath.Dir(shim), 0o700)
		os.WriteFile(shim, []byte("#!/bin/sh\nexec '"+testBinary(t)+"' __fake-"+id+" \"$@\"\n"), 0o700)
		t.Setenv("CSM_"+strings.ToUpper(id), shim)

		p, _ := provider.New(id, shim)

		for _, name := range names {
			a, err := s.addAccount(id, name, time.Now())
			if err != nil {
				t.Fatal(err)
			}

			os.MkdirAll(a.ConfigDir, 0o700)

			if err := p.Login(a.ConfigDir); err != nil {
				t.Fatal(err)
			}

			ident, err := verifyProfile(p, a)
			if err != nil {
				t.Fatal(err)
			}

			a.Email, a.VerifiedAt = ident.Email, time.Now()
		}
	}

	s.Config.setPolicy(policyAutomatic)

	if configure != nil {
		configure(s)
	}

	if err := s.save(); err != nil {
		t.Fatal(err)
	}

	h.state = s
	os.MkdirAll(h.repo, 0o755)
	h.git("init", "-q", "-b", "main")
	h.write(h.repo, "go.mod", "module example.com/demo\n\ngo 1.22\n")
	h.write(h.repo, "demo.go", "package demo\n\nfunc Name() string { return \"demo\" }\n")
	h.write(h.repo, "demo_test.go", "package demo\n\nimport \"testing\"\n\nfunc TestName(t *testing.T) {\n\tif Name() != \"demo\" {\n\t\tt.Fatal(Name())\n\t}\n}\n")
	h.git("add", ".")
	h.git("commit", "-q", "-m", "init")
	h.write(h.repo, "NOTES.md", "uncommitted user work\n")

	bus := newEventBus()
	log := newActivityLog(h.home, bus)
	h.runs = newRunManager(h.home, testBinary(t), bus, log)
	h.engine = newBuildEngine(h.home, h.runs, log, bus)
	h.engine.start()

	t.Cleanup(func() {
		h.engine.close()
		h.runs.stopAll(2 * time.Second)
	})

	return h
}

func (h *buildHarness) git(args ...string) string {
	h.t.Helper()

	out, err := exec.Command("git", append([]string{"-C", h.repo, "-c", "user.name=csm", "-c", "user.email=csm@example.test"}, args...)...).CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %v: %v\n%s", args, err, out)
	}

	return string(out)
}

func (h *buildHarness) write(dir, name, content string) {
	h.t.Helper()

	path := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(path), 0o755)

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// fixture scripts what a test agent does for one Build Room step: the files it writes and the report it leaves.
func (h *buildHarness) fixture(step string, files map[string]string, report any) {
	h.t.Helper()

	for name, content := range files {
		h.write(filepath.Join(h.fixtures, step, "files"), name, content)
	}

	data, _ := json.Marshal(report)
	h.write(filepath.Join(h.fixtures, step), "report.json", string(data))
}

func (h *buildHarness) create(instruction string, policy BuildPolicy) *Build {
	h.t.Helper()

	b, err := h.engine.create(h.state, createBuildRequest{Instruction: instruction, ProjectDir: h.repo, Policy: policy})
	if err != nil {
		h.t.Fatal(err)
	}

	return b
}

func (h *buildHarness) snapshot(id string) Build {
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()

	data, _ := json.Marshal(h.engine.builds[id])

	var b Build

	json.Unmarshal(data, &b)

	return b
}

func (h *buildHarness) waitFor(id string, timeout time.Duration, cond func(Build) bool) Build {
	h.t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		b := h.snapshot(id)

		if cond(b) {
			return b
		}

		if time.Now().After(deadline) {
			data, _ := json.MarshalIndent(b, "", "  ")
			h.t.Fatalf("timed out; run is %s (%s)\n%s\nevents: %+v", b.Status, b.Attention, data, h.engine.store.events(id))
		}

		time.Sleep(100 * time.Millisecond)
	}
}

func statusIs(states ...BuildStatus) func(Build) bool {
	return func(b Build) bool { return slices.Contains(states, b.Status) }
}

func (h *buildHarness) act(id, action string, req actionRequest) {
	h.t.Helper()

	if err := h.engine.act(id, action, req); err != nil {
		h.t.Fatalf("%s: %v", action, err)
	}
}

func planFixture(tasks ...map[string]any) map[string]any {
	return map[string]any{"summary": "Add a greeting API and a formatter.", "layers": []string{"demo package"}, "considerations": []string{"Keep Name() unchanged"}, "suggested_checks": []string{"rm -rf /"}, "tasks": tasks}
}

func workReportFixture(files ...string) map[string]any {
	return map[string]any{"summary": "Implemented the task.", "files_changed": files, "commands": []string{"go test ./..."}, "tests": []string{"go test ./...: ok"}, "acceptance": []map[string]any{{"criterion": "works", "addressed": true}}}
}

func TestBuildRunEndToEnd(t *testing.T) {
	h := newBuildHarness(t, map[string][]string{provider.ClaudeID: {"alpha"}, provider.CodexID: {"work-codex"}}, nil)
	statusBefore := h.git("status", "--porcelain", "--branch")
	headBefore := h.git("rev-parse", "HEAD")

	h.fixture("plan", nil, planFixture(
		map[string]any{"id": "greet", "title": "Greeting function", "objective": "Add Greet(name)", "acceptance": []string{"Greet returns Hello, name"}, "files": []string{"greet.go"}, "agent": "claude"},
		map[string]any{"id": "format", "title": "Formatter", "objective": "Add Shout(s)", "files": []string{"format.go"}, "agent": "codex"},
		map[string]any{"id": "docs", "title": "Docs", "objective": "Document both", "files": []string{"README.md"}, "depends_on": []string{"greet", "format"}},
	))
	h.fixture("greet", map[string]string{"greet.go": "package demo\n\nfunc Greet(name string) string { return \"Hello, \" + name }\n", "greet_test.go": "package demo\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif Greet(\"Ada\") != \"Hello, Ada\" {\n\t\tt.Fatal(Greet(\"Ada\"))\n\t}\n}\n"}, workReportFixture("greet.go", "greet_test.go"))
	h.fixture("format", map[string]string{"format.go": "package demo\n\nimport \"strings\"\n\nfunc Shout(s string) string { return strings.ToUpper(s) }\n"}, workReportFixture("format.go", "missing.go"))
	h.fixture("docs", map[string]string{"README.md": "# demo\n\nGreet and Shout.\n"}, workReportFixture("README.md"))
	h.fixture("review-1", nil, map[string]any{"verdict": "approve", "summary": "Looks right.", "findings": []map[string]any{{"severity": "low", "file": "format.go", "line": 5, "title": "Consider a doc comment", "blocking": true}}})

	b := h.create("Add a greeting and a formatter, with docs", BuildPolicy{RequirePlanApproval: true, Review: true, RunBaseline: true, AllowRemote: true, MaxParallel: 2})

	got := h.waitFor(b.ID, 30*time.Second, statusIs(BuildAwaitingPlan, BuildNeedsAttention))
	if got.Status != BuildAwaitingPlan || len(got.Tasks) != 3 {
		t.Fatalf("plan: %s %s %d tasks", got.Status, got.Attention, len(got.Tasks))
	}

	if got.Plan.SuggestedChecks[0] != "rm -rf /" || slices.ContainsFunc(got.Policy.Checks, func(c CheckDef) bool { return strings.Contains(strings.Join(c.Command, " "), "rm") }) {
		t.Fatalf("suggested checks must be shown, never run: %+v", got.Policy.Checks)
	}

	if err := h.engine.act(b.ID, "retry-task", actionRequest{Task: "greet"}); err == nil {
		t.Fatal("retrying a task before the plan is approved must be refused")
	}

	h.act(b.ID, "approve-plan", actionRequest{})

	if err := h.engine.act(b.ID, "approve-plan", actionRequest{}); err == nil {
		t.Fatal("approving twice must be refused")
	}

	running := h.waitFor(b.ID, 30*time.Second, func(b Build) bool {
		return b.task("greet").Status == TaskRunning && b.task("format").Status == TaskRunning || b.Status == BuildNeedsAttention || b.task("greet").Status == TaskCompleted
	})

	if running.task("docs").Status != TaskBlocked && running.task("docs").Status != TaskReady {
		t.Fatalf("docs started before its dependencies finished: %s", running.task("docs").Status)
	}

	final := h.waitFor(b.ID, 90*time.Second, statusIs(BuildAwaitingFinal, BuildNeedsAttention, BuildFailed))
	if final.Status != BuildAwaitingFinal {
		t.Fatalf("status %s: %s", final.Status, final.Attention)
	}

	greet, format, docs := final.task("greet"), final.task("format"), final.task("docs")

	if greet.Provider != provider.ClaudeID || format.Provider != provider.CodexID {
		t.Fatalf("assignments: greet=%s format=%s", greet.Provider, format.Provider)
	}

	if greet.Worktree == format.Worktree || greet.Branch == format.Branch || strings.HasPrefix(greet.Worktree, h.repo) {
		t.Fatalf("tasks must have their own worktrees outside the repo: %s %s", greet.Worktree, format.Worktree)
	}

	if !slices.Equal(greet.Completion.FilesChanged, []string{"greet.go", "greet_test.go"}) || greet.Completion.Commit == "" {
		t.Fatalf("greet completion: %+v", greet.Completion)
	}

	if !slices.ContainsFunc(format.Completion.Discrepancies, func(d string) bool { return strings.Contains(d, "missing.go") }) {
		t.Fatalf("a claimed file that did not change must be flagged: %+v", format.Completion.Discrepancies)
	}

	if !slices.Equal(docs.Completion.FilesChanged, []string{"README.md"}) || !slices.Equal(changedSince(docs.Worktree, final.BaseCommit), []string{"README.md", "format.go", "greet.go", "greet_test.go"}) {
		t.Fatalf("docs must start from its dependencies' changes and own only its own: %v", docs.Completion.FilesChanged)
	}

	if !slices.Equal(final.Integration.Merged, []string{"greet", "format", "docs"}) {
		t.Fatalf("merged %v", final.Integration.Merged)
	}

	var integ, base int

	for _, c := range final.Checks {
		switch c.Stage {
		case "integration":
			integ++

			if c.State != CheckPassed {
				t.Errorf("check %s: %s %s\n%s", c.Name, c.State, c.Reason, c.Output)
			}
		case "baseline":
			base++
		}
	}

	if integ != 3 || base != 3 {
		t.Fatalf("checks: %d integration, %d baseline", integ, base)
	}

	if len(final.Reviews) != 1 || final.Reviews[0].Provider == "" || len(final.Findings) != 1 || final.Findings[0].Blocking {
		t.Fatalf("review: %+v findings %+v", final.Reviews, final.Findings)
	}

	if !strings.Contains(final.Findings[0].Source, "not verified") || final.Findings[0].TaskID != "format" {
		t.Fatalf("finding source/owner: %+v", final.Findings[0])
	}

	ready := h.waitFor(b.ID, 20*time.Second, func(b Build) bool { return b.PR.State == "prepared" })
	if ready.PR.Blocker == "" || !strings.Contains(ready.PR.Blocker, "no origin remote") {
		t.Fatalf("pr blocker: %q", ready.PR.Blocker)
	}

	if err := h.engine.act(b.ID, "approve-pr", actionRequest{}); err == nil {
		t.Fatal("pushing without a remote must be refused")
	}

	body, _ := os.ReadFile(ready.PR.BodyFile)

	for _, want := range []string{"## Summary", "Greeting function", "`go test`: passed", "Round 1 by"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}

	h.act(b.ID, "finish", actionRequest{})

	if done := h.snapshot(b.ID); done.Status != BuildCompleted {
		t.Fatalf("status %s", done.Status)
	}

	if after := h.git("status", "--porcelain", "--branch"); after != statusBefore {
		t.Fatalf("the user's working tree changed:\n%s\n%s", statusBefore, after)
	}

	if after := h.git("rev-parse", "HEAD"); after != headBefore {
		t.Fatal("the user's branch moved")
	}

	if _, err := os.Stat(greet.Worktree); err != nil {
		t.Fatal("worktrees must be kept after the run")
	}

	kinds := map[string]bool{}

	for _, ev := range h.engine.store.events(b.ID) {
		kinds[ev.Kind] = true
	}

	for _, k := range []string{"created", "plan.generated", "plan.approved", "task.scheduled", "agent.started", "agent.reported", "agent.exited", "task.completed", "integration.merged", "validation.completed", "review.started", "review.completed", "pr.prepared", "completed"} {
		if !kinds[k] {
			t.Errorf("no %s event", k)
		}
	}

	reloaded := newBuildEngine(h.home, h.runs, h.engine.log, h.engine.bus)

	if rb := reloaded.builds[b.ID]; rb == nil || rb.Status != BuildCompleted || len(rb.Tasks) != 3 {
		t.Fatal("the run did not persist")
	}
}
