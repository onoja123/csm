package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/onoja123/csm/engrun"
)

const (
	autofixConfigFile = "autofix.json"
	signalsDir        = "signals"
	incidentsFile     = "incidents.json"
	signalLogFile     = "signals.jsonl"
	maxSignalBody     = 1 << 20
	signalsPerMinute  = 60
	maxIncidents      = 500
)

// RepoMapping says which local repository a source's project lives in. Signals for an unmapped project are kept but no
// run starts, so an alert can never point CSM at a directory the user did not choose.
type RepoMapping struct {
	Source     string   `json:"source"`
	Project    string   `json:"project"`
	Repo       string   `json:"repo"`
	BaseBranch string   `json:"base_branch,omitempty"`
	Agents     []string `json:"agents,omitempty"`
	Disabled   bool     `json:"disabled,omitempty"`
}

type autofixConfig struct {
	// SentrySecret verifies Sentry-Hook-Signature on webhooks the service receives directly. It is never returned by the API.
	SentrySecret string        `json:"sentry_secret,omitempty"`
	Mappings     []RepoMapping `json:"mappings,omitempty"`
	AutoStart    *bool         `json:"auto_start,omitempty"`
	Hub          *hubConfig    `json:"hub,omitempty"`
	Sentry       *sentryConfig `json:"sentry,omitempty"`
}

func (c autofixConfig) autoStart() bool { return c.AutoStart == nil || *c.AutoStart }

func (c autofixConfig) mapping(source, project string) (RepoMapping, bool) {
	for _, m := range c.Mappings {
		if !m.Disabled && m.Source == source && m.Project == project {
			return m, true
		}
	}

	return RepoMapping{}, false
}

// Incident groups signals that share a fingerprint. A run belongs to an incident; recurring events attach to it rather
// than starting another run.
type Incident struct {
	ID          string         `json:"id"`
	Fingerprint string         `json:"fingerprint"`
	Source      string         `json:"source"`
	Project     string         `json:"project,omitempty"`
	Title       string         `json:"title"`
	Severity    string         `json:"severity"`
	Environment string         `json:"environment,omitempty"`
	FirstSeen   time.Time      `json:"first_seen"`
	LastSeen    time.Time      `json:"last_seen"`
	Count       int            `json:"count"`
	Signals     []string       `json:"signals"`
	Runs        []string       `json:"runs"`
	Status      string         `json:"status"`
	Note        string         `json:"note,omitempty"`
	Latest      *engrun.Signal `json:"latest,omitempty"`
}

const (
	incidentOpen     = "open"
	incidentUnmapped = "unmapped"
	incidentRunning  = "run_started"
)

// signalStore keeps incidents and the delivery ids already seen. Files under ~/.csm/signals are the record; the maps are
// a cache rebuilt at start.
type signalStore struct {
	mu        sync.Mutex
	dir       string
	incidents []*Incident
	seen      map[string]bool
	limiter   map[string][]time.Time
	seq       int
}

func newSignalStore(home string) *signalStore {
	st := &signalStore{dir: filepath.Join(home, signalsDir), seen: map[string]bool{}, limiter: map[string][]time.Time{}}
	readJSON(filepath.Join(st.dir, incidentsFile), &st.incidents)

	if data, err := os.ReadFile(filepath.Join(st.dir, signalLogFile)); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			var s engrun.Signal

			if json.Unmarshal([]byte(line), &s) == nil && s.ExternalID != "" {
				st.seen[s.Source+"\x00"+s.ExternalID] = true
			}
		}
	}

	return st
}

func (st *signalStore) saveLocked() error {
	if err := os.MkdirAll(st.dir, 0o700); err != nil {
		return err
	}

	return writeJSON(filepath.Join(st.dir, incidentsFile), st.incidents)
}

// allow is a sliding one-minute window per source, so a burst of alerts cannot flood the service or start a storm of runs.
func (st *signalStore) allow(source string, now time.Time) bool {
	var recent []time.Time

	for _, t := range st.limiter[source] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}

	if len(recent) >= signalsPerMinute {
		st.limiter[source] = recent

		return false
	}

	st.limiter[source] = append(recent, now)

	return true
}

type intakeResult struct {
	Status   string    `json:"status"`
	Incident *Incident `json:"incident,omitempty"`
	RunID    string    `json:"run_id,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

var errRateLimited = errors.New("too many signals from this source; try again in a minute")

// record stores a signal once and returns the incident it belongs to, and whether a run should start for it.
func (st *signalStore) record(sig engrun.Signal, mapped bool, now time.Time) (intakeResult, *Incident, bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	key := sig.Source + "\x00" + sig.ExternalID

	if sig.ExternalID != "" && st.seen[key] {
		return intakeResult{Status: "duplicate", Reason: "this delivery was already recorded"}, nil, false, nil
	}

	if !st.allow(sig.Source, now) {
		return intakeResult{}, nil, false, errRateLimited
	}

	st.seq++
	sig.ID = fmt.Sprintf("sig-%s-%d", now.UTC().Format("20060102T150405"), st.seq)

	if err := os.MkdirAll(st.dir, 0o700); err != nil {
		return intakeResult{}, nil, false, err
	}

	if f, err := os.OpenFile(filepath.Join(st.dir, signalLogFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		line, _ := json.Marshal(sig)
		f.Write(append(line, '\n'))
		f.Close()
	}

	st.seen[key] = true

	var inc *Incident

	for _, i := range st.incidents {
		if i.Fingerprint == sig.Fingerprint {
			inc = i
		}
	}

	if inc == nil {
		inc = &Incident{ID: "inc-" + strings.TrimPrefix(sig.ID, "sig-"), Fingerprint: sig.Fingerprint, Source: sig.Source, Project: sig.Repository.Project, FirstSeen: sig.OccurredAt, Status: incidentOpen}
		st.incidents = append([]*Incident{inc}, st.incidents...)

		if len(st.incidents) > maxIncidents {
			st.incidents = st.incidents[:maxIncidents]
		}
	}

	inc.Title, inc.Severity, inc.Environment, inc.LastSeen, inc.Latest = sig.Title, sig.Severity, sig.Environment, sig.OccurredAt, &sig
	inc.Count++
	inc.Signals = append(inc.Signals, sig.ID)

	if len(inc.Signals) > 50 {
		inc.Signals = inc.Signals[len(inc.Signals)-50:]
	}

	start := false

	switch {
	case !mapped:
		inc.Status, inc.Note = incidentUnmapped, "No repository is mapped to "+sig.Source+" project "+orDash(sig.Repository.Project)
	case len(inc.Runs) == 0:
		start = true
	}

	err := st.saveLocked()

	return intakeResult{Status: "recorded", Incident: inc}, inc, start, err
}

func (st *signalStore) attachRun(incidentID, runID, note string) {
	st.mu.Lock()
	defer st.mu.Unlock()

	for _, i := range st.incidents {
		if i.ID == incidentID {
			if runID != "" && !slices.Contains(i.Runs, runID) {
				i.Runs = append(i.Runs, runID)
				i.Status, i.Note = incidentRunning, ""
			}

			if note != "" {
				i.Note = note
			}
		}
	}

	st.saveLocked()
}

func (st *signalStore) byFingerprint(fp string) (Incident, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()

	for _, i := range st.incidents {
		if i.Fingerprint == fp {
			return *i, true
		}
	}

	return Incident{}, false
}

func (st *signalStore) list() []Incident {
	st.mu.Lock()
	defer st.mu.Unlock()

	out := make([]Incident, 0, len(st.incidents))

	for _, i := range st.incidents {
		out = append(out, *i)
	}

	return out
}

func (st *signalStore) incident(id string) (Incident, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()

	for _, i := range st.incidents {
		if i.ID == id {
			return *i, true
		}
	}

	return Incident{}, false
}

func (d *daemon) autofixConfig() autofixConfig {
	var c autofixConfig

	readJSON(filepath.Join(d.home, autofixConfigFile), &c)

	return c
}

// intake is the one path every signal takes, from a webhook, the desktop or CSM Hub: normalize, deduplicate, map to a
// repository, and start a run only for a new incident on a mapped repository.
func (d *daemon) intake(sig engrun.Signal, origin string) (intakeResult, error) {
	sig = engrun.Normalize(sig)
	cfg := d.autofixConfig()
	m, mapped := cfg.mapping(sig.Source, sig.Repository.Project)

	res, inc, start, err := d.signals.record(sig, mapped, time.Now())
	if err != nil || inc == nil {
		return res, err
	}

	d.log.add(Activity{Kind: "signal.received", Title: fmt.Sprintf("%s signal: %s", sig.Source, sig.Title), Detail: origin, Refs: map[string]string{"incident": inc.ID}})
	d.bus.publish("signals", nil)

	if !start || !cfg.autoStart() {
		if inc.Count > 1 {
			res.Reason = fmt.Sprintf("attached to incident %s (%d occurrences)", inc.ID, inc.Count)
		}

		return res, nil
	}

	s, err := d.state()
	if err != nil {
		return res, err
	}

	b, err := d.builds.createAutoFix(s, sig, inc.ID, m)
	if err != nil {
		d.signals.attachRun(inc.ID, "", "Could not start a run: "+err.Error())
		res.Reason = "recorded; no run started: " + err.Error()

		return res, nil
	}

	d.signals.attachRun(inc.ID, b.ID, "")
	res.RunID = b.ID
	d.rememberProject(b.Repo)

	return res, nil
}

func (d *daemon) postSentrySignal(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSignalBody+1))
	if err != nil || len(body) > maxSignalBody {
		writeErr(w, 413, errors.New("the payload is larger than 1 MB"))

		return
	}

	cfg := d.autofixConfig()

	if !engrun.VerifySentrySignature(cfg.SentrySecret, body, r.Header.Get("Sentry-Hook-Signature")) {
		d.log.add(Activity{Kind: "signal.rejected", Title: "Rejected a Sentry webhook with an invalid or missing signature"})
		writeErr(w, 401, engrun.ErrBadSignature)

		return
	}

	sig, err := engrun.ParseSentry(r.Header.Get("Sentry-Hook-Resource"), body, r.Header.Get("Request-ID"), time.Now())
	if errors.Is(err, engrun.ErrIgnored) {
		writeOK(w, intakeResult{Status: "ignored", Reason: err.Error()})

		return
	}

	if err != nil {
		writeErr(w, 400, err)

		return
	}

	d.respondIntake(w, sig, "sentry webhook")
}

// postSignal accepts a signal already in the shared envelope: the desktop's "simulate", other local tools, and tests.
// It is only reachable on the local socket or with the network token.
func (d *daemon) postSignal(w http.ResponseWriter, r *http.Request) {
	var sig engrun.Signal

	if err := readBody(r, &sig); err != nil {
		writeErr(w, 400, err)

		return
	}

	if sig.Source == "" || sig.Title == "" || sig.ExternalID == "" {
		writeErr(w, 400, errors.New("a signal needs a source, a title and an external_id"))

		return
	}

	d.respondIntake(w, sig, "posted to the CSM service")
}

func (d *daemon) respondIntake(w http.ResponseWriter, sig engrun.Signal, origin string) {
	res, err := d.intake(sig, origin)

	switch {
	case errors.Is(err, errRateLimited):
		writeErr(w, 429, err)
	case err != nil:
		writeErr(w, 500, err)
	case res.Status == "duplicate":
		writeOK(w, res)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(res)
	}
}

func (d *daemon) getSignals(w http.ResponseWriter, r *http.Request) {
	writeOK(w, d.signals.list())
}

func (d *daemon) postIncidentRun(w http.ResponseWriter, r *http.Request) {
	inc, ok := d.signals.incident(r.PathValue("id"))
	if !ok || inc.Latest == nil {
		writeErr(w, 404, errors.New("no such incident"))

		return
	}

	m, mapped := d.autofixConfig().mapping(inc.Source, inc.Project)
	if !mapped {
		writeErr(w, 409, fmt.Errorf("map %s project %q to a repository first", inc.Source, inc.Project))

		return
	}

	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	b, err := d.builds.createAutoFix(s, *inc.Latest, inc.ID, m)
	if err != nil {
		writeErr(w, 400, err)

		return
	}

	d.signals.attachRun(inc.ID, b.ID, "")
	writeOK(w, b.summary())
}

type autofixConfigView struct {
	HasSentrySecret bool          `json:"has_sentry_secret"`
	Mappings        []RepoMapping `json:"mappings"`
	AutoStart       bool          `json:"auto_start"`
	Hub             *hubView      `json:"hub,omitempty"`
	WebhookPath     string        `json:"webhook_path"`
}

func (d *daemon) getAutofixConfig(w http.ResponseWriter, r *http.Request) {
	cfg := d.autofixConfig()
	v := autofixConfigView{HasSentrySecret: cfg.SentrySecret != "", Mappings: cfg.Mappings, AutoStart: cfg.autoStart(), WebhookPath: "/v1/signals/sentry"}

	if v.Mappings == nil {
		v.Mappings = []RepoMapping{}
	}

	if cfg.Hub != nil {
		v.Hub = &hubView{URL: cfg.Hub.URL, Connected: cfg.Hub.Token != "", Runner: cfg.Hub.RunnerName, Workspace: cfg.Hub.WorkspaceName}

		last, lastErr := d.hub.status()

		if !last.IsZero() {
			v.Hub.LastSync = last.Format(time.RFC3339)
		}

		v.Hub.LastError = lastErr
	}

	writeOK(w, v)
}

func (d *daemon) putAutofixConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SentrySecret *string       `json:"sentry_secret"`
		Mappings     []RepoMapping `json:"mappings"`
		AutoStart    *bool         `json:"auto_start"`
		HubURL       *string       `json:"hub_url"`
		HubToken     *string       `json:"hub_token"`
	}

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	cfg := d.autofixConfig()

	if in.SentrySecret != nil {
		cfg.SentrySecret = strings.TrimSpace(*in.SentrySecret)
	}

	if in.AutoStart != nil {
		cfg.AutoStart = in.AutoStart
	}

	switch {
	case in.HubURL != nil && strings.TrimSpace(*in.HubURL) == "":
		cfg.Hub = nil
	case in.HubURL != nil:
		u, err := validHubURL(*in.HubURL)
		if err != nil {
			writeErr(w, 400, err)

			return
		}

		token := ""

		if cfg.Hub != nil {
			token = cfg.Hub.Token
		}

		if in.HubToken != nil {
			token = strings.TrimSpace(*in.HubToken)
		}

		if token == "" {
			writeErr(w, 400, errors.New("give the runner token CSM Hub issued for this machine"))

			return
		}

		name, _ := os.Hostname()
		cfg.Hub = &hubConfig{URL: u, Token: token, RunnerName: name}
	}

	if in.Mappings != nil {
		var out []RepoMapping

		for _, m := range in.Mappings {
			m.Source, m.Project = strings.ToLower(strings.TrimSpace(m.Source)), strings.TrimSpace(m.Project)

			if m.Source == "" || m.Project == "" {
				writeErr(w, 400, errors.New("a mapping needs a source and a project"))

				return
			}

			dir, err := cleanProjectDir(m.Repo)
			if err != nil {
				writeErr(w, 400, err)

				return
			}

			rs, err := inspectRepo(dir)
			if err != nil {
				writeErr(w, 400, err)

				return
			}

			m.Repo = rs.Root
			out = append(out, m)
		}

		cfg.Mappings = out
	}

	if err := d.saveAutofixConfig(cfg); err != nil {
		writeErr(w, 500, err)

		return
	}

	d.log.add(Activity{Kind: "autofix.config", Title: "AutoFix settings changed"})
	d.bus.publish("signals", nil)
	d.getAutofixConfig(w, r)
}
