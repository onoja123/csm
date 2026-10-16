package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/onoja123/csm/internal/provider"
	"github.com/onoja123/csm/internal/skills"
)

const skillsUsage = `usage: csm skills <command>

  list [--provider p] [--account a] [--project]   Installed skills, or what one agent account can load here
  search [query] [--refresh] [--registry]           Search installed skills, the catalog and, with --registry, skills.sh
  inspect <source|name> [--skill name] [--full]     Read a skill before installing it
  install <source> [--skill name...|--all]          Install from a folder, GitHub, a .git URL or the catalog
          [--project] [--global] [--provider p,...] [--pin] [--replace|--keep-both]
          [--accept-risk] [--approve-permissions]
  show <name>                                       Details, placements, compatibility and history
  compatibility <name|source> [provider]            Why a skill is compatible, adapted, partial or unsupported
  enable|disable <name> [--provider p,...]          Turn a skill on or off, or limit it to some agents
  scope <name> [--global|--no-global] [--project|--no-project]
  update [name...|--all] [--yes] [--unpin] [--accept-risk]   Check upstream; apply with --yes
  rollback <name>                                   Return to the revision before the last update
  diff <name> [--upstream]                          Show edits to placed copies, or upstream changes
  keep <name> <folder>                              Make your edited copy the installed version
  revert <name> <folder>                            Back up your edited copy and restore csm's
  remove <name> [--keep-changed]                    Uninstall csm's copies and records
  sync                                              Place or refresh every installed skill
  resolve "<task>" [--provider p] [--skill name...] [--mode m]   Preview the skills a task would get
  policy [--mode manual|recommended|automatic] [--default name] [--no-default name]
         [--max-auto n] [--exclude name] [--include name]
  packs | pack show <id> | pack install <id>        Curated groups of public skills
  task <build> <task>                               Skills resolved for a Build Room task
  handoff [id]                                      Skills carried in a handoff
  doctor                                            Check the library, placements and agent folders

Add --json to any command for machine-readable output.`

// cliArgs splits flags from positional arguments in any order.
type cliArgs struct {
	flags map[string][]string
	pos   []string
}

func parseCLI(args []string, allowed []string, withValue ...string) (cliArgs, error) {
	a := cliArgs{flags: map[string][]string{}}

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "--" {
			a.pos = append(a.pos, args[i+1:]...)

			break
		}

		if !strings.HasPrefix(arg, "--") {
			a.pos = append(a.pos, arg)

			continue
		}

		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")

		if !slices.Contains(allowed, name) && !slices.Contains(withValue, name) && name != "json" {
			return a, fmt.Errorf("unknown flag --%s\n\n%s", name, skillsUsage)
		}

		if slices.Contains(withValue, name) && !hasValue {
			if i+1 >= len(args) {
				return a, fmt.Errorf("--%s needs a value", name)
			}

			i++
			value, hasValue = args[i], true
		}

		if !hasValue {
			value = "true"
		}

		a.flags[name] = append(a.flags[name], value)
	}

	return a, nil
}

func (a cliArgs) has(name string) bool { return len(a.flags[name]) > 0 }

func (a cliArgs) value(name string) string {
	if v := a.flags[name]; len(v) > 0 {
		return v[len(v)-1]
	}

	return ""
}

func (a cliArgs) list(name string) []string {
	var out []string

	for _, v := range a.flags[name] {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}

	return out
}

func printJSON(log *Logger, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}

	log.Info("%s", data)

	return nil
}

func cmdSkills(log *Logger, home string, args []string) error {
	sub := "list"

	if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
		sub, args = args[0], args[1:]
	}

	s, err := loadState(home)
	if err != nil {
		return err
	}

	lib := skills.Open(home)

	switch sub {
	case "list", "ls":
		return skillsList(log, s, lib, args)
	case "search":
		return skillsSearch(log, lib, args)
	case "inspect":
		return skillsInspect(log, lib, args)
	case "install", "add":
		return skillsInstall(log, s, lib, args)
	case "show", "info":
		return skillsShow(log, s, lib, args)
	case "compatibility", "compat":
		return skillsCompat(log, lib, args)
	case "enable", "disable":
		return skillsEnable(log, s, lib, sub == "enable", args)
	case "scope":
		return skillsScope(log, s, lib, args)
	case "update":
		return skillsUpdate(log, s, lib, args)
	case "rollback":
		return skillsRollback(log, s, lib, args)
	case "diff":
		return skillsDiff(log, lib, args)
	case "keep":
		return skillsKeep(log, s, lib, args)
	case "revert":
		return skillsRevert(log, s, lib, args)
	case "remove", "rm", "uninstall":
		return skillsRemove(log, lib, args)
	case "sync":
		return skillsSync(log, s, lib, args)
	case "resolve":
		return skillsResolve(log, s, lib, args)
	case "policy":
		return skillsPolicy(log, s, lib, args)
	case "packs":
		return skillsPacks(log, lib, args)
	case "pack":
		return skillsPack(log, s, lib, args)
	case "task":
		return skillsTask(log, home, args)
	case "handoff":
		return skillsHandoff(log, s, args)
	case "doctor":
		return skillsDoctor(log, s, lib, args)
	case "help", "-h", "--help":
		log.Info("%s", skillsUsage)

		return nil
	}

	return fmt.Errorf("unknown skills command %q\n\n%s", sub, skillsUsage)
}

func currentProject() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	return detectProject(cwd).Dir
}

func newSkillFetcher(lib *skills.Library) *skills.Fetcher {
	entries, _, _ := lib.Catalog(skills.CuratedSources)

	return &skills.Fetcher{GitHub: skills.NewGitHub(), Catalog: entries, AgentDirs: ownAgentDirs()}
}

// ownAgentDirs are the user's own skill folders for each agent, outside csm.
func ownAgentDirs() map[string][]string {
	home := userHome()
	out := map[string][]string{}

	for id, a := range skillAgents() {
		if a.DefaultProfile != "" {
			for _, d := range a.UserDirs {
				out[id] = append(out[id], resolvedDir(joinPath(home, a.DefaultProfile, d)))
			}
		}

		for _, d := range a.HomeDirs {
			out[id] = append(out[id], resolvedDir(joinPath(home, d)))
		}
	}

	return out
}

func skillRef(r skills.Record) string {
	if r.Revision != "" {
		return r.Source.String() + " @ " + shortHash(r.Revision)
	}

	return r.Source.String() + " (content " + shortHash(r.Hash) + ")"
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}

	return h
}

func skillsList(log *Logger, s *State, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, []string{"project", "global", "all"}, "provider", "account")
	if err != nil {
		return err
	}

	if a.has("provider") || a.has("account") || a.has("all") {
		return skillsAvailability(log, s, lib, a)
	}

	records, err := lib.Records()
	if err != nil {
		return err
	}

	project := currentProject()

	if a.has("project") {
		records = slices.DeleteFunc(records, func(r skills.Record) bool { return !slices.Contains(r.Projects, project) })
	}

	if a.has("global") {
		records = slices.DeleteFunc(records, func(r skills.Record) bool { return !r.Global })
	}

	if a.has("json") {
		return printJSON(log, records)
	}

	if len(records) == 0 {
		log.Info("No skills installed. Find one with:\n\n    csm skills search <words>\n\nor see what your agents already load with: csm skills list --all")

		return nil
	}

	log.Info("Installed skills (%d)", len(records))

	for _, r := range records {
		state := "enabled"

		if !r.Enabled {
			state = "disabled"
		}

		log.Blank()
		log.Info("%-28s %-9s %-18s %s", r.Name, state, r.ScopeLabel(), r.Inspection.Summary())
		log.Info("  %s", skillRef(r))

		if line := placementSummary(r); line != "" {
			log.Info("  %s", line)
		}
	}

	return nil
}

func placementSummary(r skills.Record) string {
	byAgent := map[string]int{}

	for _, p := range r.Projections {
		byAgent[p.Provider]++
	}

	var parts []string

	for _, id := range provider.IDs {
		if n := byAgent[id]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s (%d)", providerName(id), n))
		}
	}

	if len(parts) == 0 {
		return "not placed for any agent yet"
	}

	return "placed for " + strings.Join(parts, ", ")
}

func skillsSearch(log *Logger, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, []string{"refresh", "installed", "available", "registry"}, "source")
	if err != nil {
		return err
	}

	query := strings.Join(a.pos, " ")
	records, err := lib.Records()

	if err != nil {
		return err
	}

	entries, fetched, _ := lib.Catalog(skills.CuratedSources)
	var refreshErrs []string

	if !a.has("installed") && (a.has("refresh") || len(fetched) == 0) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		gh := skills.NewGitHub()

		for _, src := range skills.CuratedSources {
			if err := lib.Refresh(ctx, gh, src); err != nil {
				refreshErrs = append(refreshErrs, src.Repo+": "+err.Error())
			}
		}

		entries, fetched, _ = lib.Catalog(skills.CuratedSources)
	}

	installed := map[string]bool{}
	var items []skills.Item

	for _, r := range records {
		installed[r.ID] = true
		items = append(items, skills.Item{ID: r.ID, Name: r.Name, Description: r.Description, Tags: append([]string{r.Category}, r.Tags...), Source: r.Source.String(), Installed: true})
	}

	if !a.has("installed") {
		for _, e := range entries {
			if installed[e.ID] || a.has("source") && e.Source.Repo != a.value("source") {
				continue
			}

			items = append(items, skills.Item{ID: e.ID, Name: e.Name, Description: e.Description, Source: e.Source.Repo, Installed: false})
		}
	}

	var extra []skills.Item

	if a.has("registry") {
		if extra, err = registryItems(records, query); err != nil {
			refreshErrs = append(refreshErrs, err.Error())
		}
	}

	if a.has("available") {
		items = slices.DeleteFunc(items, func(it skills.Item) bool { return it.Installed })
		extra = slices.DeleteFunc(extra, func(it skills.Item) bool { return it.Installed })
	}

	found := withRegistry(skills.Search(append(items, extra...), query), extra)

	if a.has("json") {
		return printJSON(log, map[string]any{"query": query, "results": found, "catalog_fetched": fetched, "errors": refreshErrs})
	}

	for _, e := range refreshErrs {
		log.Info("! catalog unavailable: %s", e)
	}

	if len(fetched) > 0 {
		oldest := time.Now()

		for _, t := range fetched {
			if t.Before(oldest) {
				oldest = t
			}
		}

		log.Info("Catalog: %d skills from %d repositories, read %s ago (refresh with --refresh)", len(entries), len(fetched), time.Since(oldest).Round(time.Minute))
	} else if !a.has("installed") {
		log.Info("Catalog: not available offline; showing installed skills only")
	}

	if len(found) == 0 {
		log.Info("\nNo skill matches %q.", query)

		return nil
	}

	log.Blank()

	for _, it := range found {
		mark := " "

		if it.Installed {
			mark = "✓"
		}

		log.Info("%s %-30s %-34s %s", mark, it.Name, truncateText(it.Source, 34), truncateText(it.Description, 90))
	}

	log.Info("\n✓ = installed. Inspect one with: csm skills inspect <name>, or skills.sh:<owner>/<repo>/<skill> for registry results")

	return nil
}

// withRegistry keeps the registry's own matches that csm's ranking did not find, after csm's results.
func withRegistry(found, extra []skills.Item) []skills.Item {
	for _, e := range extra {
		if !slices.ContainsFunc(found, func(f skills.Item) bool { return f.ID == e.ID }) {
			found = append(found, e)
		}
	}

	return found
}

// registryItems are skills.sh results not already in the library; their ID is the source that installs them.
func registryItems(records []skills.Record, query string) ([]skills.Item, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	entries, err := skills.NewRegistry().Search(ctx, query, 30)

	var out []skills.Item

	for _, e := range entries {
		installed := slices.ContainsFunc(records, func(r skills.Record) bool { return r.Source.Repo == e.Repo && r.Name == e.Skill })
		out = append(out, skills.Item{ID: e.SourceRef(), Name: e.Name, Description: "from the skills.sh registry; inspect to read it", Source: "skills.sh · " + e.Repo, Installed: installed})
	}

	return out, err
}

func truncateText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")

	if len([]rune(s)) <= n {
		return s
	}

	return string([]rune(s)[:n-1]) + "…"
}

// fetchCandidates resolves a source to the skill folders it holds and downloads the chosen ones.
func fetchCandidates(lib *skills.Library, src string, names []string, all bool) ([]skills.Candidate, skills.Listing, error) {
	if repo, skill, ok := skills.ParseRegistryRef(src); ok {
		src = "github.com/" + repo

		if len(names) == 0 {
			names = []string{skill}
		}
	}

	f := newSkillFetcher(lib)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	listing, err := f.List(ctx, src)
	if err != nil {
		return nil, listing, err
	}

	var dirs []string

	switch {
	case all:
	case len(names) > 0:
		for _, n := range names {
			found := false

			for _, d := range listing.Dirs {
				if listing.Names[d] == n || lastPart(d) == n {
					dirs, found = append(dirs, d), true
				}
			}

			if !found {
				return nil, listing, fmt.Errorf("%s has no skill named %q; it has: %s", src, n, strings.Join(listingNames(listing), ", "))
			}
		}
	case len(listing.Dirs) > 1:
		return nil, listing, fmt.Errorf("%s holds %d skills: %s\n\nPick some with --skill <name> or take all with --all", src, len(listing.Dirs), strings.Join(listingNames(listing), ", "))
	}

	if skills.Kind(src) == "catalog" {
		dirs = nil
	}

	cands, err := f.Fetch(ctx, src, dirs)

	return cands, listing, err
}

func lastPart(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}

	return p
}

func listingNames(l skills.Listing) []string {
	var out []string

	for _, d := range l.Dirs {
		if n := l.Names[d]; n != "" {
			out = append(out, n)
		} else {
			out = append(out, lastPart(d))
		}
	}

	return out
}

func skillsInspect(log *Logger, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, []string{"full", "all"}, "skill")
	if err != nil {
		return err
	}

	if len(a.pos) != 1 {
		return errors.New("usage: csm skills inspect <source|name> [--skill name] [--full] [--json]")
	}

	if rec, err := lib.Find(a.pos[0]); err == nil && skills.Kind(a.pos[0]) == "catalog" {
		return skillsShowRecord(log, lib, *rec, a.has("json"), a.has("full"))
	}

	cands, _, err := fetchCandidates(lib, a.pos[0], a.list("skill"), a.has("all"))
	if err != nil {
		return err
	}

	if a.has("json") {
		var out []map[string]any

		for _, c := range cands {
			out = append(out, candidateJSON(c))
		}

		return printJSON(log, out)
	}

	for i, c := range cands {
		if i > 0 {
			log.Blank()
		}

		printCandidate(log, c, a.has("full"))
	}

	return nil
}

func candidateJSON(c skills.Candidate) map[string]any {
	out := map[string]any{"candidate": c, "compatibility": compatAll(c.Skill)}

	if c.Skill != nil {
		out["preview"] = skills.RedactSecrets(c.Skill.Body)
	}

	return out
}

func compatAll(s *skills.Skill) []skills.Compat {
	if s == nil {
		return nil
	}

	var out []skills.Compat

	for _, id := range provider.IDs {
		out = append(out, skills.Assess(s, provider.SkillsAgent(id), false))
	}

	return out
}

func printCandidate(log *Logger, c skills.Candidate, full bool) {
	if c.Skill == nil {
		log.Info("✗ %s: %s", c.ID, c.Err)

		return
	}

	s := c.Skill
	log.Info("%s", s.Name)
	log.Info("  %s", truncateText(skills.RedactSecrets(s.Description), 400))
	log.Blank()
	log.Row("Source", c.Source.String())

	if c.Revision != "" {
		log.Row("Revision", c.Revision)
	}

	log.Row("Content hash", shortHash(s.Hash))

	log.Row("License", licenseText(s.License, c.RepoLicense))

	log.Row("Files", fmt.Sprintf("%d (%d script(s))", len(s.Files), len(c.Inspection.Scripts)))

	if len(s.AllowedTools) > 0 {
		log.Row("Asks for tools", strings.Join(s.AllowedTools, ", "))
	}

	if _, ok := s.Extra["hooks"]; ok {
		log.Row("Defines hooks", "yes (commands that run on agent events)")
	}

	if c.Err != "" {
		log.Info("\n✗ Not installable: %s", c.Err)
	}

	for _, p := range c.Problems {
		mark := "!"

		if p.Fatal {
			mark = "✗"
		}

		log.Info("%s %s", mark, p.Message)
	}

	log.Info("\nInspection: %s", c.Inspection.Summary())

	for _, f := range c.Inspection.Findings {
		loc := f.File

		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}

		log.Info("  %-7s %s — %s", f.Severity, loc, f.Message)
	}

	log.Info("  %s", skills.ScanLimitation)
	log.Info("\nCompatibility")

	for _, cp := range compatAll(s) {
		log.Info("  %-20s %s", providerName(cp.Provider), compatLine(cp))
	}

	log.Info("\nFiles")

	for _, f := range s.Files {
		mark := " "

		if f.Executable {
			mark = "x"
		}

		log.Info("  %s %-60s %8d bytes", mark, f.Path, f.Size)
	}

	body := skills.RedactSecrets(s.Body)
	lines := strings.Split(strings.TrimSpace(body), "\n")

	if !full && len(lines) > 30 {
		lines = append(lines[:30], fmt.Sprintf("… %d more lines (add --full)", len(lines)-30))
	}

	log.Info("\nSKILL.md")

	for _, l := range lines {
		log.Info("  │ %s", l)
	}
}

func licenseText(skill, repo string) string {
	switch {
	case skill != "":
		return skill
	case repo != "":
		return "not stated in the skill; the repository is " + repo
	}

	return "not stated"
}

func compatLine(c skills.Compat) string {
	parts := []string{string(c.Level)}

	if len(c.Reasons) > 0 {
		parts = append(parts, strings.Join(c.Reasons, "; "))
	}

	if len(c.Adaptations) > 0 {
		parts = append(parts, "csm will adapt: "+strings.Join(c.Adaptations, "; "))
	}

	return strings.Join(parts, " — ")
}

func skillsInstall(log *Logger, s *State, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, []string{"all", "project", "global", "pin", "replace", "keep-both", "accept-risk", "approve-permissions"}, "skill", "provider")
	if err != nil {
		return err
	}

	if len(a.pos) != 1 {
		return errors.New("usage: csm skills install <source> [--skill name...|--all] [--project] [--global] [--provider p,...] [--pin] [--replace|--keep-both] [--accept-risk] [--approve-permissions]")
	}

	providers, err := checkProviders(a.list("provider"))
	if err != nil {
		return err
	}

	cands, _, err := fetchCandidates(lib, a.pos[0], a.list("skill"), a.has("all"))
	if err != nil {
		return err
	}

	opts := skills.InstallOptions{Providers: providers, Global: a.has("global") || !a.has("project"), Pin: a.has("pin"), Replace: a.has("replace"), KeepBoth: a.has("keep-both"), AcceptRisk: a.has("accept-risk"), ApprovePermissions: a.has("approve-permissions")}

	if a.has("project") {
		opts.Project = currentProject()
	}

	results, failed := installCandidates(log, s, lib, cands, opts, a.has("json"))

	if a.has("json") {
		printJSON(log, results)
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d skill(s) were not installed", failed, len(cands))
	}

	return nil
}

func checkProviders(ids []string) ([]string, error) {
	for _, id := range ids {
		if !slices.Contains(provider.IDs, id) {
			return nil, fmt.Errorf("unknown agent %q; use %s", id, strings.Join(provider.IDs, ", "))
		}
	}

	return ids, nil
}

type installReport struct {
	ID       string           `json:"id"`
	Name     string           `json:"name"`
	Action   string           `json:"action,omitempty"`
	Error    string           `json:"error,omitempty"`
	Outcomes []skills.Outcome `json:"placements,omitempty"`
	Findings []skills.Finding `json:"findings,omitempty"`
	Record   *skills.Record   `json:"record,omitempty"`
}

func installCandidates(log *Logger, s *State, lib *skills.Library, cands []skills.Candidate, opts skills.InstallOptions, quiet bool) ([]installReport, int) {
	var reports []installReport
	failed := 0

	for _, c := range cands {
		rep := installReport{ID: c.ID, Findings: c.Inspection.Findings}

		if c.Skill != nil {
			rep.Name = c.Skill.Name
		}

		res, err := lib.Install(c, opts, time.Now())

		if err != nil {
			rep.Error = err.Error()
			failed++
			reports = append(reports, rep)

			if !quiet {
				log.Info("✗ %s: %v", orText(rep.Name, c.ID), err)
			}

			continue
		}

		rep.Action, rep.Record = res.Action, &res.Record
		outcomes, _ := syncSkills(lib, s, func(r skills.Record) bool { return r.ID == res.Record.ID })
		rep.Outcomes = outcomes[res.Record.ID]
		reports = append(reports, rep)

		if quiet {
			continue
		}

		log.Info("✓ %s %s (%s)", res.Record.Name, res.Action, skillRef(res.Record))

		if res.Replaced != nil {
			log.Info("  replaced %s from %s", res.Replaced.Name, res.Replaced.Source.String())
		}

		if !res.Record.Enabled {
			log.Info("  installed disabled beside the other %s; enable one with: csm skills enable %s", res.Record.Name, res.Record.ID)
		}

		log.Info("  inspection: %s", c.Inspection.Summary())

		if skills.RequestsPermissions(c.Skill) && !res.Record.PermissionsApproved {
			log.Info("  ! it asks for tool permissions or hooks; csm removed them from the copies agents load. Approve them with: csm skills install %s --approve-permissions", c.Source.String())
		}

		printOutcomes(log, rep.Outcomes)
	}

	return reports, failed
}

func orText(a, b string) string {
	if a != "" {
		return a
	}

	return b
}

func printOutcomes(log *Logger, outcomes []skills.Outcome) {
	if len(outcomes) == 0 {
		log.Info("  no agent account or project to place it in yet; it is placed when one starts")

		return
	}

	for _, o := range outcomes {
		mark := "✓"

		switch o.Status {
		case skills.StatusConflict, skills.StatusFailed:
			mark = "✗"
		case skills.StatusUnsupported, skills.StatusChanged:
			mark = "!"
		case skills.StatusRemoved:
			mark = "–"
		}

		who := providerName(o.Provider)

		if o.Account != "" {
			who += " (" + o.Account + ")"
		} else if o.Scope == skills.ScopeProject {
			who += " (project)"
		}

		line := fmt.Sprintf("  %s %-34s %s", mark, who, o.Status)

		if o.Dir != "" {
			line += "  " + placementLabel(o.Dir)
		}

		log.Info("%s", line)

		if o.Detail != "" {
			log.Info("      %s", o.Detail)
		}

		for _, ad := range o.Adaptations {
			log.Info("      adapted: %s", ad)
		}
	}
}

func skillsShow(log *Logger, s *State, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, []string{"full"})
	if err != nil {
		return err
	}

	if len(a.pos) != 1 {
		return errors.New("usage: csm skills show <name> [--json]")
	}

	rec, err := lib.Find(a.pos[0])
	if err != nil {
		return err
	}

	return skillsShowRecord(log, lib, *rec, a.has("json"), a.has("full"))
}

// projectionState checks a placed copy on disk right now.
func projectionState(p skills.Projection, id string) string {
	if _, err := os.Lstat(p.Dir); err != nil {
		return "missing (run: csm skills sync)"
	}

	if _, err := skills.CheckOwned(p.Dir, id); err != nil {
		if errors.Is(err, skills.ErrModified) {
			return "edited after csm placed it"
		}

		return "no longer csm's"
	}

	return "ok"
}

func skillsShowRecord(log *Logger, lib *skills.Library, r skills.Record, asJSON, full bool) error {
	compat := map[string]skills.Compat{}

	for _, id := range provider.IDs {
		compat[id] = skills.Assess(&r.Skill, provider.SkillsAgent(id), r.PermissionsApproved)
	}

	states := map[string]string{}

	for _, p := range r.Projections {
		states[p.Dir] = projectionState(p, r.ID)
	}

	if asJSON {
		return printJSON(log, map[string]any{"record": r, "compatibility": compat, "placements": states})
	}

	state := "enabled"

	if !r.Enabled {
		state = "disabled"
	}

	log.Info("%s  (%s)", r.Name, state)
	log.Info("  %s", truncateText(r.Description, 400))
	log.Blank()
	log.Row("ID", r.ID)
	log.Row("Source", r.Source.String())

	if r.Revision != "" {
		log.Row("Revision", r.Revision)
	}

	log.Row("Content hash", shortHash(r.Hash))

	if r.UpstreamHash != "" && r.UpstreamHash != r.Hash {
		log.Row("Local changes", "kept over upstream "+shortHash(r.UpstreamHash))
	}

	if r.Pinned {
		log.Row("Pinned", "yes; updates need --unpin")
	}

	log.Row("License", licenseText(r.License, r.RepoLicense))
	log.Row("Scope", r.ScopeLabel())

	for _, p := range r.Projects {
		log.Row("  project", displayPath(p))
	}

	agents := "every agent that can use it"

	if len(r.Providers) > 0 {
		var names []string

		for _, p := range r.Providers {
			names = append(names, providerName(p))
		}

		agents = strings.Join(names, ", ")
	}

	log.Row("Agents", agents)
	log.Row("Inspection", r.Inspection.Summary())

	if r.RiskAccepted {
		log.Row("", "risk accepted at install")
	}

	if skills.RequestsPermissions(&r.Skill) {
		approved := "no; removed from placed copies"

		if r.PermissionsApproved {
			approved = "yes"
		}

		log.Row("Tool permissions", approved)
	}

	if r.Upstream != nil {
		up := "checked " + r.Upstream.CheckedAt.Format("2006-01-02 15:04")

		switch {
		case r.Upstream.Error != "":
			up += ": " + r.Upstream.Error
		case r.Upstream.Hash == r.UpstreamHash:
			up += ": up to date"
		default:
			up += ": a newer version exists (csm skills update " + r.Name + ")"
		}

		log.Row("Upstream", up)
	}

	log.Info("\nPlaced copies")

	if len(r.Projections) == 0 {
		log.Info("  none")
	}

	for _, p := range r.Projections {
		log.Info("  %-20s %-10s %s  %s", providerName(p.Provider), p.Scope, placementLabel(p.Dir), states[p.Dir])
	}

	log.Info("\nCompatibility")

	for _, id := range provider.IDs {
		log.Info("  %-20s %s", providerName(id), compatLine(compat[id]))
	}

	for _, f := range r.Inspection.Findings {
		if f.Severity != skills.SeverityInfo || full {
			log.Info("  %-7s %s:%d %s", f.Severity, f.File, f.Line, f.Message)
		}
	}

	log.Info("\nHistory")

	for _, h := range r.History {
		ref := shortHash(h.Hash)

		if h.Revision != "" {
			ref = shortHash(h.Revision)
		}

		log.Info("  %s  %-12s %s", h.At.Format("2006-01-02 15:04"), ref, h.Note)
	}

	return nil
}

func skillsCompat(log *Logger, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, nil, "skill")
	if err != nil {
		return err
	}

	if len(a.pos) < 1 || len(a.pos) > 2 {
		return errors.New("usage: csm skills compatibility <name|source> [provider] [--json]")
	}

	var sk *skills.Skill
	approved := false

	if rec, err := lib.Find(a.pos[0]); err == nil {
		sk, approved = &rec.Skill, rec.PermissionsApproved
	} else {
		cands, _, err := fetchCandidates(lib, a.pos[0], a.list("skill"), false)
		if err != nil {
			return err
		}

		if len(cands) == 0 || cands[0].Skill == nil {
			return fmt.Errorf("%s could not be read", a.pos[0])
		}

		sk = cands[0].Skill
	}

	ids := provider.IDs

	if len(a.pos) == 2 {
		if _, err := checkProviders(a.pos[1:]); err != nil {
			return err
		}

		ids = a.pos[1:]
	}

	var out []skills.Compat

	for _, id := range ids {
		out = append(out, skills.Assess(sk, provider.SkillsAgent(id), approved))
	}

	if a.has("json") {
		return printJSON(log, out)
	}

	for _, c := range out {
		agent := provider.SkillsAgent(c.Provider)
		log.Info("%s: %s", agent.Name, c.Level)

		for _, r := range c.Reasons {
			log.Info("  - %s", r)
		}

		for _, ad := range c.Adaptations {
			log.Info("  adapted: %s", ad)
		}

		for _, n := range c.Notes {
			log.Info("  note: %s", n)
		}

		if agent.Verified != "" {
			log.Info("  checked against: %s", agent.Verified)
		}
	}

	return nil
}

func skillsEnable(log *Logger, s *State, lib *skills.Library, enable bool, args []string) error {
	a, err := parseCLI(args, nil, "provider")
	if err != nil {
		return err
	}

	if len(a.pos) != 1 {
		return errors.New("usage: csm skills enable|disable <name> [--provider p,...]")
	}

	providers, err := checkProviders(a.list("provider"))
	if err != nil {
		return err
	}

	rec, err := lib.Find(a.pos[0])
	if err != nil {
		return err
	}

	err = lib.Update(func(f *skills.LibraryFile) error {
		for i := range f.Skills {
			r := &f.Skills[i]

			if r.ID == rec.ID {
				applyEnable(r, enable, providers)
			} else if enable && r.Name == rec.Name && r.Enabled && len(providers) == 0 {
				return fmt.Errorf("another skill named %s (%s) is enabled; disable it first, since agents find skills by name", r.Name, r.ID)
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	outcomes, err := syncSkills(lib, s, func(r skills.Record) bool { return r.ID == rec.ID })
	if err != nil {
		return err
	}

	if a.has("json") {
		return printJSON(log, outcomes[rec.ID])
	}

	updated, _ := lib.Find(rec.ID)
	state := "disabled"

	if updated != nil && updated.Enabled {
		state = "enabled"

		if len(updated.Providers) > 0 {
			var names []string

			for _, id := range updated.Providers {
				names = append(names, providerName(id))
			}

			state += " for " + strings.Join(names, ", ")
		}
	}

	log.Info("%s is now %s", rec.Name, state)
	printOutcomes(log, outcomes[rec.ID])

	return nil
}

// applyEnable turns a skill on or off, or adds and removes agents from the ones it is limited to.
func applyEnable(r *skills.Record, enable bool, providers []string) {
	if len(providers) == 0 {
		r.Enabled = enable

		return
	}

	current := r.Providers

	if len(current) == 0 {
		current = slices.Clone(provider.IDs)
	}

	for _, p := range providers {
		if enable && !slices.Contains(current, p) {
			current = append(current, p)
		}

		if !enable {
			current = slices.DeleteFunc(current, func(x string) bool { return x == p })
		}
	}

	if enable {
		r.Enabled = true
	}

	if len(current) == len(provider.IDs) {
		current = nil
	}

	r.Providers = current

	if !enable && len(current) == 0 {
		r.Enabled, r.Providers = false, nil
	}
}

func skillsScope(log *Logger, s *State, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, []string{"global", "no-global", "project", "no-project"})
	if err != nil {
		return err
	}

	if len(a.pos) != 1 {
		return errors.New("usage: csm skills scope <name> [--global|--no-global] [--project|--no-project]")
	}

	rec, err := lib.Find(a.pos[0])
	if err != nil {
		return err
	}

	project := currentProject()

	err = lib.UpdateRecord(rec.ID, func(r *skills.Record) error {
		switch {
		case a.has("global"):
			r.Global = true
		case a.has("no-global"):
			r.Global = false
		}

		switch {
		case a.has("project") && !slices.Contains(r.Projects, project):
			r.Projects = append(r.Projects, project)
		case a.has("no-project"):
			r.Projects = slices.DeleteFunc(r.Projects, func(p string) bool { return p == project })
		}

		return nil
	})
	if err != nil {
		return err
	}

	outcomes, err := syncSkills(lib, s, func(r skills.Record) bool { return r.ID == rec.ID })
	if err != nil {
		return err
	}

	if a.has("json") {
		return printJSON(log, outcomes[rec.ID])
	}

	updated, _ := lib.Find(rec.ID)
	log.Info("%s: %s", rec.Name, updated.ScopeLabel())
	printOutcomes(log, outcomes[rec.ID])

	return nil
}

func skillsRemove(log *Logger, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, []string{"keep-changed"})
	if err != nil {
		return err
	}

	if len(a.pos) != 1 {
		return errors.New("usage: csm skills remove <name> [--keep-changed]")
	}

	res, err := lib.Remove(a.pos[0], a.has("keep-changed"))
	if err != nil {
		return err
	}

	if a.has("json") {
		return printJSON(log, res)
	}

	log.Info("✓ Removed %s (%s)", res.Record.Name, res.Record.Source.String())

	for _, d := range res.Removed {
		log.Info("  removed %s", placementLabel(d))
	}

	for _, d := range res.Kept {
		log.Info("  ! left %s in place: it was edited or is not csm's", placementLabel(d))
	}

	return nil
}

func skillsSync(log *Logger, s *State, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, nil)
	if err != nil {
		return err
	}

	if n, _ := lib.CleanStaging(); n > 0 && !a.has("json") {
		log.Info("Removed %d unfinished install(s) from staging.", n)
	}

	outcomes, err := syncSkills(lib, s, nil)
	if err != nil {
		return err
	}

	if a.has("json") {
		return printJSON(log, outcomes)
	}

	records, _ := lib.Records()

	if len(records) == 0 {
		log.Info("No skills installed.")
	}

	for _, r := range records {
		log.Info("%s", r.Name)
		printOutcomes(log, outcomes[r.ID])
	}

	return nil
}

func skillsResolve(log *Logger, s *State, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, nil, "provider", "skill", "mode")
	if err != nil {
		return err
	}

	if len(a.pos) == 0 {
		return errors.New(`usage: csm skills resolve "<task>" [--provider p] [--skill name...] [--mode manual|recommended|automatic]`)
	}

	providerID := orText(a.value("provider"), provider.ClaudeID)

	if _, err := checkProviders([]string{providerID}); err != nil {
		return err
	}

	records, err := lib.Records()
	if err != nil {
		return err
	}

	pol := s.Config.Skills

	if m := a.value("mode"); m != "" {
		if !slices.Contains([]string{skills.ModeManual, skills.ModeRecommended, skills.ModeAutomatic}, m) {
			return fmt.Errorf("unknown mode %q; use manual, recommended or automatic", m)
		}

		pol.Mode = m
	}

	project := currentProject()
	res := skills.Resolve(records, provider.SkillsAgent(providerID), skills.Request{Text: strings.Join(a.pos, " "), Provider: providerID, Project: project, Explicit: a.list("skill"), Facts: skills.ProjectFacts(project)}, pol)

	if a.has("json") {
		return printJSON(log, res)
	}

	printResolution(log, res, providerID)

	return nil
}

func printResolution(log *Logger, res skills.Result, providerID string) {
	log.Info("Mode: %s, agent: %s", res.Mode, providerName(providerID))

	if len(res.Selected) == 0 {
		log.Info("\nSelected: none")
	} else {
		log.Info("\nSelected")

		for _, c := range res.Selected {
			log.Info("  ✓ %-28s %-10s %s", c.Name, c.Origin, c.Reason)
		}
	}

	if len(res.Recommended) > 0 {
		log.Info("\nRecommended (not selected; pick with --skill)")

		for _, c := range res.Recommended {
			log.Info("  · %-28s %s", c.Name, c.Reason)
		}
	}

	for _, sk := range res.Skipped {
		log.Info("  – %s: %s", sk.Name, sk.Reason)
	}

	for _, m := range res.Missing {
		log.Info("  ✗ %s is not installed; csm never installs skills on its own (csm skills install %s)", m, m)
	}

	for _, c := range res.Conflicts {
		mark := "!"

		if c.NeedsDecision {
			mark = "✗"
		}

		log.Info("  %s %s", mark, c.String())
	}
}

func skillsPolicy(log *Logger, s *State, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, nil, "mode", "default", "no-default", "max-auto", "exclude", "include")
	if err != nil {
		return err
	}

	changed := len(a.flags) > 0 && !(len(a.flags) == 1 && a.has("json"))

	if changed {
		records, err := lib.Records()
		if err != nil {
			return err
		}

		idOf := func(ref string) (string, error) {
			r, err := skills.FindIn(records, ref)
			if err != nil {
				return "", err
			}

			return r.ID, nil
		}

		project := currentProject()

		err = s.update(func(st *State) error {
			p := &st.Config.Skills

			if m := a.value("mode"); m != "" {
				if !slices.Contains([]string{skills.ModeManual, skills.ModeRecommended, skills.ModeAutomatic}, m) {
					return fmt.Errorf("unknown mode %q; use manual, recommended or automatic", m)
				}

				p.Mode = m
			}

			if v := a.value("max-auto"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 1 || n > 10 {
					return errors.New("--max-auto takes a number from 1 to 10")
				}

				p.MaxAuto = n
			}

			for _, ref := range a.list("default") {
				id, err := idOf(ref)
				if err != nil {
					return err
				}

				if !slices.Contains(p.Defaults, id) {
					p.Defaults = append(p.Defaults, id)
				}
			}

			for _, ref := range a.list("no-default") {
				id, err := idOf(ref)
				if err != nil {
					return err
				}

				p.Defaults = slices.DeleteFunc(p.Defaults, func(x string) bool { return x == id })
			}

			for _, ref := range a.list("exclude") {
				id, err := idOf(ref)
				if err != nil {
					return err
				}

				if p.Excluded == nil {
					p.Excluded = map[string][]string{}
				}

				if !slices.Contains(p.Excluded[project], id) {
					p.Excluded[project] = append(p.Excluded[project], id)
				}
			}

			for _, ref := range a.list("include") {
				id, err := idOf(ref)
				if err != nil {
					return err
				}

				p.Excluded[project] = slices.DeleteFunc(p.Excluded[project], func(x string) bool { return x == id })

				if len(p.Excluded[project]) == 0 {
					delete(p.Excluded, project)
				}
			}

			return nil
		})
		if err != nil {
			return err
		}

		s, err = loadState(s.Home)
		if err != nil {
			return err
		}
	}

	p := s.Config.Skills

	if a.has("json") {
		return printJSON(log, map[string]any{"mode": p.EffectiveMode(), "defaults": p.Defaults, "max_auto": p.MaxAuto, "excluded": p.Excluded})
	}

	log.Row("Selection mode", p.EffectiveMode())
	log.Row("Defaults", orText(strings.Join(p.Defaults, ", "), "none"))

	maxAuto := p.MaxAuto

	if maxAuto == 0 {
		maxAuto = 3
	}

	log.Row("Most automatic picks", strconv.Itoa(maxAuto))

	projects := make([]string, 0, len(p.Excluded))

	for dir := range p.Excluded {
		projects = append(projects, dir)
	}

	sort.Strings(projects)

	for _, dir := range projects {
		log.Row("Excluded in "+displayPath(dir), strings.Join(p.Excluded[dir], ", "))
	}

	log.Info("\nmanual: only skills you pick. recommended: csm suggests, you pick. automatic: csm also picks up to %d matching skills you installed; it never installs any.", maxAuto)

	return nil
}

func skillsPacks(log *Logger, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, nil)
	if err != nil {
		return err
	}

	records, _ := lib.Records()
	installedNames := map[string]bool{}

	for _, r := range records {
		installedNames[r.Source.String()] = true
	}

	if a.has("json") {
		return printJSON(log, map[string]any{"packs": skills.CuratedPacks, "disclaimer": skills.PackDisclaimer})
	}

	log.Info("%s\n", skills.PackDisclaimer)

	for _, p := range skills.CuratedPacks {
		have := 0

		for _, m := range p.Members {
			if installedNames[m.Source] {
				have++
			}
		}

		log.Info("%-16s %-26s %d skills (%d installed)  %s", p.ID, p.Name, len(p.Members), have, p.Description)
	}

	log.Info("\nSee one with: csm skills pack show <id>")

	return nil
}

func skillsPack(log *Logger, s *State, lib *skills.Library, args []string) error {
	if len(args) < 2 || args[0] != "show" && args[0] != "install" {
		return errors.New("usage: csm skills pack show <id> | pack install <id> [--project] [--provider p,...] [--accept-risk] [--approve-permissions]")
	}

	a, err := parseCLI(args[2:], []string{"project", "global", "accept-risk", "approve-permissions"}, "provider")
	if err != nil {
		return err
	}

	p, err := skills.FindPack(args[1])
	if err != nil {
		return err
	}

	if args[0] == "show" {
		if a.has("json") {
			return printJSON(log, p)
		}

		log.Info("%s (version %s)\n  %s\n  %s\n", p.Name, p.Version, p.Description, skills.PackDisclaimer)

		for _, m := range p.Members {
			opt := ""

			if m.Optional {
				opt = " (optional)"
			}

			log.Info("  %-30s %s%s", m.Name, m.Source, opt)
		}

		return nil
	}

	providers, err := checkProviders(a.list("provider"))
	if err != nil {
		return err
	}

	opts := skills.InstallOptions{Providers: providers, Global: a.has("global") || !a.has("project"), AcceptRisk: a.has("accept-risk"), ApprovePermissions: a.has("approve-permissions"), Pack: p.ID}

	if a.has("project") {
		opts.Project = currentProject()
	}

	var members []string
	var failures []string
	var reports []installReport

	for _, m := range p.Members {
		cands, _, err := fetchCandidates(lib, m.Source, nil, false)

		if err != nil {
			failures = append(failures, m.Name+": "+err.Error())

			continue
		}

		rep, failed := installCandidates(log, s, lib, cands, opts, a.has("json"))
		reports = append(reports, rep...)

		for _, r := range rep {
			if r.Record != nil {
				members = append(members, r.Record.ID)
			}
		}

		if failed > 0 && !m.Optional {
			failures = append(failures, m.Name+": not installed")
		}
	}

	if err := lib.RecordPack(p, members, time.Now()); err != nil {
		return err
	}

	if a.has("json") {
		return printJSON(log, map[string]any{"pack": p.ID, "installed": members, "failures": failures, "reports": reports})
	}

	log.Info("\n%s: %d of %d skills installed", p.Name, len(members), len(p.Members))

	if len(failures) > 0 {
		return fmt.Errorf("some required skills were not installed:\n  %s", strings.Join(failures, "\n  "))
	}

	return nil
}
