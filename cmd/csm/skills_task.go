package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onoja123/csm/internal/handoff"
	"github.com/onoja123/csm/internal/provider"
	"github.com/onoja123/csm/internal/skills"
)

const skillsManifestFile = "skills.json"

// skillsPath is the manifest a runner works with: a Build Room task's, one a handoff carried, or the session's own.
func (r *runner) skillsPath() string {
	if p := os.Getenv("CSM_TASK_SKILLS"); p != "" {
		return p
	}

	if r.skillsFile != "" {
		return r.skillsFile
	}

	return filepath.Join(r.stateDir, skillsManifestFile)
}

func userRoots(a *Account, agent skills.Agent) []string {
	c, ok := accountConsumer(a, agent)
	if !ok {
		return nil
	}

	return c.Read
}

func exists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// selectSkills pins the skills a user named for an interactive session; csm never installs a missing one.
func (r *runner) selectSkills(names []string) error {
	lib := skills.Open(r.state.Home)
	records, err := lib.Records()

	if err != nil {
		return err
	}

	agent := provider.SkillsAgent(r.provider.ID())
	res := skills.Resolve(records, agent, skills.Request{Provider: r.provider.ID(), Project: r.project.Dir, Explicit: names, Facts: skills.ProjectFacts(r.project.Dir)}, skills.Policy{Mode: skills.ModeManual})

	var problems []string

	for _, m := range res.Missing {
		problems = append(problems, m+" is not installed (csm skills install "+m+")")
	}

	for _, sk := range res.Skipped {
		problems = append(problems, sk.Name+": "+sk.Reason)
	}

	for _, c := range res.Conflicts {
		if c.NeedsDecision {
			problems = append(problems, c.String())
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("these skills cannot be used for this session:\n  %s", strings.Join(problems, "\n  "))
	}

	m := skills.NewManifest(res, records, "interactive "+agent.Name+" session in "+r.project.Dir, time.Now())

	return skills.UpdateManifest(r.skillsPath(), func(cur *skills.Manifest) error {
		*cur = m

		return nil
	})
}

// lastPlacedFor is the agent the manifest was placed for first, so a change of agent counts as a handoff.
func firstProvider(m *skills.Manifest) string {
	for _, ts := range m.Skills {
		if len(ts.Placements) > 0 {
			return ts.Placements[0].Provider
		}
	}

	return ""
}

// prepareSkills runs before every agent start: it places installed skills for the account and project, then the
// skills pinned to the task or session.
func (r *runner) prepareSkills(acct *Account) {
	for _, note := range placeForLaunch(r.state.Home, r.state, r.provider.ID(), acct.Name, r.project.Dir) {
		r.log.Info("  ! skill %s", note)
	}

	path := r.skillsPath()

	if !exists(path) {
		return
	}

	lib := skills.Open(r.state.Home)
	records, err := lib.Records()

	if err != nil {
		slog.Debug("skills library", "err", err)

		return
	}

	agent := provider.SkillsAgent(r.provider.ID())
	var ready, missing []string

	err = skills.UpdateManifest(path, func(m *skills.Manifest) error {
		first := firstProvider(m)
		lib.Prepare(m, skills.Target{Agent: agent, Account: acct.Name, Dir: r.cwd, ProfileDir: acct.ConfigDir, Handoff: first != "" && first != agent.Provider, UserRoots: userRoots(acct, agent), Untrusted: untrusted(agent.Provider, acct.ConfigDir, r.cwd)}, records, time.Now())

		for _, ts := range m.Skills {
			if p := ts.Placement(agent.Provider); p != nil && p.Dir != "" && p.State != skills.StateUnavailable && p.State != skills.StateFailed {
				ready = append(ready, ts.Name)
			} else if p != nil {
				missing = append(missing, ts.Name+": "+p.Problem)
			}
		}

		return nil
	})
	if err != nil {
		slog.Debug("prepare skills", "err", err)

		return
	}

	if len(ready)+len(missing) == 0 {
		return
	}

	r.step(fmt.Sprintf("Skills for %s: %s", agent.Name, orText(strings.Join(ready, ", "), "none available")), len(missing) == 0)

	for _, m := range missing {
		r.log.Info("    ✗ %s", m)
	}
}

// observeSkills reads the agent's own session record for skills it loaded. Nothing else counts as evidence.
func (r *runner) observeSkills(p provider.Provider, acct *Account, sessionID string) []skills.Evidence {
	if sessionID == "" {
		return nil
	}

	agent := provider.SkillsAgent(p.ID())
	var evidence []skills.Evidence
	readable := false

	if reader, ok := provider.SkillEvidence(p); ok {
		ev, err := reader.SkillEvidence(acct.ConfigDir, r.cwd, sessionID)

		if err != nil {
			slog.Debug("skill evidence", "provider", p.ID(), "err", err)
		}

		evidence, readable = ev, err == nil
	}

	path := r.skillsPath()

	if !exists(path) && len(evidence) == 0 {
		return nil
	}

	err := skills.UpdateManifest(path, func(m *skills.Manifest) error {
		m.Observe(agent, sessionID, evidence, readable, time.Now())

		return nil
	})
	if err != nil {
		slog.Debug("record skill evidence", "err", err)
	}

	return evidence
}

// findSkillFolder looks for a skill by name where an agent account loads skills from.
func findSkillFolder(agent skills.Agent, profileDir, project, name string) string {
	var roots []string

	for _, d := range agent.UserDirs {
		roots = append(roots, joinPath(profileDir, d))
	}

	for _, d := range agent.HomeDirs {
		roots = append(roots, joinPath(userHome(), d))
	}

	for _, d := range agent.ProjectDirs {
		roots = append(roots, joinPath(project, d))
	}

	for _, root := range roots {
		for _, f := range skills.Scan(root) {
			if f.Name == name {
				return f.Dir
			}
		}
	}

	return ""
}

// carrySkills is the skills step of a handoff: it records what the previous agent used, pins the task's skills, makes
// them available to the receiving agent in its own way and says plainly what could not be carried.
func (r *runner) carrySkills(source provider.Provider, from *Account, sessionID string, target handoffTarget) *handoff.Skills {
	r.adoptSkillsManifest(source)

	evidence := r.observeSkills(source, from, sessionID)
	path := r.skillsPath()

	if !exists(path) {
		return nil
	}

	lib := skills.Open(r.state.Home)
	records, err := lib.Records()

	if err != nil {
		return &handoff.Skills{Manifest: path, NotCarried: []handoff.CarriedSkill{{Name: "skills library", Reason: err.Error()}}}
	}

	srcAgent, dstAgent := provider.SkillsAgent(source.ID()), provider.SkillsAgent(target.provider.ID())
	out := &handoff.Skills{Manifest: path}
	now := time.Now()

	err = skills.UpdateManifest(path, func(m *skills.Manifest) error {
		for _, e := range append(evidence, m.Observed...) {
			if rec, err := skills.FindIn(records, e.Skill); err == nil && rec.Enabled {
				m.Add(*rec, skills.OriginHandoff, "loaded by "+srcAgent.Name+" before the handoff", now)
			} else if !seenCarry(out.NotCarried, e.Skill) {
				reason := "csm does not manage it, so it was not carried"

				if dir := findSkillFolder(srcAgent, from.ConfigDir, r.project.Dir, e.Skill); dir != "" {
					reason += fmt.Sprintf("; %s used it from %s. Install it with: csm skills install %s", srcAgent.Name, displayPath(dir), displayPath(dir))
				}

				out.NotCarried = append(out.NotCarried, handoff.CarriedSkill{Name: e.Skill, Used: "invoked", Reason: reason})
			}
		}

		lib.Prepare(m, skills.Target{Agent: dstAgent, Account: target.account.Name, Dir: r.cwd, ProfileDir: target.account.ConfigDir, Handoff: source.ID() != target.provider.ID(), UserRoots: userRoots(target.account, dstAgent), Untrusted: untrusted(dstAgent.Provider, target.account.ConfigDir, r.cwd)}, records, now)

		for _, ts := range m.Skills {
			c := handoff.CarriedSkill{ID: ts.ID, Name: ts.Name, Hash: ts.Hash, Source: ts.Source, Origin: ts.Origin}
			c.Version = ts.Revision

			if c.Version == "" {
				c.Version = "content " + shortHash(ts.Hash)
			}

			if sp := ts.Placement(source.ID()); sp != nil {
				c.From, c.Used = sp.Dir, string(sp.State)
			}

			dp := ts.Placement(target.provider.ID())

			if dp == nil || dp.Dir == "" || dp.State == skills.StateUnavailable || dp.State == skills.StateFailed {
				if dp != nil {
					c.Reason = dp.Problem
				}

				out.NotCarried = append(out.NotCarried, c)

				continue
			}

			c.To, c.Level, c.Adaptations, c.Use = dp.Dir, string(dp.Level), dp.Adaptations, dp.Request
			out.Carried = append(out.Carried, c)
		}

		for _, name := range m.Missing {
			out.NotCarried = append(out.NotCarried, handoff.CarriedSkill{Name: name, Reason: "selected for the task but not installed"})
		}

		m.Requested(dstAgent.Provider, now)
		out.Brief = handoffSkillBrief(*m, dstAgent, out.NotCarried)

		return nil
	})
	if err != nil {
		slog.Debug("carry skills", "err", err)
		out.NotCarried = append(out.NotCarried, handoff.CarriedSkill{Name: "skill manifest", Reason: err.Error()})
	}

	return out
}

// adoptSkillsManifest gives a runner started for a manual or resumed handoff the skill manifest of the session it
// takes over: the source agent's own, or the one an earlier, interrupted handoff of this project carried.
func (r *runner) adoptSkillsManifest(source provider.Provider) {
	path := r.skillsPath()

	if exists(path) || os.Getenv("CSM_TASK_SKILLS") != "" {
		return
	}

	candidates := []string{filepath.Join(r.state.projectStateDir(source.ID(), r.project.Dir), skillsManifestFile)}

	if _, dir, err := handoff.Latest(r.state.handoffRoot(r.project.Dir)); err == nil {
		candidates = append(candidates, filepath.Join(dir, skillsManifestFile))
	}

	for _, c := range candidates {
		if c == path || !exists(c) {
			continue
		}

		data, err := os.ReadFile(c)
		if err != nil {
			continue
		}

		if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil && os.WriteFile(path, data, 0o600) == nil {
			slog.Debug("adopted skill manifest", "from", c)
		}

		return
	}
}

func seenCarry(list []handoff.CarriedSkill, name string) bool {
	for _, c := range list {
		if c.Name == name {
			return true
		}
	}

	return false
}

func handoffSkillBrief(m skills.Manifest, agent skills.Agent, notCarried []handoff.CarriedSkill) string {
	brief := m.Brief(agent)

	if len(notCarried) == 0 {
		return brief
	}

	var b strings.Builder

	if brief != "" {
		b.WriteString(brief + "\n")
	}

	b.WriteString("These skills of the task are not available to you:")

	for _, c := range notCarried {
		fmt.Fprintf(&b, "\n- %s: %s", c.Name, c.Reason)
	}

	b.WriteString("\nDo not assume their instructions; if the task depends on one, say so to the user.")

	return b.String()
}

// releaseSessionSkills removes the task copies a session placed once csm stops supervising it. A Build Room task keeps
// the copies in its worktree for retries and only loses those placed outside it, in an account's own folder.
func releaseSessionSkills(path, cwd string) {
	if !exists(path) {
		return
	}

	m, err := skills.LoadManifest(path)
	if err != nil {
		return
	}

	var only func(string) bool

	if os.Getenv("CSM_TASK_SKILLS") != "" {
		only = func(dir string) bool { return cwd == "" || !strings.HasPrefix(dir, cwd+string(filepath.Separator)) }
	}

	for _, id := range provider.IDs {
		for _, kept := range skills.Release(m, id, only) {
			slog.Debug("skill copy left in place", "dir", kept)
		}
	}
}

// untrusted reports whether an agent account would ignore the project skills in dir.
func untrusted(providerID, profileDir, dir string) bool {
	p, err := provider.New(providerID, "")

	return err == nil && provider.SkillsAgent(providerID).TrustedProjectsOnly && !provider.TrustsFolder(p, profileDir, dir)
}

func buildSkillsPath(store buildStore, b *Build, t *BuildTask) string {
	return filepath.Join(store.dir(b.ID), "skills", t.ID+".json")
}

// resolveTaskSkills pins a Build Room task's skills the first time it starts; retries keep the same pins.
func (e *buildEngine) resolveTaskSkills(b *Build, t *BuildTask, providerID string) error {
	path := buildSkillsPath(e.store, b, t)

	if t.SkillManifest != "" && exists(t.SkillManifest) {
		return nil
	}

	s, err := e.state()
	if err != nil {
		return err
	}

	lib := skills.Open(e.home)
	records, err := lib.Records()

	if err != nil {
		return err
	}

	if len(records) == 0 && len(t.Skills) == 0 {
		return nil
	}

	text := t.Title + "\n" + t.Objective + "\n" + strings.Join(t.Acceptance, "\n")
	res := skills.Resolve(records, provider.SkillsAgent(providerID), skills.Request{Text: text, Provider: providerID, Project: b.Repo, Explicit: t.Skills, Facts: skills.ProjectFacts(b.Repo)}, s.Config.Skills)

	switch {
	case len(res.Missing) > 0:
		return fmt.Errorf("%w: %s %s not installed; install them or remove them from the task", errSkillsBlocked, strings.Join(res.Missing, ", "), plural(len(res.Missing), "is", "are"))
	case res.NeedsDecision():
		var lines []string

		for _, c := range res.Conflicts {
			if c.NeedsDecision {
				lines = append(lines, c.String())
			}
		}

		return fmt.Errorf("%w: %s", errSkillsBlocked, strings.Join(lines, "; "))
	}

	for _, sk := range res.Skipped {
		for _, ref := range t.Skills {
			if ref == sk.Name || ref == sk.ID {
				return fmt.Errorf("%w: %s cannot be used: %s", errSkillsBlocked, sk.Name, sk.Reason)
			}
		}
	}

	if len(res.Selected)+len(res.Recommended) == 0 {
		return nil
	}

	m := skills.NewManifest(res, records, text, time.Now())

	if err := skills.UpdateManifest(path, func(cur *skills.Manifest) error { *cur = m; return nil }); err != nil {
		return err
	}

	t.SkillManifest = path
	e.event(b, "skills.selected", t.ID, fmt.Sprintf("Task %q: %s", t.Title, manifestSummary(m)))

	return nil
}

var errSkillsBlocked = errors.New("the task's skills need a decision")

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}

	return many
}

func manifestSummary(m skills.Manifest) string {
	var names []string

	for _, ts := range m.Skills {
		names = append(names, ts.Name+" ("+ts.Origin+")")
	}

	out := "skills " + orText(strings.Join(names, ", "), "none selected")

	if len(m.Recommended) > 0 {
		var rec []string

		for _, c := range m.Recommended {
			rec = append(rec, c.Name)
		}

		out += "; recommended, not selected: " + strings.Join(rec, ", ")
	}

	return out
}

// prepareTaskSkills places a task's pinned skills in its worktree for the agent about to start and returns the
// instruction text that names them.
func (e *buildEngine) prepareTaskSkills(b *Build, t *BuildTask, providerID string, acct *Account, dir string) string {
	if t.SkillManifest == "" || !exists(t.SkillManifest) {
		return ""
	}

	lib := skills.Open(e.home)
	records, err := lib.Records()

	if err != nil {
		return ""
	}

	agent := provider.SkillsAgent(providerID)
	var brief string
	var unavailable []string

	err = skills.UpdateManifest(t.SkillManifest, func(m *skills.Manifest) error {
		first := firstProvider(m)
		lib.Prepare(m, skills.Target{Agent: agent, Account: acct.Name, Dir: dir, ProfileDir: acct.ConfigDir, Handoff: first != "" && first != providerID, UserRoots: userRoots(acct, agent), Untrusted: untrusted(providerID, acct.ConfigDir, dir)}, records, time.Now())
		m.Requested(providerID, time.Now())
		unavailable = m.Unavailable(providerID)
		brief = m.Brief(agent)

		return nil
	})
	if err != nil {
		e.event(b, "skills.failed", t.ID, "Could not prepare the task's skills: "+err.Error())

		return ""
	}

	if len(unavailable) > 0 {
		e.event(b, "skills.unavailable", t.ID, agent.Name+" cannot use: "+strings.Join(unavailable, "; "))
		brief += "\nThese skills of the task are not available to you: " + strings.Join(unavailable, "; ") + ". Do not assume their instructions."
	}

	return strings.TrimSpace(brief)
}

// carryFixSkills gives a fix task the pinned skills of the tasks it repairs.
func (e *buildEngine) carryFixSkills(b *Build, fix *BuildTask) {
	var carried skills.Manifest
	now := time.Now()

	for _, t := range b.Tasks {
		if t.ID == fix.ID || t.SkillManifest == "" {
			continue
		}

		m, err := skills.LoadManifest(t.SkillManifest)
		if err != nil {
			continue
		}

		carried.Mode = m.Mode

		for _, ts := range m.Skills {
			dup := false

			for _, have := range carried.Skills {
				dup = dup || have.ID == ts.ID
			}

			if dup {
				continue
			}

			ts.Origin, ts.Reason, ts.State, ts.Placements = skills.OriginHandoff, "carried from task "+t.ID, skills.StateSelected, nil
			carried.Skills = append(carried.Skills, ts)
		}
	}

	if len(carried.Skills) == 0 {
		return
	}

	carried.Task, carried.CreatedAt = fix.Title, now
	path := buildSkillsPath(e.store, b, fix)

	if err := skills.UpdateManifest(path, func(cur *skills.Manifest) error { *cur = carried; return nil }); err == nil {
		fix.SkillManifest = path
		e.event(b, "skills.selected", fix.ID, fmt.Sprintf("Task %q: %s", fix.Title, manifestSummary(carried)))
	}
}

func skillsTask(log *Logger, home string, args []string) error {
	a, err := parseCLI(args, nil)
	if err != nil {
		return err
	}

	if len(a.pos) != 2 {
		return errors.New("usage: csm skills task <build> <task> [--json]")
	}

	store := newBuildStore(home)
	b, err := store.load(a.pos[0])

	if err != nil {
		return fmt.Errorf("no build run %q", a.pos[0])
	}

	t := b.task(a.pos[1])
	if t == nil {
		return fmt.Errorf("run %s has no task %q", b.ID, a.pos[1])
	}

	if t.SkillManifest == "" {
		if a.has("json") {
			return printJSON(log, nil)
		}

		log.Info("Task %s has no skills: none were selected and none matched.", t.ID)

		return nil
	}

	m, err := skills.LoadManifest(t.SkillManifest)
	if err != nil {
		return err
	}

	if a.has("json") {
		return printJSON(log, m)
	}

	printManifest(log, m)

	return nil
}

func printManifest(log *Logger, m skills.Manifest) {
	log.Info("Selection mode: %s", m.Mode)

	for _, ts := range m.Skills {
		log.Info("\n%s  %s  (%s: %s)", ts.Name, ts.State, ts.Origin, ts.Reason)
		log.Info("  %s @ %s", ts.Source, orText(shortHash(ts.Revision), "content "+shortHash(ts.Hash)))

		for _, p := range ts.Placements {
			who := providerName(p.Provider)

			if p.Account != "" {
				who += " (" + p.Account + ")"
			}

			line := fmt.Sprintf("  %-30s %-11s", who, p.State)

			if p.Dir != "" {
				line += " " + displayPath(p.Dir)
			}

			log.Info("%s", line)

			for _, ad := range p.Adaptations {
				log.Info("      adapted: %s", ad)
			}

			if p.Problem != "" {
				log.Info("      %s", p.Problem)
			}

			for _, ev := range p.Evidence {
				log.Info("      evidence: %s, session %s at %s", ev.Kind, ev.Session, ev.At.Format("15:04:05"))
			}
		}
	}

	for _, c := range m.Recommended {
		log.Info("\nrecommended, not selected: %s (%s)", c.Name, c.Reason)
	}

	for _, c := range m.Conflicts {
		log.Info("conflict: %s", c.String())
	}

	for _, ev := range m.Observed {
		log.Info("also used without being selected: %s (%s, %s)", ev.Skill, providerName(ev.Provider), ev.Kind)
	}

	log.Info("\n\"invoked\" means the agent's own session record shows it loading the skill; \"unverified\" means no such record exists or can be read.")
}

func skillsHandoff(log *Logger, s *State, args []string) error {
	a, err := parseCLI(args, nil)
	if err != nil {
		return err
	}

	root := s.handoffRoot(currentProject())
	var st handoff.State

	switch len(a.pos) {
	case 0:
		st, _, err = handoff.Latest(root)
	case 1:
		if a.pos[0] != filepath.Base(a.pos[0]) {
			return fmt.Errorf("unexpected handoff id %q", a.pos[0])
		}

		st, err = handoff.Load(filepath.Join(root, a.pos[0]))
	default:
		return errors.New("usage: csm skills handoff [id] [--json]")
	}

	if err != nil {
		return fmt.Errorf("no handoff found for this project: %w", err)
	}

	if a.has("json") {
		return printJSON(log, st.Skills)
	}

	log.Info("Handoff %s: %s → %s", st.ID, st.Source.String(), st.Target.String())

	if st.Skills == nil || st.Skills.Empty() {
		log.Info("No skills were selected for the task or seen in the previous agent's session.")

		return nil
	}

	log.Info("%s", st.Skills.Render())

	if st.Skills.Manifest != "" {
		log.Info("Manifest: %s", displayPath(st.Skills.Manifest))
	}

	return nil
}
