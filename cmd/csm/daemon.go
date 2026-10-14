package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onoja123/csm/internal/handoff"
	"github.com/onoja123/csm/internal/mcp"
	"github.com/onoja123/csm/internal/provider"
)

const (
	desktopSettingsFile = "desktop.json"
	maxRecentProjects   = 12
	providerCacheFor    = 5 * time.Minute
)

type desktopSettings struct {
	ApprovalsEnabled *bool    `json:"approvals_enabled,omitempty"`
	RecentProjects   []string `json:"recent_projects,omitempty"`
}

func (s desktopSettings) approvals() bool {
	return s.ApprovalsEnabled == nil || *s.ApprovalsEnabled
}

type providerInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	Error     string `json:"error,omitempty"`
	Resume    bool   `json:"resume"`
}

type daemon struct {
	home      string
	csmPath   string
	version   string
	startedAt time.Time
	bus       *eventBus
	log       *activityLog
	runs      *runManager
	approvals *approvalQueue
	builds    *buildEngine
	signals   *signalStore
	hub       *hubRunner

	mu        sync.Mutex
	settings  desktopSettings
	providers []providerInfo
	probedAt  time.Time
}

func newDaemon(home, csmPath, version string) (*daemon, error) {
	bus := newEventBus()
	log := newActivityLog(home, bus)
	d := &daemon{home: home, csmPath: csmPath, version: version, startedAt: time.Now(), bus: bus, log: log, approvals: newApprovalQueue(bus, log)}
	d.runs = newRunManager(home, csmPath, bus, log)
	d.builds = newBuildEngine(home, d.runs, log, bus)
	d.signals = newSignalStore(home)
	d.hub = &hubRunner{d: d}
	readJSON(filepath.Join(home, desktopSettingsFile), &d.settings)

	return d, nil
}

func (d *daemon) saveSettings() error {
	return writeJSON(filepath.Join(d.home, desktopSettingsFile), d.settings)
}

func (d *daemon) state() (*State, error) { return loadState(d.home) }

type apiError struct {
	Error string `json:"error"`
}

func writeErr(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(apiError{Error: err.Error()})
}

func writeOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")

	if v == nil {
		v = map[string]bool{"ok": true}
	}

	json.NewEncoder(w).Encode(v)
}

func readBody(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}

	if len(data) == 0 {
		return nil
	}

	return json.Unmarshal(data, v)
}

func (d *daemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", d.getStatus)
	mux.HandleFunc("GET /v1/settings", d.getSettings)
	mux.HandleFunc("PUT /v1/settings", d.putSettings)
	mux.HandleFunc("GET /v1/events", d.events)
	mux.HandleFunc("GET /v1/activity", d.getActivity)
	mux.HandleFunc("GET /v1/projects", d.getProjects)
	mux.HandleFunc("POST /v1/projects", d.postProject)
	mux.HandleFunc("GET /v1/accounts", d.getAccounts)
	mux.HandleFunc("GET /v1/accounts/{name}", d.getAccount)
	mux.HandleFunc("POST /v1/accounts/{name}/switch", d.switchAccount)
	mux.HandleFunc("POST /v1/accounts/{name}/enable", d.setEnabled(true))
	mux.HandleFunc("POST /v1/accounts/{name}/disable", d.setEnabled(false))
	mux.HandleFunc("POST /v1/accounts/{name}/priority", d.setPriority)
	mux.HandleFunc("POST /v1/accounts/{name}/usage/refresh", d.refreshAccountUsage)
	mux.HandleFunc("GET /v1/failover", d.getFailover)
	mux.HandleFunc("PUT /v1/failover", d.putFailover)
	mux.HandleFunc("GET /v1/runs", d.getRuns)
	mux.HandleFunc("POST /v1/runs", d.postRun)
	mux.HandleFunc("GET /v1/runs/{id}", d.getRun)
	mux.HandleFunc("GET /v1/runs/{id}/output", d.getRunOutput)
	mux.HandleFunc("GET /v1/runs/{id}/screen", d.getRunScreen)
	mux.HandleFunc("POST /v1/runs/{id}/input", d.postRunInput)
	mux.HandleFunc("POST /v1/runs/{id}/resize", d.postRunResize)
	mux.HandleFunc("POST /v1/runs/{id}/stop", d.postRunStop)
	mux.HandleFunc("DELETE /v1/runs/{id}", d.deleteRun)
	mux.HandleFunc("GET /v1/sessions", d.getSessions)
	mux.HandleFunc("POST /v1/sessions/resume", d.resumeSession)
	mux.HandleFunc("GET /v1/handoffs", d.getHandoffs)
	mux.HandleFunc("GET /v1/handoffs/{project}/{id}", d.getHandoff)
	mux.HandleFunc("POST /v1/handoffs", d.postHandoff)
	mux.HandleFunc("PATCH /v1/handoffs/{project}/{id}", d.patchHandoff)
	mux.HandleFunc("POST /v1/checkpoints", d.postCheckpoint)
	mux.HandleFunc("GET /v1/builds", d.getBuilds)
	mux.HandleFunc("GET /v1/builds/preflight", d.getBuildPreflight)
	mux.HandleFunc("POST /v1/builds", d.postBuild)
	mux.HandleFunc("GET /v1/builds/{id}", d.getBuild)
	mux.HandleFunc("GET /v1/builds/{id}/diff", d.getBuildDiff)
	mux.HandleFunc("GET /v1/builds/{id}/receipt", d.getBuildReceipt)
	mux.HandleFunc("PUT /v1/builds/{id}/plan", d.putBuildPlan)
	mux.HandleFunc("POST /v1/builds/{id}/actions/{action}", d.postBuildAction)
	mux.HandleFunc("GET /v1/signals", d.getSignals)
	mux.HandleFunc("POST /v1/signals", d.postSignal)
	mux.HandleFunc("POST /v1/signals/sentry", d.postSentrySignal)
	mux.HandleFunc("POST /v1/incidents/{id}/runs", d.postIncidentRun)
	mux.HandleFunc("GET /v1/autofix", d.getAutofixConfig)
	mux.HandleFunc("PUT /v1/autofix", d.putAutofixConfig)
	mux.HandleFunc("POST /v1/autofix/hub/signin", d.postHubSignin)
	mux.HandleFunc("POST /v1/autofix/hub/signout", d.postHubSignout)
	mux.HandleFunc("POST /v1/autofix/hub/setup", d.postHubSetup)
	mux.HandleFunc("PUT /v1/autofix/hub/secret", d.putHubSecret)
	mux.HandleFunc("GET /v1/autofix/hub/status", d.getHubStatus)
	mux.HandleFunc("POST /v1/autofix/hub/test", d.postHubTest)
	mux.HandleFunc("POST /v1/sentry/fix", d.postSentryFix)
	mux.HandleFunc("GET /v1/mcp", d.getMCP)
	mux.HandleFunc("GET /v1/approvals", d.getApprovals)
	mux.HandleFunc("POST /v1/approvals/{id}/decision", d.decideApproval)
	mux.HandleFunc("POST /v1/hooks/pre-tool-use", d.hookPreToolUse)

	return mux
}

// Status

type statusView struct {
	Service struct {
		Version   string    `json:"version"`
		PID       int       `json:"pid"`
		StartedAt time.Time `json:"started_at"`
		Socket    string    `json:"socket"`
		Home      string    `json:"home"`
	} `json:"service"`
	Policy           string         `json:"failover_policy"`
	Providers        []providerInfo `json:"providers"`
	Accounts         int            `json:"accounts"`
	LiveRuns         int            `json:"live_runs"`
	PendingApprovals int            `json:"pending_approvals"`
	ApprovalsEnabled bool           `json:"approvals_enabled"`
}

func (d *daemon) probeProviders() []providerInfo {
	d.mu.Lock()
	cached, at := d.providers, d.probedAt
	d.mu.Unlock()

	if cached != nil && time.Since(at) < providerCacheFor {
		return cached
	}

	var infos []providerInfo

	for _, id := range provider.IDs {
		info := providerInfo{ID: id, Name: providerName(id)}
		p, err := provider.Find(id)

		if err == nil {
			info.Installed = true
			info.Version, err = p.Version()

			if f, ferr := p.Features(); ferr == nil {
				info.Resume = f.Resume
			}
		}

		if err != nil {
			info.Error = firstLine(err.Error())
		}

		infos = append(infos, info)
	}

	d.mu.Lock()
	d.providers, d.probedAt = infos, time.Now()
	d.mu.Unlock()

	return infos
}

func (d *daemon) status(s *State) statusView {
	var v statusView
	v.Service.Version = d.version
	v.Service.PID = os.Getpid()
	v.Service.StartedAt = d.startedAt
	v.Service.Socket = socketPath(d.home)
	v.Service.Home = d.home
	v.Policy = s.Config.policy()
	v.Providers = d.probeProviders()
	v.Accounts = len(s.Accounts)

	for _, r := range d.runs.list() {
		if r.Status == runStatusLive {
			v.LiveRuns++
		}
	}

	pending, _ := d.approvals.list()
	v.PendingApprovals = len(pending)
	d.mu.Lock()
	v.ApprovalsEnabled = d.settings.approvals()
	d.mu.Unlock()

	return v
}

func (d *daemon) getStatus(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	writeOK(w, d.status(s))
}

func (d *daemon) getSettings(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()

	writeOK(w, map[string]any{"approvals_enabled": d.settings.approvals(), "recent_projects": d.settings.RecentProjects})
}

func (d *daemon) putSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ApprovalsEnabled *bool `json:"approvals_enabled"`
	}

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	d.mu.Lock()

	if in.ApprovalsEnabled != nil {
		d.settings.ApprovalsEnabled = in.ApprovalsEnabled
	}

	err := d.saveSettings()
	d.mu.Unlock()

	if err != nil {
		writeErr(w, 500, err)

		return
	}

	d.bus.publish("status", nil)
	d.getSettings(w, r)
}

// Events

func (d *daemon) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, errors.New("streaming unsupported"))

		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	ch, cancel := d.bus.subscribe()

	defer cancel()

	write := func(ev Event) bool {
		data := ev.Data

		if data == nil {
			data = json.RawMessage("null")
		}

		_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
		flusher.Flush()

		return err == nil
	}

	hello, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "version": d.version})
	write(Event{Type: "hello", Data: hello})
	ticker := time.NewTicker(15 * time.Second)

	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if !write(ev) {
				return
			}
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (d *daemon) getActivity(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	if limit <= 0 {
		limit = 100
	}

	entries := d.log.list(limit)

	if entries == nil {
		entries = []Activity{}
	}

	writeOK(w, entries)
}

// Projects

type projectView struct {
	Dir      string    `json:"dir"`
	Display  string    `json:"display"`
	Branch   string    `json:"branch,omitempty"`
	IsGit    bool      `json:"is_git"`
	LastSeen time.Time `json:"last_seen,omitzero"`
	Exists   bool      `json:"exists"`
}

func (d *daemon) projects(s *State) []projectView {
	seen := map[string]*projectView{}
	add := func(dir string, at time.Time) {
		if dir == "" {
			return
		}

		if p, ok := seen[dir]; ok {
			if at.After(p.LastSeen) {
				p.LastSeen = at
			}

			return
		}

		_, err := os.Stat(dir)
		p := &projectView{Dir: dir, Display: displayPath(dir), LastSeen: at, Exists: err == nil}

		if p.Exists {
			proj := detectProject(dir)
			p.Branch, p.IsGit = proj.Branch, proj.IsGit
		}

		seen[dir] = p
	}

	d.mu.Lock()
	recent := slices.Clone(d.settings.RecentProjects)
	d.mu.Unlock()

	for i, dir := range recent {
		add(dir, time.Now().Add(-time.Duration(i)*time.Second))
	}

	for _, r := range d.runs.list() {
		add(r.ProjectDir, r.StartedAt)
	}

	entries, _ := os.ReadDir(s.projectsDir())

	for _, e := range entries {
		dir := filepath.Join(s.projectsDir(), e.Name())
		var sess Session
		var cp Checkpoint

		if readJSON(filepath.Join(dir, sessionFile), &sess) == nil {
			add(sess.ProjectDir, sess.StartedAt)
		}

		if readJSON(filepath.Join(dir, checkpointFile), &cp) == nil {
			add(cp.ProjectDir, cp.CheckpointedAt)
		}
	}

	out := make([]projectView, 0, len(seen))

	for _, p := range seen {
		out = append(out, *p)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })

	return out
}

func (d *daemon) getProjects(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	writeOK(w, d.projects(s))
}

func (d *daemon) postProject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Dir string `json:"dir"`
	}

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	dir, err := cleanProjectDir(in.Dir)
	if err != nil {
		writeErr(w, 400, err)

		return
	}

	d.rememberProject(dir)
	d.bus.publish("projects", nil)
	writeOK(w, projectView{Dir: dir, Display: displayPath(dir), Exists: true})
}

func (d *daemon) rememberProject(dir string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.settings.RecentProjects = slices.DeleteFunc(d.settings.RecentProjects, func(p string) bool { return p == dir })
	d.settings.RecentProjects = append([]string{dir}, d.settings.RecentProjects...)

	if len(d.settings.RecentProjects) > maxRecentProjects {
		d.settings.RecentProjects = d.settings.RecentProjects[:maxRecentProjects]
	}

	d.saveSettings()
}

// Accounts

type accountView struct {
	Name             string          `json:"name"`
	Provider         string          `json:"provider"`
	ProviderName     string          `json:"provider_name"`
	Email            string          `json:"email,omitempty"`
	Active           bool            `json:"active"`
	Enabled          bool            `json:"enabled"`
	Health           string          `json:"health"`
	Status           string          `json:"status"`
	Priority         int             `json:"priority"`
	UsageLabel       string          `json:"usage_label"`
	Usage            *provider.Usage `json:"usage,omitempty"`
	UsageUpdatedAt   time.Time       `json:"usage_updated_at,omitzero"`
	LastUsedAt       time.Time       `json:"last_used_at,omitzero"`
	LastSuccessAt    time.Time       `json:"last_success_at,omitzero"`
	LastFailureAt    time.Time       `json:"last_failure_at,omitzero"`
	LastFailure      string          `json:"last_failure,omitempty"`
	LastFailureText  string          `json:"last_failure_text,omitempty"`
	Failures24h      int             `json:"failures_24h"`
	CooldownUntil    time.Time       `json:"cooldown_until,omitzero"`
	VerifiedAt       time.Time       `json:"verified_at,omitzero"`
	CreatedAt        time.Time       `json:"created_at"`
	ProfileDir       string          `json:"profile_dir"`
	FailoverEligible bool            `json:"failover_eligible"`
	Advice           string          `json:"advice,omitempty"`
	Running          bool            `json:"running"`
}

func (d *daemon) accountView(s *State, a *Account, now time.Time, live []Session) accountView {
	rec, hasUsage := s.loadUsage(a.Name)
	h := a.health(now, rec, hasUsage)
	v := accountView{
		Name:          a.Name,
		Provider:      a.providerID(),
		ProviderName:  providerName(a.providerID()),
		Email:         a.Email,
		Active:        s.active(a.providerID()) == a.Name,
		Enabled:       a.Enabled,
		Health:        string(h),
		Status:        string(a.status(now)),
		Priority:      s.priority(a),
		UsageLabel:    s.usageLabel(a.Name, now),
		LastUsedAt:    a.LastUsedAt,
		LastSuccessAt: a.LastSuccessAt,
		LastFailureAt: a.LastFailureAt,
		Failures24h:   len(a.recentFailures(now)),
		VerifiedAt:    a.VerifiedAt,
		CreatedAt:     a.CreatedAt,
		ProfileDir:    displayPath(a.ConfigDir),
		Advice:        healthAdvice(h, a),
	}

	if hasUsage && !rec.Usage.Empty() {
		u := rec.Usage
		v.Usage = &u
		v.UsageUpdatedAt = rec.UpdatedAt
	}

	if a.LastFailure != "" {
		v.LastFailure = string(a.LastFailure)
		v.LastFailureText = a.LastFailure.Describe()
	}

	if now.Before(a.CooldownUntil) {
		v.CooldownUntil = a.CooldownUntil
	}

	v.FailoverEligible = h.usable() && a.status(now) == statusReady

	for _, sess := range live {
		if sess.Account == a.Name {
			v.Running = true
		}
	}

	return v
}

func (d *daemon) getAccounts(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	now := time.Now()
	live := s.liveSessions()
	out := []accountView{}

	for _, id := range provider.IDs {
		for _, name := range s.accountNames(id) {
			if a, err := s.resolveAccount(name); err == nil {
				out = append(out, d.accountView(s, a, now, live))
			}
		}
	}

	writeOK(w, out)
}

func (d *daemon) getAccount(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	a, err := s.resolveAccount(r.PathValue("name"))
	if err != nil {
		writeErr(w, 404, err)

		return
	}

	writeOK(w, d.accountView(s, a, time.Now(), s.liveSessions()))
}

type switchResult struct {
	Mode    string `json:"mode"`
	Account string `json:"account"`
	Project string `json:"project,omitempty"`
	Message string `json:"message"`
}

func (d *daemon) switchAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProjectDir string `json:"project_dir"`
	}

	readBody(r, &in)
	s, err := d.state()

	if err != nil {
		writeErr(w, 500, err)

		return
	}

	target, err := s.resolveAccount(r.PathValue("name"))
	if err != nil {
		writeErr(w, 404, err)

		return
	}

	switch target.status(time.Now()) {
	case statusDisabled:
		writeErr(w, 409, fmt.Errorf("%s is disabled; enable it first", target.Name))

		return
	case statusNotAuthenticated:
		writeErr(w, 409, fmt.Errorf("%s is not authenticated; run: csm account login %s", target.Name, target.Name))

		return
	}

	p, err := provider.Find(target.providerID())
	if err != nil {
		writeErr(w, 409, err)

		return
	}

	if _, err := verifyProfile(p, target); err != nil {
		writeErr(w, 409, err)

		return
	}

	res, err := d.doSwitch(s, target, in.ProjectDir)
	if err != nil {
		writeErr(w, 409, err)

		return
	}

	d.bus.publish("accounts", nil)
	writeOK(w, res)
}

func (d *daemon) doSwitch(s *State, target *Account, projectDir string) (switchResult, error) {
	var live []Session

	for _, sess := range s.liveSessions() {
		if providerOrDefault(sess.Provider) == target.providerID() && (projectDir == "" || sess.ProjectDir == projectDir) {
			live = append(live, sess)
		}
	}

	if len(live) > 1 {
		return switchResult{}, errors.New("several sessions of this agent are running; pass project_dir to choose one")
	}

	if len(live) == 1 {
		sess := live[0]

		if sess.Account == target.Name {
			return switchResult{Mode: "none", Account: target.Name, Project: sess.ProjectDir, Message: "The running session already uses " + target.Name + "."}, nil
		}

		if err := requestSwitch(&Logger{out: io.Discard, err: io.Discard}, s, sess, target.Name); err != nil {
			return switchResult{}, err
		}

		d.log.add(Activity{Kind: "switch.requested", Provider: target.providerID(), Account: target.Name, Title: "Switch requested: " + sess.Account + " → " + target.Name, Detail: displayPath(sess.ProjectDir)})

		return switchResult{Mode: "session", Account: target.Name, Project: sess.ProjectDir, Message: "The running session is checkpointing and restarting as " + target.Name + "."}, nil
	}

	err := s.update(func(state *State) error {
		state.setActive(target.providerID(), target.Name)

		return nil
	})
	if err != nil {
		return switchResult{}, err
	}

	d.log.add(Activity{Kind: "account.activated", Provider: target.providerID(), Account: target.Name, Title: "Active " + providerName(target.providerID()) + " account is now " + target.Name})

	return switchResult{Mode: "active", Account: target.Name, Message: "Active " + providerName(target.providerID()) + " account changed to " + target.Name + "."}, nil
}

func (d *daemon) setEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, err := d.state()
		if err != nil {
			writeErr(w, 500, err)

			return
		}

		name := r.PathValue("name")
		err = s.update(func(state *State) error {
			a, err := state.resolveAccount(name)
			if err != nil {
				return err
			}

			a.Enabled = enabled

			return nil
		})
		if err != nil {
			writeErr(w, 404, err)

			return
		}

		d.bus.publish("accounts", nil)
		a, _ := s.resolveAccount(name)
		writeOK(w, d.accountView(s, a, time.Now(), s.liveSessions()))
	}
}

func (d *daemon) setPriority(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Position int `json:"position"`
	}

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	name := r.PathValue("name")

	if err := s.update(func(state *State) error { return state.setPriority(name, in.Position) }); err != nil {
		writeErr(w, 400, err)

		return
	}

	d.bus.publish("accounts", nil)
	a, _ := s.resolveAccount(name)
	writeOK(w, d.accountView(s, a, time.Now(), s.liveSessions()))
}

func (d *daemon) refreshAccountUsage(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	a, err := s.resolveAccount(r.PathValue("name"))
	if err != nil {
		writeErr(w, 404, err)

		return
	}

	p, err := usageProvider(a.providerID())
	if err != nil {
		writeErr(w, 409, err)

		return
	}

	if errs := refreshUsage(s, p, []string{a.Name}, time.Now()); errs[a.Name] != nil {
		writeErr(w, 502, errs[a.Name])

		return
	}

	d.bus.publish("accounts", nil)
	writeOK(w, d.accountView(s, a, time.Now(), s.liveSessions()))
}

// Failover

type failoverView struct {
	Policy      string                 `json:"policy"`
	Description string                 `json:"description"`
	Providers   []failoverProviderView `json:"providers"`
	HandoffTo   []string               `json:"handoff_to"`
	HandoffText string                 `json:"handoff_description"`
}

type failoverProviderView struct {
	Provider     string   `json:"provider"`
	ProviderName string   `json:"provider_name"`
	Priority     []string `json:"priority"`
	WouldSwitch  []string `json:"would_switch"`
}

func (d *daemon) failover(s *State) failoverView {
	v := failoverView{Policy: s.Config.policy(), Description: describePolicy(s.Config.policy()), Providers: []failoverProviderView{}, HandoffTo: append([]string{}, s.Config.HandoffTo...), HandoffText: describeHandoffTo(s.Config.HandoffTo)}
	now := time.Now()

	for _, id := range s.providersInUse() {
		pv := failoverProviderView{Provider: id, ProviderName: providerName(id), Priority: s.accountNames(id), WouldSwitch: []string{}}

		for _, c := range s.rankAccounts(id, nil, now) {
			pv.WouldSwitch = append(pv.WouldSwitch, c.account.Name)
		}

		v.Providers = append(v.Providers, pv)
	}

	return v
}

func (d *daemon) getFailover(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	writeOK(w, d.failover(s))
}

func (d *daemon) putFailover(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Policy    string    `json:"policy"`
		HandoffTo *[]string `json:"handoff_to"`
	}

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	err = s.update(func(state *State) error {
		if in.Policy != "" {
			if err := state.Config.setPolicy(in.Policy); err != nil {
				return err
			}
		}

		if in.HandoffTo != nil {
			return state.Config.setHandoffTo(*in.HandoffTo)
		}

		return nil
	})
	if err != nil {
		writeErr(w, 400, err)

		return
	}

	if in.Policy != "" {
		d.log.add(Activity{Kind: "failover.policy", Title: "Failover policy set to " + s.Config.policy()})
	}

	if in.HandoffTo != nil {
		d.log.add(Activity{Kind: "failover.policy", Title: "Cross-agent handoff: " + describeHandoffTo(s.Config.HandoffTo)})
	}
	d.bus.publish("status", nil)
	writeOK(w, d.failover(s))
}

// Runs

type externalRun struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Provider   string    `json:"provider"`
	Account    string    `json:"account"`
	ProjectDir string    `json:"project_dir"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	CSMPID     int       `json:"csm_pid"`
	SessionID  string    `json:"session_id,omitempty"`
	AgentPID   int       `json:"agent_pid,omitempty"`
	Duration   int64     `json:"duration_seconds"`
}

type runsView struct {
	Runs     []runView     `json:"runs"`
	External []externalRun `json:"external"`
}

func (d *daemon) runsView(s *State) runsView {
	v := runsView{Runs: []runView{}, External: []externalRun{}}
	managed := map[int]bool{}

	for _, r := range d.runs.list() {
		v.Runs = append(v.Runs, d.runs.view(s, r))
		managed[r.CSMPID] = true
	}

	for _, sess := range s.liveSessions() {
		if managed[sess.CSMPID] {
			continue
		}

		v.External = append(v.External, externalRun{
			ID:         "external-" + strconv.Itoa(sess.CSMPID),
			Kind:       "external",
			Provider:   providerOrDefault(sess.Provider),
			Account:    sess.Account,
			ProjectDir: sess.ProjectDir,
			Status:     runStatusLive,
			StartedAt:  sess.StartedAt,
			CSMPID:     sess.CSMPID,
			SessionID:  sess.SessionID,
			AgentPID:   sess.ProcessPID,
			Duration:   int64(time.Since(sess.StartedAt).Seconds()),
		})
	}

	return v
}

func (d *daemon) getRuns(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	writeOK(w, d.runsView(s))
}

func (d *daemon) postRun(w http.ResponseWriter, r *http.Request) {
	var req startRequest

	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, err)

		return
	}

	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	rn, err := d.runs.start(s, req)
	if err != nil {
		writeErr(w, 409, err)

		return
	}

	d.rememberProject(rn.ProjectDir)
	writeOK(w, d.runs.view(s, rn))
}

func (d *daemon) lookupRun(w http.ResponseWriter, r *http.Request) (*agentRun, bool) {
	rn, ok := d.runs.get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, errors.New("no such run"))
	}

	return rn, ok
}

func (d *daemon) getRun(w http.ResponseWriter, r *http.Request) {
	rn, ok := d.lookupRun(w, r)
	if !ok {
		return
	}

	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	writeOK(w, d.runs.view(s, rn))
}

func (d *daemon) getRunOutput(w http.ResponseWriter, r *http.Request) {
	rn, ok := d.lookupRun(w, r)
	if !ok {
		return
	}

	writeOK(w, map[string]string{"run_id": rn.ID, "data": base64.StdEncoding.EncodeToString(rn.output())})
}

func (d *daemon) getRunScreen(w http.ResponseWriter, r *http.Request) {
	rn, ok := d.lookupRun(w, r)
	if !ok {
		return
	}

	writeOK(w, rn.screen())
}

func (d *daemon) postRunInput(w http.ResponseWriter, r *http.Request) {
	rn, ok := d.lookupRun(w, r)
	if !ok {
		return
	}

	var in struct {
		Data string `json:"data"`
		Text string `json:"text"`
	}

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	var data []byte

	if in.Data != "" {
		decoded, err := base64.StdEncoding.DecodeString(in.Data)
		if err != nil {
			writeErr(w, 400, err)

			return
		}

		data = decoded
	} else {
		data = []byte(in.Text)
	}

	if err := rn.write(data); err != nil {
		writeErr(w, 409, err)

		return
	}

	writeOK(w, nil)
}

func (d *daemon) postRunResize(w http.ResponseWriter, r *http.Request) {
	rn, ok := d.lookupRun(w, r)
	if !ok {
		return
	}

	var in struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	if err := rn.resize(in.Cols, in.Rows); err != nil {
		writeErr(w, 409, err)

		return
	}

	writeOK(w, nil)
}

func (d *daemon) postRunStop(w http.ResponseWriter, r *http.Request) {
	rn, ok := d.lookupRun(w, r)
	if !ok {
		return
	}

	rn.stop(stopGracePeriod)
	d.log.add(Activity{Kind: "run.stop", Provider: rn.Provider, Account: rn.Account, Title: "Stop requested for " + providerName(rn.Provider), RunID: rn.ID})
	writeOK(w, nil)
}

func (d *daemon) deleteRun(w http.ResponseWriter, r *http.Request) {
	if err := d.runs.forget(r.PathValue("id")); err != nil {
		writeErr(w, 409, err)

		return
	}

	writeOK(w, nil)
}

// Sessions

type sessionView struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	Provider       string    `json:"provider"`
	Account        string    `json:"account"`
	SessionID      string    `json:"session_id,omitempty"`
	ProjectDir     string    `json:"project_dir"`
	Branch         string    `json:"branch,omitempty"`
	StartedAt      time.Time `json:"started_at,omitzero"`
	CheckpointedAt time.Time `json:"checkpointed_at,omitzero"`
	Reason         string    `json:"reason,omitempty"`
	ReasonText     string    `json:"reason_text,omitempty"`
	Detail         string    `json:"detail,omitempty"`
	Live           bool      `json:"live"`
	CanResume      bool      `json:"can_resume"`
}

func (d *daemon) sessions(s *State) []sessionView {
	out := []sessionView{}
	entries, _ := os.ReadDir(s.projectsDir())
	resumable := map[string]bool{}

	for _, id := range provider.IDs {
		if p, err := provider.New(id, ""); err == nil {
			if f, err := p.Features(); err == nil {
				resumable[id] = f.Resume
			}
		}
	}

	for _, e := range entries {
		dir := filepath.Join(s.projectsDir(), e.Name())
		var sess Session

		if readJSON(filepath.Join(dir, sessionFile), &sess) == nil && processAlive(sess.CSMPID) {
			out = append(out, sessionView{ID: "live-" + strconv.Itoa(sess.CSMPID), Kind: "live", Provider: providerOrDefault(sess.Provider), Account: sess.Account, SessionID: sess.SessionID, ProjectDir: sess.ProjectDir, StartedAt: sess.StartedAt, Live: true})
		}

		files, _ := filepath.Glob(filepath.Join(dir, historyDir, "*.json"))

		for _, f := range files {
			var cp Checkpoint

			if readJSON(f, &cp) != nil {
				continue
			}

			pid := providerOrDefault(cp.Provider)
			out = append(out, sessionView{
				ID:             e.Name() + "/" + strings.TrimSuffix(filepath.Base(f), ".json"),
				Kind:           "checkpoint",
				Provider:       pid,
				Account:        cp.Account,
				SessionID:      cp.SessionID,
				ProjectDir:     cp.ProjectDir,
				Branch:         cp.Branch,
				StartedAt:      cp.StartedAt,
				CheckpointedAt: cp.CheckpointedAt,
				Reason:         string(cp.Reason),
				ReasonText:     cp.Reason.Describe(),
				Detail:         cp.Detail,
				CanResume:      cp.SessionID != "" && resumable[pid],
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].CheckpointedAt, out[j].CheckpointedAt

		if out[i].Live {
			ti = out[i].StartedAt
		}

		if out[j].Live {
			tj = out[j].StartedAt
		}

		return ti.After(tj)
	})

	return out
}

func (d *daemon) getSessions(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	writeOK(w, d.sessions(s))
}

func (d *daemon) resumeSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider   string `json:"provider"`
		ProjectDir string `json:"project_dir"`
		SessionID  string `json:"session_id"`
		Cols       int    `json:"cols"`
		Rows       int    `json:"rows"`
	}

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	if in.SessionID == "" {
		writeErr(w, 400, errors.New("session_id is required"))

		return
	}

	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	rn, err := d.runs.start(s, startRequest{Provider: in.Provider, ProjectDir: in.ProjectDir, Resume: in.SessionID, Cols: in.Cols, Rows: in.Rows})
	if err != nil {
		writeErr(w, 409, err)

		return
	}

	d.log.add(Activity{Kind: "session.resumed", Provider: rn.Provider, Account: rn.Account, Title: "Resuming a " + providerName(rn.Provider) + " session", Detail: in.SessionID, RunID: rn.ID})
	writeOK(w, d.runs.view(s, rn))
}

// Handoffs

type handoffView struct {
	handoff.State
	Project    string `json:"project"`
	ProjectDir string `json:"project_dir,omitempty"`
	Dir        string `json:"dir"`
	Markdown   string `json:"markdown,omitempty"`
	// Current compares a saved checkpoint with the repository as it is now; only the detail view computes it.
	Current *handoff.Verification `json:"current,omitempty"`
}

func (d *daemon) handoffs(s *State, withMarkdown func(handoffView) bool) []handoffView {
	out := []handoffView{}
	root := filepath.Join(s.Home, handoffsDir)
	projects, _ := os.ReadDir(root)

	for _, p := range projects {
		if !p.IsDir() {
			continue
		}

		ids, _ := os.ReadDir(filepath.Join(root, p.Name()))

		for _, e := range ids {
			if !e.IsDir() {
				continue
			}

			dir := filepath.Join(root, p.Name(), e.Name())
			st, err := handoff.Load(dir)

			if err != nil {
				continue
			}

			v := handoffView{State: st, Project: p.Name(), Dir: dir, ProjectDir: st.Git.Root}

			if withMarkdown != nil && withMarkdown(v) {
				if md, err := os.ReadFile(filepath.Join(dir, handoff.FileMD)); err == nil {
					v.Markdown = string(md)
				}

				if st.Kind == handoff.KindCheckpoint && st.Git.Root != "" {
					cur := handoff.Verify(st, st.Git.Root, time.Now())
					v.Current = &cur
				}
			}

			out = append(out, v)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })

	return out
}

func (d *daemon) getHandoffs(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	writeOK(w, d.handoffs(s, nil))
}

func (d *daemon) getHandoff(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	project, id := r.PathValue("project"), r.PathValue("id")

	for _, h := range d.handoffs(s, func(v handoffView) bool { return v.Project == project && v.ID == id }) {
		if h.Project == project && h.ID == id {
			writeOK(w, h)

			return
		}
	}

	writeErr(w, 404, errors.New("no such handoff"))
}

type handoffPreview struct {
	Mode          string   `json:"mode"`
	Source        string   `json:"source,omitempty"`
	SourceAccount string   `json:"source_account,omitempty"`
	Target        string   `json:"target"`
	TargetAccount string   `json:"target_account"`
	SessionID     string   `json:"session_id,omitempty"`
	TaskContext   string   `json:"task_context"`
	GitState      string   `json:"git_state"`
	Message       string   `json:"message"`
	RunID         string   `json:"run_id,omitempty"`
	Checkpoint    string   `json:"checkpoint,omitempty"`
	Changes       []string `json:"changes,omitempty"`
}

func (d *daemon) handoffView(s *State, dir string) (handoffView, error) {
	st, err := handoff.Load(dir)
	if err != nil {
		return handoffView{}, err
	}

	project := filepath.Base(filepath.Dir(dir))

	for _, v := range d.handoffs(s, func(v handoffView) bool { return v.Project == project && v.ID == st.ID }) {
		if v.Project == project && v.ID == st.ID {
			return v, nil
		}
	}

	return handoffView{}, errors.New("no such handoff")
}

type checkpointInput struct {
	ProjectDir string   `json:"project_dir"`
	Task       string   `json:"task"`
	NextSteps  []string `json:"next_steps"`
	Notes      string   `json:"notes"`
}

func (in checkpointInput) edits() handoff.Edits {
	return handoff.Edits{Task: in.Task, NextSteps: in.NextSteps, Notes: in.Notes}
}

func (d *daemon) postCheckpoint(w http.ResponseWriter, r *http.Request) {
	var in checkpointInput

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	dir, err := cleanProjectDir(in.ProjectDir)
	if err != nil {
		writeErr(w, 400, err)

		return
	}

	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	_, cpDir, err := s.captureCheckpoint(dir, in.edits(), time.Now())
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	d.rememberProject(dir)
	d.bus.publish("handoffs", nil)

	v, err := d.handoffView(s, cpDir)
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	writeOK(w, v)
}

func (d *daemon) patchHandoff(w http.ResponseWriter, r *http.Request) {
	var in checkpointInput

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	project, id := r.PathValue("project"), r.PathValue("id")

	if !handoff.ValidID(project) || !handoff.ValidID(id) {
		writeErr(w, 400, errors.New("invalid checkpoint path"))

		return
	}

	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	dir := filepath.Join(s.Home, handoffsDir, project, id)
	st, err := handoff.Load(dir)

	if err != nil {
		writeErr(w, 404, err)

		return
	}

	if err := editCheckpoint(dir, &st, in.edits(), time.Now()); err != nil {
		writeErr(w, 409, err)

		return
	}

	d.bus.publish("handoffs", nil)

	v, err := d.handoffView(s, dir)
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	writeOK(w, v)
}

func (d *daemon) postHandoff(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProjectDir    string `json:"project_dir"`
		TargetAccount string `json:"target_account"`
		CheckpointID  string `json:"checkpoint_id"`
		Preview       bool   `json:"preview"`
		Cols          int    `json:"cols"`
		Rows          int    `json:"rows"`
	}

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	target, err := s.resolveAccount(in.TargetAccount)
	if err != nil {
		writeErr(w, 404, err)

		return
	}

	dir, err := cleanProjectDir(in.ProjectDir)
	if err != nil {
		writeErr(w, 400, err)

		return
	}

	switch target.status(time.Now()) {
	case statusDisabled:
		writeErr(w, 409, fmt.Errorf("%s is disabled", target.Name))

		return
	case statusNotAuthenticated:
		writeErr(w, 409, fmt.Errorf("%s is not authenticated; run: csm account login %s", target.Name, target.Name))

		return
	}

	proj := detectProject(dir)
	pv := handoffPreview{Target: target.providerID(), TargetAccount: target.Name, GitState: "not a git repository", Checkpoint: in.CheckpointID}

	if proj.IsGit {
		pv.GitState = proj.Branch
	}

	var saved handoff.State

	if in.CheckpointID != "" {
		if saved, _, err = s.findHandoff(in.CheckpointID); err != nil {
			writeErr(w, 404, err)

			return
		}

		if saved.Kind != handoff.KindCheckpoint {
			writeErr(w, 409, fmt.Errorf("%s is a handoff record, not a saved checkpoint", in.CheckpointID))

			return
		}

		if saved.Git.Root != "" && detectProject(saved.Git.Root).Dir != proj.Dir {
			writeErr(w, 409, fmt.Errorf("checkpoint %s belongs to %s, not %s", in.CheckpointID, displayPath(saved.Git.Root), displayPath(proj.Dir)))

			return
		}

		v := handoff.Verify(saved, proj.Dir, time.Now())
		pv.Changes = v.Problems()
	}

	var live *Session

	for _, id := range provider.IDs {
		if sess, ok := s.loadLiveSession(id, proj.Dir); ok {
			sess := sess
			live = &sess
		}
	}

	if live != nil {
		pv.Mode, pv.Source, pv.SourceAccount, pv.SessionID = "session", providerOrDefault(live.Provider), live.Account, live.SessionID
		pv.TaskContext = "captured at the switch"
		pv.Message = "The running session will checkpoint, stop and continue as " + target.Name + "."

		if live.Account == target.Name {
			pv.Mode, pv.Message = "none", "The running session already uses "+target.Name+"."
		}
	} else if in.CheckpointID != "" {
		pv.Mode, pv.Source, pv.SourceAccount, pv.SessionID = "checkpoint", saved.Source.Provider, saved.Source.Account, saved.Source.SessionID
		pv.TaskContext = "from checkpoint " + saved.ID + " (" + ago(saved.CreatedAt, time.Now()) + ")"
		pv.Message = providerName(target.providerID()) + " will start here as " + target.Name + " with this checkpoint as its brief."
	} else if cp, found := s.latestCheckpoint(proj.Dir); found {
		pv.Mode, pv.Source, pv.SourceAccount, pv.SessionID = "checkpoint", providerOrDefault(cp.Provider), cp.Account, cp.SessionID
		pv.TaskContext = "from the last checkpoint (" + ago(cp.CheckpointedAt, time.Now()) + ")"
		pv.Message = providerName(target.providerID()) + " will start here as " + target.Name + " with the handoff."

		if cp.SessionID == "" {
			pv.TaskContext = "none; repository state only"
		}
	} else {
		pv.Mode, pv.TaskContext = "fresh", "none; repository state only"
		pv.Message = providerName(target.providerID()) + " will start here as " + target.Name + "; no previous agent is recorded for this project."
	}

	if in.Preview || pv.Mode == "none" {
		writeOK(w, pv)

		return
	}

	if pv.Mode == "session" {
		if err := requestSwitchWith(&Logger{out: io.Discard, err: io.Discard}, s, *live, target.Name, in.CheckpointID); err != nil {
			writeErr(w, 409, err)

			return
		}

		d.log.add(Activity{Kind: "handoff.requested", Provider: target.providerID(), Account: target.Name, Title: "Handoff requested: " + live.Account + " → " + target.Name, Detail: displayPath(proj.Dir)})
		writeOK(w, pv)

		return
	}

	rn, err := d.runs.startHandoff(s, target.providerID(), proj.Dir, target.Name, in.CheckpointID, in.Cols, in.Rows)
	if err != nil {
		writeErr(w, 409, err)

		return
	}

	d.rememberProject(proj.Dir)
	pv.RunID = rn.ID
	writeOK(w, pv)
}

// MCP

type mcpServerView struct {
	mcp.Server
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	RequiresAuth bool   `json:"requires_auth"`
}

type mcpAccountView struct {
	Account      string          `json:"account"`
	Provider     string          `json:"provider"`
	ProviderName string          `json:"provider_name"`
	Error        string          `json:"error,omitempty"`
	Servers      []mcpServerView `json:"servers"`
}

func (d *daemon) getMCP(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	cwd := r.URL.Query().Get("project_dir")

	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}

	out := []mcpAccountView{}

	for i := range s.Accounts {
		a := &s.Accounts[i]

		if !a.Enabled {
			continue
		}

		v := mcpAccountView{Account: a.Name, Provider: a.providerID(), ProviderName: providerName(a.providerID()), Servers: []mcpServerView{}}
		p, _ := provider.New(a.providerID(), "")
		adapter, ok := provider.MCPAdapter(p)

		if !ok {
			v.Error = mcp.ErrNoMCP.Error()
			out = append(out, v)

			continue
		}

		snap, err := mcp.Take(adapter, a.ConfigDir, cwd, time.Now())

		if err != nil {
			v.Error = firstLine(err.Error())
			out = append(out, v)

			continue
		}

		results := map[string]mcp.Result{}

		if inspected, err := mcp.Inspect(adapter, a.ConfigDir, cwd, envPresent); err == nil {
			for _, res := range inspected {
				results[res.Server] = res
			}
		}

		for _, srv := range snap.Servers {
			sv := mcpServerView{Server: srv, Status: string(mcp.Unknown)}

			if res, ok := results[srv.Name]; ok {
				sv.Status, sv.Reason, sv.RequiresAuth = string(res.Status), res.Reason, res.RequiresAuth
			}

			v.Servers = append(v.Servers, sv)
		}

		out = append(out, v)
	}

	writeOK(w, out)
}

// Approvals

func (d *daemon) getApprovals(w http.ResponseWriter, r *http.Request) {
	pending, recent := d.approvals.list()

	if pending == nil {
		pending = []*Approval{}
	}

	if recent == nil {
		recent = []*Approval{}
	}

	sort.Slice(pending, func(i, j int) bool { return pending[i].RequestedAt.Before(pending[j].RequestedAt) })
	writeOK(w, map[string]any{"pending": pending, "recent": recent})
}

func (d *daemon) decideApproval(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	in.Decision = strings.ToLower(in.Decision)

	if !validDecision(in.Decision) {
		writeErr(w, 400, errors.New("decision must be allow or deny"))

		return
	}

	a, err := d.approvals.resolve(r.PathValue("id"), in.Decision, in.Reason)
	if err != nil {
		writeErr(w, 404, err)

		return
	}

	writeOK(w, a)
}

func (d *daemon) hookPreToolUse(w http.ResponseWriter, r *http.Request) {
	var req hookRequest

	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, err)

		return
	}

	d.mu.Lock()
	enabled := d.settings.approvals()
	d.mu.Unlock()

	if !enabled || d.bus.subscribers() == 0 {
		writeOK(w, map[string]string{"decision": ""})

		return
	}

	rn, _ := d.runs.byPID(req.CSMPID)
	decision := d.approvals.ask(rn, req, r.Context().Done())
	writeOK(w, map[string]string{"decision": decision})
}

// watch notices what other csm processes write to disk, so sessions started from a terminal, failovers and handoffs all reach the desktop.
func (d *daemon) watch(ctx context.Context) {
	type snapshot struct {
		accounts    string
		usage       string
		live        string
		checkpoints map[string]bool
		transitions map[string]string
		handoffs    map[string]string
	}

	take := func() snapshot {
		snap := snapshot{checkpoints: map[string]bool{}, transitions: map[string]string{}, handoffs: map[string]string{}}
		snap.accounts = mtimes(filepath.Join(d.home, "accounts.json"), filepath.Join(d.home, "config.json"))
		usage, _ := filepath.Glob(filepath.Join(d.home, "usage", "*.json"))
		snap.usage = mtimes(usage...)
		sessions, _ := filepath.Glob(filepath.Join(d.home, "projects", "*", sessionFile))

		var live []string

		for _, f := range sessions {
			var sess Session

			if readJSON(f, &sess) == nil && processAlive(sess.CSMPID) {
				live = append(live, f+":"+strconv.Itoa(sess.CSMPID)+":"+sess.Account)
			}
		}

		sort.Strings(live)
		snap.live = strings.Join(live, "|")
		history, _ := filepath.Glob(filepath.Join(d.home, "projects", "*", historyDir, "*.json"))

		for _, f := range history {
			snap.checkpoints[f] = true
		}

		transitions, _ := filepath.Glob(filepath.Join(d.home, "projects", "*", transitionFile))

		for _, f := range transitions {
			snap.transitions[f] = mtimes(f)
		}

		handoffs, _ := filepath.Glob(filepath.Join(d.home, handoffsDir, "*", "*", handoff.FileJSON))

		for _, f := range handoffs {
			snap.handoffs[f] = mtimes(f)
		}

		return snap
	}

	prev := take()
	ticker := time.NewTicker(time.Second)

	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		cur := take()

		if cur.accounts != prev.accounts || cur.usage != prev.usage {
			d.bus.publish("accounts", nil)
		}

		if cur.live != prev.live {
			d.bus.publish("runs", nil)
			d.bus.publish("sessions", nil)
		}

		for f := range cur.checkpoints {
			if prev.checkpoints[f] {
				continue
			}

			var cp Checkpoint

			if readJSON(f, &cp) == nil {
				d.noteCheckpoint(cp)
			}

			d.bus.publish("sessions", nil)
		}

		for f, m := range cur.transitions {
			if prev.transitions[f] == m {
				continue
			}

			var t Transition

			if readJSON(f, &t) == nil {
				d.log.add(Activity{Kind: "switch", Provider: providerOrDefault(t.Provider), Account: t.To, Title: "CSM switched " + t.From + " → " + t.To, Detail: t.Reason.Describe() + " · " + displayPath(t.ProjectDir)})
			}
		}

		for f, m := range cur.handoffs {
			if prev.handoffs[f] == m {
				continue
			}

			if st, err := handoff.Load(filepath.Dir(f)); err == nil {
				d.noteHandoff(st, prev.handoffs[f] == "")
			}

			d.bus.publish("handoffs", nil)
		}

		prev = cur
	}
}

func (d *daemon) noteCheckpoint(cp Checkpoint) {
	pid := providerOrDefault(cp.Provider)
	a := Activity{Kind: "checkpoint", Provider: pid, Account: cp.Account, Detail: displayPath(cp.ProjectDir)}

	switch cp.Reason {
	case provider.StopReasonUsageLimit:
		a.Kind, a.Title = "usage_limit", providerName(pid)+" hit a usage limit on "+cp.Account
	case provider.StopReasonManualSwitch:
		a.Title = "Checkpoint saved for a switch from " + cp.Account
	default:
		a.Title = "Checkpoint saved: " + cp.Reason.Describe()
	}

	d.log.add(a)
}

// noteHandoff reads the switch from the handoff package itself, since the transition file is gone within a poll interval.
func (d *daemon) noteHandoff(st handoff.State, isNew bool) {
	if st.Kind == handoff.KindCheckpoint {
		a := Activity{Kind: "checkpoint.saved", Provider: st.Source.Provider, Account: st.Source.Account, Title: "Context checkpoint saved", Refs: map[string]string{"handoff_id": st.ID}}

		if !isNew {
			a.Kind, a.Title = "checkpoint.edited", "Context checkpoint edited"
		}

		if task, _ := st.Task(); task != "" {
			a.Detail = firstLine(task)
		}

		if st.Git.Root != "" {
			a.Detail = strings.TrimPrefix(a.Detail+" · "+displayPath(st.Git.Root), " · ")
		}

		d.log.add(a)

		return
	}

	a := Activity{Kind: "handoff." + st.Stage, Provider: st.Target.Provider, Account: st.Target.Account, Refs: map[string]string{"handoff_id": st.ID}}
	sameAgent := st.Source.Provider == st.Target.Provider && st.Source.Account != ""

	if st.Stage == handoff.StageTargetStarted && st.Outcome != nil && st.Outcome.Status == handoff.OutcomePartial && !sameAgent {
		a.Kind = "handoff.partial"
		a.Title = "Handoff started with gaps: " + st.Source.String() + " → " + st.Target.String()

		if len(st.Outcome.Problems) > 0 {
			a.Detail = st.Outcome.Problems[0]
		}

		d.log.add(a)

		return
	}

	switch {
	case st.Stage == handoff.StageTargetStarted && sameAgent:
		a.Kind = "switch"
		a.Title = "CSM switched " + st.Source.Account + " → " + st.Target.Account
		a.Detail = provider.StopReason(st.Reason).Describe()

		if st.Git.Root != "" {
			a.Detail += " · " + displayPath(st.Git.Root)
		}
	case st.Stage == handoff.StageTargetStarted && st.Reason == string(provider.StopReasonUsageLimit):
		a.Kind = "handoff.automatic"
		a.Title = "Automatic handoff after a usage limit: " + st.Source.String() + " → " + st.Target.String()

		if st.Git.Root != "" {
			a.Detail = displayPath(st.Git.Root)
		}
	case st.Stage == handoff.StageTargetStarted:
		a.Title = "Handoff completed: " + st.Source.String() + " → " + st.Target.String()
	case st.Stage == handoff.StageFailed:
		a.Title = "Handoff failed: " + st.Source.String() + " → " + st.Target.String()
		a.Detail = st.Error
	case isNew:
		a.Title = "Handoff started: " + st.Source.String() + " → " + st.Target.String()
	default:
		a.Title = "Handoff " + st.Stage + ": " + st.Source.String() + " → " + st.Target.String()
	}

	d.log.add(a)
}

func mtimes(paths ...string) string {
	var parts []string

	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			parts = append(parts, p+"@"+strconv.FormatInt(fi.ModTime().UnixNano(), 10)+":"+strconv.FormatInt(fi.Size(), 10))
		}
	}

	return strings.Join(parts, "|")
}
