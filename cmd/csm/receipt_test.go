package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

func intp(n int) *int { return &n }

const headA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const headB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func receiptBuild(status BuildStatus, checks ...CheckResult) *Build {
	now := time.Now()

	return &Build{
		Version: buildVersion, ID: "20261014-101500-abc123", Title: "Add a greeting", Instruction: "Add a greeting", Repo: "/tmp/repo", RepoName: "repo",
		BaseBranch: "main", BaseCommit: "cccccccccccccccccccccccccccccccccccccccc", Status: status, CreatedAt: now.Add(-time.Minute), StartedAt: now.Add(-time.Minute), CompletedAt: now,
		Policy:      BuildPolicy{Review: true, RunBaseline: true},
		Integration: Integration{Branch: "csm/build/integration", Head: headA, Files: []string{"greet.go"}, DiffStat: " greet.go | 3 +++\n 1 file changed, 3 insertions(+)"},
		Checks:      checks,
		Tasks: []*BuildTask{{ID: "greet", Title: "Greeting", Provider: provider.ClaudeID, Account: "alpha", Status: TaskCompleted, Completion: &Completion{
			FilesChanged: []string{"greet.go"}, ClaimedFiles: []string{"greet.go"}, ClaimedTests: []string{"go test ./...: all pass"}, ClaimedCommands: []string{"go build ./...", "git status"},
			Acceptance: []AcceptanceClaim{{Criterion: "Greet returns Hello", Addressed: true}},
		}}},
		Reviews: []ReviewRound{{Cycle: 1, Provider: provider.CodexID, Account: "work-codex", Independent: true, Verdict: "approve", Status: "completed", Commit: headA}},
	}
}

func check(name string, cmd []string, state CheckState, code *int, commit, output string) CheckResult {
	return CheckResult{Name: name, Command: cmd, Source: "go.mod", Stage: "integration", State: state, ExitCode: code, DurationMS: 1200, Commit: commit, Output: output, At: time.Now()}
}

func claimFor(r *Receipt, prefix string) ReceiptClaim {
	for _, c := range r.Claims {
		if strings.HasPrefix(c.Claim, prefix) {
			return c
		}
	}

	return ReceiptClaim{}
}

func TestReceiptOfAVerifiedRun(t *testing.T) {
	b := receiptBuild(BuildCompleted,
		check("go build", []string{"go", "build", "./..."}, CheckPassed, intp(0), headA, ""),
		check("go test", []string{"go", "test", "./..."}, CheckPassed, intp(0), headA, "ok  \texample.com/demo\t0.2s\nok  \texample.com/demo/api\t0.1s\n?   \texample.com/demo/cmd\t[no test files]\n"),
	)
	r := newReceipt(b, time.Now())

	if r.Outcome != OutcomeVerified || r.Revision.Commit != headA || !r.Acceptance.Passed || r.Partial || !r.Final {
		t.Fatalf("%+v", r)
	}

	if r.Headline != "Verified on aaaaaaaa: 2 of 2 checks passed" {
		t.Fatal(r.Headline)
	}

	if tc := r.Checks[1].Tests; tc == nil || tc.Unit != "packages" || tc.Passed != 2 || tc.Skipped != 1 || tc.Total != 3 {
		t.Fatalf("%+v", tc)
	}

	if c := claimFor(r, "go test"); c.Status != ClaimConfirmed || !strings.Contains(c.Evidence, "CSM ran go test ./... on aaaaaaaa: passed (exit 0)") {
		t.Fatalf("%+v", c)
	}

	if c := claimFor(r, "go build"); c.Status != ClaimConfirmed {
		t.Fatalf("%+v", c)
	}

	if c := claimFor(r, "ran 1 other"); c.Status != ClaimUnverifiable {
		t.Fatalf("an unmatched command is summarized, not confirmed: %+v", r.Claims)
	}

	if c := claimFor(r, "addressed: Greet"); c.Status != ClaimUnverifiable {
		t.Fatalf("acceptance claims are never confirmed: %+v", c)
	}

	if !r.Review.Independent || r.Review.Rounds[0].Agent != "Codex (work-codex)" {
		t.Fatalf("%+v", r.Review)
	}
}

func TestReceiptReportsFailuresTimeoutsAndContradictedClaims(t *testing.T) {
	timedOut := check("go vet", []string{"go", "vet", "./..."}, CheckFailed, intp(-1), headA, "")
	timedOut.TimedOut, timedOut.Reason = true, "timed out after 10m0s"
	b := receiptBuild(BuildNeedsAttention,
		check("go test", []string{"go", "test", "./..."}, CheckFailed, intp(1), headA, "--- FAIL: TestGreet (0.00s)\nFAIL\texample.com/demo\t0.2s\nok  \texample.com/demo/api\t0.1s\nFAIL\n"),
		timedOut,
	)
	b.Tasks[0].Completion.ClaimedTests = []string{"go test ./...: all pass", "go vet ./... fails on the old code"}
	b.Findings = []Finding{{ID: "f1", Cycle: 1, Severity: "high", Title: "Check failed: go test", Blocking: true, Status: findingOpen, Source: "CSM ran go test"}}
	r := newReceipt(b, time.Now())

	if r.Outcome != OutcomeFailed || r.Acceptance.Passed {
		t.Fatalf("%+v", r)
	}

	if r.Checks[0].Status != EvidenceFailed || *r.Checks[0].ExitCode != 1 || r.Checks[1].Status != EvidenceTimedOut {
		t.Fatalf("%+v", r.Checks)
	}

	if tc := r.Checks[0].Tests; tc == nil || tc.Failed != 1 || tc.Passed != 1 {
		t.Fatalf("failing go test counts %+v", tc)
	}

	if c := claimFor(r, "go test"); c.Status != ClaimContradicted || !strings.Contains(c.Evidence, "failed (exit 1)") {
		t.Fatalf("a claim of passing tests against a failing run is contradicted: %+v", c)
	}

	if c := claimFor(r, "go vet"); c.Status != ClaimConfirmed {
		t.Fatalf("a claim that a check fails is confirmed by a failing check: %+v", c)
	}

	if r.Review.OpenBlocking != 1 || r.Review.CheckFindings != 1 || !strings.Contains(strings.Join(r.Acceptance.Reasons, ";"), "1 blocking finding(s) open") {
		t.Fatalf("%+v %+v", r.Review, r.Acceptance)
	}

	if r.Final || !r.Partial {
		t.Fatalf("a run needing attention is not final: %+v", r.PartialReasons)
	}
}

func TestReceiptNeverCountsMissingOrStaleEvidenceAsPassed(t *testing.T) {
	stale := check("go build", []string{"go", "build", "./..."}, CheckPassed, intp(0), headB, "")
	noExit := check("go vet", []string{"go", "vet", "./..."}, CheckPassed, nil, headA, "")
	blocked := check("npm run lint", []string{"npm", "run", "lint"}, CheckBlocked, nil, headA, "")
	blocked.Reason = "npm is not installed or not on PATH"
	notRun := check("checks", nil, CheckNotRun, nil, "", "")
	baseline := check("go test", []string{"go", "test", "./..."}, CheckBaselineFailure, intp(1), headA, "")
	b := receiptBuild(BuildCompleted, stale, noExit, blocked, notRun, baseline)
	b.Integration.Head = headA
	r := newReceipt(b, time.Now())

	want := []string{EvidenceInconclusive, EvidenceInconclusive, EvidenceUnavailable, EvidenceSkipped, EvidencePreExisting}

	for i, c := range r.Checks {
		if c.Status != want[i] {
			t.Fatalf("check %d (%s) is %s, want %s: %s", i, c.Name, c.Status, want[i], c.Reason)
		}
	}

	if !strings.Contains(r.Checks[0].Reason, "the branch has since moved to aaaaaaaa") {
		t.Fatal(r.Checks[0].Reason)
	}

	if r.Outcome != OutcomeUnverified || r.Acceptance.Passed || !r.Partial || !r.Revision.Mixed {
		t.Fatalf("%+v", r)
	}

	if c := claimFor(r, "go test"); c.Status != ClaimContradicted {
		t.Fatalf("a check that also failed on the base still contradicts a passing claim: %+v", c)
	}

	if c := claimFor(r, "go build"); c.Status != ClaimUnverifiable {
		t.Fatalf("a stale check cannot confirm a claim: %+v", c)
	}
}

func TestClaimsMatchWholeCommands(t *testing.T) {
	checks := []ReceiptCheck{{Name: "go test", Command: "go test ./...", Stage: "integration", Status: EvidencePassed}, {Name: "npm run lint", Command: "npm run lint", Stage: "integration", Status: EvidenceFailed}}

	for claim, want := range map[string]string{"go test ./... passes": "go test", "ran `go test` again": "go test", "cargo test passes": "", "npm lint is clean": "npm run lint", "npm run linter": "", "gotest": ""} {
		got := ""

		if c := matchCheck(claim, checks); c != nil {
			got = c.Name
		}

		if got != want {
			t.Errorf("%q matched %q, want %q", claim, got, want)
		}
	}
}

func TestReceiptPartialWhenCancelledOrUnreviewed(t *testing.T) {
	b := receiptBuild(BuildCancelled)
	b.Reviews = nil
	r := newReceipt(b, time.Now())

	if !r.Final || !r.Partial || r.Outcome != OutcomeUnverified || r.Headline != "Unverified: CSM has not run any checks on this change" {
		t.Fatalf("%+v", r)
	}

	joined := strings.Join(r.PartialReasons, ";")

	if !strings.Contains(joined, "cancelled before CSM ran the checks") || !strings.Contains(joined, "no review ran") {
		t.Fatal(joined)
	}

	b = receiptBuild(BuildCompleted, check("go test", []string{"go", "test", "./..."}, CheckPassed, intp(0), headA, ""))
	b.Reviews[0].Independent = false
	b.Findings = []Finding{{ID: "f1", Blocking: true, Status: findingAccepted, Title: "x"}}

	if r := newReceipt(b, time.Now()); !r.Acceptance.Passed || !strings.Contains(strings.Join(r.Limitations, ";"), "also wrote part of the change") || !strings.Contains(strings.Join(r.Acceptance.Reasons, ";"), "accepted by the user") {
		t.Fatalf("%+v %+v", r.Acceptance, r.Limitations)
	}
}

func TestTestCountsAreReportedOnlyWhenReliable(t *testing.T) {
	cases := []struct {
		name   string
		cmd    []string
		state  CheckState
		output string
		want   *TestCounts
	}{
		{"pytest", []string{"python3", "-m", "pytest", "-q"}, CheckFailed, "....F\n==== 1 failed, 4 passed, 2 skipped in 0.31s ====\n", &TestCounts{Unit: "tests", Passed: 4, Failed: 1, Skipped: 2, Total: 7}},
		{"jest", []string{"npm", "run", "test"}, CheckPassed, "Test Suites: 2 passed, 2 total\nTests:       1 skipped, 12 passed, 13 total\n", &TestCounts{Unit: "tests", Passed: 12, Skipped: 1, Total: 13}},
		{"vitest", []string{"pnpm", "run", "test"}, CheckFailed, " Test Files  1 failed | 3 passed (4)\n      Tests  2 failed | 11 passed (13)\n", &TestCounts{Unit: "tests", Passed: 11, Failed: 2, Total: 13}},
		{"cargo", []string{"cargo", "test"}, CheckPassed, "test result: ok. 3 passed; 0 failed; 1 ignored; 0 measured\ntest result: ok. 2 passed; 0 failed; 0 ignored; 0 measured\n", &TestCounts{Unit: "tests", Passed: 5, Skipped: 1, Total: 6}},
		{"go truncated", []string{"go", "test", "./..."}, CheckPassed, "…ok  \texample.com/demo\t0.2s\n", nil},
		{"cargo truncated", []string{"cargo", "test"}, CheckPassed, "…test result: ok. 3 passed; 0 failed; 0 ignored\n", nil},
		{"no summary", []string{"go", "test", "./..."}, CheckFailed, "# example.com/demo\n./demo.go:3:1: syntax error\n", nil},
		{"passing run reporting failures", []string{"npm", "run", "test"}, CheckPassed, "Tests:  1 failed, 3 passed, 4 total\n", nil},
		{"not a test command", []string{"go", "build", "./..."}, CheckPassed, "ok  \tfake\n", nil},
	}

	for _, c := range cases {
		got := parseTestCounts(CheckResult{Command: c.cmd, State: c.state, Output: c.output})

		if c.want == nil {
			if got != nil {
				t.Errorf("%s: want no count, got %+v", c.name, got)
			}

			continue
		}

		if got == nil {
			t.Errorf("%s: no count", c.name)

			continue
		}

		got.From = ""

		if *got != *c.want {
			t.Errorf("%s: got %+v want %+v", c.name, *got, *c.want)
		}
	}
}

func TestReceiptIsSavedAndReloadedAfterARestart(t *testing.T) {
	home := t.TempDir()
	store := newBuildStore(home)
	b := receiptBuild(BuildCompleted, check("go test", []string{"go", "test", "./..."}, CheckPassed, intp(0), headA, ""))

	if err := store.save(b); err != nil {
		t.Fatal(err)
	}

	store.saveReceipt(b)

	var saved Receipt

	data, err := os.ReadFile(filepath.Join(store.dir(b.ID), receiptFile))
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.Outcome != OutcomeVerified {
		t.Fatalf("receipt not stored: %v %s", err, data)
	}

	r, loaded, err := newBuildStore(home).loadReceipt(b.ID, time.Now())
	if err != nil || loaded == nil || r.Outcome != OutcomeVerified || r.Drift == nil || !r.Drift.Unreadable {
		t.Fatalf("%v %+v", err, r)
	}

	os.WriteFile(filepath.Join(store.dir(b.ID), buildFile), []byte("{corrupt"), 0o600)

	r, loaded, err = store.loadReceipt(b.ID, time.Now())
	if err != nil || loaded != nil || r.Outcome != OutcomeVerified || r.Drift != nil || !strings.Contains(strings.Join(r.Limitations, ";"), "could not be read") {
		t.Fatalf("stored receipt is the fallback: %v %+v", err, r)
	}

	os.Remove(filepath.Join(store.dir(b.ID), receiptFile))

	if _, _, err := store.loadReceipt(b.ID, time.Now()); !errors.Is(err, errNoReceipt) {
		t.Fatalf("corrupt run without a receipt: %v", err)
	}

	if _, _, err := store.loadReceipt("20990101-000000-ffffff", time.Now()); err == nil || !strings.Contains(err.Error(), "no build run") {
		t.Fatal(err)
	}

	if _, _, err := store.loadReceipt("../etc", time.Now()); err == nil || !strings.Contains(err.Error(), "not a build run id") {
		t.Fatal(err)
	}
}

func TestReceiptOfALegacyRunWithoutRevisions(t *testing.T) {
	home := t.TempDir()
	store := newBuildStore(home)
	b := receiptBuild(BuildCompleted, check("go test", []string{"go", "test", "./..."}, CheckPassed, intp(0), "", ""))
	store.save(b)

	data, _ := os.ReadFile(filepath.Join(store.dir(b.ID), buildFile))
	legacy := strings.ReplaceAll(string(data), `"commit":"",`, "")
	os.WriteFile(filepath.Join(store.dir(b.ID), buildFile), []byte(legacy), 0o600)

	r, _, err := store.loadReceipt(b.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	if r.Revision.Commit != "" || !strings.Contains(strings.Join(r.Limitations, ";"), "cannot be tied to a commit") || r.Drift == nil || !r.Drift.Unreadable {
		t.Fatalf("%+v %+v", r.Limitations, r.Drift)
	}
}

func TestReceiptNoticesChangesAfterVerification(t *testing.T) {
	repo := gitRepo(t)
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", args, out)
		}

		return strings.TrimSpace(string(out))
	}

	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "a")

	res := atRevision(func(string, CheckDef, string) CheckResult {
		return CheckResult{Name: "true", Command: []string{"true"}, Stage: "integration", State: CheckPassed, ExitCode: intp(0)}
	})(repo, CheckDef{}, "integration")
	head := git("rev-parse", "HEAD")

	if res.Commit != head || res.Dirty {
		t.Fatalf("%+v", res)
	}

	b := receiptBuild(BuildAwaitingFinal, res)
	b.Repo, b.Integration.Head, b.Integration.Worktree = repo, head, repo
	r := newReceipt(b, time.Now())
	r.measureDrift(repo, time.Now())

	if !r.Drift.Same {
		t.Fatalf("%+v", r.Drift)
	}

	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("two\n"), 0o644)
	r.measureDrift(repo, time.Now())

	if r.Drift.Same || !strings.Contains(r.Drift.Note, "1 uncommitted change(s)") {
		t.Fatalf("%+v", r.Drift)
	}

	git("commit", "-q", "-am", "b")
	r.measureDrift(repo, time.Now())

	if r.Drift.Same || !strings.Contains(r.Drift.Note, "have not been verified") {
		t.Fatalf("%+v", r.Drift)
	}

	if res := atRevision(func(string, CheckDef, string) CheckResult { return CheckResult{} })(repo, CheckDef{}, "x"); res.Dirty {
		t.Fatal("clean tree marked dirty")
	}

	os.WriteFile(filepath.Join(repo, "b.txt"), []byte("new\n"), 0o644)

	if res := atRevision(func(string, CheckDef, string) CheckResult { return CheckResult{} })(repo, CheckDef{}, "x"); !res.Dirty {
		t.Fatal("dirty tree not recorded")
	}
}

func TestRunCheckRecordsATimeout(t *testing.T) {
	saved := checkTimeout
	checkTimeout = 200 * time.Millisecond

	t.Cleanup(func() { checkTimeout = saved })

	r := runCheck(t.TempDir(), CheckDef{Name: "slow", Command: []string{"/bin/sh", "-c", "sleep 5"}}, "integration")

	if r.State != CheckFailed || !r.TimedOut || receiptCheck(r, "").Status != EvidenceTimedOut {
		t.Fatalf("%+v", r)
	}
}

func TestReceiptRedactsSecrets(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	c := check("deploy check", []string{"/bin/sh", "-c", "curl -H 'Authorization: token " + secret + "' x"}, CheckFailed, intp(1), headA, "export GITHUB_TOKEN="+secret+"\nfailed")
	c.Reason = "token=" + secret
	b := receiptBuild(BuildCompleted, c)
	b.Tasks[0].Completion.ClaimedTests = []string{"used GITHUB_TOKEN=" + secret}
	r := newReceipt(b, time.Now())

	data, _ := json.Marshal(r)

	if strings.Contains(string(data), secret) {
		t.Fatalf("secret in the receipt: %s", data)
	}

	var out strings.Builder

	printReceipt(&Logger{out: &out, err: &out}, r)

	if strings.Contains(out.String(), secret) {
		t.Fatal("secret in the printed receipt")
	}

	if !strings.Contains(receiptPRSection(r), "Verification by CSM") || strings.Contains(receiptPRSection(r), secret) {
		t.Fatal("PR section")
	}
}

func TestBuildReceiptCommand(t *testing.T) {
	home := t.TempDir()
	store := newBuildStore(home)
	b := receiptBuild(BuildCompleted,
		check("go build", []string{"go", "build", "./..."}, CheckPassed, intp(0), headA, ""),
		check("go test", []string{"go", "test", "./..."}, CheckFailed, intp(1), headA, "--- FAIL: TestGreet\nFAIL\texample.com/demo\t0.1s\nFAIL\n"),
	)
	store.save(b)

	var out strings.Builder
	log := &Logger{out: &out, err: &out}

	if err := cmdBuild(log, home, []string{"receipt", b.ID}); err != nil {
		t.Fatal(err)
	}

	text := out.String()

	for _, want := range []string{"Receipt for run " + b.ID + " (final)", "Outcome      Failed on aaaaaaaa", "Verified     " + headA, "✗ failed", "exit 1", "0/1 packages passed", "✗ contradicted", "Output of go test ./...", "  │ --- FAIL: TestGreet", "Limitations", "round 1 by Codex (work-codex), a different agent: approve"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}

	out.Reset()

	if err := cmdBuild(log, home, []string{"receipt", b.ID, "--json"}); err != nil {
		t.Fatal(err)
	}

	var r Receipt

	if err := json.Unmarshal([]byte(out.String()), &r); err != nil || r.BuildID != b.ID || r.Outcome != OutcomeFailed {
		t.Fatalf("%v %s", err, out.String())
	}

	if err := cmdBuild(log, home, []string{"receipt", "20990101-000000-ffffff"}); err == nil || !strings.Contains(err.Error(), "csm build list") {
		t.Fatal(err)
	}

	if err := cmdBuild(log, home, []string{"receipt"}); err == nil || !strings.Contains(err.Error(), "usage: csm build receipt") {
		t.Fatal(err)
	}
}

func TestReceiptEvidenceReachesTheNextAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CSM_HOME", home)

	b := receiptBuild(BuildFixing, check("go test", []string{"go", "test", "./..."}, CheckFailed, intp(1), headA, "FAIL\texample.com/demo\t0.1s\nFAIL\n"))
	b.Status = BuildFixing
	b.Findings = []Finding{{ID: "f1", Title: "Check failed: go test", Blocking: true, Status: findingOpen, Source: "CSM ran go test", Severity: "high"}}
	newBuildStore(home).save(b)

	t.Setenv("CSM_BUILD_ID", b.ID)
	t.Chdir(t.TempDir())

	got := buildEvidence()

	for _, want := range []string{"csm build receipt " + b.ID, "go test ./...: failed (exit 1)", "1 blocking finding(s) are open", "1 earlier agent claim(s) were contradicted", "has not been checked by CSM"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}

	if len(got) > 2000 {
		t.Fatalf("the brief should stay short, got %d bytes", len(got))
	}

	fix := &BuildTask{ID: "fix-1", Kind: TaskKindFix, Title: "Fix", Objective: "Fix the failing check", Findings: []string{"f1"}, BaseCommit: headA, Worktree: "/tmp/w", Branch: "b"}

	if s := taskInstruction(b, fix, nil); !strings.Contains(s, "Your worktree started from the verified revision aaaaaaaa") {
		t.Fatal(s)
	}

	t.Setenv("CSM_BUILD_ID", "")

	if buildEvidence() != "" {
		t.Fatal("no evidence outside a build")
	}
}

func TestPRBodyStatesOnlyMeasuredVerification(t *testing.T) {
	b := receiptBuild(BuildAwaitingFinal, check("go test", []string{"go", "test", "./..."}, CheckPassed, intp(0), headA, "ok  \texample.com/demo\t0.1s\n"))
	b.Integration.Head = headB
	body := prBody(b)

	for _, want := range []string{"## Verification by CSM", "| `go test ./...` | not conclusive: passed ran on aaaaaaaa; the branch has since moved to bbbbbbbb", "csm build receipt " + b.ID} {
		if !strings.Contains(strings.ReplaceAll(body, "inconclusive", "not conclusive"), want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}

	if strings.Contains(body, "Pull request opened") || strings.Contains(body, "merged") {
		t.Fatal("the body must not claim remote operations")
	}
}

func TestBuildRunWritesAReceipt(t *testing.T) {
	h := newBuildHarness(t, map[string][]string{provider.ClaudeID: {"alpha"}, provider.CodexID: {"work-codex"}}, nil)
	h.fixture("plan", nil, singleTaskPlan("claude"))
	h.fixture("greet", map[string]string{"greet.go": greetGo}, workReportFixture("greet.go"))
	h.fixture("review-1", nil, map[string]any{"verdict": "approve", "summary": "Looks right.", "findings": []any{}})

	b := approvedRun(h, BuildPolicy{RequirePlanApproval: true, RunBaseline: true, Review: true})
	final := h.waitFor(b.ID, 90*time.Second, statusIs(BuildAwaitingFinal, BuildNeedsAttention))

	if final.Status != BuildAwaitingFinal {
		t.Fatalf("status %s: %s", final.Status, final.Attention)
	}

	var stored Receipt

	data, err := os.ReadFile(filepath.Join(h.engine.store.dir(b.ID), receiptFile))
	if err != nil || json.Unmarshal(data, &stored) != nil {
		t.Fatalf("no stored receipt: %v", err)
	}

	r, err := h.engine.receipt(b.ID)
	if err != nil {
		t.Fatal(err)
	}

	if r.Revision.Commit == "" || r.Revision.Commit != final.Integration.Head || r.Outcome != OutcomeVerified {
		t.Fatalf("%s %s %+v", r.Outcome, r.Revision.Commit, r.Checks)
	}

	if r.Drift == nil || !r.Drift.Same {
		t.Fatalf("%+v", r.Drift)
	}

	if len(r.Baseline) == 0 || r.Baseline[0].Commit != final.BaseCommit {
		t.Fatalf("baseline %+v", r.Baseline)
	}

	if c := claimFor(r, "go test"); c.Status != ClaimConfirmed {
		t.Fatalf("%+v", r.Claims)
	}

	if len(r.Review.Rounds) != 1 || !r.Review.Independent {
		t.Fatalf("%+v", r.Review)
	}

	h.write(final.Integration.Worktree, "late.go", "package demo\n")

	if r, _ := h.engine.receipt(b.ID); r.Drift.Same {
		t.Fatal("a change after verification must show")
	}
}
