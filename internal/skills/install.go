package skills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type InstallOptions struct {
	// Providers limits the agents the skill is offered to; empty means every agent that can use it.
	Providers []string
	Global    bool
	// Project is the absolute project folder for a project-scoped install.
	Project string
	Pin     bool
	// Replace swaps out a different installed skill with the same name; KeepBoth installs this one disabled beside it.
	Replace            bool
	KeepBoth           bool
	AcceptRisk         bool
	ApprovePermissions bool
	Pack               string
	Category           string
	Tags               []string
}

// NameConflictError means a different skill with the same name is installed; agents find skills by folder name, so
// both cannot be active for the same agent.
type NameConflictError struct {
	Existing Record
	ID       string
}

func (e *NameConflictError) Error() string {
	return fmt.Sprintf("a different skill named %q is already installed from %s.\n\nKeep it and stop here, replace it with --replace, or install this one disabled beside it with --keep-both.", e.Existing.Name, e.Existing.Source.String())
}

// RiskError means the inspection found something serious and the user has not accepted it.
type RiskError struct {
	Name       string
	Inspection Inspection
}

func (e *RiskError) Error() string {
	var lines []string

	for _, f := range e.Inspection.Findings {
		if f.Severity == SeverityHigh {
			lines = append(lines, fmt.Sprintf("  %s:%d %s", f.File, f.Line, f.Message))
		}
	}

	return fmt.Sprintf("%s was not installed: the inspection found serious problems.\n%s\n\n%s\nRead the files, then rerun with --accept-risk to install it anyway.", e.Name, strings.Join(lines, "\n"), ScanLimitation)
}

// InstalledError means another revision of the same skill is installed; changing it is an update.
type InstalledError struct {
	Existing Record
}

func (e *InstalledError) Error() string {
	return fmt.Sprintf("%s is already installed at %s; to move it to another revision run: csm skills update %s", e.Existing.Name, e.Existing.VersionLabel(), e.Existing.Name)
}

// VersionLabel is the most precise version csm knows for a record.
func (r Record) VersionLabel() string {
	if r.Revision != "" {
		return short(r.Revision)
	}

	return "content " + short(r.Hash)
}

type InstallResult struct {
	Record   Record  `json:"record"`
	Action   string  `json:"action"`
	Replaced *Record `json:"replaced,omitempty"`
}

// RequestsPermissions reports whether a skill asks an agent for permissions or automatic commands.
func RequestsPermissions(s *Skill) bool {
	_, hooks := s.Extra["hooks"]

	return len(s.AllowedTools) > 0 || hooks
}

// Install records a checked candidate and stores its canonical copy. It writes nothing where agents look; Sync does.
func (l *Library) Install(c Candidate, opts InstallOptions, now time.Time) (InstallResult, error) {
	if c.Skill == nil || c.Err != "" {
		return InstallResult{}, fmt.Errorf("%s cannot be installed: %s", c.ID, c.Err)
	}

	if c.Inspection.Status == InspectionRisky && !opts.AcceptRisk {
		return InstallResult{}, &RiskError{Name: c.Skill.Name, Inspection: c.Inspection}
	}

	var res InstallResult

	err := l.Update(func(f *LibraryFile) error {
		for i := range f.Skills {
			r := &f.Skills[i]

			if r.ID != c.ID {
				continue
			}

			if r.Hash != c.Skill.Hash {
				return &InstalledError{Existing: *r}
			}

			res.Action = "unchanged"

			if opts.Global && !r.Global {
				r.Global, res.Action = true, "scope added"
			}

			if opts.Project != "" && !contains(r.Projects, opts.Project) {
				r.Projects, res.Action = append(r.Projects, opts.Project), "scope added"
			}

			if len(opts.Providers) > 0 && len(r.Providers) > 0 {
				for _, p := range opts.Providers {
					if !contains(r.Providers, p) {
						r.Providers, res.Action = append(r.Providers, p), "providers added"
					}
				}
			}

			if opts.Pack != "" && !contains(r.Packs, opts.Pack) {
				r.Packs = append(r.Packs, opts.Pack)
			}

			r.UpdatedAt = now
			res.Record = *r

			return nil
		}

		enabled := true

		for i := 0; i < len(f.Skills); i++ {
			r := f.Skills[i]

			if r.Name != c.Skill.Name {
				continue
			}

			switch {
			case opts.Replace:
				if err := removeProjections(r); err != nil {
					return err
				}

				replaced := r
				res.Replaced = &replaced
				f.Skills = append(f.Skills[:i], f.Skills[i+1:]...)
				i--
			case opts.KeepBoth:
				enabled = false
			default:
				return &NameConflictError{Existing: r, ID: c.ID}
			}
		}

		if _, err := l.Store(c.ID, c.Skill, c.Contents, c.Modes); err != nil {
			return fmt.Errorf("store %s: %w", c.Skill.Name, err)
		}

		rec := Record{
			ID:                  c.ID,
			Name:                c.Skill.Name,
			Description:         c.Skill.Description,
			Source:              c.Source,
			Revision:            c.Revision,
			Hash:                c.Skill.Hash,
			UpstreamHash:        c.Skill.Hash,
			Pinned:              opts.Pin || commitHash.MatchString(c.Source.Ref),
			License:             c.Skill.License,
			RepoLicense:         c.RepoLicense,
			Category:            opts.Category,
			Tags:                opts.Tags,
			Enabled:             enabled,
			Providers:           opts.Providers,
			Global:              opts.Global,
			PermissionsApproved: opts.ApprovePermissions && RequestsPermissions(c.Skill),
			RiskAccepted:        opts.AcceptRisk && c.Inspection.Status == InspectionRisky,
			Skill:               *c.Skill,
			Inspection:          c.Inspection,
			Problems:            c.Problems,
			History:             []Revision{{Hash: c.Skill.Hash, Revision: c.Revision, Upstream: c.Skill.Hash, At: now, Note: "installed"}},
			Facets:              FacetsOf(c.Skill.Body),
			InstalledAt:         now,
			UpdatedAt:           now,
		}

		if opts.Project != "" {
			rec.Projects = []string{opts.Project}
		}

		if opts.Pack != "" {
			rec.Packs = []string{opts.Pack}
		}

		if rec.Category == "" {
			rec.Category = c.Skill.Metadata["category"]
		}

		if len(rec.Tags) == 0 && c.Skill.Metadata["tags"] != "" {
			rec.Tags = splitTools(c.Skill.Metadata["tags"])
		}

		f.Skills = append(f.Skills, rec)
		res.Record, res.Action = rec, "installed"

		return nil
	})

	return res, err
}

// removeProjections checks every projection first and removes them only if all are csm's and unchanged.
func removeProjections(r Record) error {
	var blocked []string

	for _, p := range r.Projections {
		if _, err := os.Lstat(p.Dir); errors.Is(err, fs.ErrNotExist) {
			continue
		}

		if _, err := CheckOwned(p.Dir, r.ID); err != nil {
			blocked = append(blocked, fmt.Sprintf("%s (%v)", p.Dir, err))
		}
	}

	if len(blocked) > 0 {
		return &OwnershipError{Name: r.Name, Dirs: blocked}
	}

	for _, p := range r.Projections {
		if err := Unproject(p.Dir, r.ID); err != nil {
			return err
		}
	}

	return nil
}

// OwnershipError lists folders csm cannot prove it may delete.
type OwnershipError struct {
	Name string
	Dirs []string
}

func (e *OwnershipError) Error() string {
	return fmt.Sprintf("csm stopped before removing %s: these copies were changed or are not csm's, so csm will not delete them:\n  %s\n\nMove your changes somewhere safe or delete those folders yourself, then retry. To remove only csm's own records and unchanged copies, add --keep-changed.", e.Name, strings.Join(e.Dirs, "\n  "))
}

type RemoveResult struct {
	Record  Record   `json:"record"`
	Removed []string `json:"removed"`
	Kept    []string `json:"kept,omitempty"`
}

// Remove uninstalls a skill: its unchanged projections, its stored revisions and its record. Folders csm cannot
// prove are its own stop the removal, unless keepChanged leaves them in place.
func (l *Library) Remove(ref string, keepChanged bool) (RemoveResult, error) {
	var res RemoveResult

	err := l.Update(func(f *LibraryFile) error {
		r, err := FindIn(f.Skills, ref)
		if err != nil {
			return err
		}

		if err := removeProjections(*r); err != nil {
			var oe *OwnershipError

			if !keepChanged || !errors.As(err, &oe) {
				return err
			}

			for _, p := range r.Projections {
				if err := Unproject(p.Dir, r.ID); err != nil {
					res.Kept = append(res.Kept, p.Dir)
				} else {
					res.Removed = append(res.Removed, p.Dir)
				}
			}
		} else {
			for _, p := range r.Projections {
				res.Removed = append(res.Removed, p.Dir)
			}
		}

		res.Record = *r

		for i := range f.Skills {
			if f.Skills[i].ID == r.ID {
				f.Skills = append(f.Skills[:i], f.Skills[i+1:]...)

				break
			}
		}

		for i := range f.Packs {
			f.Packs[i].Members = remove(f.Packs[i].Members, r.ID)
		}

		return l.RemoveRevisions(r.ID)
	})

	return res, err
}

func remove(list []string, s string) []string {
	out := list[:0]

	for _, v := range list {
		if v != s {
			out = append(out, v)
		}
	}

	return out
}

// SourceRef is the source string that fetches a record's skill again, at ref when given.
func (r Record) SourceRef(ref string) string {
	s := r.Source

	switch s.Type {
	case "github":
		out := "github.com/" + s.Repo

		if s.Path != "" {
			out += "/" + s.Path
		}

		if ref != "" {
			out += "@" + ref
		}

		return out
	case "git":
		out := s.URL

		if s.Path != "" {
			out += "//" + s.Path
		}

		if ref != "" {
			out += "@" + ref
		}

		return out
	}

	return s.Path
}

// Check reads the skill's source again: for a pinned skill it still reports what upstream has, it never changes anything.
func (f *Fetcher) Check(ctx context.Context, r Record) (*Candidate, Upstream) {
	up := Upstream{CheckedAt: time.Now()}
	ref := r.Source.Ref

	if commitHash.MatchString(ref) {
		ref = ""
	}

	src := r.SourceRef(ref)
	var dirs []string

	switch r.Source.Type {
	case "github", "git":
		dirs = []string{r.Source.Path}
	}

	cands, err := f.Fetch(ctx, src, dirs)

	if err == nil && len(cands) != 1 {
		err = fmt.Errorf("%s no longer holds exactly one skill at %q", src, r.Source.Path)
	}

	if err != nil {
		up.Error = err.Error()

		return nil, up
	}

	c := cands[0]
	up.Revision = c.Revision

	if c.Skill != nil {
		up.Hash = c.Skill.Hash
	}

	if c.Err != "" {
		up.Error = c.Err
	}

	return &c, up
}

// FileChange is one file that differs between two revisions.
type FileChange struct {
	Path   string `json:"path"`
	Change string `json:"change"`
	Diff   string `json:"diff,omitempty"`
}

// Changes lists what differs between two sets of files, with a line diff for text files.
func Changes(from, to map[string][]byte) []FileChange {
	paths := map[string]bool{}

	for p := range from {
		paths[p] = true
	}

	for p := range to {
		paths[p] = true
	}

	sorted := make([]string, 0, len(paths))

	for p := range paths {
		sorted = append(sorted, p)
	}

	sort.Strings(sorted)

	var out []FileChange

	for _, p := range sorted {
		a, inA := from[p]
		b, inB := to[p]

		switch {
		case !inA:
			out = append(out, FileChange{Path: p, Change: "added", Diff: UnifiedDiff(p, nil, b)})
		case !inB:
			out = append(out, FileChange{Path: p, Change: "removed", Diff: UnifiedDiff(p, a, nil)})
		case !bytes.Equal(a, b):
			out = append(out, FileChange{Path: p, Change: "modified", Diff: UnifiedDiff(p, a, b)})
		}
	}

	return out
}

// UpdatePlan is everything a user should see before an update is applied.
type UpdatePlan struct {
	Record    Record       `json:"record"`
	Candidate Candidate    `json:"candidate"`
	Changes   []FileChange `json:"changes"`
	// LocalChanges are projections someone edited; an update leaves them alone unless asked otherwise.
	LocalChanges []string `json:"local_changes,omitempty"`
	RiskIncrease bool     `json:"risk_increase,omitempty"`
	// NewPermissions means the new revision asks for permissions the installed one did not.
	NewPermissions bool `json:"new_permissions,omitempty"`
	UpToDate       bool `json:"up_to_date"`
}

// PlanUpdate compares the installed revision with a candidate from the same source.
func (l *Library) PlanUpdate(r Record, c Candidate) (UpdatePlan, error) {
	plan := UpdatePlan{Record: r, Candidate: c}

	if c.Skill == nil || c.Err != "" {
		return plan, fmt.Errorf("the new revision of %s is not valid: %s", r.Name, c.Err)
	}

	if c.Skill.Hash == r.UpstreamHash || c.Skill.Hash == r.Hash {
		plan.UpToDate = true

		return plan, nil
	}

	_, current, _, err := l.LoadRevision(r.ID, r.Hash)
	if err != nil {
		return plan, err
	}

	plan.Changes = Changes(current, c.Contents)
	plan.RiskIncrease = rank(c.Inspection.Status) > rank(r.Inspection.Status)
	plan.NewPermissions = RequestsPermissions(c.Skill) && !permissionsEqual(&r.Skill, c.Skill)

	for _, p := range r.Projections {
		if _, err := CheckOwned(p.Dir, r.ID); errors.Is(err, ErrModified) {
			plan.LocalChanges = append(plan.LocalChanges, p.Dir)
		}
	}

	return plan, nil
}

func rank(status string) int {
	switch status {
	case InspectionRisky:
		return 2
	case InspectionCheck:
		return 1
	}

	return 0
}

func permissionsEqual(a, b *Skill) bool {
	return strings.Join(a.AllowedTools, "\x00") == strings.Join(b.AllowedTools, "\x00") && fmt.Sprint(a.Extra["hooks"]) == fmt.Sprint(b.Extra["hooks"])
}

type UpdateOptions struct {
	AcceptRisk bool
	// Unpin allows moving a pinned skill.
	Unpin bool
}

// ApplyUpdate moves a record to the candidate's revision and keeps the previous one stored for rollback.
func (l *Library) ApplyUpdate(plan UpdatePlan, opts UpdateOptions, now time.Time) (Record, error) {
	c := plan.Candidate

	switch {
	case plan.UpToDate:
		return plan.Record, nil
	case plan.Record.Pinned && !opts.Unpin:
		return plan.Record, fmt.Errorf("%s is pinned to %s; update it with --unpin to move it", plan.Record.Name, plan.Record.VersionLabel())
	case c.Inspection.Status == InspectionRisky && !opts.AcceptRisk:
		return plan.Record, &RiskError{Name: c.Skill.Name, Inspection: c.Inspection}
	}

	var out Record

	err := l.Update(func(f *LibraryFile) error {
		for i := range f.Skills {
			r := &f.Skills[i]

			if r.ID != plan.Record.ID {
				continue
			}

			if r.Hash != plan.Record.Hash {
				return fmt.Errorf("%s changed while the update was being prepared; check again", r.Name)
			}

			if _, err := l.Store(r.ID, c.Skill, c.Contents, c.Modes); err != nil {
				return err
			}

			r.Revision, r.Hash, r.UpstreamHash = c.Revision, c.Skill.Hash, c.Skill.Hash
			r.Description, r.License, r.Skill, r.Inspection, r.Problems = c.Skill.Description, c.Skill.License, *c.Skill, c.Inspection, c.Problems
			r.Pinned = r.Pinned && !opts.Unpin
			r.RiskAccepted = c.Inspection.Status == InspectionRisky
			r.PermissionsApproved = r.PermissionsApproved && !plan.NewPermissions
			r.History = append(r.History, Revision{Hash: c.Skill.Hash, Revision: c.Revision, Upstream: c.Skill.Hash, At: now, Note: "updated"})
			r.Facets = FacetsOf(c.Skill.Body)
			r.Upstream = &Upstream{Revision: c.Revision, Hash: c.Skill.Hash, CheckedAt: now}
			r.UpdatedAt = now
			out = *r

			return nil
		}

		return fmt.Errorf("%s is no longer installed", plan.Record.Name)
	})

	return out, err
}

// Rollback returns a skill to the revision it had before its last update.
func (l *Library) Rollback(ref string, now time.Time) (Record, error) {
	var out Record

	err := l.Update(func(f *LibraryFile) error {
		r, err := FindIn(f.Skills, ref)
		if err != nil {
			return err
		}

		var prev *Revision

		for i := len(r.History) - 1; i >= 0; i-- {
			if r.History[i].Hash != r.Hash {
				prev = &r.History[i]

				break
			}
		}

		if prev == nil {
			return fmt.Errorf("%s has no earlier revision to return to", r.Name)
		}

		s, _, _, err := l.LoadRevision(r.ID, prev.Hash)
		if err != nil {
			return err
		}

		for i := range f.Skills {
			if f.Skills[i].ID != r.ID {
				continue
			}

			rec := &f.Skills[i]
			rec.Revision, rec.Hash, rec.Skill, rec.Description = prev.Revision, prev.Hash, *s, s.Description
			rec.UpstreamHash = prev.Upstream

			if rec.UpstreamHash == "" {
				rec.UpstreamHash = prev.Hash
			}

			rec.Inspection = Inspect(s, mustContents(l, rec.ID, prev.Hash))
			rec.Pinned = true
			rec.Facets = FacetsOf(s.Body)
			rec.History = append(rec.History, Revision{Hash: prev.Hash, Revision: prev.Revision, Upstream: rec.UpstreamHash, At: now, Note: "rolled back"})
			rec.UpdatedAt = now
			out = *rec
		}

		return nil
	})

	return out, err
}

func mustContents(l *Library, id, hash string) map[string][]byte {
	_, contents, _, err := l.LoadRevision(id, hash)
	if err != nil {
		return nil
	}

	return contents
}

// Adopt makes the edited copy at dir the installed revision, keeping the upstream revision it came from on record.
func (l *Library) Adopt(ref, dir string, now time.Time) (Record, error) {
	var out Record

	err := l.Update(func(f *LibraryFile) error {
		r, err := FindIn(f.Skills, ref)
		if err != nil {
			return err
		}

		owned := false

		for _, p := range r.Projections {
			if samePath(p.Dir, dir) {
				owned = true
			}
		}

		if !owned {
			return fmt.Errorf("%s is not a copy csm placed for %s", dir, r.Name)
		}

		contents, modes, err := ReadTree(dir, []string{gitignoreFile})
		if err != nil {
			return err
		}

		s, problems, err := FromFiles(r.Name, contents, modes)
		if err != nil {
			return err
		}

		if s.Name != r.Name {
			return fmt.Errorf("the edited copy renamed the skill to %q; csm keeps one name per installed skill", s.Name)
		}

		if _, err := l.Store(r.ID, s, contents, modes); err != nil {
			return err
		}

		for i := range f.Skills {
			if f.Skills[i].ID != r.ID {
				continue
			}

			rec := &f.Skills[i]
			rec.Hash, rec.Skill, rec.Description, rec.Problems = s.Hash, *s, s.Description, problems
			rec.Inspection = Inspect(s, contents)
			rec.History = append(rec.History, Revision{Hash: s.Hash, Revision: rec.Revision, Upstream: rec.UpstreamHash, At: now, Note: "kept local changes from " + dir})
			rec.Facets = FacetsOf(s.Body)
			rec.UpdatedAt = now
			out = *rec
		}

		return nil
	})

	return out, err
}

func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)

	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}

	return ra == rb
}

// Merge3 merges a local edit with an upstream change of the same file using git merge-file; conflict reports
// whether the result holds conflict markers.
func Merge3(base, local, upstream []byte) ([]byte, bool, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, false, errors.New("git is needed to merge local changes")
	}

	dir, err := os.MkdirTemp("", "csm-merge-")
	if err != nil {
		return nil, false, err
	}

	defer os.RemoveAll(dir)

	paths := []string{filepath.Join(dir, "local"), filepath.Join(dir, "base"), filepath.Join(dir, "upstream")}

	for i, data := range [][]byte{local, base, upstream} {
		if err := os.WriteFile(paths[i], data, 0o600); err != nil {
			return nil, false, err
		}
	}

	cmd := exec.Command(git, "merge-file", "-p", "-L", "your copy", "-L", "installed", "-L", "upstream", paths[0], paths[1], paths[2])
	out, err := cmd.Output()

	var exit *exec.ExitError

	switch {
	case err == nil:
		return out, false, nil
	case errors.As(err, &exit) && exit.ExitCode() > 0 && exit.ExitCode() < 128:
		return out, true, nil
	}

	return nil, false, err
}
