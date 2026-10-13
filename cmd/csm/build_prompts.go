package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/onoja123/csm/internal/handoff"
)

const maxReportBytes = 256 << 10

const reportShapeWork = `{"summary": "what you changed", "files_changed": ["path"], "commands": ["commands you ran"], "tests": ["tests you ran and their result"], "limitations": ["anything left undone"], "acceptance": [{"criterion": "text", "addressed": true, "note": "how"}]}`

const reportShapePlan = `{"summary": "one paragraph", "layers": ["modules involved"], "considerations": ["security, migration, API or compatibility notes"], "suggested_checks": ["commands"], "tasks": [{"id": "api", "title": "short title", "objective": "what the task delivers", "scope": "what is in and out of scope", "acceptance": ["criterion"], "context": "patterns and files to follow", "files": ["paths likely to change"], "depends_on": ["id of an earlier task"], "agent": "claude|codex|gemini|copilot or empty"}]}`

const reportShapeInvestigate = `{"classification": "code_defect|environment|dependency|insufficient_evidence|unsafe_change", "summary": "what is failing and why, in two or three sentences", "root_cause": "the defect, with file and line", "evidence": ["what you checked: file:line and what it shows"], "files": ["files involved"], "fix_plan": "how to fix it", "confidence": "low|medium|high", "reproduction": {"language": "go|python", "package": "./path (go only)", "test": "TestName", "files": ["path/to/x_test.go"]}}`

const reportShapeReview = `{"verdict": "approve or changes_requested", "summary": "overall assessment", "findings": [{"severity": "critical|high|medium|low|info", "file": "path", "line": 0, "title": "short", "explanation": "what is wrong", "reasoning": "how you know or how to reproduce", "recommendation": "the fix", "blocking": true}]}`

func contractText(role, report string) string {
	shape := reportShapeWork

	switch role {
	case RolePlan:
		shape = reportShapePlan
	case RoleReview:
		shape = reportShapeReview
	case RoleInvestigate:
		shape = reportShapeInvestigate
	}

	return fmt.Sprintf("CSM Build Room contract: when you have finished, write a JSON report to %s with this shape:\n%s\nWrite it only once, when the work is done; CSM reads it as your account of the work and checks the repository itself. Do not commit, push, open pull requests, switch branches, or run deployment or production migration commands; CSM records and integrates your changes.", report, shape)
}

func planInstruction(b *Build, dir string) string {
	var s strings.Builder

	fmt.Fprintf(&s, "You are the planner for a change coordinated by CSM Build Room. Do not modify any files.\n\nRequest: %s\n\n", b.Instruction)
	fmt.Fprintf(&s, "Repository: %s (a read-only copy at %s, base %s on %s).\n\n", b.RepoName, dir, shortSHA(b.BaseCommit), orDash(b.BaseBranch))
	s.WriteString("Inspect the repository, then turn the request into a small plan: the layers and modules involved, the files likely to change, existing patterns and tests to follow, and security, migration, API and compatibility considerations. ")
	fmt.Fprintf(&s, "Split the work into 1 to %d tasks that can each be implemented and reviewed on their own. Prefer fewer, meaningful tasks. Mark a task as depending on another only when it needs that task's changes; tasks without dependencies run in parallel in separate git worktrees, so do not give two parallel tasks the same files.", maxPlanTasks)

	return s.String()
}

func taskInstruction(b *Build, t *BuildTask, previous *Attempt) string {
	var s strings.Builder

	fmt.Fprintf(&s, "You are implementing one task of a larger change coordinated by CSM Build Room.\n\nOverall request: %s\n\nYour task (%s): %s\nObjective: %s\n", b.Instruction, t.ID, t.Title, t.Objective)

	if t.Scope != "" {
		fmt.Fprintf(&s, "Scope: %s\n", t.Scope)
	}

	if len(t.Acceptance) > 0 {
		s.WriteString("Acceptance criteria:\n")

		for _, a := range t.Acceptance {
			fmt.Fprintf(&s, "- %s\n", a)
		}
	}

	if t.Context != "" {
		fmt.Fprintf(&s, "Context: %s\n", t.Context)
	}

	if len(t.Files) > 0 {
		fmt.Fprintf(&s, "Files likely involved: %s\n", strings.Join(t.Files, ", "))
	}

	if t.Kind == TaskKindFix {
		s.WriteString("\nReview findings to fix:\n")

		for _, id := range t.Findings {
			for _, f := range b.Findings {
				if f.ID == id {
					fmt.Fprintf(&s, "- [%s] %s%s: %s Recommended: %s\n", f.Severity, f.Title, location(f), f.Explanation, f.Recommendation)
				}
			}
		}
	}

	fmt.Fprintf(&s, "\nWork only in %s (branch %s). Other tasks run in their own worktrees; stay within this task's scope.", t.Worktree, t.Branch)
	s.WriteString(autofixContext(b))

	if previous != nil {
		fmt.Fprintf(&s, "\n\nA previous attempt by %s stopped before finishing (%s). Its changes are still in this worktree: run git status and git diff first, keep what is correct and continue. Never reset, clean or discard them.", agentLabel(previous.Provider, previous.Account), orDash(previous.Note))
	}

	return s.String()
}

func resolveInstruction(b *Build, c Conflict, dir string) string {
	var s strings.Builder

	fmt.Fprintf(&s, "You are resolving a merge conflict for CSM Build Room in %s.\n\nOverall request: %s\n\n", dir, b.Instruction)
	fmt.Fprintf(&s, "Merging task %s conflicts with work already integrated from %s in: %s.\n", c.TaskID, strings.Join(c.Tasks, ", "), strings.Join(c.Files, ", "))

	for _, id := range append([]string{c.TaskID}, c.Tasks...) {
		if t := b.task(id); t != nil {
			fmt.Fprintf(&s, "- %s: %s. %s\n", t.ID, t.Title, t.Objective)
		}
	}

	s.WriteString("\nResolve every conflict so both tasks' intent is kept. Do not simply take one side. Remove all conflict markers and leave the files unstaged; do not commit or abort the merge.")

	return s.String()
}

func reviewInstruction(b *Build, dir string, independent bool) string {
	var s strings.Builder

	fmt.Fprintf(&s, "You are reviewing a change for CSM Build Room. This is a review only: do not modify any files.\n\nOriginal request: %s\n\n", b.Instruction)
	fmt.Fprintf(&s, "The change is in %s. See it with: git diff %s..HEAD\n\nTasks and their acceptance criteria:\n", dir, b.BaseCommit)

	for _, t := range b.Tasks {
		fmt.Fprintf(&s, "- %s (%s): %s\n", t.ID, t.Title, t.Objective)

		for _, a := range t.Acceptance {
			fmt.Fprintf(&s, "    - %s\n", a)
		}
	}

	if prev := b.previousFindings(); prev != "" {
		fmt.Fprintf(&s, "\nFindings from the previous review round, which a fix attempted to address:\n%s", prev)
	}

	s.WriteString("\nCheck the diff against the request and criteria, the repository's conventions, security, error handling, data integrity, API compatibility, tests and edge cases, regressions, and dependency or migration order. Mark a finding blocking only when it must be fixed before merging; style preferences are never blocking. Give a file and line where you can.")

	s.WriteString(autofixContext(b))

	if !independent {
		s.WriteString(" Note: no different agent was available, so the same agent that wrote part of this change is reviewing it.")
	}

	return s.String()
}

func (b *Build) previousFindings() string {
	var s strings.Builder

	for _, f := range b.Findings {
		if f.Cycle == b.Cycle-1 {
			fmt.Fprintf(&s, "- [%s] %s%s\n", f.Severity, f.Title, location(f))
		}
	}

	return s.String()
}

func location(f Finding) string {
	switch {
	case f.File != "" && f.Line > 0:
		return fmt.Sprintf(" (%s:%d)", f.File, f.Line)
	case f.File != "":
		return " (" + f.File + ")"
	}

	return ""
}

func agentLabel(providerID, account string) string {
	if providerID == "" {
		return "unassigned"
	}

	if account == "" {
		return providerName(providerID)
	}

	return providerName(providerID) + " (" + account + ")"
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}

	return sha
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}

	return s
}

var errNoReport = errors.New("no report")

func readReport(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return errNoReport
	}

	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxReportBytes+1))
	if err != nil {
		return err
	}

	if len(data) > maxReportBytes {
		return errors.New("the report is larger than 256 KB")
	}

	data = []byte(strings.TrimSpace(string(data)))

	if i := strings.IndexByte(string(data), '{'); i > 0 {
		data = data[i:]
	}

	return json.Unmarshal(data, v)
}

type workReport struct {
	Summary      string            `json:"summary"`
	FilesChanged []string          `json:"files_changed"`
	Commands     []string          `json:"commands"`
	Tests        []string          `json:"tests"`
	Limitations  []string          `json:"limitations"`
	Acceptance   []AcceptanceClaim `json:"acceptance"`
}

type planReport struct {
	Summary         string   `json:"summary"`
	Layers          []string `json:"layers"`
	Considerations  []string `json:"considerations"`
	SuggestedChecks []string `json:"suggested_checks"`
	Tasks           []struct {
		ID         string   `json:"id"`
		Title      string   `json:"title"`
		Objective  string   `json:"objective"`
		Scope      string   `json:"scope"`
		Acceptance []string `json:"acceptance"`
		Context    string   `json:"context"`
		Files      []string `json:"files"`
		DependsOn  []string `json:"depends_on"`
		Agent      string   `json:"agent"`
	} `json:"tasks"`
}

type reviewReport struct {
	Verdict  string `json:"verdict"`
	Summary  string `json:"summary"`
	Findings []struct {
		Severity       string `json:"severity"`
		File           string `json:"file"`
		Line           int    `json:"line"`
		Title          string `json:"title"`
		Explanation    string `json:"explanation"`
		Reasoning      string `json:"reasoning"`
		Recommendation string `json:"recommendation"`
		Blocking       bool   `json:"blocking"`
	} `json:"findings"`
}

// clean bounds and redacts text an agent wrote before CSM stores or shows it.
func clean(s string, n int) string {
	s = strings.TrimSpace(handoff.Redact(s))

	if len(s) > n {
		s = s[:n] + "…"
	}

	return s
}

func cleanAll(items []string, max, n int) []string {
	var out []string

	for _, it := range items {
		if c := clean(it, n); c != "" && !slices.Contains(out, c) {
			out = append(out, c)
		}

		if len(out) == max {
			break
		}
	}

	return out
}

func cleanPath(p string) string {
	p = strings.TrimSpace(strings.TrimPrefix(p, "./"))

	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "..") || len(p) > 300 {
		return ""
	}

	return p
}

func (r planReport) tasks() []*BuildTask {
	var out []*BuildTask

	for i, t := range r.Tasks {
		bt := &BuildTask{
			ID:         strings.ToLower(clean(t.ID, 40)),
			Kind:       TaskKindImplement,
			Title:      clean(t.Title, 120),
			Objective:  clean(t.Objective, 2000),
			Scope:      clean(t.Scope, 1000),
			Acceptance: cleanAll(t.Acceptance, 12, 300),
			Context:    clean(t.Context, 2000),
			DependsOn:  cleanAll(t.DependsOn, maxPlanTasks, 40),
			Provider:   strings.ToLower(clean(t.Agent, 20)),
			Status:     TaskBlocked,
			Order:      i,
		}

		for _, f := range t.Files {
			if p := cleanPath(f); p != "" && len(bt.Files) < 30 {
				bt.Files = append(bt.Files, p)
			}
		}

		out = append(out, bt)
	}

	return out
}

var severities = []string{"critical", "high", "medium", "low", "info"}

func normalizeSeverity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))

	if slices.Contains(severities, s) {
		return s
	}

	return "medium"
}
