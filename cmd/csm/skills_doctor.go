package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/onoja123/csm/internal/provider"
	"github.com/onoja123/csm/internal/skills"
)

// skillView is everything one agent account would load in one project.
type skillView struct {
	Provider string       `json:"provider"`
	Account  string       `json:"account,omitempty"`
	Profile  string       `json:"profile,omitempty"`
	Roots    []skillRoot  `json:"roots"`
	Entries  []skillEntry `json:"skills"`
	Listing  string       `json:"-"`
}

type skillRoot struct {
	Dir    string `json:"dir"`
	Scope  string `json:"scope"`
	Link   string `json:"link,omitempty"`
	Exists bool   `json:"exists"`
	Broken bool   `json:"broken,omitempty"`
}

type skillEntry struct {
	Name    string       `json:"name"`
	Dir     string       `json:"dir,omitempty"`
	Scope   string       `json:"scope"`
	Managed string       `json:"managed,omitempty"`
	Source  string       `json:"source,omitempty"`
	Version string       `json:"version,omitempty"`
	Level   skills.Level `json:"compatibility,omitempty"`
	Status  string       `json:"status"`
	Detail  string       `json:"detail,omitempty"`
}

const (
	entryAvailable = "available"
	entryPartial   = "available, partly"
	entryDuplicate = "duplicate"
	entryInvalid   = "invalid"
	entryBlocked   = "unusable"
	entryMissing   = "not placed"
	entryEdited    = "edited"
)

func rootOf(dir, scope string) skillRoot {
	r := skillRoot{Dir: dir, Scope: scope}

	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(dir)
			r.Link = target

			if _, err := os.Stat(dir); err != nil {
				r.Broken = true

				return r
			}
		}

		r.Exists = true
	}

	return r
}

// buildViews lists what each agent account loads; a provider without accounts gets one view of its default profile.
func buildViews(s *State, records []skills.Record, providerFilter, accountFilter, project string, includeProject bool) []skillView {
	var views []skillView
	home := userHome()

	for _, id := range provider.IDs {
		agent := provider.SkillsAgent(id)

		if !agent.Supported() || providerFilter != "" && id != providerFilter {
			continue
		}

		type acct struct{ name, profile string }
		var accounts []acct

		for i := range s.Accounts {
			a := &s.Accounts[i]

			if a.providerID() == id && (accountFilter == "" || a.Name == accountFilter) {
				accounts = append(accounts, acct{a.Name, a.ConfigDir})
			}
		}

		if len(accounts) == 0 && accountFilter == "" && providerFilter == id && agent.DefaultProfile != "" {
			accounts = append(accounts, acct{"", joinPath(home, agent.DefaultProfile)})
		}

		for _, a := range accounts {
			v := skillView{Provider: id, Account: a.name, Profile: a.profile}

			for _, d := range agent.UserDirs {
				v.Roots = append(v.Roots, rootOf(joinPath(a.profile, d), skills.ScopeGlobal))
			}

			for _, d := range agent.HomeDirs {
				v.Roots = append(v.Roots, rootOf(joinPath(home, d), skills.ScopeGlobal))
			}

			if includeProject && project != "" {
				for _, d := range agent.ProjectDirs {
					v.Roots = append(v.Roots, rootOf(joinPath(project, d), skills.ScopeProject))
				}
			}

			v.Entries = viewEntries(v, agent, records, project, includeProject, !untrusted(id, a.profile, project))
			views = append(views, v)
		}
	}

	return views
}

func viewEntries(v skillView, agent skills.Agent, records []skills.Record, project string, includeProject, trusted bool) []skillEntry {
	var out []skillEntry
	first := map[string]int{}
	seenRoot := map[string]bool{}
	managed := map[string]bool{}

	for _, root := range v.Roots {
		real := resolvedDir(root.Dir)

		if !root.Exists || seenRoot[real] {
			continue
		}

		seenRoot[real] = true

		for _, f := range skills.Scan(root.Dir) {
			e := skillEntry{Name: f.Name, Dir: f.Dir, Scope: root.Scope, Managed: f.Managed}

			if f.Managed != "" {
				managed[f.Managed] = true

				if r, err := skills.FindIn(records, f.Managed); err == nil && r.ID == f.Managed {
					e.Source, e.Version = r.Source.String(), r.VersionLabel()
				}
			} else {
				e.Source = "yours"
			}

			switch {
			case f.Problem != "":
				e.Status, e.Detail = entryInvalid, f.Problem
			case f.Skill == nil:
				e.Status = entryInvalid
			default:
				approved := false

				if r, err := skills.FindIn(records, f.Managed); err == nil && r.ID == f.Managed {
					approved = r.PermissionsApproved
				}

				c := skills.Assess(f.Skill, agent, approved || f.Managed == "")
				e.Level = c.Level

				switch {
				case !c.Level.Usable():
					e.Status, e.Detail = entryBlocked, strings.Join(c.Reasons, "; ")
				case c.Level == skills.Partial:
					e.Status, e.Detail = entryPartial, strings.Join(c.Reasons, "; ")
				default:
					e.Status = entryAvailable
				}

				if f.Edited {
					e.Status, e.Detail = entryEdited, "csm's copy was edited after it was placed (csm skills diff "+f.Name+")"
				}

				if root.Scope == skills.ScopeProject && !trusted {
					e.Status, e.Detail = entryBlocked, agent.Name+" loads project skills only in folders you trust; trust "+displayPath(project)+" in "+agent.Name+" to use it"
				}
			}

			if i, dup := first[e.Name]; dup && e.Status != entryInvalid {
				prev := out[i]
				e.Status = entryDuplicate

				if f.Hash != "" && hashOfDir(prev.Dir) == f.Hash {
					e.Detail = "the same skill is also at " + displayPath(prev.Dir)
				} else {
					e.Detail = "a different skill with this name is at " + displayPath(prev.Dir) + "; which one " + agent.Name + " uses depends on its own precedence"
				}
			} else if e.Status != entryInvalid {
				first[e.Name] = len(out)
			}

			out = append(out, e)
		}
	}

	for _, r := range records {
		if managed[r.ID] || !r.Enabled || !r.For(agent.Provider) {
			continue
		}

		if !r.Global && !(includeProject && slices.Contains(r.Projects, project)) {
			continue
		}

		e := skillEntry{Name: r.Name, Scope: skills.ScopeGlobal, Managed: r.ID, Source: r.Source.String(), Version: r.VersionLabel(), Status: entryMissing}
		c := skills.Assess(&r.Skill, agent, r.PermissionsApproved)
		e.Level = c.Level

		switch {
		case !c.Level.Usable():
			e.Detail = string(c.Level) + ": " + strings.Join(c.Reasons, "; ")
		case first[r.Name] > 0 || len(out) > 0 && out[0].Name == r.Name:
			e.Detail = "another skill named " + r.Name + " is already in this agent's folders; csm did not overwrite it"
		default:
			e.Detail = "run csm skills sync, or start " + agent.Name + " through csm"
		}

		out = append(out, e)
	}

	return out
}

func hashOfDir(dir string) string {
	s, _, _ := skills.Load(dir)

	if s == nil {
		return ""
	}

	return s.Hash
}

func skillsAvailability(log *Logger, s *State, lib *skills.Library, a cliArgs) error {
	providerID := a.value("provider")

	if providerID != "" {
		if _, err := checkProviders([]string{providerID}); err != nil {
			return err
		}
	}

	records, err := lib.Records()
	if err != nil {
		return err
	}

	project := currentProject()
	views := buildViews(s, records, providerID, a.value("account"), project, !a.has("global"))

	if a.has("json") {
		return printJSON(log, views)
	}

	if len(views) == 0 {
		log.Info("No matching agent accounts. Add one with: csm account add <name> --provider <agent>")

		return nil
	}

	for i, v := range views {
		if i > 0 {
			log.Blank()
		}

		printView(log, v)
	}

	log.Info("\n✓ available  ! available with limits or a duplicate  ✗ will not load. \"Available\" means the agent can find it; it does not mean the agent used it.")

	return nil
}

func printView(log *Logger, v skillView) {
	title := providerName(v.Provider)

	if v.Account != "" {
		title += " · account " + v.Account
	} else {
		title += " · outside csm"
	}

	log.Info("%s", title)

	for _, root := range v.Roots {
		note := ""

		switch {
		case root.Broken:
			note = "  ✗ broken link to " + root.Link
		case root.Link != "":
			note = "  (linked to " + displayPath(root.Link) + ")"
		case !root.Exists:
			note = "  (does not exist)"
		}

		log.Info("  %-8s %s%s", root.Scope, displayPath(root.Dir), note)
	}

	if len(v.Entries) == 0 {
		log.Info("    no skills")
	}

	for _, e := range v.Entries {
		mark := "✓"

		switch e.Status {
		case entryPartial, entryDuplicate, entryEdited:
			mark = "!"
		case entryInvalid, entryBlocked, entryMissing:
			mark = "✗"
		}

		owner := "yours"

		if e.Managed != "" {
			owner = "csm"
		}

		where := displayPath(e.Dir)

		if where == "" {
			where = "-"
		}

		log.Info("    %s %-28s %-5s %-18s %s", mark, e.Name, owner, e.Status, where)

		if e.Managed != "" && e.Version != "" {
			log.Info("        %s @ %s", e.Source, e.Version)
		}

		if e.Detail != "" {
			log.Info("        %s", e.Detail)
		}
	}
}

// doctorFinding is one line of the skills doctor.
type doctorFinding struct {
	Level  string `json:"level"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

const (
	findOK   = "ok"
	findWarn = "warn"
	findFail = "fail"
)

// skillChecks inspects the library, the placed copies and every agent account's skill folders.
func skillChecks(s *State, lib *skills.Library, project string, verify bool) []doctorFinding {
	var out []doctorFinding
	add := func(level, label, detail string) { out = append(out, doctorFinding{level, label, detail}) }

	records, err := lib.Records()
	if err != nil {
		add(findFail, "Skills library", err.Error())

		return out
	}

	add(findOK, fmt.Sprintf("Skills library (%d installed)", len(records)), "")

	if entries, _ := os.ReadDir(filepath.Join(lib.Dir, "staging")); len(entries) > 0 {
		add(findWarn, "Unfinished installs", fmt.Sprintf("%d left in staging by an interrupted install; csm skills sync removes them", len(entries)))
	}

	for _, r := range records {
		if _, _, _, err := lib.LoadRevision(r.ID, r.Hash); err != nil {
			add(findFail, "Skill "+r.Name, err.Error())
		}

		for _, p := range r.Projections {
			switch st := projectionState(p, r.ID); st {
			case "ok":
			case "missing (run: csm skills sync)":
				add(findWarn, "Skill "+r.Name, "the copy for "+providerName(p.Provider)+" at "+displayPath(p.Dir)+" is missing; csm skills sync puts it back")
			default:
				add(findWarn, "Skill "+r.Name, "the copy at "+displayPath(p.Dir)+" is "+st+" (csm skills diff "+r.Name+")")
			}
		}

		for _, id := range provider.IDs {
			if !r.Enabled || !r.For(id) || len(s.accountNames(id)) == 0 {
				continue
			}

			if c := skills.Assess(&r.Skill, provider.SkillsAgent(id), r.PermissionsApproved); c.Level == skills.Partial || c.Level == skills.Unsupported && len(r.Providers) > 0 {
				add(findWarn, "Skill "+r.Name, fmt.Sprintf("%s for %s: %s", c.Level, providerName(id), strings.Join(c.Reasons, "; ")))
			}
		}
	}

	out = append(out, linkChecks(s)...)

	for _, v := range buildViews(s, records, "", "", project, project != "") {
		who := providerName(v.Provider) + " (" + orText(v.Account, "default") + ")"

		for _, root := range v.Roots {
			if root.Broken {
				add(findFail, who+" skills folder", displayPath(root.Dir)+" is a broken link to "+root.Link)
			}
		}

		for _, e := range v.Entries {
			switch e.Status {
			case entryInvalid:
				add(findWarn, who, fmt.Sprintf("%s may not load %s: %s", providerName(v.Provider), displayPath(e.Dir), e.Detail))
			case entryDuplicate:
				if strings.HasPrefix(e.Detail, "a different skill") {
					add(findWarn, who, "conflicting skill "+e.Name+": "+e.Detail)
				}
			case entryMissing:
				add(findFail, who, "installed skill "+e.Name+" is not available: "+e.Detail)
			case entryBlocked:
				add(findWarn, who, providerName(v.Provider)+" will not load "+e.Name+": "+e.Detail)
			}
		}

		if verify {
			out = append(out, verifyView(s, v, project)...)
		}
	}

	return out
}

// verifyView asks the agent itself which skills it can load, with a command that makes no model request.
func verifyView(s *State, v skillView, project string) []doctorFinding {
	who := providerName(v.Provider) + " (" + orText(v.Account, "default") + ")"
	p, err := provider.Find(v.Provider)

	if err != nil {
		return []doctorFinding{{findWarn, who + " discovery check", err.Error()}}
	}

	lister, ok := provider.SkillListing(p)
	if !ok {
		return []doctorFinding{{findWarn, who + " discovery check", p.Name() + " has no command that lists its skills without a model request, so csm cannot check what it loads"}}
	}

	cwd := project

	if cwd == "" {
		cwd = userHome()
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	listing, err := lister.SkillListing(ctx, v.Profile, cwd)
	if err != nil {
		return []doctorFinding{{findWarn, who + " discovery check", err.Error()}}
	}

	var out []doctorFinding
	checked := 0

	for _, e := range v.Entries {
		if e.Status != entryAvailable && e.Status != entryPartial {
			continue
		}

		checked++

		if !provider.ListingHas(listing, e.Name) {
			out = append(out, doctorFinding{findFail, who + " discovery check", fmt.Sprintf("%s is at %s but %s does not list it", e.Name, displayPath(e.Dir), p.Name())})
		}
	}

	if len(out) == 0 {
		out = append(out, doctorFinding{findOK, fmt.Sprintf("%s lists all %d skills csm expects", who, checked), ""})
	}

	return out
}

// linkChecks finds accounts set up to share the user's own settings that do not see the user's own skills.
func linkChecks(s *State) []doctorFinding {
	var out []doctorFinding

	for i := range s.Accounts {
		a := &s.Accounts[i]
		p, err := provider.New(a.providerID(), "")

		if err != nil {
			continue
		}

		for _, gap := range provider.SkillLinkGaps(p, a.ConfigDir) {
			out = append(out, doctorFinding{findWarn, "Account " + a.Name, gap + "; run: csm account link " + a.Name})
		}
	}

	return out
}

func skillsDoctor(log *Logger, s *State, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, []string{"verify"})
	if err != nil {
		return err
	}

	findings := skillChecks(s, lib, currentProject(), a.has("verify"))

	if a.has("json") {
		return printJSON(log, findings)
	}

	failed := 0

	for _, f := range findings {
		switch f.Level {
		case findOK:
			log.Check(f.Label, true)
		case findWarn:
			log.Info("! %s: %s", f.Label, f.Detail)
		default:
			failed++
			log.Check(f.Label, false)
			log.Info("    %s", f.Detail)
		}
	}

	if !a.has("verify") {
		log.Info("\nAdd --verify to ask each agent which skills it actually finds (no model request; Cursor CLI cannot be asked).")
	}

	if failed > 0 {
		return fmt.Errorf("%d skill problem(s) found", failed)
	}

	return nil
}

// doctorSkills is the skills part of csm doctor.
func doctorSkills(log *Logger, s *State, cwd string, fail func(label, reason string)) {
	lib := skills.Open(s.Home)
	project := detectProject(cwd).Dir

	for _, f := range skillChecks(s, lib, project, false) {
		switch f.Level {
		case findOK:
			log.Check(f.Label, true)
		case findWarn:
			log.Row(f.Label, f.Detail)
		default:
			fail(f.Label, f.Detail)
		}
	}
}

var errNoManifest = errors.New("no skills were selected")
