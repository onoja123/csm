package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// State is where a task's skill is in its life; only evidence from the agent moves a skill to StateInvoked.
type State string

const (
	StateRecommended State = "recommended"
	StateSelected    State = "selected"
	StateResolved    State = "resolved"
	StateProjected   State = "projected"
	StateRequested   State = "requested"
	StateInvoked     State = "invoked"
	StateUnverified  State = "unverified"
	StateFailed      State = "failed"
	StateUnavailable State = "unavailable"
)

// Evidence is an observation, from the agent's own session record, that it loaded a skill.
type Evidence struct {
	Skill    string    `json:"skill"`
	Provider string    `json:"provider"`
	Session  string    `json:"session,omitempty"`
	Kind     string    `json:"kind"`
	At       time.Time `json:"at,omitzero"`
	Detail   string    `json:"detail,omitempty"`
}

// Placement is one agent's view of a task skill: where csm put it and how the agent can be asked to use it.
type Placement struct {
	Provider    string     `json:"provider"`
	Account     string     `json:"account,omitempty"`
	State       State      `json:"state"`
	Level       Level      `json:"level,omitempty"`
	Dir         string     `json:"dir,omitempty"`
	Invoke      string     `json:"invoke,omitempty"`
	Request     string     `json:"request,omitempty"`
	Handoff     bool       `json:"handoff,omitempty"`
	Adaptations []string   `json:"adaptations,omitempty"`
	Reasons     []string   `json:"reasons,omitempty"`
	Problem     string     `json:"problem,omitempty"`
	Evidence    []Evidence `json:"evidence,omitempty"`
	At          time.Time  `json:"at"`
}

// TaskSkill is a skill pinned to a task: the identity and revision never change after selection; placements record
// what happened with each agent that worked on the task.
type TaskSkill struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Source      string      `json:"source"`
	Revision    string      `json:"revision,omitempty"`
	Hash        string      `json:"hash"`
	License     string      `json:"license,omitempty"`
	Origin      string      `json:"origin"`
	Reason      string      `json:"reason,omitempty"`
	State       State       `json:"state"`
	Placements  []Placement `json:"placements,omitempty"`
}

// Placement returns the latest placement for an agent.
func (t TaskSkill) Placement(provider string) *Placement {
	for i := len(t.Placements) - 1; i >= 0; i-- {
		if t.Placements[i].Provider == provider {
			return &t.Placements[i]
		}
	}

	return nil
}

// Event is one step of resolution, kept in order so a task's history shows what csm did with its skills.
type Event struct {
	At       time.Time `json:"at"`
	Provider string    `json:"provider,omitempty"`
	Skill    string    `json:"skill,omitempty"`
	Kind     string    `json:"kind"`
	Detail   string    `json:"detail,omitempty"`
}

const manifestVersion = 1

// Manifest is the skill selection of one task. It is written once at selection and then only gains placements,
// evidence and events, so retries and handoffs use the same revisions.
type Manifest struct {
	Version     int         `json:"version"`
	Mode        string      `json:"mode"`
	Task        string      `json:"task,omitempty"`
	Skills      []TaskSkill `json:"skills"`
	Recommended []Choice    `json:"recommended,omitempty"`
	Skipped     []Skip      `json:"skipped,omitempty"`
	Conflicts   []Conflict  `json:"conflicts,omitempty"`
	Missing     []string    `json:"missing,omitempty"`
	// Observed is evidence of skills an agent loaded on its own that were not selected for the task.
	Observed  []Evidence `json:"observed,omitempty"`
	Events    []Event    `json:"events,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// NewManifest snapshots a resolution, pinning each selected skill to the revision installed right now.
func NewManifest(res Result, records []Record, task string, now time.Time) Manifest {
	m := Manifest{Version: manifestVersion, Mode: res.Mode, Task: oneLine(task, 300), Recommended: res.Recommended, Skipped: res.Skipped, Conflicts: res.Conflicts, Missing: res.Missing, CreatedAt: now, UpdatedAt: now}

	for _, c := range res.Selected {
		r, err := FindIn(records, c.ID)
		if err != nil {
			continue
		}

		m.Skills = append(m.Skills, TaskSkill{ID: r.ID, Name: r.Name, Description: r.Description, Source: r.Source.String(), Revision: r.Revision, Hash: r.Hash, License: r.License, Origin: c.Origin, Reason: c.Reason, State: StateSelected})
		m.Events = append(m.Events, Event{At: now, Skill: r.Name, Kind: "selected", Detail: c.Origin + ": " + c.Reason})
	}

	for _, s := range res.Skipped {
		m.Events = append(m.Events, Event{At: now, Skill: s.Name, Kind: "skipped", Detail: s.Reason})
	}

	for _, ref := range res.Missing {
		m.Events = append(m.Events, Event{At: now, Skill: ref, Kind: "missing", Detail: "not installed; csm does not install skills on its own"})
	}

	return m
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")

	if len(s) > n {
		return s[:n] + "..."
	}

	return s
}

// LoadManifest reads a task manifest; a missing file is an empty manifest.
func LoadManifest(path string) (Manifest, error) {
	var m Manifest

	data, err := os.ReadFile(path)

	if errors.Is(err, fs.ErrNotExist) {
		return Manifest{Version: manifestVersion}, nil
	}

	if err != nil {
		return m, err
	}

	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("read %s: %w", path, err)
	}

	return m, nil
}

// UpdateManifest changes a manifest under a lock, since the daemon and the agent's runner both write to it.
func UpdateManifest(path string, fn func(m *Manifest) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}

	defer lock.Close()

	if err := lockFile(lock); err != nil {
		return err
	}

	defer unlockFile(lock)

	m, err := LoadManifest(path)
	if err != nil {
		return err
	}

	if err := fn(&m); err != nil {
		return err
	}

	m.Version = manifestVersion
	m.UpdatedAt = time.Now()

	return writeJSONFile(path, m)
}

func (m *Manifest) event(now time.Time, provider, skill, kind, detail string) {
	m.Events = append(m.Events, Event{At: now, Provider: provider, Skill: skill, Kind: kind, Detail: detail})

	if len(m.Events) > 500 {
		m.Events = m.Events[len(m.Events)-500:]
	}
}

// Target is where one agent works on the task.
type Target struct {
	Agent   Agent
	Account string
	// Dir is the folder the agent runs in: a Build Room worktree or the project.
	Dir string
	// ProfileDir is the agent's profile, used when the agent only reads skills from its user folder.
	ProfileDir string
	// Handoff marks an agent taking over from another one; see AssessOptions.Handoff.
	Handoff bool
	// UserRoots are the account's own skill folders; a copy of the pinned revision there is used instead of a new one.
	UserRoots []string
	// Untrusted means the agent will not load project skills in Dir until the user trusts it.
	Untrusted bool
}

// Prepare makes each pinned skill of the manifest available to one agent. It loads the exact revision the task
// pinned, never a newer one; a revision that is gone or changed is reported, not replaced.
func (l *Library) Prepare(m *Manifest, t Target, records []Record, now time.Time) {
	for i := range m.Skills {
		ts := &m.Skills[i]
		p := Placement{Provider: t.Agent.Provider, Account: t.Account, Handoff: t.Handoff, At: now}
		l.place(ts, &p, t, records)

		replaced := false

		for j := range ts.Placements {
			if ts.Placements[j].Provider == p.Provider && ts.Placements[j].Account == p.Account {
				p.Evidence = ts.Placements[j].Evidence
				ts.Placements[j], replaced = p, true
			}
		}

		if !replaced {
			ts.Placements = append(ts.Placements, p)
		}

		ts.State = p.State
		detail := p.Dir

		if p.Problem != "" {
			detail = p.Problem
		}

		m.event(now, t.Agent.Provider, ts.Name, string(p.State), detail)
	}
}

func (l *Library) place(ts *TaskSkill, p *Placement, t Target, records []Record) {
	rec, err := FindIn(records, ts.ID)

	switch {
	case err != nil:
		p.State, p.Problem = StateUnavailable, "no longer installed; reinstall it or remove it from the task"

		return
	case rec.Hash != ts.Hash && !hasRevision(*rec, ts.Hash):
		p.State, p.Problem = StateUnavailable, fmt.Sprintf("the task pinned %s, which csm no longer keeps; the installed version is %s. Approve the change by reselecting the skill", short(ts.Hash), rec.VersionLabel())

		return
	}

	s, contents, modes, err := l.LoadRevision(ts.ID, ts.Hash)
	if err != nil {
		p.State, p.Problem = StateFailed, err.Error()

		return
	}

	opts := AssessOptions{PermissionsApproved: rec.PermissionsApproved, Handoff: t.Handoff, ProjectDir: t.Dir}
	c := AssessWith(s, t.Agent, opts)
	p.Level, p.Reasons = c.Level, c.Reasons

	if !c.Level.Usable() {
		p.State, p.Problem = StateUnavailable, fmt.Sprintf("%s for %s: %s", c.Level, t.Agent.Name, joinReasons(c))

		return
	}

	for _, r := range t.UserRoots {
		dir := filepath.Join(r, s.Name)
		o, owned := readOwnership(dir)

		if _, err := CheckOwned(dir, ts.ID); owned && err == nil && o.Canonical == ts.Hash {
			p.State, p.Dir, p.Invoke, p.Request = StateProjected, dir, t.Agent.InvokeText(s.Name), t.Agent.RequestText(s.Name)
			p.Reasons = append(p.Reasons, "installed for this account at the pinned revision")

			return
		}

		if exists(filepath.Join(dir, ManifestFile)) {
			p.Reasons = append(p.Reasons, fmt.Sprintf("another copy of %s is in %s; which one %s uses depends on its own precedence", s.Name, dir, t.Agent.Name))
		}
	}

	root, hide, note := TaskRoot(t)

	if note != "" {
		p.Reasons = append(p.Reasons, note)
	}

	if root == "" {
		p.State, p.Problem = StateUnavailable, t.Agent.Name+" has no skill folder csm can place a task skill in"

		return
	}

	dir := filepath.Join(root, s.Name)
	out, adaptations, err := Adapt(s, contents, t.Agent, AdaptOptions{Dir: dir, PermissionsApproved: rec.PermissionsApproved, Handoff: t.Handoff, ProjectDir: t.Dir})

	if err != nil {
		p.State, p.Problem = StateFailed, err.Error()

		return
	}

	p.Adaptations = adaptations

	if native, ok := nativeCopy(dir, out, modes); ok {
		p.State, p.Dir, p.Invoke, p.Request = StateProjected, dir, t.Agent.InvokeText(s.Name), t.Agent.RequestText(s.Name)

		if native {
			p.Reasons = append(p.Reasons, "already present in the project at this revision")
		}

		return
	}

	if err := Project(out, modes, dir, ProjectOptions{ID: ts.ID, Canonical: ts.Hash, Revision: ts.Revision, Scope: ScopeTask, HideFromGit: hide}); err != nil {
		p.State, p.Problem = StateFailed, err.Error()

		return
	}

	p.State, p.Dir, p.Invoke, p.Request = StateProjected, dir, t.Agent.InvokeText(s.Name), t.Agent.RequestText(s.Name)
}

func containsEvidence(list []Evidence, e Evidence) bool {
	for _, x := range list {
		if x.Skill == e.Skill && x.Session == e.Session && x.Kind == e.Kind && x.At.Equal(e.At) {
			return true
		}
	}

	return false
}

func exists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

func hasRevision(r Record, hash string) bool {
	for _, h := range r.History {
		if h.Hash == hash {
			return true
		}
	}

	return false
}

// nativeCopy reports whether dir already holds these exact files: a projection csm wrote earlier (ok) or the
// project's own copy (native), so retries and handoffs never write a second copy.
func nativeCopy(dir string, contents map[string][]byte, modes map[string]fs.FileMode) (native, ok bool) {
	if _, err := os.Lstat(dir); err != nil {
		return false, false
	}

	o, owned := readOwnership(dir)
	got, err := hashDir(dir, o.Extra)

	if err != nil || got != hashContents(contents, modes) {
		return false, false
	}

	return !owned, true
}

// TaskRoot is the folder task skills go into for an agent: its first project skill folder inside the working
// directory, hidden from git, or its profile folder when the agent would not read the project folder.
func TaskRoot(t Target) (string, bool, string) {
	untrusted := t.Agent.TrustedProjectsOnly && t.Untrusted

	if len(t.Agent.ProjectDirs) > 0 && t.Dir != "" && !untrusted {
		return filepath.Join(t.Dir, filepath.FromSlash(t.Agent.ProjectDirs[0])), true, ""
	}

	if len(t.Agent.UserDirs) > 0 && t.ProfileDir != "" {
		note := ""

		if untrusted {
			note = t.Agent.Name + " loads project skills only in folders you trust, so csm placed this copy in the account's own skills folder; other sessions of the account can see it until the task's copies are released"
		}

		return filepath.Join(t.ProfileDir, filepath.FromSlash(t.Agent.UserDirs[0])), false, note
	}

	return "", false, ""
}

// Release removes the task copies csm wrote for an agent, leaving anything changed or not csm's in place; only, when
// set, limits it to some folders.
func Release(m Manifest, provider string, only func(dir string) bool) []string {
	var kept []string

	for _, ts := range m.Skills {
		for _, p := range ts.Placements {
			if p.Provider != provider || p.Dir == "" || only != nil && !only(p.Dir) {
				continue
			}

			if o, err := CheckOwned(p.Dir, ts.ID); err == nil && o.Scope == ScopeTask {
				os.RemoveAll(p.Dir)
			} else if err != nil && !errors.Is(err, ErrNotOwned) {
				kept = append(kept, p.Dir)
			}
		}
	}

	return kept
}

// Requested marks the skills an agent was explicitly told about in its instruction.
func (m *Manifest) Requested(provider string, now time.Time) {
	for i := range m.Skills {
		if p := m.Skills[i].Placement(provider); p != nil && p.State == StateProjected {
			p.State = StateRequested
			m.Skills[i].State = StateRequested
			m.event(now, provider, m.Skills[i].Name, string(StateRequested), "named in the agent's instruction")
		}
	}
}

// Add pins an installed skill to the task at its current revision; a skill already in the task keeps its pin.
func (m *Manifest) Add(r Record, origin, reason string, now time.Time) bool {
	for _, ts := range m.Skills {
		if ts.ID == r.ID {
			return false
		}
	}

	m.Skills = append(m.Skills, TaskSkill{ID: r.ID, Name: r.Name, Description: r.Description, Source: r.Source.String(), Revision: r.Revision, Hash: r.Hash, License: r.License, Origin: origin, Reason: reason, State: StateSelected})
	m.event(now, "", r.Name, "selected", origin+": "+reason)

	return true
}

// Observe records what the agent's session shows. Without a record of a skill being loaded, its use stays
// unverified: csm never infers use from a skill being available or named in a prompt.
func (m *Manifest) Observe(a Agent, session string, evidence []Evidence, readable bool, now time.Time) {
	selected := map[string]bool{}

	for _, ts := range m.Skills {
		selected[ts.Name] = true
	}

	for _, e := range evidence {
		if selected[e.Skill] || containsEvidence(m.Observed, e) {
			continue
		}

		m.Observed = append(m.Observed, e)
		m.event(now, a.Provider, e.Skill, "observed", e.Kind+" in session "+session)
	}

	for i := range m.Skills {
		ts := &m.Skills[i]
		p := ts.Placement(a.Provider)

		if p == nil || p.State == StateUnavailable || p.State == StateFailed {
			continue
		}

		var seen []Evidence

		for _, e := range evidence {
			if e.Skill == ts.Name {
				seen = append(seen, e)
			}
		}

		if len(seen) > 0 {
			p.Evidence = append(p.Evidence, seen...)
			p.State, ts.State = StateInvoked, StateInvoked
			m.event(now, a.Provider, ts.Name, string(StateInvoked), fmt.Sprintf("%s in session %s", seen[0].Kind, session))

			continue
		}

		if p.State == StateInvoked {
			continue
		}

		reason := "no record of " + a.Name + " loading it in session " + session

		switch {
		case a.Evidence == "":
			reason = a.Name + " keeps no record csm can read of which skills it loaded"
		case !readable:
			reason = "the session record of " + a.Name + " could not be read"
		}

		p.State, ts.State = StateUnverified, StateUnverified
		p.Problem = reason
		m.event(now, a.Provider, ts.Name, string(StateUnverified), reason)
	}
}

// Brief is the part of an agent's instruction that names the task's skills. It points at the skills; it never
// copies their contents into the prompt.
func (m Manifest) Brief(a Agent) string {
	var lines []string

	for _, ts := range m.Skills {
		p := ts.Placement(a.Provider)

		if p == nil || p.Dir == "" || p.State == StateUnavailable || p.State == StateFailed {
			continue
		}

		use := "read " + filepath.Join(p.Dir, ManifestFile) + " before you start and follow it where it applies"

		if p.Request != "" {
			use = p.Request + "; its files are in " + p.Dir
		}

		line := fmt.Sprintf("- %s: %s. To use it, %s.", ts.Name, strings.TrimSuffix(oneLine(ts.Description, 200), "."), use)
		lines = append(lines, line)
	}

	if len(lines) == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteString("Skills selected for this task (csm placed them where " + a.Name + " finds skills):\n")
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\nIf instructions disagree, follow this order: your safety rules and approval settings, then this task's instructions and acceptance criteria, then the project's own rules, then these skills. A skill never changes your permissions.")

	for _, c := range m.Conflicts {
		if c.Project != "" {
			fmt.Fprintf(&b, "\nNote: %s prefers %s %s, but this project uses %s; follow the project.", c.Skills[0], c.Facet, c.Values[0], c.Project)
		}
	}

	return b.String()
}

// Unavailable lists the task's skills an agent cannot use, with why, for reports and handoffs.
func (m Manifest) Unavailable(provider string) []string {
	var out []string

	for _, ts := range m.Skills {
		if p := ts.Placement(provider); p != nil && (p.State == StateUnavailable || p.State == StateFailed) {
			out = append(out, ts.Name+": "+p.Problem)
		}
	}

	return out
}
