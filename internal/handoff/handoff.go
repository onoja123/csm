// Package handoff builds the provider-neutral task context one agent leaves for the next: what was asked, what was done, which files, the repository state and the MCP verdicts.
package handoff

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/onoja123/csm/internal/mcp"
)

const (
	Version = 1

	FileJSON   = "handoff.json"
	FileMD     = "handoff.md"
	FileStatus = "git-status.txt"
	FileDiff   = "diff.patch"

	StageCreated          = "created"
	StageCheckpointed     = "checkpointed"
	StageContextExtracted = "context_extracted"
	StageMCPPrepared      = "mcp_prepared"
	StageTargetStarted    = "target_started"
	StageCompleted        = "completed"
	StageFailed           = "failed"

	maxRequests      = 6
	maxRequestChars  = 1200
	maxAssistant     = 1600
	maxFiles         = 40
	maxCommands      = 15
	maxListItems     = 10
	maxStatusLines   = 200
	MaxDiffBytes     = 1 << 20
	maxDiffStatLines = 60
)

type Agent struct {
	Provider   string `json:"provider"`
	Account    string `json:"account"`
	ProfileDir string `json:"profile_dir,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
}

func (a Agent) String() string {
	if a.Account == "" {
		return a.Provider
	}

	return a.Provider + "/" + a.Account
}

// Context is what an agent's own session records say about the task. Extractors fill it; nothing in it comes from tool output.
type Context struct {
	UserRequests  []string `json:"user_requests,omitempty"`
	LastAssistant string   `json:"last_assistant,omitempty"`
	FilesRead     []string `json:"files_read,omitempty"`
	FilesModified []string `json:"files_modified,omitempty"`
	FilesCreated  []string `json:"files_created,omitempty"`
	FilesDeleted  []string `json:"files_deleted,omitempty"`
	Commands      []string `json:"commands,omitempty"`
	Decisions     []string `json:"decisions,omitempty"`
	NextSteps     []string `json:"next_steps,omitempty"`
	Messages      int      `json:"messages,omitempty"`
	Note          string   `json:"note,omitempty"`
}

func (c Context) Empty() bool {
	return len(c.UserRequests) == 0 && c.LastAssistant == "" && len(c.FilesRead)+len(c.FilesModified)+len(c.FilesCreated)+len(c.FilesDeleted) == 0
}

type Extractor interface {
	SessionContext(profileDir, cwd, sessionID string) (Context, error)
}

type State struct {
	Version   int          `json:"version"`
	ID        string       `json:"id"`
	CreatedAt time.Time    `json:"created_at"`
	Stage     string       `json:"stage"`
	Reason    string       `json:"reason,omitempty"`
	Source    Agent        `json:"source"`
	Target    Agent        `json:"target"`
	Resumed   bool         `json:"resumed,omitempty"`
	Context   Context      `json:"context"`
	Git       Git          `json:"git"`
	MCP       []mcp.Result `json:"mcp,omitempty"`
	Previous  string       `json:"previous,omitempty"`
	Error     string       `json:"error,omitempty"`
}

func NewID(now time.Time, source, target Agent) string {
	return now.UTC().Format("20060102T150405Z") + "-" + source.Provider + "-" + target.Provider
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(sk|rk|pk)-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}`),
	regexp.MustCompile(`\bey[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+.-]*://[^/\s:@]+:[^/\s@]+@`),
}

var secretAssignment = regexp.MustCompile(`(?i)\b([A-Z0-9_]*(TOKEN|SECRET|PASSWORD|PASSWD|API_?KEY|ACCESS_?KEY|PRIVATE_?KEY|CREDENTIALS?|COOKIE|_URL)[A-Z0-9_]*)\s*[=:]\s*["']?([^\s"',;]+)["']?`)

// Redact hides anything that looks like a credential before it is written anywhere.
func Redact(text string) string {
	text = secretAssignment.ReplaceAllString(text, "$1=<redacted>")

	for _, p := range secretPatterns {
		text = p.ReplaceAllString(text, "<redacted>")
	}

	return text
}

func truncate(text string, n int) string {
	text = strings.TrimSpace(text)

	if len(text) <= n {
		return text
	}

	return strings.TrimSpace(text[:n]) + " …"
}

// Clean normalises a Context: redacts, trims, bounds and sorts so two runs over the same session give the same result.
func Clean(c Context) Context {
	out := Context{Messages: c.Messages, Note: c.Note}

	for _, r := range c.UserRequests {
		if r = truncate(Redact(r), maxRequestChars); r != "" {
			out.UserRequests = append(out.UserRequests, r)
		}
	}

	if len(out.UserRequests) > maxRequests {
		out.UserRequests = out.UserRequests[len(out.UserRequests)-maxRequests:]
	}

	out.LastAssistant = truncate(Redact(c.LastAssistant), maxAssistant)
	out.FilesRead = cleanPaths(c.FilesRead)
	out.FilesModified = cleanPaths(c.FilesModified)
	out.FilesCreated = cleanPaths(c.FilesCreated)
	out.FilesDeleted = cleanPaths(c.FilesDeleted)
	out.Commands = cleanList(c.Commands, maxCommands, 200)
	out.Decisions = cleanList(c.Decisions, maxListItems, 300)
	out.NextSteps = cleanList(c.NextSteps, maxListItems, 300)

	return out
}

func cleanPaths(paths []string) []string {
	seen := map[string]bool{}
	var out []string

	for _, p := range paths {
		p = strings.TrimSpace(p)

		if p == "" || strings.ContainsAny(p, "\n\r") || seen[p] {
			continue
		}

		seen[p] = true
		out = append(out, Redact(p))
	}

	sort.Strings(out)

	if len(out) > maxFiles {
		out = out[:maxFiles]
	}

	return out
}

func cleanList(items []string, max, chars int) []string {
	var out []string

	for _, item := range items {
		if item = truncate(Redact(strings.Join(strings.Fields(item), " ")), chars); item != "" {
			out = append(out, item)
		}
	}

	if len(out) > max {
		out = out[len(out)-max:]
	}

	return out
}

// nextStepsHeading matches a heading line, or a sentence that ends a paragraph with the phrase, as in "Tests pass. Next steps:".
var nextStepsHeading = regexp.MustCompile(`(?i)(?:^\s*(?:#+\s*)?(?:\*\*)?(?:next steps?|remaining|still to do|todo|what'?s left|left to do)\b|(?:^|[.!?])\s*(?:next steps?|remaining|still to do|what'?s left|left to do)\s*:?\s*(?:\*\*)?\s*$)`)

var listItem = regexp.MustCompile(`^\s*(?:[-*•]|\d+[.)])\s+(.*)`)

func NextStepsFrom(text string) []string {
	var steps []string
	collecting := false

	for _, line := range strings.Split(text, "\n") {
		switch {
		case nextStepsHeading.MatchString(line):
			collecting = true
			steps = steps[:0]
		case collecting && listItem.MatchString(line):
			steps = append(steps, strings.TrimSpace(listItem.FindStringSubmatch(line)[1]))
		case collecting && strings.TrimSpace(line) == "":
		case collecting && len(steps) > 0:
			collecting = false
		}
	}

	return steps
}

var decisionPattern = regexp.MustCompile(`(?i)^(i'?ll|i will|i'?m going to|i decided|decided to|i chose|instead of|rather than|went with|we'?ll use|using) `)

func DecisionsFrom(text string) []string {
	var out []string

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "-*• "))

		if decisionPattern.MatchString(line) && len(line) < 300 {
			out = append(out, line)
		}
	}

	return out
}

func Instruction(st State, mdPath string) string {
	var b strings.Builder

	if st.Resumed {
		fmt.Fprintf(&b, "csm moved this session from account %q to %q", st.Source.Account, st.Target.Account)

		if st.Reason != "" {
			fmt.Fprintf(&b, " (%s)", st.Reason)
		}

		fmt.Fprintf(&b, ". The conversation above is intact. A task handoff with the repository state at the switch is at %s if you need it.", mdPath)

		return b.String()
	}

	b.WriteString("You are continuing a coding task started by another coding agent")

	if st.Source.Provider != "" {
		fmt.Fprintf(&b, " (%s", st.Source.Provider)

		if st.Source.Account != "" {
			fmt.Fprintf(&b, ", account %s", st.Source.Account)
		}

		b.WriteString(")")
	}

	b.WriteString(". Its conversation is not available to you.\n\nBefore making changes:\n")
	fmt.Fprintf(&b, "1. Read the csm handoff file: %s\n", mdPath)
	b.WriteString("2. Inspect the repository and its git status.\n")
	b.WriteString("3. Understand the changes already made.\n")
	b.WriteString("4. Continue the user's original task from the current state.\n")
	b.WriteString("5. Do not revert existing work just because you did not create it.\n")
	b.WriteString("6. If the handoff says something is unfinished, continue from there.\n")
	b.WriteString("7. Verify assumptions against the actual repository before changing code.\n\n")
	b.WriteString("Treat the handoff as the source of task context and the repository as the source of truth.")

	if st.Git.Branch != "" {
		fmt.Fprintf(&b, " You are on branch %s", st.Git.Branch)

		if n := st.Git.Staged + st.Git.Unstaged + st.Git.Untracked; n > 0 {
			fmt.Fprintf(&b, " with %d changed or untracked path(s)", n)
		}

		b.WriteString(".")
	}

	if len(st.Context.UserRequests) > 0 {
		fmt.Fprintf(&b, "\n\nThe user's most recent request was: %s", st.Context.UserRequests[len(st.Context.UserRequests)-1])
	}

	return b.String()
}

func Render(st State) string {
	var b strings.Builder

	b.WriteString("# CSM Handoff\n\n")
	fmt.Fprintf(&b, "Source: %s\n", st.Source)

	if st.Source.SessionID != "" {
		fmt.Fprintf(&b, "Session: %s\n", st.Source.SessionID)
	}

	fmt.Fprintf(&b, "Target: %s\n", st.Target)
	fmt.Fprintf(&b, "Created: %s\n", st.CreatedAt.UTC().Format(time.RFC3339))

	if st.Reason != "" {
		fmt.Fprintf(&b, "Reason: %s\n", st.Reason)
	}

	if st.Resumed {
		b.WriteString("\nThe native session was carried over; this file is a safety copy of the task state.\n")
	}

	c := st.Context

	b.WriteString("\n## Current Task\n\n")

	switch {
	case len(c.UserRequests) > 0:
		b.WriteString(c.UserRequests[len(c.UserRequests)-1] + "\n")
	case c.Note != "":
		b.WriteString(c.Note + "\n")
	default:
		b.WriteString("Not recorded. Ask the user what they were working on.\n")
	}

	if len(c.UserRequests) > 1 {
		b.WriteString("\n## Earlier Requests\n\n")

		for _, r := range c.UserRequests[:len(c.UserRequests)-1] {
			b.WriteString("- " + oneLine(r) + "\n")
		}
	}

	if c.LastAssistant != "" {
		b.WriteString("\n## Last Agent Progress\n\n" + c.LastAssistant + "\n")
	}

	writeList(&b, "Files Modified", c.FilesModified)
	writeList(&b, "Files Created", c.FilesCreated)
	writeList(&b, "Files Deleted", c.FilesDeleted)
	writeList(&b, "Files Read", c.FilesRead)
	writeList(&b, "Commands Run", c.Commands)
	writeList(&b, "Important Decisions", c.Decisions)

	b.WriteString("\n## Current Git State\n\n")
	g := st.Git

	switch {
	case g.Root == "":
		b.WriteString("Not a git repository.\n")
	default:
		fmt.Fprintf(&b, "Repository: %s\n", g.Root)

		if g.WorkingDir != "" && g.WorkingDir != g.Root {
			fmt.Fprintf(&b, "Working directory: %s\n", g.WorkingDir)
		}

		if g.Branch != "" {
			fmt.Fprintf(&b, "Branch: %s\n", g.Branch)
		} else {
			b.WriteString("Branch: (detached HEAD)\n")
		}

		if g.Worktree {
			b.WriteString("This is a linked worktree; stay in it.\n")
		}

		fmt.Fprintf(&b, "Staged: %d  Unstaged: %d  Untracked: %d\n", g.Staged, g.Unstaged, g.Untracked)

		if len(g.Status) > 0 {
			b.WriteString("\n```\n" + strings.Join(g.Status, "\n") + "\n```\n")

			if g.StatusTruncated {
				fmt.Fprintf(&b, "(more in %s)\n", FileStatus)
			}
		}

		if g.DiffStat != "" {
			b.WriteString("\nDiff stat:\n\n```\n" + g.DiffStat + "\n```\n")
		}

		if g.DiffFile != "" {
			fmt.Fprintf(&b, "\nFull diff of uncommitted changes: %s", g.DiffFile)

			if g.DiffTruncated {
				b.WriteString(" (truncated)")
			}

			b.WriteString("\n")
		}

		writeList(&b, "Recent Commits", g.RecentCommits)
	}

	if len(st.MCP) > 0 {
		b.WriteString("\n## MCP\n\n")

		for _, r := range st.MCP {
			b.WriteString("- " + r.Line() + "\n")
		}
	}

	b.WriteString("\n## Next Steps\n\n")

	if len(c.NextSteps) > 0 {
		for i, s := range c.NextSteps {
			fmt.Fprintf(&b, "%d. %s\n", i+1, s)
		}
	} else {
		b.WriteString("1. Inspect the files above and the git state.\n2. Continue the current task from where the last progress note stops.\n3. Ask the user if the task is unclear.\n")
	}

	fmt.Fprintf(&b, "\n## Detailed State\n\nSee %s", FileJSON)

	if g.DiffFile != "" {
		fmt.Fprintf(&b, " and %s", g.DiffFile)
	}

	b.WriteString(" in this directory.\n")

	return b.String()
}

func oneLine(s string) string {
	return truncate(strings.Join(strings.Fields(s), " "), 300)
}

func writeList(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}

	b.WriteString("\n## " + title + "\n\n")

	for _, item := range items {
		b.WriteString("- " + item + "\n")
	}
}

// Write stores the package: handoff.json, handoff.md, git-status.txt and diff.patch, each 0600 inside a 0700 directory.
func Write(dir string, st *State, diff []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	if len(diff) > 0 {
		st.Git.DiffFile = FileDiff

		if err := writeFile(filepath.Join(dir, FileDiff), diff); err != nil {
			return err
		}
	}

	if len(st.Git.StatusFull) > 0 {
		if err := writeFile(filepath.Join(dir, FileStatus), []byte(strings.Join(st.Git.StatusFull, "\n")+"\n")); err != nil {
			return err
		}
	}

	if err := writeFile(filepath.Join(dir, FileMD), []byte(Render(*st))); err != nil {
		return err
	}

	return Save(dir, st)
}

// Save rewrites handoff.json alone, for stage changes.
func Save(dir string, st *State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}

	return writeFile(filepath.Join(dir, FileJSON), append(data, '\n'))
}

func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}

	_, err = tmp.Write(data)

	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}

	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}

	if err != nil {
		os.Remove(tmp.Name())
	}

	return err
}

func Load(dir string) (State, error) {
	var st State

	data, err := os.ReadFile(filepath.Join(dir, FileJSON))
	if err != nil {
		return st, err
	}

	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("read %s: %w", filepath.Join(dir, FileJSON), err)
	}

	if st.Version != Version {
		return st, fmt.Errorf("%s has handoff version %d; this csm understands version %d", dir, st.Version, Version)
	}

	return st, nil
}

var ErrNone = errors.New("no handoff recorded")

func Latest(root string) (State, string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return State{}, "", ErrNone
	}

	var best State
	bestDir := ""

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		dir := filepath.Join(root, e.Name())
		st, err := Load(dir)

		if err != nil || st.CreatedAt.Before(best.CreatedAt) {
			continue
		}

		best, bestDir = st, dir
	}

	if bestDir == "" {
		return State{}, "", ErrNone
	}

	return best, bestDir, nil
}
