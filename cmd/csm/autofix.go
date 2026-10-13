package main

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/onoja123/csm/engrun"
	"github.com/onoja123/csm/internal/sandbox"
)

const (
	KindAutoFix     = "autofix"
	opReproduce     = "reproduce"
	reproTimeout    = 5 * time.Minute
	fixTaskID       = "fix"
	investigateStep = "investigate"
)

// Investigation is the investigating agent's report, bounded and redacted. It is an agent's analysis: CSM records it and
// acts on the classification, but the only facts it treats as established are the ones it measured itself.
type Investigation struct {
	Classification engrun.Classification `json:"classification"`
	Summary        string                `json:"summary,omitempty"`
	RootCause      string                `json:"root_cause,omitempty"`
	Evidence       []string              `json:"evidence,omitempty"`
	Files          []string              `json:"files,omitempty"`
	FixPlan        string                `json:"fix_plan,omitempty"`
	Confidence     string                `json:"confidence,omitempty"`
	Test           *ReproTest            `json:"test,omitempty"`
	Commit         string                `json:"commit,omitempty"`
	Worktree       string                `json:"worktree,omitempty"`
	Branch         string                `json:"branch,omitempty"`
	IgnoredEdits   []string              `json:"ignored_edits,omitempty"`
	Source         string                `json:"source,omitempty"`
}

// ReproTest is the regression test the investigator wrote. CSM builds the command from these fields itself; it never runs
// a command an agent wrote.
type ReproTest struct {
	Language string   `json:"language"`
	Package  string   `json:"package,omitempty"`
	Name     string   `json:"name"`
	Files    []string `json:"files"`
}

// Verification is what CSM measured, step by step, about whether the change fixes the reported failure.
type Verification struct {
	Reproduction  engrun.Reproduction `json:"reproduction"`
	ReproReason   string              `json:"reproduction_reason,omitempty"`
	Before        *sandbox.Result     `json:"before,omitempty"`
	After         *sandbox.Result     `json:"after,omitempty"`
	FailureGone   *bool               `json:"failure_gone,omitempty"`
	TestUnchanged *bool               `json:"test_unchanged,omitempty"`
	ChecksPassed  *bool               `json:"checks_passed,omitempty"`
	NewFailures   []string            `json:"new_failures,omitempty"`
	Verdict       engrun.Verdict      `json:"verdict"`
	Notes         []string            `json:"notes,omitempty"`
}

// HubLink ties a local run to its engineering run in CSM Hub.
type HubLink struct {
	URL    string `json:"url"`
	RunID  string `json:"run_id"`
	JobID  string `json:"job_id,omitempty"`
	Synced int    `json:"synced_events"`
	State  string `json:"state,omitempty"`
	// SyncedStates counts the entries of the run's StateLog Hub has accepted.
	SyncedStates int `json:"synced_states"`
}

func (b *Build) autofix() bool { return b.Kind == KindAutoFix }

// createAutoFix records a run for a signal on the mapped repository. Nothing starts before it is saved; the engine
// investigates on its next step.
func (e *buildEngine) createAutoFix(s *State, sig engrun.Signal, incidentID string, m RepoMapping) (*Build, error) {
	dir, err := cleanProjectDir(m.Repo)
	if err != nil {
		return nil, err
	}

	rs, err := inspectRepo(dir)
	if err != nil {
		return nil, err
	}

	base := orValue(m.BaseBranch, rs.Branch)

	if !validBranchName(rs.Root, base) || !branchExists(rs.Root, base) {
		return nil, fmt.Errorf("%q is not a local branch of %s", base, displayName(rs.Root))
	}

	commit, err := resolveCommit(rs.Root, base)
	if err != nil {
		return nil, err
	}

	var ready []string

	for _, c := range agentChoices(s) {
		if c.Eligible {
			ready = append(ready, c.ID)
		}
	}

	if len(ready) == 0 {
		return nil, fmt.Errorf("no coding agent is ready to investigate")
	}

	policy := normalizePolicy(BuildPolicy{MaxParallel: 1, Review: true, RunBaseline: true, AllowRemote: true, Sandbox: true, Agents: m.Agents, MaxReviewCycles: defaultReviewCycles})
	policy.Checks = detectChecks(rs.Root)

	if len(policy.Agents) == 0 {
		policy.Agents = ready
	}

	now := time.Now()
	b := &Build{
		Version:     buildVersion,
		ID:          newBuildID(now),
		Kind:        KindAutoFix,
		Signal:      &sig,
		IncidentID:  incidentID,
		Instruction: "Fix the failure reported by " + sig.Source + ": " + sig.Title,
		Title:       clean("Fix: "+sig.Title, 90),
		Repo:        rs.Root,
		RepoName:    displayName(rs.Root),
		BaseBranch:  base,
		BaseCommit:  commit,
		Status:      BuildDraft,
		Policy:      policy,
		Tasks:       []*BuildTask{},
		CreatedAt:   now,
		Verification: &Verification{
			Reproduction: engrun.ReproNotAttempted,
			Verdict:      engrun.VerdictNotRun,
		},
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	b.logState()
	e.builds[b.ID] = b
	e.save(b)
	e.event(b, "signal.received", "", fmt.Sprintf("%s %s: %s (%s, %s)", sig.Source, sig.EventType, sig.Title, sig.Severity, orDash(sig.Environment)))
	e.event(b, "created", "", fmt.Sprintf("AutoFix run created on %s (%s at %s)", b.RepoName, base, shortSHA(commit)))
	e.poke()

	return b, nil
}

func investigateInstruction(b *Build, dir string) string {
	var s strings.Builder

	fmt.Fprintf(&s, "You are investigating a production failure for CSM AutoFix in %s (a copy of %s at %s).\n\n", dir, b.RepoName, shortSHA(b.BaseCommit))
	s.WriteString(engrun.Brief(*b.Signal))
	s.WriteString("\n\nDecide whether this is a defect in this repository's code that a code change should fix. Locate the code the evidence points to and read it. Do not invent evidence: say what you checked, and if the evidence is not enough, classify it as insufficient_evidence.\n\n")
	s.WriteString("If it is a code defect, write ONE regression test that reproduces the failure: it must fail now, for the reported reason, and pass once the defect is fixed. Put it next to the code it tests and follow the repository's test conventions. Only add or change test files; do not fix the code.\n\n")
	s.WriteString("Classify it as one of: code_defect, environment (configuration or infrastructure), dependency (an external service or library), insufficient_evidence, unsafe_change (a fix would need destructive or risky changes).")

	return s.String()
}

func (e *buildEngine) startInvestigation(b *Build) {
	if b.Paused || b.Signal == nil {
		return
	}

	s, err := e.state()
	if err != nil {
		e.attention(b, err.Error(), "retry_investigation")

		return
	}

	agent, err := e.pickAgent(s, b, "", nil)
	if err != nil {
		e.attention(b, "No agent can investigate: "+err.Error(), "retry_investigation")

		return
	}

	inv := b.Investigation

	if inv == nil {
		inv = &Investigation{Worktree: e.worktreePath(b, "repro"), Branch: b.branch("repro")}
		b.Investigation = inv
	}

	if err := addWorktree(b.Repo, inv.Worktree, inv.Branch, b.BaseCommit); err != nil {
		e.attention(b, "Could not prepare the investigation worktree: "+err.Error(), "retry_investigation")

		return
	}

	if !e.setStatus(b, BuildInvestigating) {
		return
	}

	if _, err := e.launch(b, RoleInvestigate, "", agent, inv.Worktree, investigateStep, "investigation", investigateInstruction(b, inv.Worktree)); err != nil {
		e.attention(b, "Could not start the investigator: "+err.Error(), "retry_investigation")

		return
	}

	e.event(b, "investigation.started", "", providerName(agent)+" is investigating the failure in "+b.RepoName)
}

type investigationReport struct {
	Classification string   `json:"classification"`
	Summary        string   `json:"summary"`
	RootCause      string   `json:"root_cause"`
	Evidence       []string `json:"evidence"`
	Files          []string `json:"files"`
	FixPlan        string   `json:"fix_plan"`
	Confidence     string   `json:"confidence"`
	Reproduction   *struct {
		Language string   `json:"language"`
		Package  string   `json:"package"`
		Test     string   `json:"test"`
		Files    []string `json:"files"`
	} `json:"reproduction"`
}

var (
	goTestName  = regexp.MustCompile(`^Test[A-Za-z0-9_]{0,100}$`)
	goPackage   = regexp.MustCompile(`^\./[A-Za-z0-9_./-]{0,200}$|^\.$`)
	pyTestName  = regexp.MustCompile(`^test_[A-Za-z0-9_]{0,100}$`)
	testFileFor = map[string]*regexp.Regexp{
		"go":     regexp.MustCompile(`_test\.go$`),
		"python": regexp.MustCompile(`(^|/)test_[^/]+\.py$|_test\.py$`),
	}
)

// reproTest validates the investigator's test so the command CSM builds from it cannot carry anything but a test name and
// a package path inside the repository.
func reproTest(r *investigationReport) (*ReproTest, error) {
	if r.Reproduction == nil || r.Reproduction.Test == "" {
		return nil, fmt.Errorf("the investigator did not write a reproduction test")
	}

	t := &ReproTest{Language: strings.ToLower(r.Reproduction.Language), Package: strings.TrimSpace(r.Reproduction.Package), Name: strings.TrimSpace(r.Reproduction.Test)}
	pattern, ok := testFileFor[t.Language]

	if !ok {
		return nil, fmt.Errorf("CSM cannot run %q reproduction tests yet (supported: go, python)", t.Language)
	}

	for _, f := range r.Reproduction.Files {
		p := cleanPath(f)

		if p == "" || !pattern.MatchString(p) {
			return nil, fmt.Errorf("%q is not a %s test file", f, t.Language)
		}

		t.Files = append(t.Files, p)
	}

	if len(t.Files) == 0 {
		return nil, fmt.Errorf("the report names no test file")
	}

	switch t.Language {
	case "go":
		if t.Package == "" {
			t.Package = "./" + filepath.Dir(t.Files[0])

			if t.Package == "./." {
				t.Package = "."
			}
		}

		if !goTestName.MatchString(t.Name) || !goPackage.MatchString(t.Package) || strings.Contains(t.Package, "..") {
			return nil, fmt.Errorf("invalid Go test %q in %q", t.Name, t.Package)
		}
	case "python":
		if !pyTestName.MatchString(t.Name) {
			return nil, fmt.Errorf("invalid pytest test %q", t.Name)
		}
	}

	return t, nil
}

func (t ReproTest) command() []string {
	if t.Language == "python" {
		return []string{"python3", "-m", "pytest", "-q", "-p", "no:cacheprovider", t.Files[0] + "::" + t.Name}
	}

	return []string{"go", "test", "-count=1", "-run", "^" + t.Name + "$", t.Package}
}

func (e *buildEngine) finishInvestigation(b *Build, a *Attempt) {
	inv := b.Investigation

	if a.Status != AttemptCompleted {
		e.attention(b, "The investigation did not finish: "+a.Note, "retry_investigation")

		return
	}

	var rep investigationReport

	if err := readReport(a.Report, &rep); err != nil {
		e.attention(b, "The investigation report could not be read: "+err.Error(), "retry_investigation")

		return
	}

	inv.Classification = engrun.Classification(strings.ToLower(strings.TrimSpace(rep.Classification)))
	inv.Summary, inv.RootCause, inv.FixPlan, inv.Confidence = clean(rep.Summary, 2000), clean(rep.RootCause, 3000), clean(rep.FixPlan, 2000), clean(rep.Confidence, 20)
	inv.Evidence = cleanAll(rep.Evidence, 20, 400)
	inv.Source = agentLabel(a.Provider, a.Account)
	inv.Files = nil

	for _, f := range rep.Files {
		if p := cleanPath(f); p != "" && len(inv.Files) < 30 {
			inv.Files = append(inv.Files, p)
		}
	}

	if !inv.Classification.Valid() {
		e.attention(b, fmt.Sprintf("The investigator returned an unknown classification %q", rep.Classification), "retry_investigation")

		return
	}

	e.event(b, "investigation.completed", "", fmt.Sprintf("Investigation by %s: %s. %s", inv.Source, strings.ReplaceAll(string(inv.Classification), "_", " "), firstLine(inv.Summary)))

	if !inv.Classification.Actionable() {
		e.attention(b, fmt.Sprintf("The investigator classified this as %s, so no fix was attempted: %s", strings.ReplaceAll(string(inv.Classification), "_", " "), orDash(firstLine(inv.Summary))), "retry_investigation")

		return
	}

	test, err := reproTest(&rep)
	if err != nil {
		b.Verification.Reproduction, b.Verification.ReproReason = engrun.ReproNotAttempted, err.Error()
		e.attention(b, "No reproduction is possible yet: "+err.Error(), "continue_unreproduced", "retry_investigation")

		return
	}

	inv.Test = test

	if _, err := gitIn(inv.Worktree, append([]string{"add", "--"}, test.Files...)...); err != nil {
		e.attention(b, "The reproduction test files could not be staged: "+err.Error(), "retry_investigation")

		return
	}

	head, committed, err := commitStaged(inv.Worktree, fmt.Sprintf("Add a regression test reproducing %s\n\nCSM AutoFix run %s\n\nCSM-Agent: %s/%s\nCSM-Build: %s", b.Signal.Title, b.ID, a.Provider, a.Account, b.ID))
	if err != nil || !committed {
		e.attention(b, "The reproduction test could not be committed: "+orDash(errString(err)), "retry_investigation")

		return
	}

	inv.Commit = head

	for _, line := range worktreeChanges(inv.Worktree) {
		inv.IgnoredEdits = append(inv.IgnoredEdits, strings.TrimSpace(line))
	}

	if len(inv.IgnoredEdits) > 0 {
		e.event(b, "investigation.ignored_edits", "", fmt.Sprintf("The investigator changed %d file(s) besides the regression test; those changes were not used", len(inv.IgnoredEdits)))
	}

	e.setStatus(b, BuildReproducing)
	e.event(b, "reproduction.started", "", fmt.Sprintf("Running %s at %s with the regression test, in a sandbox", test.Name, shortSHA(b.BaseCommit)))
}

func errString(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

// commitStaged commits what is staged, leaving everything else in the worktree as it is.
func commitStaged(dir, message string) (string, bool, error) {
	if _, err := gitOut(dir, "diff", "--cached", "--quiet"); err == nil {
		return "", false, nil
	}

	if _, err := gitIn(dir, append(identityArgs(dir), "commit", "--no-verify", "-q", "-m", message)...); err != nil {
		return "", false, err
	}

	head, err := gitOut(dir, "rev-parse", "HEAD")

	return head, err == nil, err
}

func (e *buildEngine) sandboxSpec(b *Build, dir string, command []string, timeout time.Duration) sandbox.Spec {
	return sandbox.Spec{Dir: dir, Command: command, Image: sandbox.Image(dir), Timeout: timeout, Network: b.Policy.AllowNetwork, ReadOnly: sandbox.Caches(dir), Cache: e.cacheDir(b)}
}

// reproduce runs the regression test at the base plus the test, in the sandbox, and records only what that run showed.
func (e *buildEngine) reproduce(b *Build) {
	if e.busy(b, opReproduce) || b.Paused {
		return
	}

	inv := b.Investigation
	spec := e.sandboxSpec(b, inv.Worktree, inv.Test.command(), reproTimeout)
	test := *inv.Test
	runner := e.sandbox

	e.async(b, opReproduce, func() func(*Build) {
		res := runner.Run(context.Background(), spec)

		return func(b *Build) {
			v := b.Verification
			v.Before = &res
			v.Reproduction, v.ReproReason = classifyRepro(test, res)
			e.event(b, "reproduction.completed", "", fmt.Sprintf("Reproduction: %s (%s)", strings.ReplaceAll(string(v.Reproduction), "_", " "), v.ReproReason))

			if b.CancelAsked {
				return
			}

			if v.Reproduction != engrun.ReproReproduced {
				recovery := []string{"retry_reproduction", "continue_unreproduced", "retry_investigation"}
				e.attention(b, "The failure was not reproduced: "+v.ReproReason+". A fix now could not be verified against it.", recovery...)

				return
			}

			e.startFixTask(b)
		}
	})
}

// classifyRepro reads a test run strictly: only a run where the named test itself failed counts as reproduced.
func classifyRepro(t ReproTest, r sandbox.Result) (engrun.Reproduction, string) {
	switch {
	case sandbox.Unavailable(r):
		return engrun.ReproBlocked, r.Error
	case !r.Ran():
		return engrun.ReproBlocked, "the sandbox could not run the test: " + r.Error
	case r.TimedOut:
		return engrun.ReproInconclusive, "the test did not finish within " + reproTimeout.String()
	case r.ExitCode == 0:
		return engrun.ReproNotReproduced, t.Name + " passed on the current code"
	case testFailed(t, r.Output):
		return engrun.ReproReproduced, fmt.Sprintf("%s failed on the current code (exit %d)", t.Name, r.ExitCode)
	default:
		return engrun.ReproInconclusive, fmt.Sprintf("the test run exited %d without %s failing; it may not compile or its dependencies are missing", r.ExitCode, t.Name)
	}
}

func testFailed(t ReproTest, output string) bool {
	if t.Language == "python" {
		return strings.Contains(output, "FAILED") && strings.Contains(output, t.Name)
	}

	return strings.Contains(output, "--- FAIL: "+t.Name)
}

func (e *buildEngine) startFixTask(b *Build) {
	inv := b.Investigation
	acceptance := []string{"Existing checks still pass", "The regression test is not weakened, skipped or removed"}

	if inv.Test != nil {
		acceptance = append([]string{"The regression test " + inv.Test.Name + " passes"}, acceptance...)
	}

	base := inv.Commit

	if base == "" {
		base = b.BaseCommit
	}

	b.Tasks = []*BuildTask{{ID: fixTaskID, Kind: TaskKindImplement, Title: clean("Fix "+b.Signal.Title, 120), Objective: orValue(inv.FixPlan, "Fix the defect described by the investigation so the reported failure no longer happens."), Scope: "Change only what the fix needs.", Acceptance: acceptance, Files: inv.Files, Status: TaskReady, BaseRef: base}}

	if e.setStatus(b, BuildRunning) {
		b.StartedAt = time.Now()
		e.event(b, "fix.started", fixTaskID, "Implementing the fix in an isolated worktree")
		e.baseline(b)
	}
}

// autofixContext is what every agent in an AutoFix run learns about the incident: the evidence, the investigation as the
// investigator's analysis, and only the reproduction facts CSM recorded.
func autofixContext(b *Build) string {
	if !b.autofix() || b.Signal == nil {
		return ""
	}

	var s strings.Builder

	s.WriteString("\n\nThis run was started by an engineering signal.\n")
	s.WriteString(engrun.Brief(*b.Signal))

	if inv := b.Investigation; inv != nil && inv.Summary != "" {
		fmt.Fprintf(&s, "\n\nInvestigation by %s (an agent's analysis, not verified): %s\nSuspected root cause: %s\n", inv.Source, inv.Summary, orDash(inv.RootCause))
	}

	if v := b.Verification; v != nil {
		fmt.Fprintf(&s, "\nReproduction recorded by CSM: %s. %s.", strings.ReplaceAll(string(v.Reproduction), "_", " "), orDash(v.ReproReason))

		if inv := b.Investigation; inv != nil && inv.Test != nil {
			fmt.Fprintf(&s, " Regression test: %s in %s (%s). Make it pass; do not weaken, skip or delete it.", inv.Test.Name, strings.Join(inv.Test.Files, ", "), strings.Join(inv.Test.command(), " "))
		}

		if v.Reproduction != engrun.ReproReproduced {
			s.WriteString(" The failure has NOT been reproduced: do not claim that it was, or that your change is verified against it.")
		}
	}

	return s.String()
}

// verifyFix prepares, under the engine lock, a check to run outside it: the regression test against the integrated change
// in the sandbox, and whether the test itself changed after it reproduced the failure.
func (e *buildEngine) verifyFix(b *Build, dir string) func() func(*Verification) {
	inv := b.Investigation

	if inv == nil || inv.Test == nil {
		return func() func(*Verification) {
			return func(v *Verification) {
				v.Notes = append(v.Notes, "No regression test: the change can only be checked with the repository's own checks.")
			}
		}
	}

	spec := e.sandboxSpec(b, dir, inv.Test.command(), reproTimeout)
	files, reproCommit, runner := slicesClone(inv.Test.Files), inv.Commit, e.sandbox

	return func() func(*Verification) {
		res := runner.Run(context.Background(), spec)
		changed := gitList(dir, append([]string{"diff", "--name-only", reproCommit, "HEAD", "--"}, files...)...)

		return func(v *Verification) {
			v.After = &res
			unchanged := len(changed) == 0
			v.TestUnchanged = &unchanged

			if res.Ran() && !res.TimedOut {
				gone := res.ExitCode == 0
				v.FailureGone = &gone
			}

			if !unchanged {
				v.Notes = append(v.Notes, "The fix changed the regression test ("+strings.Join(changed, ", ")+"), so it no longer proves the original failure is gone.")
			}
		}
	}
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }

// verdict combines what was measured. Verified needs all of it: reproduced before, gone after, test untouched, checks green.
func (v *Verification) verdict() engrun.Verdict {
	checksOK := v.ChecksPassed != nil && *v.ChecksPassed && len(v.NewFailures) == 0

	switch {
	case v.After != nil && !v.After.Ran():
		return engrun.VerdictBlocked
	case v.FailureGone != nil && !*v.FailureGone, v.TestUnchanged != nil && !*v.TestUnchanged, v.ChecksPassed != nil && !checksOK:
		return engrun.VerdictFailed
	case v.Reproduction == engrun.ReproReproduced && v.FailureGone != nil && *v.FailureGone && checksOK:
		return engrun.VerdictVerified
	case checksOK:
		return engrun.VerdictTestsPassedUnconfirmed
	case v.ChecksPassed == nil && v.FailureGone == nil:
		return engrun.VerdictNotRun
	}

	return engrun.VerdictFailed
}

// applyChecks folds the repository checks into the verification: anything failing that did not fail on the base is new,
// and a check that could not run keeps the result from passing. With no checks at all there is nothing to fold in.
func (v *Verification) applyChecks(results []CheckResult) {
	passed, ran := true, false

	v.NewFailures = nil

	for _, r := range results {
		switch r.State {
		case CheckPassed:
			ran = true
		case CheckFailed:
			ran, passed = true, false
			v.NewFailures = append(v.NewFailures, r.Name)
		case CheckBaselineFailure:
			ran = true
			v.Notes = append(v.Notes, r.Name+" already failed on the base commit; it is not counted against the fix.")
		case CheckBlocked, CheckSkipped:
			ran, passed = true, false
			v.Notes = append(v.Notes, r.Name+" could not run: "+orDash(r.Reason))
		}
	}

	if !ran {
		v.ChecksPassed = nil
		v.Notes = append(v.Notes, "The repository has no detected checks; only the regression test was run.")

		return
	}

	v.ChecksPassed = &passed
}

// recordVerification completes the verification after a validation pass and sends anything that disproves the fix back
// as a blocking finding, so the bounded fix round handles it like any other failure.
func (e *buildEngine) recordVerification(b *Build, results []CheckResult, apply func(*Verification), round int) {
	v := b.Verification
	v.Notes = nil
	apply(v)
	v.applyChecks(results)
	v.Verdict = v.verdict()

	if v.FailureGone != nil && !*v.FailureGone {
		out := ""

		if v.After != nil {
			out = tail(v.After.Output, 1500)
		}

		b.Findings = append(b.Findings, Finding{ID: b.nextID("f"), Cycle: round, Severity: "critical", Title: "The regression test still fails after the fix", Explanation: out, Recommendation: "Fix the defect so " + b.Investigation.Test.Name + " passes without changing the test", Blocking: true, Status: findingOpen, Source: "CSM ran the regression test in the sandbox", TaskID: fixTaskID})
	}

	if v.TestUnchanged != nil && !*v.TestUnchanged {
		b.Findings = append(b.Findings, Finding{ID: b.nextID("f"), Cycle: round, Severity: "critical", Title: "The fix changed the regression test", Recommendation: "Restore the regression test exactly and fix the code instead", Blocking: true, Status: findingOpen, Source: "CSM compared the test with the commit that reproduced the failure", TaskID: fixTaskID})
	}

	e.event(b, "verification.completed", "", "Verification: "+verdictLabel(v.Verdict))
}

// checker returns how checks run for this build: in the sandbox for AutoFix, on the host for a developer's Build Room run.
func (e *buildEngine) checker(b *Build) func(dir string, def CheckDef, stage string) CheckResult {
	if !b.Policy.Sandbox {
		return runCheck
	}

	runner, network, cache := e.sandbox, b.Policy.AllowNetwork, e.cacheDir(b)

	return func(dir string, def CheckDef, stage string) CheckResult {
		res := CheckResult{Name: def.Name, Command: def.Command, Source: def.Source, Stage: stage, At: time.Now()}
		image := sandbox.Image(dir)

		if image == "" {
			res.State, res.Reason = CheckBlocked, "no sandbox image is known for this repository"

			return res
		}

		r := runner.Run(context.Background(), sandbox.Spec{Dir: dir, Command: def.Command, Image: image, Timeout: checkTimeout, Network: network, ReadOnly: sandbox.Caches(dir), Cache: cache})
		res.DurationMS, res.Output = r.Duration.Milliseconds(), tail(r.Output, checkOutputMax)

		switch {
		case !r.Ran():
			res.State, res.Reason = CheckBlocked, "sandbox: "+r.Error
		case r.TimedOut:
			res.State, res.Reason = CheckFailed, "timed out in the sandbox"
		case r.ExitCode == 0:
			res.State = CheckPassed
		default:
			res.State = CheckFailed
		}

		if r.Ran() {
			code := r.ExitCode
			res.ExitCode = &code
			res.Reason = strings.TrimSpace(res.Reason + " sandboxed: " + r.Limits)
		}

		return res
	}
}

func (b *Build) logState() {
	s := engrunState(b)

	if n := len(b.StateLog); n == 0 || b.StateLog[n-1] != s {
		b.StateLog = append(b.StateLog, s)
	}
}

func (e *buildEngine) cacheDir(b *Build) string {
	return filepath.Join(e.store.dir(b.ID), "sandbox-cache")
}

// engrunState maps a run's detailed status onto the shared lifecycle CSM Hub and other clients understand.
func engrunState(b *Build) engrun.State {
	switch b.Status {
	case BuildDraft:
		return engrun.StateReady
	case BuildInvestigating:
		return engrun.StateInvestigating
	case BuildReproducing:
		return engrun.StateReproducing
	case BuildPlanning:
		return engrun.StatePlanning
	case BuildAwaitingPlan:
		return engrun.StateAwaitApproval
	case BuildReady, BuildRunning:
		return engrun.StateImplementing
	case BuildIntegrating:
		return engrun.StateIntegrating
	case BuildValidating:
		return engrun.StateVerifying
	case BuildReviewing:
		return engrun.StateReviewing
	case BuildFixing:
		return engrun.StateCorrecting
	case BuildAwaitingFinal:
		return engrun.StateAwaitPR
	case BuildNeedsAttention:
		return engrun.StateAttention
	case BuildCompleted:
		if b.PR.URL != "" {
			return engrun.StatePROpened
		}

		return engrun.StateCompleted
	case BuildCancelled:
		return engrun.StateCancelled
	}

	return engrun.StateFailed
}

func autofixPRBody(b *Build) string {
	var s strings.Builder
	sig, inv, v := b.Signal, b.Investigation, b.Verification

	fmt.Fprintf(&s, "## Summary\n\nFixes a %s-severity %s error reported by %s: **%s**", sig.Severity, orValue(sig.Environment, "production"), sourceName(sig.Source), sig.Title)

	if e := sig.Evidence.Sentry; e != nil && e.Count > 0 {
		fmt.Fprintf(&s, " (%d occurrences)", e.Count)
	}

	s.WriteString(".\n\n")

	if inv != nil {
		fmt.Fprintf(&s, "## Suspected defect\n\n%s\n\n**Root cause (investigation by %s; an agent's analysis):** %s\n\n", orDash(inv.Summary), inv.Source, orDash(inv.RootCause))
	}

	s.WriteString("## Reproduction\n\n")
	fmt.Fprintf(&s, "- %s: %s\n", strings.ReplaceAll(string(v.Reproduction), "_", " "), orDash(v.ReproReason))

	if inv != nil && inv.Test != nil {
		fmt.Fprintf(&s, "- Regression test `%s` in %s, run as `%s`", inv.Test.Name, strings.Join(inv.Test.Files, ", "), strings.Join(inv.Test.command(), " "))

		if v.Before != nil && v.Before.Sandboxed {
			fmt.Fprintf(&s, " in a sandbox (%s)", v.Before.Limits)
		}

		s.WriteString("\n")
	}

	s.WriteString("\n## Verification run by CSM\n\n")
	fmt.Fprintf(&s, "**%s**\n\n", verdictLabel(v.Verdict))

	if v.FailureGone != nil {
		fmt.Fprintf(&s, "- Regression test after the fix: %s\n", map[bool]string{true: "passes", false: "still fails"}[*v.FailureGone])
	}

	if v.TestUnchanged != nil {
		fmt.Fprintf(&s, "- Regression test unchanged by the fix: %s\n", map[bool]string{true: "yes", false: "no"}[*v.TestUnchanged])
	}

	for _, n := range v.Notes {
		fmt.Fprintf(&s, "- %s\n", n)
	}

	body := prBody(b)

	if i := strings.Index(body, "## Tasks"); i >= 0 {
		s.WriteString("\n" + body[i:])
	}

	return s.String()
}

func sourceName(s string) string {
	switch s {
	case engrun.SourceSentry:
		return "Sentry"
	case engrun.SourceCI:
		return "CI"
	}

	return s
}

func verdictLabel(v engrun.Verdict) string {
	switch v {
	case engrun.VerdictVerified:
		return "Fixed and verified: the failure reproduced before the change and no longer occurs after it, under the same sandboxed conditions, and the checks pass."
	case engrun.VerdictTestsPassedUnconfirmed:
		return "Tests pass, but the original failure was not reproduced, so the fix is not confirmed against it."
	case engrun.VerdictBlocked:
		return "Verification was blocked: the sandbox could not run."
	case engrun.VerdictFailed:
		return "Verification failed."
	}

	return "Not verified yet."
}

func orValue(a, b string) string {
	if a != "" {
		return a
	}

	return b
}
