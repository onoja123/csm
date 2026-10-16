package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/onoja123/csm/internal/handoff"
	"github.com/onoja123/csm/internal/provider"
	"github.com/onoja123/csm/internal/skills"
)

func (d *daemon) skillRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/skills", d.getSkills)
	mux.HandleFunc("GET /v1/skills/catalog", d.getSkillCatalog)
	mux.HandleFunc("GET /v1/skills/detail", d.getSkillDetail)
	mux.HandleFunc("GET /v1/skills/availability", d.getSkillAvailability)
	mux.HandleFunc("GET /v1/skills/doctor", d.getSkillDoctor)
	mux.HandleFunc("GET /v1/skills/diff", d.getSkillDiff)
	mux.HandleFunc("GET /v1/skills/task", d.getSkillTask)
	mux.HandleFunc("GET /v1/skills/session", d.getSkillSession)
	mux.HandleFunc("GET /v1/skills/agents", d.getSkillAgents)
	mux.HandleFunc("GET /v1/skills/packs", d.getSkillPacks)
	mux.HandleFunc("GET /v1/skills/policy", d.getSkillPolicy)
	mux.HandleFunc("PUT /v1/skills/policy", d.putSkillPolicy)
	mux.HandleFunc("POST /v1/skills/inspect", d.postSkillInspect)
	mux.HandleFunc("POST /v1/skills/install", d.postSkillInstall)
	mux.HandleFunc("POST /v1/skills/packs/{id}/install", d.postSkillPackInstall)
	mux.HandleFunc("POST /v1/skills/resolve", d.postSkillResolve)
	mux.HandleFunc("POST /v1/skills/check", d.postSkillCheck)
	mux.HandleFunc("POST /v1/skills/{action}", d.postSkillAction)
}

type skillRecordView struct {
	skills.Record
	Compatibility []skills.Compat   `json:"compatibility"`
	Placements    map[string]string `json:"placement_states"`
	Version       string            `json:"version"`
}

func recordView(r skills.Record) skillRecordView {
	v := skillRecordView{Record: r, Placements: map[string]string{}, Version: r.VersionLabel()}

	for _, id := range provider.IDs {
		v.Compatibility = append(v.Compatibility, skills.Assess(&r.Skill, provider.SkillsAgent(id), r.PermissionsApproved))
	}

	for _, p := range r.Projections {
		v.Placements[p.Dir] = projectionState(p, r.ID)
	}

	return v
}

func (d *daemon) getSkills(w http.ResponseWriter, r *http.Request) {
	lib := skills.Open(d.home)
	records, err := lib.Records()

	if err != nil {
		writeErr(w, 500, err)

		return
	}

	packs, _ := lib.Packs()
	out := struct {
		Skills []skillRecordView   `json:"skills"`
		Packs  []skills.PackRecord `json:"packs"`
	}{Skills: []skillRecordView{}, Packs: packs}

	for _, rec := range records {
		out.Skills = append(out.Skills, recordView(rec))
	}

	writeOK(w, out)
}

func (d *daemon) getSkillCatalog(w http.ResponseWriter, r *http.Request) {
	lib := skills.Open(d.home)
	q := r.URL.Query()
	entries, fetched, _ := lib.Catalog(skills.CuratedSources)
	errs := []string{}

	if q.Get("refresh") == "1" || len(fetched) == 0 && q.Get("offline") != "1" {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()

		gh := skills.NewGitHub()

		for _, src := range skills.CuratedSources {
			if err := lib.Refresh(ctx, gh, src); err != nil {
				errs = append(errs, src.Repo+": "+err.Error())
			}
		}

		entries, fetched, _ = lib.Catalog(skills.CuratedSources)
	}

	records, _ := lib.Records()
	installed := map[string]bool{}
	items := []skills.Item{}

	for _, rec := range records {
		installed[rec.ID] = true
		items = append(items, skills.Item{ID: rec.ID, Name: rec.Name, Description: rec.Description, Tags: append([]string{rec.Category}, rec.Tags...), Source: rec.Source.String(), Installed: true})
	}

	byID := map[string]skills.CatalogEntry{}

	for _, e := range entries {
		byID[e.ID] = e

		if !installed[e.ID] {
			items = append(items, skills.Item{ID: e.ID, Name: e.Name, Description: e.Description, Source: e.Source.Repo})
		}
	}

	var extra []skills.Item

	if q.Get("registry") == "1" {
		var err error

		if extra, err = registryItems(records, q.Get("q")); err != nil {
			errs = append(errs, err.Error())
		}
	}

	results := withRegistry(skills.Search(append(items, extra...), q.Get("q")), extra)

	if results == nil {
		results = []skills.Item{}
	}

	writeOK(w, map[string]any{"results": results, "entries": byID, "fetched": fetched, "sources": skills.CuratedSources, "errors": errs})
}

func (d *daemon) getSkillDetail(w http.ResponseWriter, r *http.Request) {
	lib := skills.Open(d.home)
	rec, err := lib.Find(r.URL.Query().Get("id"))

	if err != nil {
		writeErr(w, 404, err)

		return
	}

	out := map[string]any{"record": recordView(*rec)}

	if s, contents, _, err := lib.LoadRevision(rec.ID, rec.Hash); err == nil {
		out["preview"] = skills.RedactSecrets(s.Body)
		out["files"] = s.Files
		_ = contents
	} else {
		out["store_error"] = err.Error()
	}

	writeOK(w, out)
}

func (d *daemon) getSkillAvailability(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	records, err := skills.Open(d.home).Records()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	q := r.URL.Query()
	project := q.Get("project_dir")

	if project != "" {
		project = detectProject(project).Dir
	}

	views := buildViews(s, records, q.Get("provider"), q.Get("account"), project, project != "")

	if views == nil {
		views = []skillView{}
	}

	writeOK(w, views)
}

func (d *daemon) getSkillDoctor(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	project := r.URL.Query().Get("project_dir")

	if project != "" {
		project = detectProject(project).Dir
	}

	writeOK(w, skillChecks(s, skills.Open(d.home), project, r.URL.Query().Get("verify") == "1"))
}

func (d *daemon) getSkillDiff(w http.ResponseWriter, r *http.Request) {
	lib := skills.Open(d.home)
	rec, err := lib.Find(r.URL.Query().Get("id"))

	if err != nil {
		writeErr(w, 404, err)

		return
	}

	type diffView struct {
		Dir     string              `json:"dir"`
		Changes []skills.FileChange `json:"changes"`
	}

	out := []diffView{}

	if r.URL.Query().Get("upstream") == "1" {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()

		cand, up := newSkillFetcher(lib).Check(ctx, *rec)
		if cand == nil {
			writeErr(w, 502, errors.New(up.Error))

			return
		}

		_, current, _, err := lib.LoadRevision(rec.ID, rec.Hash)
		if err != nil {
			writeErr(w, 500, err)

			return
		}

		out = append(out, diffView{Dir: rec.Source.String(), Changes: redactChanges(skills.Changes(current, cand.Contents))})
	} else {
		for _, p := range rec.Projections {
			if _, err := skills.CheckOwned(p.Dir, rec.ID); !errors.Is(err, skills.ErrModified) {
				continue
			}

			want, err := expectedCopy(lib, *rec, p)
			if err != nil {
				continue
			}

			have, _, err := skills.ReadTree(p.Dir, []string{".gitignore"})
			if err != nil {
				continue
			}

			out = append(out, diffView{Dir: p.Dir, Changes: redactChanges(skills.Changes(want, have))})
		}
	}

	writeOK(w, out)
}

func redactChanges(changes []skills.FileChange) []skills.FileChange {
	for i := range changes {
		changes[i].Diff = skills.RedactSecrets(changes[i].Diff)
	}

	if changes == nil {
		return []skills.FileChange{}
	}

	return changes
}

func (d *daemon) getSkillTask(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	b, err := d.builds.store.load(q.Get("build"))

	if err != nil {
		writeErr(w, 404, fmt.Errorf("no build run %q", q.Get("build")))

		return
	}

	t := b.task(q.Get("task"))
	if t == nil || t.SkillManifest == "" {
		writeOK(w, map[string]any{"skills": []any{}})

		return
	}

	m, err := skills.LoadManifest(t.SkillManifest)
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	writeOK(w, m)
}

// getSkillSession is the manifest of the skills pinned to an interactive session: the agent's own, or the one the
// latest handoff to that agent carried.
func (d *daemon) getSkillSession(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	q := r.URL.Query()
	providerID, project := q.Get("provider"), detectProject(q.Get("project_dir")).Dir

	if _, err := checkProviders([]string{providerID}); err != nil || q.Get("project_dir") == "" {
		writeErr(w, 400, errors.New("provider and project_dir are required"))

		return
	}

	path := filepath.Join(s.projectStateDir(providerID, project), skillsManifestFile)

	if !exists(path) {
		if st, dir, err := handoff.Latest(s.handoffRoot(project)); err == nil && st.Target.Provider == providerID {
			path = filepath.Join(dir, skillsManifestFile)
		}
	}

	if !exists(path) {
		writeOK(w, map[string]any{"skills": []any{}})

		return
	}

	m, err := skills.LoadManifest(path)
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	writeOK(w, m)
}

func (d *daemon) getSkillAgents(w http.ResponseWriter, r *http.Request) {
	out := []skills.Agent{}

	for _, id := range provider.IDs {
		out = append(out, provider.SkillsAgent(id))
	}

	writeOK(w, out)
}

func (d *daemon) getSkillPacks(w http.ResponseWriter, r *http.Request) {
	records, _ := skills.Open(d.home).Records()
	installed := map[string]string{}

	for _, rec := range records {
		installed[rec.Source.String()] = rec.ID
	}

	type memberView struct {
		skills.PackMember
		Installed string `json:"installed,omitempty"`
	}

	type packView struct {
		skills.Pack
		Members []memberView `json:"members"`
	}

	out := []packView{}

	for _, p := range skills.CuratedPacks {
		pv := packView{Pack: p}

		for _, m := range p.Members {
			pv.Members = append(pv.Members, memberView{PackMember: m, Installed: installed[m.Source]})
		}

		out = append(out, pv)
	}

	writeOK(w, map[string]any{"packs": out, "disclaimer": skills.PackDisclaimer})
}

func (d *daemon) getSkillPolicy(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	p := s.Config.Skills
	writeOK(w, map[string]any{"mode": p.EffectiveMode(), "defaults": orEmpty(p.Defaults), "max_auto": p.MaxAuto, "excluded": p.Excluded})
}

func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}

	return list
}

func (d *daemon) putSkillPolicy(w http.ResponseWriter, r *http.Request) {
	var req skills.Policy

	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, err)

		return
	}

	if req.Mode != "" && !slices.Contains([]string{skills.ModeManual, skills.ModeRecommended, skills.ModeAutomatic}, req.Mode) {
		writeErr(w, 400, fmt.Errorf("unknown mode %q", req.Mode))

		return
	}

	if req.MaxAuto < 0 || req.MaxAuto > 10 {
		writeErr(w, 400, errors.New("max_auto must be between 1 and 10"))

		return
	}

	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	records, _ := skills.Open(d.home).Records()

	for _, id := range req.Defaults {
		if _, err := skills.FindIn(records, id); err != nil {
			writeErr(w, 400, err)

			return
		}
	}

	if err := s.update(func(st *State) error { st.Config.Skills = req; return nil }); err != nil {
		writeErr(w, 500, err)

		return
	}

	d.log.add(Activity{Kind: "skills.policy", Title: "Skill selection set to " + req.EffectiveMode()})
	d.getSkillPolicy(w, r)
}

type skillInstallRequest struct {
	Source             string   `json:"source"`
	Skills             []string `json:"skills,omitempty"`
	All                bool     `json:"all,omitempty"`
	Global             bool     `json:"global"`
	ProjectDir         string   `json:"project_dir,omitempty"`
	Providers          []string `json:"providers,omitempty"`
	Pin                bool     `json:"pin,omitempty"`
	Replace            bool     `json:"replace,omitempty"`
	KeepBoth           bool     `json:"keep_both,omitempty"`
	AcceptRisk         bool     `json:"accept_risk,omitempty"`
	ApprovePermissions bool     `json:"approve_permissions,omitempty"`
}

func (d *daemon) postSkillInspect(w http.ResponseWriter, r *http.Request) {
	var req skillInstallRequest

	if err := readBody(r, &req); err != nil || req.Source == "" {
		writeErr(w, 400, errors.New("source is required"))

		return
	}

	cands, listing, err := fetchCandidates(skills.Open(d.home), req.Source, req.Skills, req.All)
	if err != nil {
		writeOK(w, map[string]any{"listing": listing, "error": err.Error(), "candidates": []any{}})

		return
	}

	out := []map[string]any{}

	for _, c := range cands {
		out = append(out, candidateJSON(c))
	}

	writeOK(w, map[string]any{"listing": listing, "candidates": out, "limitation": skills.ScanLimitation})
}

func (req skillInstallRequest) options() (skills.InstallOptions, error) {
	if _, err := checkProviders(req.Providers); err != nil {
		return skills.InstallOptions{}, err
	}

	opts := skills.InstallOptions{Providers: req.Providers, Global: req.Global, Pin: req.Pin, Replace: req.Replace, KeepBoth: req.KeepBoth, AcceptRisk: req.AcceptRisk, ApprovePermissions: req.ApprovePermissions}

	if req.ProjectDir != "" {
		if info, err := os.Stat(req.ProjectDir); err != nil || !info.IsDir() {
			return opts, fmt.Errorf("%s is not a folder", req.ProjectDir)
		}

		opts.Project = detectProject(req.ProjectDir).Dir
	}

	if !opts.Global && opts.Project == "" {
		return opts, errors.New("choose global, a project, or both")
	}

	return opts, nil
}

func (d *daemon) postSkillInstall(w http.ResponseWriter, r *http.Request) {
	var req skillInstallRequest

	if err := readBody(r, &req); err != nil || req.Source == "" {
		writeErr(w, 400, errors.New("source is required"))

		return
	}

	opts, err := req.options()
	if err != nil {
		writeErr(w, 400, err)

		return
	}

	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	lib := skills.Open(d.home)
	cands, _, err := fetchCandidates(lib, req.Source, req.Skills, req.All)

	if err != nil {
		writeErr(w, 400, err)

		return
	}

	reports, _ := installCandidates(nil, s, lib, cands, opts, true)

	for _, rep := range reports {
		if rep.Record != nil {
			d.log.add(Activity{Kind: "skills.installed", Title: fmt.Sprintf("Installed skill %s (%s)", rep.Record.Name, skillRef(*rep.Record))})
		}
	}

	d.bus.publish("skills", nil)
	writeOK(w, map[string]any{"reports": reports})
}

func (d *daemon) postSkillPackInstall(w http.ResponseWriter, r *http.Request) {
	var req skillInstallRequest

	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, err)

		return
	}

	p, err := skills.FindPack(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)

		return
	}

	opts, err := req.options()
	if err != nil {
		writeErr(w, 400, err)

		return
	}

	opts.Pack = p.ID
	s, err := d.state()

	if err != nil {
		writeErr(w, 500, err)

		return
	}

	lib := skills.Open(d.home)
	var reports []installReport
	var members []string
	failures := []string{}

	for _, m := range p.Members {
		cands, _, err := fetchCandidates(lib, m.Source, nil, false)
		if err != nil {
			failures = append(failures, m.Name+": "+err.Error())

			continue
		}

		rep, failed := installCandidates(nil, s, lib, cands, opts, true)
		reports = append(reports, rep...)

		for _, x := range rep {
			if x.Record != nil {
				members = append(members, x.Record.ID)
			}
		}

		if failed > 0 && !m.Optional {
			failures = append(failures, m.Name+": not installed")
		}
	}

	lib.RecordPack(p, members, time.Now())
	d.log.add(Activity{Kind: "skills.pack", Title: fmt.Sprintf("Installed %d of %d skills from the %s pack", len(members), len(p.Members), p.Name)})
	d.bus.publish("skills", nil)
	writeOK(w, map[string]any{"pack": p.ID, "installed": members, "failures": failures, "reports": reports})
}

type skillResolveRequest struct {
	Text       string   `json:"text"`
	Provider   string   `json:"provider"`
	Skills     []string `json:"skills,omitempty"`
	Mode       string   `json:"mode,omitempty"`
	ProjectDir string   `json:"project_dir,omitempty"`
}

func (d *daemon) postSkillResolve(w http.ResponseWriter, r *http.Request) {
	var req skillResolveRequest

	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, err)

		return
	}

	if req.Provider == "" {
		req.Provider = provider.ClaudeID
	}

	if _, err := checkProviders([]string{req.Provider}); err != nil {
		writeErr(w, 400, err)

		return
	}

	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	records, err := skills.Open(d.home).Records()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	pol := s.Config.Skills

	if req.Mode != "" {
		pol.Mode = req.Mode
	}

	project := ""

	if req.ProjectDir != "" {
		project = detectProject(req.ProjectDir).Dir
	}

	res := skills.Resolve(records, provider.SkillsAgent(req.Provider), skills.Request{Text: req.Text, Provider: req.Provider, Project: project, Explicit: req.Skills, Facts: skills.ProjectFacts(project)}, pol)
	writeOK(w, map[string]any{"result": res, "needs_decision": res.NeedsDecision()})
}

func (d *daemon) postSkillCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}

	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, err)

		return
	}

	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	writeOK(w, checkSkillUpdates(s, skills.Open(d.home), req.IDs, false, skills.UpdateOptions{}))
}

type skillActionRequest struct {
	ID          string   `json:"id"`
	Providers   []string `json:"providers,omitempty"`
	Global      *bool    `json:"global,omitempty"`
	ProjectDir  string   `json:"project_dir,omitempty"`
	AddProject  bool     `json:"add_project,omitempty"`
	Dir         string   `json:"dir,omitempty"`
	KeepChanged bool     `json:"keep_changed,omitempty"`
	Unpin       bool     `json:"unpin,omitempty"`
	AcceptRisk  bool     `json:"accept_risk,omitempty"`
}

// postSkillAction applies one change to an installed skill; each one ends with a sync so the agents' folders match.
func (d *daemon) postSkillAction(w http.ResponseWriter, r *http.Request) {
	var req skillActionRequest

	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, err)

		return
	}

	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	lib := skills.Open(d.home)
	action := r.PathValue("action")

	if action == "sync" {
		lib.CleanStaging()

		out, err := syncSkills(lib, s, nil)
		if err != nil {
			writeErr(w, 500, err)

			return
		}

		d.bus.publish("skills", nil)
		writeOK(w, out)

		return
	}

	rec, err := lib.Find(req.ID)
	if err != nil {
		writeErr(w, 404, err)

		return
	}

	var result any

	switch action {
	case "enable", "disable":
		if _, err = checkProviders(req.Providers); err == nil {
			err = lib.UpdateRecord(rec.ID, func(x *skills.Record) error { applyEnable(x, action == "enable", req.Providers); return nil })
		}
	case "scope":
		err = lib.UpdateRecord(rec.ID, func(x *skills.Record) error {
			if req.Global != nil {
				x.Global = *req.Global
			}

			if req.ProjectDir != "" {
				dir := detectProject(req.ProjectDir).Dir
				x.Projects = slices.DeleteFunc(x.Projects, func(p string) bool { return p == dir })

				if req.AddProject {
					x.Projects = append(x.Projects, dir)
				}
			}

			return nil
		})
	case "remove":
		var res skills.RemoveResult

		res, err = lib.Remove(rec.ID, req.KeepChanged)
		result = res
	case "update":
		reports := checkSkillUpdates(s, lib, []string{rec.ID}, true, skills.UpdateOptions{Unpin: req.Unpin, AcceptRisk: req.AcceptRisk})
		result = reports

		if len(reports) == 1 && reports[0].Error != "" {
			err = errors.New(reports[0].Error)
		}
	case "rollback":
		_, err = lib.Rollback(rec.ID, time.Now())
	case "keep":
		if p, ok := projectionFor(*rec, req.Dir); !ok {
			err = fmt.Errorf("%s is not a copy csm placed for %s", req.Dir, rec.Name)
		} else if updated, aerr := lib.Adopt(rec.ID, p.Dir, time.Now()); aerr != nil {
			err = aerr
		} else {
			err = skills.Reclaim(p.Dir, rec.ID, updated.Hash, updated.Revision)
		}
	case "revert":
		p, ok := projectionFor(*rec, req.Dir)

		switch {
		case !ok:
			err = fmt.Errorf("%s is not a copy csm placed for %s", req.Dir, rec.Name)
		default:
			backup, berr := lib.Backup(p.Dir)

			if berr != nil {
				err = fmt.Errorf("could not back up the edited copy, so it was left alone: %w", berr)
			} else if err = os.RemoveAll(p.Dir); err == nil {
				result = map[string]string{"backup": backup}
			}
		}
	default:
		writeErr(w, 404, fmt.Errorf("unknown skill action %q", action))

		return
	}

	if err != nil {
		writeErr(w, 400, err)

		return
	}

	outcomes, _ := syncSkills(lib, s, func(x skills.Record) bool { return x.ID == rec.ID })
	d.log.add(Activity{Kind: "skills." + action, Title: fmt.Sprintf("Skill %s: %s", rec.Name, action)})
	d.bus.publish("skills", nil)
	writeOK(w, map[string]any{"result": result, "placements": outcomes[rec.ID]})
}

// checkSkillUpdates reads each skill's source again and, when apply is set, moves the ones with a newer version.
func checkSkillUpdates(s *State, lib *skills.Library, ids []string, apply bool, opts skills.UpdateOptions) []updateReport {
	records, err := lib.Records()
	if err != nil {
		return []updateReport{{Status: "failed", Error: err.Error()}}
	}

	f := newSkillFetcher(lib)
	reports := []updateReport{}

	for _, r := range records {
		if len(ids) > 0 && !slices.Contains(ids, r.ID) && !slices.Contains(ids, r.Name) {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		cand, up := f.Check(ctx, r)
		cancel()

		lib.UpdateRecord(r.ID, func(rec *skills.Record) error { rec.Upstream = &up; return nil })

		rep := updateReport{Name: r.Name, Upstream: &up}

		if cand == nil || up.Error != "" {
			rep.Status, rep.Error = "unreachable", up.Error
			reports = append(reports, rep)

			continue
		}

		cand.ID = r.ID
		plan, err := lib.PlanUpdate(r, *cand)

		switch {
		case err != nil:
			rep.Status, rep.Error = "invalid", err.Error()
		case plan.UpToDate:
			rep.Status, rep.Plan = "up to date", &plan
		case !apply:
			rep.Status, rep.Plan = "update available", &plan
		default:
			rep.Plan = &plan

			updated, err := lib.ApplyUpdate(plan, opts, time.Now())
			if err != nil {
				rep.Status, rep.Error = "not updated", err.Error()

				break
			}

			rep.Status = "updated to " + updated.VersionLabel()
			outcomes, _ := syncSkills(lib, s, func(x skills.Record) bool { return x.ID == r.ID })
			rep.Outcomes = outcomes[r.ID]
		}

		reports = append(reports, rep)
	}

	return reports
}
