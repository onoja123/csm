package handoff

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	KindCheckpoint = "checkpoint"

	StageSaved = "saved"

	CompletenessComplete = "complete"
	CompletenessPartial  = "partial"

	OutcomeStarting = "starting"
	OutcomeComplete = "complete"
	OutcomePartial  = "partial"
	OutcomeFailed   = "failed"

	DeliveryInstruction = "opening_instruction"
	DeliveryFileOnly    = "file_only"
	DeliveryNone        = "none"

	maxNotesChars = 2000
)

// Edits are what the user wrote over the extracted context; they never replace it, so the original record survives every edit.
type Edits struct {
	Task      string    `json:"task,omitempty"`
	NextSteps []string  `json:"next_steps,omitempty"`
	Notes     string    `json:"notes,omitempty"`
	EditedAt  time.Time `json:"edited_at,omitzero"`
}

func (e Edits) Empty() bool {
	return e.Task == "" && len(e.NextSteps) == 0 && e.Notes == ""
}

func CleanEdits(e Edits) Edits {
	return Edits{
		Task:      truncate(Redact(strings.TrimSpace(e.Task)), maxRequestChars),
		NextSteps: cleanList(e.NextSteps, maxListItems, 300),
		Notes:     truncate(Redact(strings.TrimSpace(e.Notes)), maxNotesChars),
		EditedAt:  e.EditedAt,
	}
}

// Verification is what csm itself checked in the repository; nothing in it comes from an agent.
type Verification struct {
	CheckedAt    time.Time `json:"checked_at"`
	RepoExists   bool      `json:"repo_exists"`
	SameRoot     bool      `json:"same_root"`
	BranchWas    string    `json:"branch_was,omitempty"`
	BranchNow    string    `json:"branch_now,omitempty"`
	HeadWas      string    `json:"head_was,omitempty"`
	HeadNow      string    `json:"head_now,omitempty"`
	TreeChanged  bool      `json:"tree_changed"`
	MissingFiles []string  `json:"missing_files,omitempty"`
}

func (v Verification) BranchChanged() bool { return v.BranchWas != v.BranchNow }

func (v Verification) HeadChanged() bool { return v.HeadWas != "" && v.HeadWas != v.HeadNow }

func (v Verification) Clean() bool {
	return v.RepoExists && v.SameRoot && !v.BranchChanged() && !v.HeadChanged() && !v.TreeChanged && len(v.MissingFiles) == 0
}

// Problems lists each difference as a sentence for the brief and the UI.
func (v Verification) Problems() []string {
	var out []string

	switch {
	case !v.RepoExists:
		return []string{"The repository recorded in the checkpoint no longer exists."}
	case !v.SameRoot:
		out = append(out, "The current directory is a different repository from the one in the checkpoint.")
	}

	if v.BranchChanged() {
		out = append(out, "The branch changed from "+orNone(v.BranchWas)+" to "+orNone(v.BranchNow)+" since the checkpoint.")
	}

	if v.HeadChanged() {
		out = append(out, "HEAD moved from "+short(v.HeadWas)+" to "+short(v.HeadNow)+" since the checkpoint.")
	}

	if v.TreeChanged {
		out = append(out, "The working tree changed since the checkpoint.")
	}

	if n := len(v.MissingFiles); n > 0 {
		out = append(out, "Files named in the checkpoint are missing: "+strings.Join(v.MissingFiles, ", ")+".")
	}

	return out
}

func orNone(s string) string {
	if s == "" {
		return "(detached HEAD)"
	}

	return s
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}

	return sha
}

type Outcome struct {
	Status       string       `json:"status"`
	Delivery     string       `json:"delivery"`
	Started      bool         `json:"started"`
	StartedAt    time.Time    `json:"started_at,omitzero"`
	ProjectDir   string       `json:"project_dir,omitempty"`
	Verification Verification `json:"verification"`
	Problems     []string     `json:"problems,omitempty"`
}

// Settle turns what was observed into a status: complete only when the agent started with the brief in a verified repository.
func (o *Outcome) Settle() {
	switch {
	case !o.Started:
		o.Status = OutcomeFailed
	case o.Delivery == DeliveryInstruction && len(o.Problems) == 0:
		o.Status = OutcomeComplete
	default:
		o.Status = OutcomePartial
	}
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// ValidID accepts only names csm itself generates, so an ID can never step outside the handoff directory.
func ValidID(id string) bool {
	return idPattern.MatchString(id) && !strings.Contains(id, "..")
}

func NewCheckpointID(now time.Time, source Agent) string {
	p := source.Provider

	if p == "" {
		p = "repo"
	}

	return now.UTC().Format("20060102T150405Z") + "-" + p + "-checkpoint"
}

// Task returns the task to show and where it came from: the user, the session record, or nothing.
func (st State) Task() (string, string) {
	switch {
	case st.Edits != nil && st.Edits.Task != "":
		return st.Edits.Task, "user"
	case len(st.Context.UserRequests) > 0:
		return st.Context.UserRequests[len(st.Context.UserRequests)-1], "session"
	default:
		return "", ""
	}
}

func (st State) Steps() []string {
	if st.Edits != nil && len(st.Edits.NextSteps) > 0 {
		return st.Edits.NextSteps
	}

	return st.Context.NextSteps
}

func (st *State) SetCompleteness() {
	task, _ := st.Task()

	if task != "" && st.Git.Root != "" {
		st.Completeness = CompletenessComplete
	} else {
		st.Completeness = CompletenessPartial
	}
}

// Verify compares the repository now with the one recorded in st. It reads only git metadata and file existence.
func Verify(st State, cwd string, now time.Time) Verification {
	v := Verification{CheckedAt: now, BranchWas: st.Git.Branch, HeadWas: st.Git.Head}
	root := st.Git.Root

	if root == "" {
		root = st.Git.WorkingDir
	}

	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return v
	}

	v.RepoExists = true
	cur := snapshot(cwd)
	v.SameRoot = cur.Root == st.Git.Root
	v.BranchNow, v.HeadNow = cur.Branch, cur.Head
	v.TreeChanged = st.Git.Root != "" && strings.Join(comparable(cur.StatusFull), "\n") != strings.Join(comparable(st.Git.statusKey()), "\n")

	base := cur.Root

	if base == "" {
		base = cwd
	}

	for _, f := range append(append([]string{}, st.Context.FilesModified...), st.Context.FilesCreated...) {
		path := f

		if !filepath.IsAbs(path) {
			path = filepath.Join(base, f)
		}

		if rel, err := filepath.Rel(base, path); err != nil || strings.HasPrefix(rel, "..") {
			continue
		}

		if _, err := os.Stat(path); err != nil {
			v.MissingFiles = append(v.MissingFiles, f)
		}
	}

	return v
}

// statusKey is the recorded status; StatusFull only exists before the state is written, Status after.
func (g Git) statusKey() []string {
	if len(g.StatusFull) > 0 {
		return g.StatusFull
	}

	return g.Status
}

// comparable puts a fresh and a stored status in the same redacted, bounded form.
func comparable(lines []string) []string {
	if len(lines) > maxStatusLines {
		lines = lines[:maxStatusLines]
	}

	return redactAll(lines)
}

func snapshot(cwd string) Git {
	g := Git{WorkingDir: cwd}
	root, err := git(cwd, "rev-parse", "--show-toplevel")

	if err != nil {
		return g
	}

	g.Root = root
	g.Branch, _ = git(cwd, "branch", "--show-current")
	g.Head, _ = git(cwd, "rev-parse", "HEAD")
	g.StatusFull, _ = gitLines(cwd, "status", "--short", "--untracked-files=all")

	return g
}

// LatestCheckpoint finds the newest saved checkpoint, optionally only one taken from the given agent session.
func LatestCheckpoint(root, sessionID string) (State, string, error) {
	var best State
	bestDir := ""

	for _, e := range readDirs(root) {
		dir := filepath.Join(root, e)
		st, err := Load(dir)

		if err != nil || st.Kind != KindCheckpoint || (sessionID != "" && st.Source.SessionID != sessionID) || st.CreatedAt.Before(best.CreatedAt) {
			continue
		}

		best, bestDir = st, dir
	}

	if bestDir == "" {
		return State{}, "", ErrNone
	}

	return best, bestDir, nil
}

func readDirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var out []string

	for _, e := range entries {
		if e.IsDir() && ValidID(e.Name()) {
			out = append(out, e.Name())
		}
	}

	return out
}
