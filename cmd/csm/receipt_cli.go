package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var evidenceMark = map[string]string{
	EvidencePassed: "✓", EvidenceFailed: "✗", EvidenceTimedOut: "✗", EvidencePreExisting: "!",
	EvidenceSkipped: "–", EvidenceUnavailable: "–", EvidenceInconclusive: "?",
	ClaimConfirmed: "✓", ClaimContradicted: "✗", ClaimUnverifiable: "?",
}

var evidenceWord = map[string]string{
	EvidencePassed: "passed", EvidenceFailed: "failed", EvidenceTimedOut: "timed out", EvidencePreExisting: "failing on base",
	EvidenceSkipped: "skipped", EvidenceUnavailable: "unavailable", EvidenceInconclusive: "inconclusive",
}

func cmdBuildReceipt(log *Logger, home string, args []string) error {
	asJSON := slices.Contains(args, "--json")
	args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--json" })

	if len(args) != 1 {
		return errors.New("usage: csm build receipt <id> [--json]")
	}

	r, _, err := newBuildStore(home).loadReceipt(args[0], time.Now())
	if err != nil {
		return err
	}

	if asJSON {
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}

		log.Info("%s", data)

		return nil
	}

	printReceipt(log, r)

	return nil
}

func printReceipt(log *Logger, r *Receipt) {
	state := "final"

	switch {
	case r.Status == string(BuildAwaitingFinal):
		state = "awaiting your approval"
	case !r.Final:
		state = "in progress"
	}

	if r.Partial {
		state += ", partial evidence"
	}

	log.Info("Receipt for run %s (%s)", r.BuildID, state)
	log.Info("%s\n", r.Title)

	row := func(label, value string) { log.Info("%-12s %s", label, value) }

	row("Outcome", r.Headline)
	row("Status", strings.ReplaceAll(r.Status, "_", " ")+durationNote(r))
	row("Repository", fmt.Sprintf("%s (%s at %s)", displayPath(r.Repo), orDash(r.BaseBranch), shortSHA(r.BaseCommit)))

	if r.Revision.Branch != "" {
		row("Changes", fmt.Sprintf("%s, %d file(s)%s", r.Revision.Branch, len(r.Revision.Files), diffNote(r.Revision.DiffStat)))
	}

	switch {
	case r.Revision.Commit != "":
		row("Verified", r.Revision.Commit)
	case r.Revision.Mixed:
		row("Verified", "the checks ran on different revisions")
	default:
		row("Verified", "no revision (no checks have produced a result on a known commit)")
	}

	if d := r.Drift; d != nil {
		mark := "!"

		if d.Same {
			mark = "✓"
		}

		row("Current", mark+" "+d.Note)
	}

	row("Review", reviewLine(r.Review))
	row("Acceptance", acceptanceLine(r.Acceptance))
	row("Pull request", prLine(r.PR))

	if len(r.PartialReasons) > 0 {
		log.Blank()
		log.Info("Missing evidence")

		for _, p := range r.PartialReasons {
			log.Info("  - %s", p)
		}
	}

	log.Blank()
	log.Info("Checks on the integrated branch")

	if len(r.Checks) == 0 {
		log.Info("  none yet")
	}

	for _, c := range r.Checks {
		printReceiptCheck(log, c)
	}

	if len(r.Baseline) > 0 {
		log.Blank()
		log.Info("Checks on the base commit %s", shortSHA(r.BaseCommit))

		for _, c := range r.Baseline {
			printReceiptCheck(log, c)
		}
	}

	if r.AutoFix != nil {
		log.Blank()
		log.Info("AutoFix")
		log.Info("  verdict %s, reproduction %s", strings.ReplaceAll(r.AutoFix.Verdict, "_", " "), strings.ReplaceAll(orDash(r.AutoFix.Reproduced), "_", " "))
	}

	if len(r.Claims) > 0 {
		log.Blank()
		log.Info("Agent claims")

		for _, c := range r.Claims {
			log.Info("  %s %-12s %s %s: %s", evidenceMark[c.Status], c.Status, c.TaskID, c.Agent, c.Claim)
			log.Info("    %s", c.Evidence)
		}
	}

	if len(r.Agents) > 0 {
		log.Blank()
		log.Info("Agents")

		for _, a := range r.Agents {
			task := ""

			if a.TaskID != "" {
				task = " " + a.TaskID
			}

			log.Info("  %-16s %-25s %-10s %s", a.Role+task, a.Agent, a.Status, timeRange(a.Started, a.Ended))

			for _, sw := range a.Switches {
				log.Info("    handoff %s → %s (%s)%s", sw.From, sw.To, strings.ReplaceAll(orDash(sw.Reason), "_", " "), commitNote(sw.Commit))
			}
		}
	}

	if len(r.Review.FindingSummaries) > 0 {
		log.Blank()
		log.Info("Open findings")

		for _, f := range r.Review.FindingSummaries {
			log.Info("  - %s", f)
		}
	}

	var failing []ReceiptCheck

	for _, c := range r.Checks {
		if (c.Status == EvidenceFailed || c.Status == EvidenceTimedOut) && c.Output != "" {
			failing = append(failing, c)
		}
	}

	for _, c := range failing {
		log.Blank()
		log.Info("Output of %s (last lines)", c.Command)

		lines := strings.Split(strings.TrimRight(c.Output, "\n"), "\n")

		for _, l := range lines[max(0, len(lines)-12):] {
			log.Info("  │ %s", l)
		}
	}

	log.Blank()
	log.Info("Limitations")

	for _, l := range r.Limitations {
		log.Info("  - %s", l)
	}
}

func printReceiptCheck(log *Logger, c ReceiptCheck) {
	detail := []string{}

	if c.ExitCode != nil {
		detail = append(detail, fmt.Sprintf("exit %d", *c.ExitCode))
	}

	if c.DurationMS > 0 {
		detail = append(detail, fmt.Sprintf("%.1fs", float64(c.DurationMS)/1000))
	}

	if c.Tests != nil {
		detail = append(detail, fmt.Sprintf("%d/%d %s passed", c.Tests.Passed, c.Tests.Total, c.Tests.Unit))
	}

	log.Info("  %s %-16s %s", evidenceMark[c.Status], evidenceWord[c.Status], c.Command)

	if len(detail) > 0 {
		log.Info("    %s", strings.Join(detail, " · "))
	}

	if c.Reason != "" {
		log.Info("    %s", c.Reason)
	}
}

func durationNote(r *Receipt) string {
	if r.DurationMS <= 0 {
		return ""
	}

	return ", took " + (time.Duration(r.DurationMS) * time.Millisecond).Round(time.Second).String()
}

func diffNote(stat string) string {
	lines := strings.Split(strings.TrimSpace(stat), "\n")

	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return " (" + last + ")"
	}

	return ""
}

func reviewLine(rv ReceiptReview) string {
	if !rv.Enabled {
		return "turned off for this run"
	}

	if len(rv.Rounds) == 0 {
		return "not run yet"
	}

	last := rv.Rounds[len(rv.Rounds)-1]
	who := last.Agent

	if last.Independent {
		who += ", a different agent"
	} else {
		who += ", not independent"
	}

	return fmt.Sprintf("round %d by %s: %s; %d blocking open, %d accepted by you", last.Cycle, who, strings.ReplaceAll(orValue(last.Verdict, last.Status), "_", " "), rv.OpenBlocking, rv.AcceptedByUser)
}

func acceptanceLine(a ReceiptAccept) string {
	if a.Passed {
		return "met: " + strings.Join(a.Reasons, "; ")
	}

	return "not met: " + strings.Join(a.Reasons, "; ")
}

func prLine(pr ReceiptPR) string {
	switch {
	case pr.URL != "":
		return pr.URL
	case pr.Pushed:
		return "branch pushed, no pull request URL recorded"
	case pr.State == "not_prepared":
		return "not prepared"
	default:
		return strings.ReplaceAll(pr.State, "_", " ") + ", not pushed"
	}
}

func timeRange(a, b time.Time) string {
	if a.IsZero() {
		return ""
	}

	if b.IsZero() {
		return a.Local().Format("15:04:05") + " –"
	}

	return a.Local().Format("15:04:05") + " – " + b.Local().Format("15:04:05")
}

func commitNote(sha string) string {
	if sha == "" {
		return ""
	}

	return ", work committed as " + shortSHA(sha)
}
