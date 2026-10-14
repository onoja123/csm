package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/onoja123/csm/internal/handoff"
)

const (
	receiptFile    = "receipt.json"
	receiptVersion = 1
	receiptOutput  = 1500
)

// Evidence statuses for one check, as the receipt reports them.
const (
	EvidencePassed       = "passed"
	EvidenceFailed       = "failed"
	EvidenceTimedOut     = "timed_out"
	EvidencePreExisting  = "pre_existing_failure"
	EvidenceSkipped      = "skipped"
	EvidenceUnavailable  = "unavailable"
	EvidenceInconclusive = "inconclusive"
)

// Statuses for an agent's claim, judged only against what CSM ran itself.
const (
	ClaimConfirmed    = "confirmed"
	ClaimContradicted = "contradicted"
	ClaimUnverifiable = "unverifiable"
)

// Overall outcomes, from the checks on the verified revision alone.
const (
	OutcomeVerified   = "verified"
	OutcomeFailed     = "failed"
	OutcomePartial    = "partially_verified"
	OutcomeUnverified = "unverified"
)

// Receipt is the record of what CSM did and measured for one run. It is derived from the run, never edited by an agent,
// and states a check as passed only when CSM ran the command and saw it exit 0.
type Receipt struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	BuildID     string    `json:"build_id"`
	Kind        string    `json:"kind,omitempty"`
	Title       string    `json:"title"`
	Instruction string    `json:"instruction"`
	Status      string    `json:"status"`
	// Final means the run reached a terminal state; Partial means some evidence is missing, which a final run can also be.
	Final          bool     `json:"final"`
	Partial        bool     `json:"partial"`
	PartialReasons []string `json:"partial_reasons,omitempty"`

	Repo       string `json:"repo"`
	RepoName   string `json:"repo_name"`
	BaseBranch string `json:"base_branch,omitempty"`
	BaseCommit string `json:"base_commit,omitempty"`

	StartedAt   time.Time `json:"started_at,omitzero"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
	DurationMS  int64     `json:"duration_ms,omitempty"`

	Outcome    string          `json:"outcome"`
	Headline   string          `json:"headline"`
	Revision   ReceiptRevision `json:"revision"`
	Checks     []ReceiptCheck  `json:"checks"`
	Baseline   []ReceiptCheck  `json:"baseline,omitempty"`
	Claims     []ReceiptClaim  `json:"claims"`
	Agents     []ReceiptAgent  `json:"agents"`
	Review     ReceiptReview   `json:"review"`
	Acceptance ReceiptAccept   `json:"acceptance"`
	AutoFix    *ReceiptAutoFix `json:"autofix,omitempty"`
	PR         ReceiptPR       `json:"pull_request"`
	Drift      *ReceiptDrift   `json:"drift,omitempty"`

	Limitations []string `json:"limitations"`
}

type ReceiptRevision struct {
	Branch string `json:"branch,omitempty"`
	// Commit is the revision every final check ran on; empty when none ran, or the run predates per-check commits.
	Commit string `json:"commit,omitempty"`
	// Head is the branch's head as the run last recorded it; it differs from Commit when work landed after the checks.
	Head         string   `json:"head,omitempty"`
	DirtyAtCheck bool     `json:"dirty_at_check,omitempty"`
	Files        []string `json:"files,omitempty"`
	DiffStat     string   `json:"diff_stat,omitempty"`
	Mixed        bool     `json:"mixed,omitempty"`
}

type ReceiptCheck struct {
	Name       string      `json:"name"`
	Command    string      `json:"command"`
	Source     string      `json:"source,omitempty"`
	Stage      string      `json:"stage"`
	Status     string      `json:"status"`
	ExitCode   *int        `json:"exit_code,omitempty"`
	DurationMS int64       `json:"duration_ms"`
	Commit     string      `json:"commit,omitempty"`
	Reason     string      `json:"reason,omitempty"`
	Tests      *TestCounts `json:"tests,omitempty"`
	Output     string      `json:"output,omitempty"`
	At         time.Time   `json:"at,omitzero"`
}

// TestCounts are present only when the runner's own summary could be read in full; otherwise the receipt reports no count.
type TestCounts struct {
	Unit    string `json:"unit"`
	Passed  int    `json:"passed"`
	Failed  int    `json:"failed"`
	Skipped int    `json:"skipped,omitempty"`
	Total   int    `json:"total"`
	From    string `json:"from"`
}

type ReceiptClaim struct {
	TaskID   string `json:"task_id,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Kind     string `json:"kind"`
	Claim    string `json:"claim"`
	Status   string `json:"status"`
	Evidence string `json:"evidence"`
}

type ReceiptAgent struct {
	Role     string        `json:"role"`
	TaskID   string        `json:"task_id,omitempty"`
	Agent    string        `json:"agent"`
	Status   string        `json:"status"`
	Started  time.Time     `json:"started_at,omitzero"`
	Ended    time.Time     `json:"ended_at,omitzero"`
	Note     string        `json:"note,omitempty"`
	Switches []AgentSwitch `json:"switches,omitempty"`
}

type ReceiptReview struct {
	Enabled          bool                 `json:"enabled"`
	Rounds           []ReceiptReviewRound `json:"rounds,omitempty"`
	Independent      bool                 `json:"independent"`
	OpenBlocking     int                  `json:"open_blocking"`
	OpenNonBlocking  int                  `json:"open_non_blocking"`
	FixRequested     int                  `json:"fix_requested"`
	Rechecked        int                  `json:"rechecked"`
	AcceptedByUser   int                  `json:"accepted_by_user"`
	CheckFindings    int                  `json:"check_findings"`
	FindingSummaries []string             `json:"open_findings,omitempty"`
}

type ReceiptReviewRound struct {
	Cycle       int    `json:"cycle"`
	Agent       string `json:"agent"`
	Independent bool   `json:"independent"`
	Verdict     string `json:"verdict,omitempty"`
	Status      string `json:"status"`
	Findings    int    `json:"findings"`
	Commit      string `json:"commit,omitempty"`
}

type ReceiptAccept struct {
	Passed  bool     `json:"passed"`
	Reasons []string `json:"reasons"`
}

type ReceiptAutoFix struct {
	Verdict      string `json:"verdict"`
	Reproduced   string `json:"reproduction"`
	TestName     string `json:"test,omitempty"`
	BeforeExit   *int   `json:"before_exit,omitempty"`
	AfterExit    *int   `json:"after_exit,omitempty"`
	FailureGone  *bool  `json:"failure_gone,omitempty"`
	TestUnchange *bool  `json:"test_unchanged,omitempty"`
}

type ReceiptPR struct {
	State  string `json:"state"`
	URL    string `json:"url,omitempty"`
	Pushed bool   `json:"pushed"`
	Error  string `json:"error,omitempty"`
}

// ReceiptDrift compares the verified revision with the branch as it is now; it is measured when the receipt is read.
type ReceiptDrift struct {
	CheckedAt  time.Time `json:"checked_at"`
	Where      string    `json:"where"`
	Head       string    `json:"head,omitempty"`
	Changes    []string  `json:"uncommitted,omitempty"`
	Same       bool      `json:"same_as_verified"`
	Note       string    `json:"note"`
	Unreadable bool      `json:"unreadable,omitempty"`
}

func newReceipt(b *Build, now time.Time) *Receipt {
	r := &Receipt{
		Version: receiptVersion, GeneratedAt: now, BuildID: b.ID, Kind: b.Kind, Title: b.Title, Instruction: b.Instruction,
		Status: string(b.Status), Final: b.Status.Terminal(), Repo: b.Repo, RepoName: b.RepoName, BaseBranch: b.BaseBranch,
		BaseCommit: b.BaseCommit, StartedAt: b.StartedAt, CompletedAt: b.CompletedAt,
		Checks: []ReceiptCheck{}, Claims: []ReceiptClaim{}, Agents: []ReceiptAgent{}, Limitations: []string{},
	}

	if r.StartedAt.IsZero() {
		r.StartedAt = b.CreatedAt
	}

	if !r.CompletedAt.IsZero() && r.CompletedAt.After(r.StartedAt) {
		r.DurationMS = r.CompletedAt.Sub(r.StartedAt).Milliseconds()
	}

	r.Revision = ReceiptRevision{Branch: b.Integration.Branch, Head: b.Integration.Head, Files: b.Integration.Files, DiffStat: b.Integration.DiffStat}

	var final []CheckResult

	for _, c := range b.Checks {
		switch c.Stage {
		case "baseline":
			r.Baseline = append(r.Baseline, receiptCheck(c, ""))
		default:
			final = append(final, c)
		}
	}

	r.Revision.Commit, r.Revision.Mixed = checkedCommit(final)

	for _, c := range final {
		r.Revision.DirtyAtCheck = r.Revision.DirtyAtCheck || c.Dirty
		r.Checks = append(r.Checks, receiptCheck(c, b.Integration.Head))
	}

	r.Agents = receiptAgents(b)
	r.Claims = receiptClaims(b, r.Checks)
	r.Review = receiptReview(b)
	r.AutoFix = receiptAutoFix(b)
	r.PR = ReceiptPR{State: orValue(b.PR.State, "not_prepared"), URL: b.PR.URL, Pushed: b.PR.Pushed, Error: handoff.Redact(b.PR.Error)}
	r.Outcome = checksOutcome(r.Checks)
	r.Acceptance = acceptance(b, r)
	r.PartialReasons = partialReasons(b, r)
	r.Partial = len(r.PartialReasons) > 0
	r.Limitations = limitations(b, r, final)
	r.Headline = headline(r)

	return r
}

// checkedCommit is the single revision the final checks ran on; legacy runs recorded no revision per check.
func checkedCommit(results []CheckResult) (string, bool) {
	commit := ""

	for _, c := range results {
		if c.Commit == "" {
			continue
		}

		if commit != "" && c.Commit != commit {
			return "", true
		}

		commit = c.Commit
	}

	return commit, false
}

func receiptCheck(c CheckResult, head string) ReceiptCheck {
	rc := ReceiptCheck{
		Name: handoff.Redact(c.Name), Command: handoff.Redact(displayCommand(c)), Source: c.Source, Stage: c.Stage,
		ExitCode: c.ExitCode, DurationMS: c.DurationMS, Commit: c.Commit, Reason: handoff.Redact(c.Reason), At: c.At,
		Output: tail(handoff.Redact(c.Output), receiptOutput),
	}

	switch {
	case c.State == CheckPassed:
		rc.Status = EvidencePassed
	case c.State == CheckFailed && (c.TimedOut || strings.HasPrefix(c.Reason, "timed out")):
		rc.Status = EvidenceTimedOut
	case c.State == CheckFailed:
		rc.Status = EvidenceFailed
	case c.State == CheckBaselineFailure:
		rc.Status = EvidencePreExisting
	case c.State == CheckBlocked:
		rc.Status = EvidenceUnavailable
	case c.State == CheckSkipped, c.State == CheckNotRun:
		rc.Status = EvidenceSkipped
	default:
		rc.Status, rc.Reason = EvidenceInconclusive, strings.TrimSpace(rc.Reason+" unknown check state "+string(c.State))
	}

	if (rc.Status == EvidencePassed || rc.Status == EvidenceFailed) && c.ExitCode == nil {
		rc.Status, rc.Reason = EvidenceInconclusive, strings.TrimSpace(rc.Reason+" no exit code was recorded")
	}

	// A result on an earlier head says nothing about the branch as it is: fix work landed after this check ran.
	if c.Stage == "integration" && c.Commit != "" && head != "" && c.Commit != head && rc.Status != EvidenceSkipped && rc.Status != EvidenceUnavailable {
		rc.Reason = strings.TrimSpace(fmt.Sprintf("%s ran on %s; the branch has since moved to %s, which has not been checked", rc.Status, shortSHA(c.Commit), shortSHA(head)))
		rc.Status = EvidenceInconclusive
	}

	if rc.Status != EvidenceSkipped && rc.Status != EvidenceUnavailable {
		rc.Tests = parseTestCounts(c)
	}

	return rc
}

// displayCommand shows a custom check as the user typed it rather than through the shell that runs it.
func displayCommand(c CheckResult) string {
	if c.Source == "custom" || len(c.Command) == 0 {
		return c.Name
	}

	return strings.Join(c.Command, " ")
}

// statusPhrase is a check status as a sentence fragment.
func statusPhrase(s string) string {
	switch s {
	case EvidencePreExisting:
		return "already failing on the base commit"
	case EvidenceTimedOut:
		return "timed out"
	}

	return s
}

var (
	goPkgOK     = regexp.MustCompile(`(?m)^ok\s+\S+`)
	goPkgFail   = regexp.MustCompile(`(?m)^FAIL\s+\S+\s`)
	goPkgNone   = regexp.MustCompile(`(?m)^\?\s+\S+\s+\[no test files\]`)
	pytestSum   = regexp.MustCompile(`(?m)^=+ (.*?) in [0-9.]+s(?: \([^)]*\))? =+\s*$`)
	jestSum     = regexp.MustCompile(`(?m)^Tests:\s+(.*?)\s*(\d+) total`)
	vitestSum   = regexp.MustCompile(`(?m)^\s*Tests\s+(.*?)\s*\((\d+)\)\s*$`)
	cargoSum    = regexp.MustCompile(`(?m)^test result: \w+\. (\d+) passed; (\d+) failed; (\d+) ignored`)
	countPhrase = regexp.MustCompile(`(\d+) (passed|failed|skipped|errors?|xfailed|xpassed|todo|ignored)`)
)

// parseTestCounts reads the summary a test runner prints itself. Anything it cannot read whole, such as Go package lines
// cut off by the bounded output, gives no count rather than a guess.
func parseTestCounts(c CheckResult) *TestCounts {
	cmd := " " + strings.Join(c.Command, " ")
	out := c.Output
	truncated := strings.HasPrefix(out, "…")
	var tc *TestCounts

	switch {
	case strings.Contains(cmd, " go test"):
		if truncated {
			return nil
		}

		ok, fail, none := len(goPkgOK.FindAllString(out, -1)), len(goPkgFail.FindAllString(out, -1)), len(goPkgNone.FindAllString(out, -1))

		if ok+fail == 0 {
			return nil
		}

		tc = &TestCounts{Unit: "packages", Passed: ok, Failed: fail, Skipped: none, Total: ok + fail + none, From: "go test package lines"}
	case strings.Contains(cmd, "pytest"):
		m := pytestSum.FindAllStringSubmatch(out, -1)
		if len(m) == 0 {
			return nil
		}

		tc = phraseCounts(m[len(m)-1][1], "pytest summary")
	case strings.Contains(cmd, " cargo test"):
		if truncated {
			return nil
		}

		m := cargoSum.FindAllStringSubmatch(out, -1)
		if len(m) == 0 {
			return nil
		}

		tc = &TestCounts{Unit: "tests", From: "cargo test result lines"}

		for _, x := range m {
			p, _ := strconv.Atoi(x[1])
			f, _ := strconv.Atoi(x[2])
			i, _ := strconv.Atoi(x[3])
			tc.Passed, tc.Failed, tc.Skipped = tc.Passed+p, tc.Failed+f, tc.Skipped+i
		}

		tc.Total = tc.Passed + tc.Failed + tc.Skipped
	case strings.Contains(cmd, " test"):
		if m := jestSum.FindAllStringSubmatch(out, -1); len(m) > 0 {
			tc = phraseCounts(m[len(m)-1][1], "jest summary")
			tc.Total, _ = strconv.Atoi(m[len(m)-1][2])
		} else if m := vitestSum.FindAllStringSubmatch(out, -1); len(m) > 0 {
			tc = phraseCounts(m[len(m)-1][1], "vitest summary")
			tc.Total, _ = strconv.Atoi(m[len(m)-1][2])
		} else {
			return nil
		}
	default:
		return nil
	}

	if tc == nil || tc.Total == 0 {
		return nil
	}

	// A count that disagrees with the exit status is not trusted: a passing run cannot report failures.
	if c.State == CheckPassed && tc.Failed > 0 {
		return nil
	}

	return tc
}

func phraseCounts(s, from string) *TestCounts {
	tc := &TestCounts{Unit: "tests", From: from}

	for _, m := range countPhrase.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.Atoi(m[1])

		switch m[2] {
		case "passed", "xpassed":
			tc.Passed += n
		case "failed", "error", "errors":
			tc.Failed += n
		default:
			tc.Skipped += n
		}
	}

	tc.Total = tc.Passed + tc.Failed + tc.Skipped

	return tc
}

func receiptAgents(b *Build) []ReceiptAgent {
	out := []ReceiptAgent{}

	for _, a := range b.Attempts {
		out = append(out, ReceiptAgent{Role: a.Role, TaskID: a.TaskID, Agent: agentLabel(a.Provider, a.Account), Status: a.Status, Started: a.StartedAt, Ended: a.EndedAt, Note: handoff.Redact(a.Note), Switches: a.Switches})
	}

	return out
}

var negativeClaim = regexp.MustCompile(`(?i)\b(fail(s|ed|ing|ures?)?|errors?|broken|does not pass|didn't pass|not passing)\b`)
var zeroFailures = regexp.MustCompile(`(?i)\b(0|no|zero) (fail(ed|ures?)?|errors?)\b`)

// receiptClaims sets each thing an agent said about its work against what CSM ran. Only a matching check on the
// verified revision can confirm or contradict a claim; everything else stays unverifiable.
func receiptClaims(b *Build, checks []ReceiptCheck) []ReceiptClaim {
	out := []ReceiptClaim{}

	for _, t := range b.Tasks {
		c := t.Completion
		if c == nil {
			continue
		}

		agent := agentLabel(t.Provider, t.Account)

		judged := map[*ReceiptCheck]bool{}

		for _, claim := range c.ClaimedTests {
			judged[matchCheck(claim, checks)] = true
			out = append(out, judgeClaim(t.ID, agent, "test", claim, checks))
		}

		unmatched := 0

		for _, claim := range c.ClaimedCommands {
			m := matchCheck(claim, checks)

			if m == nil {
				unmatched++

				continue
			}

			if judged[m] {
				continue
			}

			judged[m] = true
			out = append(out, judgeClaim(t.ID, agent, "command", claim, checks))
		}

		if unmatched > 0 {
			out = append(out, ReceiptClaim{TaskID: t.ID, Agent: agent, Kind: "command", Claim: fmt.Sprintf("ran %d other command(s)", unmatched), Status: ClaimUnverifiable, Evidence: "CSM did not run these commands itself; they are listed in the task's report"})
		}

		for _, d := range c.Discrepancies {
			out = append(out, ReceiptClaim{TaskID: t.ID, Agent: agent, Kind: "files", Claim: handoff.Redact(d), Status: ClaimContradicted, Evidence: "git diff of the task branch against its base"})
		}

		if len(c.ClaimedFiles) > 0 && len(c.Discrepancies) == 0 {
			out = append(out, ReceiptClaim{TaskID: t.ID, Agent: agent, Kind: "files", Claim: fmt.Sprintf("changed %d file(s): %s", len(c.ClaimedFiles), clean(strings.Join(c.ClaimedFiles, ", "), 200)), Status: ClaimConfirmed, Evidence: "git diff of the task branch matches the reported files"})
		}

		for _, a := range c.Acceptance {
			if !a.Addressed {
				out = append(out, ReceiptClaim{TaskID: t.ID, Agent: agent, Kind: "acceptance", Claim: "did not address: " + handoff.Redact(a.Criterion), Status: ClaimUnverifiable, Evidence: "reported by the agent as not done"})

				continue
			}

			out = append(out, ReceiptClaim{TaskID: t.ID, Agent: agent, Kind: "acceptance", Claim: "addressed: " + handoff.Redact(a.Criterion), Status: ClaimUnverifiable, Evidence: "CSM does not measure acceptance criteria; the checks and the review are the evidence"})
		}

		if c.ReportMissing || c.ReportUnparseable {
			out = append(out, ReceiptClaim{TaskID: t.ID, Agent: agent, Kind: "report", Claim: "completion report", Status: ClaimUnverifiable, Evidence: "the agent's report was missing or could not be read; CSM recorded the branch's changes only"})
		}
	}

	return out
}

func judgeClaim(taskID, agent, kind, claim string, checks []ReceiptCheck) ReceiptClaim {
	rc := ReceiptClaim{TaskID: taskID, Agent: agent, Kind: kind, Claim: handoff.Redact(claim)}
	c := matchCheck(claim, checks)

	if c == nil {
		rc.Status, rc.Evidence = ClaimUnverifiable, "CSM ran no matching command; this rests on the agent's report"

		return rc
	}

	saysFailing := negativeClaim.MatchString(claim) && !zeroFailures.MatchString(claim)
	where := "on " + orValue(shortSHA(c.Commit), "the integrated branch")
	measured := fmt.Sprintf("CSM ran %s %s: %s%s", c.Command, where, statusPhrase(c.Status), exitNote(c.ExitCode))

	switch c.Status {
	case EvidencePassed:
		rc.Status = ClaimConfirmed

		if saysFailing {
			rc.Status = ClaimContradicted
		}
	case EvidenceFailed, EvidenceTimedOut, EvidencePreExisting:
		rc.Status = ClaimContradicted

		if saysFailing {
			rc.Status = ClaimConfirmed
		}
	default:
		rc.Status = ClaimUnverifiable
	}

	rc.Evidence = measured

	return rc
}

func exitNote(code *int) string {
	if code == nil {
		return ""
	}

	return fmt.Sprintf(" (exit %d)", *code)
}

// matchCheck finds the check a claim names: the check's command, its name, or the runner shorthand such as "npm test".
func matchCheck(claim string, checks []ReceiptCheck) *ReceiptCheck {
	text := strings.ToLower(claim)

	for i := range checks {
		c := &checks[i]

		if c.Stage != "integration" || c.Command == "" {
			continue
		}

		keys := []string{strings.ToLower(c.Name), strings.ToLower(c.Command)}

		if f := strings.Fields(strings.ToLower(c.Command)); len(f) >= 3 && f[1] == "run" {
			keys = append(keys, f[0]+" "+f[2])
		} else if len(f) >= 2 && f[0] != "/bin/sh" {
			keys = append(keys, f[0]+" "+f[1])
		}

		for _, k := range keys {
			if containsCommand(text, k) {
				return c
			}
		}
	}

	return nil
}

// containsCommand finds k as a whole command in text, so "go test" does not match inside "cargo test".
func containsCommand(text, k string) bool {
	for i := 0; k != "" && i <= len(text)-len(k); {
		j := strings.Index(text[i:], k)
		if j < 0 {
			return false
		}

		start, end := i+j, i+j+len(k)
		before := start == 0 || !isWordByte(text[start-1])
		after := end == len(text) || !isWordByte(text[end])

		if before && after {
			return true
		}

		i = start + 1
	}

	return false
}

func isWordByte(c byte) bool {
	return c == '_' || c == '-' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func receiptReview(b *Build) ReceiptReview {
	rv := ReceiptReview{Enabled: b.Policy.Review}

	for _, r := range b.Reviews {
		rv.Rounds = append(rv.Rounds, ReceiptReviewRound{Cycle: r.Cycle, Agent: agentLabel(r.Provider, r.Account), Independent: r.Independent, Verdict: r.Verdict, Status: r.Status, Findings: r.FindingsCount, Commit: r.Commit})
	}

	if n := len(b.Reviews); n > 0 {
		rv.Independent = b.Reviews[n-1].Independent
	}

	for _, f := range b.Findings {
		if strings.HasPrefix(f.Source, "CSM ran") {
			rv.CheckFindings++
		}

		switch f.Status {
		case findingOpen:
			if f.Blocking {
				rv.OpenBlocking++
			} else {
				rv.OpenNonBlocking++
			}

			if len(rv.FindingSummaries) < 10 {
				rv.FindingSummaries = append(rv.FindingSummaries, fmt.Sprintf("[%s%s] %s%s", f.Severity, map[bool]string{true: ", blocking", false: ""}[f.Blocking], handoff.Redact(f.Title), location(f)))
			}
		case findingAccepted:
			rv.AcceptedByUser++
		case findingFixAsked:
			rv.FixRequested++
		case findingRechecked:
			rv.Rechecked++
		}
	}

	return rv
}

func receiptAutoFix(b *Build) *ReceiptAutoFix {
	v := b.Verification
	if !b.autofix() || v == nil {
		return nil
	}

	af := &ReceiptAutoFix{Verdict: string(v.Verdict), Reproduced: string(v.Reproduction), FailureGone: v.FailureGone, TestUnchange: v.TestUnchanged}

	if b.Investigation != nil {
		af.TestName = b.Investigation.Test.Name
	}

	if v.Before != nil && v.Before.Ran() {
		code := v.Before.ExitCode
		af.BeforeExit = &code
	}

	if v.After != nil && v.After.Ran() {
		code := v.After.ExitCode
		af.AfterExit = &code
	}

	return af
}

func checksOutcome(checks []ReceiptCheck) string {
	var passed, failed, other int

	for _, c := range checks {
		switch c.Status {
		case EvidencePassed:
			passed++
		case EvidenceFailed, EvidenceTimedOut:
			failed++
		default:
			other++
		}
	}

	switch {
	case failed > 0:
		return OutcomeFailed
	case passed > 0 && other == 0:
		return OutcomeVerified
	case passed > 0:
		return OutcomePartial
	default:
		return OutcomeUnverified
	}
}

// acceptance applies the run's configured bar: every check passed on the verified revision, review ran when the
// policy asks for it, and no blocking finding is open.
func acceptance(b *Build, r *Receipt) ReceiptAccept {
	a := ReceiptAccept{Passed: true, Reasons: []string{}}
	fail := func(reason string) { a.Passed, a.Reasons = false, append(a.Reasons, reason) }

	if r.Outcome != OutcomeVerified {
		fail("checks: " + strings.ReplaceAll(r.Outcome, "_", " "))
	}

	if b.Policy.Review && len(b.Reviews) == 0 {
		fail("the review the run asked for has not run")
	}

	if r.Review.OpenBlocking > 0 {
		fail(fmt.Sprintf("%d blocking finding(s) open", r.Review.OpenBlocking))
	}

	if r.Review.AcceptedByUser > 0 {
		a.Reasons = append(a.Reasons, fmt.Sprintf("%d blocking finding(s) were accepted by the user without a fix", r.Review.AcceptedByUser))
	}

	if a.Passed {
		a.Reasons = append([]string{"every check passed on the verified revision and no blocking finding is open"}, a.Reasons...)
	}

	return a
}

func partialReasons(b *Build, r *Receipt) []string {
	var out []string

	if !r.Final && b.Status != BuildAwaitingFinal {
		out = append(out, "the run is still "+strings.ReplaceAll(string(b.Status), "_", " ")+"; evidence will be added as it is measured")
	}

	if len(r.Checks) == 0 {
		switch b.Status {
		case BuildCancelled:
			out = append(out, "the run was cancelled before CSM ran the checks")
		case BuildFailed:
			out = append(out, "the run failed before CSM ran the checks")
		default:
			if r.Final {
				out = append(out, "no checks ran on the integrated branch")
			}
		}
	}

	for _, c := range r.Checks {
		if c.Status != EvidencePassed && c.Status != EvidenceFailed && c.Status != EvidenceTimedOut {
			out = append(out, fmt.Sprintf("%s: %s", c.Command, statusPhrase(c.Status)))
		}
	}

	if b.Policy.Review && len(b.Reviews) == 0 && r.Final {
		out = append(out, "no review ran")
	}

	return out
}

func limitations(b *Build, r *Receipt, final []CheckResult) []string {
	out := []string{"Passing checks show that the repository's own commands succeeded on the verified revision; they do not show the change is free of bugs."}

	if len(final) > 0 && r.Revision.Commit == "" && !r.Revision.Mixed {
		out = append(out, "This run was recorded before CSM stored the revision each check ran on, so the checks cannot be tied to a commit.")
	}

	if r.Revision.Mixed {
		out = append(out, "The final checks ran on different revisions.")
	}

	if r.Revision.DirtyAtCheck {
		out = append(out, "The integration worktree had uncommitted changes when the checks ran, so they cover more than the committed revision.")
	}

	for _, c := range final {
		if strings.HasPrefix(c.Output, "…") {
			out = append(out, "Check output is kept bounded; the start of long output was dropped, so some test counts are not reported.")

			break
		}
	}

	if b.Policy.Sandbox {
		out = append(out, "Checks ran in a network-less sandbox; failures caused by the sandbox show as failed or unavailable.")
	}

	if len(r.Checks) > 0 && b.Policy.RunBaseline && len(r.Baseline) == 0 {
		out = append(out, "The base commit was not checked, so pre-existing failures cannot be told apart from new ones.")
	}

	if r.Review.Enabled && len(r.Review.Rounds) > 0 && !r.Review.Independent {
		out = append(out, "The last review was by an agent that also wrote part of the change.")
	}

	if !r.Review.Enabled {
		out = append(out, "Review was turned off for this run.")
	}

	out = append(out, "Agent reports are claims. Only claims matched to a check CSM ran are confirmed or contradicted.")

	return out
}

func headline(r *Receipt) string {
	counts := map[string]int{}

	for _, c := range r.Checks {
		counts[c.Status]++
	}

	parts := []string{}

	for _, st := range []string{EvidencePassed, EvidenceFailed, EvidenceTimedOut, EvidencePreExisting, EvidenceUnavailable, EvidenceSkipped, EvidenceInconclusive} {
		if n := counts[st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, statusPhrase(st)))
		}
	}

	at := ""

	if r.Revision.Commit != "" {
		at = " on " + shortSHA(r.Revision.Commit)
	}

	switch r.Outcome {
	case OutcomeVerified:
		return fmt.Sprintf("Verified%s: %d of %d checks passed", at, counts[EvidencePassed], len(r.Checks))
	case OutcomeFailed:
		return "Failed" + at + ": " + strings.Join(parts, ", ")
	case OutcomePartial:
		return "Partially verified" + at + ": " + strings.Join(parts, ", ")
	default:
		if len(r.Checks) == 0 {
			return "Unverified: CSM has not run any checks on this change"
		}

		return "Unverified: no check produced a result that holds for this revision (" + strings.Join(parts, ", ") + ")"
	}
}

// measureDrift reads the integration branch now and compares it with the revision the checks ran on.
func (r *Receipt) measureDrift(worktree string, now time.Time) {
	if r.Revision.Commit == "" {
		if len(r.Checks) > 0 {
			r.Drift = &ReceiptDrift{CheckedAt: now, Note: "not known: the checks are not tied to a revision", Unreadable: true}
		}

		return
	}

	d := &ReceiptDrift{CheckedAt: now}

	if worktree != "" {
		if _, err := os.Stat(worktree); err == nil {
			d.Where = displayPath(worktree)
			d.Head, _ = gitOut(worktree, "rev-parse", "HEAD")
			d.Changes = worktreeChanges(worktree)
		}
	}

	if d.Head == "" && r.Revision.Branch != "" && r.Repo != "" {
		d.Where = "branch " + r.Revision.Branch
		d.Head, _ = gitOut(r.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+r.Revision.Branch)
	}

	switch {
	case d.Head == "":
		d.Unreadable, d.Note = true, "the integration branch and worktree are gone, so the current code cannot be compared"
	case d.Head != r.Revision.Commit:
		d.Note = fmt.Sprintf("the branch is now at %s; the commits after %s have not been verified", shortSHA(d.Head), shortSHA(r.Revision.Commit))
	case len(d.Changes) > 0:
		d.Note = fmt.Sprintf("%d uncommitted change(s) in the worktree have not been verified", len(d.Changes))
	default:
		d.Same, d.Note = true, "the branch is still exactly the verified revision"
	}

	if len(d.Changes) > 20 {
		d.Changes = d.Changes[:20]
	}

	r.Drift = d
}

func receiptPath(store buildStore, id string) string {
	return filepath.Join(store.dir(id), receiptFile)
}

func (s buildStore) saveReceipt(b *Build) {
	if b.StartedAt.IsZero() && len(b.Checks) == 0 {
		return
	}

	writeJSON(receiptPath(s, b.ID), newReceipt(b, time.Now()))
}

var errNoReceipt = errors.New("no receipt")

// loadReceipt rebuilds the receipt from the run so older runs get one too; the stored copy is the fallback when the run
// record itself cannot be read.
func (s buildStore) loadReceipt(id string, now time.Time) (*Receipt, *Build, error) {
	if !validBuildID(id) {
		return nil, nil, fmt.Errorf("%q is not a build run id; csm build list shows them", id)
	}

	b, err := s.load(id)
	if err == nil {
		r := newReceipt(b, now)
		r.measureDrift(b.Integration.Worktree, now)

		return r, b, nil
	}

	if _, statErr := os.Stat(s.dir(id)); statErr != nil {
		return nil, nil, fmt.Errorf("no build run %q; csm build list shows the runs on this machine", id)
	}

	var stored Receipt

	data, readErr := os.ReadFile(receiptPath(s, id))
	if readErr != nil || json.Unmarshal(data, &stored) != nil || stored.BuildID != id {
		return nil, nil, fmt.Errorf("the record of run %s cannot be read (%v) and it has no stored receipt: %w", id, err, errNoReceipt)
	}

	stored.Limitations = append(stored.Limitations, "The run record could not be read, so this is the receipt as last saved at "+stored.GeneratedAt.Local().Format(time.DateTime)+"; the current branch was not compared.")
	stored.Drift = nil

	return &stored, nil, nil
}

// receiptBrief is the evidence an agent taking over needs, in a few lines; the full receipt stays on disk.
func receiptBrief(r *Receipt, workBase string) string {
	if r == nil {
		return ""
	}

	var s strings.Builder

	fmt.Fprintf(&s, "Verification evidence CSM has measured so far for run %s (full record: csm build receipt %s):\n", r.BuildID, r.BuildID)

	if len(r.Checks) == 0 {
		s.WriteString("- No checks have run on the integrated branch yet. Nothing about this change is verified.\n")
	} else {
		fmt.Fprintf(&s, "- %s\n", r.Headline)

		for _, c := range r.Checks {
			line := fmt.Sprintf("- %s: %s%s", c.Command, statusPhrase(c.Status), exitNote(c.ExitCode))

			if c.Tests != nil {
				line += fmt.Sprintf(", %d/%d %s passed", c.Tests.Passed, c.Tests.Total, c.Tests.Unit)
			}

			if c.Reason != "" && c.Status != EvidencePassed {
				line += " (" + clean(c.Reason, 160) + ")"
			}

			s.WriteString(line + "\n")
		}
	}

	if r.Review.OpenBlocking > 0 {
		fmt.Fprintf(&s, "- %d blocking finding(s) are open.\n", r.Review.OpenBlocking)
	}

	contradicted := 0

	for _, c := range r.Claims {
		if c.Status == ClaimContradicted {
			contradicted++
		}
	}

	if contradicted > 0 {
		fmt.Fprintf(&s, "- %d earlier agent claim(s) were contradicted by these measurements; rely on the checks, not on earlier reports.\n", contradicted)
	}

	switch {
	case r.Revision.Commit == "":
	case workBase == r.Revision.Commit:
		fmt.Fprintf(&s, "- Your worktree started from the verified revision %s; every change you make after it is unverified until CSM runs the checks again.\n", shortSHA(r.Revision.Commit))
	default:
		fmt.Fprintf(&s, "- This evidence is for %s on the integration branch. The code in your worktree has not been checked by CSM.\n", shortSHA(r.Revision.Commit))
	}

	return strings.TrimRight(s.String(), "\n")
}

// receiptPRSection is the verification part of a pull request description, stated only from the receipt.
func receiptPRSection(r *Receipt) string {
	var s strings.Builder

	s.WriteString("## Verification by CSM\n\n")
	fmt.Fprintf(&s, "**%s.**", r.Headline)

	if r.Revision.Commit != "" {
		fmt.Fprintf(&s, " Checks ran on `%s`.", shortSHA(r.Revision.Commit))

		if r.Revision.Head != "" && r.Revision.Head != r.Revision.Commit {
			fmt.Fprintf(&s, " The branch head `%s` is newer and was not checked.", shortSHA(r.Revision.Head))
		}
	}

	s.WriteString("\n\n")

	if len(r.Checks) > 0 {
		s.WriteString("| Check | Result | Exit | Tests |\n| --- | --- | --- | --- |\n")

		for _, c := range r.Checks {
			exit, tests := "-", "not reported"

			if c.ExitCode != nil {
				exit = strconv.Itoa(*c.ExitCode)
			}

			if c.Tests != nil {
				tests = fmt.Sprintf("%d/%d %s passed", c.Tests.Passed, c.Tests.Total, c.Tests.Unit)
			}

			result := strings.ReplaceAll(c.Status, "_", " ")

			if c.Reason != "" && c.Status != EvidencePassed {
				result += ": " + clean(strings.ReplaceAll(c.Reason, "|", "/"), 120)
			}

			fmt.Fprintf(&s, "| `%s` | %s | %s | %s |\n", strings.ReplaceAll(c.Command, "|", "\\|"), result, exit, tests)
		}

		s.WriteString("\n")
	}

	counts := map[string]int{}

	for _, c := range r.Claims {
		counts[c.Status]++
	}

	if len(r.Claims) > 0 {
		fmt.Fprintf(&s, "Agent claims against these results: %d confirmed, %d contradicted, %d unverifiable.\n\n", counts[ClaimConfirmed], counts[ClaimContradicted], counts[ClaimUnverifiable])
	}

	fmt.Fprintf(&s, "Full receipt: `csm build receipt %s`. Passing checks do not mean the change is free of bugs.\n\n", r.BuildID)

	return s.String()
}
