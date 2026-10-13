package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onoja123/csm/engrun"
)

const (
	hubPollEvery      = 10 * time.Second
	hubRequestTimeout = 20 * time.Second
	hubEventBatch     = 100
)

// hubConfig connects this service to CSM Hub as a runner. The token is a runner token Hub issued for one workspace;
// it is stored with the other AutoFix settings (0600) and never sent anywhere but the configured Hub.
type hubConfig struct {
	URL           string `json:"url"`
	Token         string `json:"token,omitempty"`
	RunnerName    string `json:"runner_name,omitempty"`
	WorkspaceID   string `json:"workspace_id,omitempty"`
	WorkspaceName string `json:"workspace_name,omitempty"`
}

type hubView struct {
	URL       string `json:"url"`
	Connected bool   `json:"connected"`
	Runner    string `json:"runner,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	LastSync  string `json:"last_sync,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// hubJob is what Hub hands a runner: one engineering run to execute, leased for a while.
type hubJob struct {
	ID         string        `json:"id"`
	RunID      string        `json:"run_id"`
	IncidentID string        `json:"incident_id"`
	Attempt    int           `json:"attempt"`
	LeaseUntil time.Time     `json:"lease_until"`
	Repository string        `json:"repository"`
	BaseBranch string        `json:"base_branch,omitempty"`
	Signal     engrun.Signal `json:"signal"`
}

type hubEvent struct {
	Seq     int       `json:"seq"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	TaskID  string    `json:"task_id,omitempty"`
	Message string    `json:"message"`
}

type hubReport struct {
	// States are the lifecycle states entered since the last accepted report, in order; State is the last of them.
	States       []engrun.State      `json:"states,omitempty"`
	State        engrun.State        `json:"state"`
	Reproduction engrun.Reproduction `json:"reproduction,omitempty"`
	Verdict      engrun.Verdict      `json:"verdict,omitempty"`
	Branch       string              `json:"branch,omitempty"`
	PRURL        string              `json:"pr_url,omitempty"`
	Attention    string              `json:"attention,omitempty"`
	LocalRunID   string              `json:"local_run_id"`
	Events       []hubEvent          `json:"events"`
}

type hubClient struct {
	cfg  hubConfig
	http *http.Client
}

// validHubURL refuses anything but https, or http to this machine for development, so a runner token is never sent in
// the clear across a network.
func validHubURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("invalid Hub URL %q", raw)
	}

	host := u.Hostname()

	if u.Scheme != "https" && !(u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")) {
		return "", errors.New("CSM Hub must be reached over https (http is allowed only for localhost)")
	}

	return u.String(), nil
}

func (c hubClient) do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader

	if in != nil {
		data, _ := json.Marshal(in)
		body = bytes.NewReader(data)
	}

	ctx, cancel := context.WithTimeout(ctx, hubRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, c.cfg.URL+path, body)
	if err != nil {
		return 0, err
	}

	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}

	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode >= 400 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}

		json.Unmarshal(data, &e)

		return resp.StatusCode, fmt.Errorf("hub %s %s: %d %s", method, path, resp.StatusCode, orValue(e.Error.Message, firstLine(string(data))))
	}

	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, err
		}
	}

	return resp.StatusCode, nil
}

// hubRunner claims jobs from Hub for the repositories mapped here, runs them as AutoFix runs, and reports state and events
// back. Hub keeps the lease; if this machine stops, the lease expires and Hub can hand the job out again.
type hubRunner struct {
	d        *daemon
	mu       sync.Mutex
	lastSync time.Time
	lastErr  string
	session  *hubSession
}

func (h *hubRunner) setStatus(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if err != nil {
		h.lastErr = err.Error()

		return
	}

	h.lastSync, h.lastErr = time.Now(), ""
}

func (h *hubRunner) status() (time.Time, string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.lastSync, h.lastErr
}

func (h *hubRunner) loop(ctx context.Context) {
	every := hubPollEvery

	if secs, err := strconv.Atoi(os.Getenv("CSM_HUB_POLL_SECONDS")); err == nil && secs > 0 {
		every = time.Duration(secs) * time.Second
	}

	t := time.NewTicker(every)
	defer t.Stop()

	for {
		h.tick(ctx)

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (h *hubRunner) client() (hubClient, bool) {
	cfg := h.d.autofixConfig()

	if cfg.Hub == nil || cfg.Hub.Token == "" || cfg.Hub.URL == "" {
		return hubClient{}, false
	}

	return hubClient{cfg: *cfg.Hub, http: &http.Client{Timeout: hubRequestTimeout}}, true
}

func (h *hubRunner) tick(ctx context.Context) {
	c, ok := h.client()
	if !ok {
		return
	}

	err := h.sync(ctx, c)

	if err == nil {
		err = h.claim(ctx, c)
	}

	if err != nil {
		slog.Debug("hub", "err", err)
	}

	h.setStatus(err)
}

func (h *hubRunner) claim(ctx context.Context, c hubClient) error {
	cfg := h.d.autofixConfig()

	var projects []string

	for _, m := range cfg.Mappings {
		if !m.Disabled {
			projects = append(projects, m.Source+":"+m.Project)
		}
	}

	if len(projects) == 0 {
		return nil
	}

	var job hubJob

	code, err := c.do(ctx, "POST", "/api/v1/runner/claim", map[string]any{"projects": projects, "runner": c.cfg.RunnerName}, &job)
	if err != nil || code == http.StatusNoContent || job.ID == "" {
		return err
	}

	if existing := h.d.builds.byHubRun(job.RunID); existing != "" {
		return nil
	}

	m, mapped := cfg.mapping(job.Signal.Source, job.Signal.Repository.Project)
	if !mapped {
		c.do(ctx, "POST", "/api/v1/runner/jobs/"+url.PathEscape(job.ID)+"/fail", map[string]any{"error": "this runner has no repository mapped for the project", "retryable": true}, nil)

		return nil
	}

	if job.BaseBranch != "" {
		m.BaseBranch = job.BaseBranch
	}

	s, err := h.d.state()
	if err != nil {
		return err
	}

	sig := engrun.Normalize(job.Signal)
	b, err := h.d.builds.createAutoFix(s, sig, job.IncidentID, m)
	if err != nil {
		c.do(ctx, "POST", "/api/v1/runner/jobs/"+url.PathEscape(job.ID)+"/fail", map[string]any{"error": err.Error(), "retryable": false}, nil)

		return nil
	}

	h.d.builds.linkHub(b.ID, HubLink{URL: c.cfg.URL, RunID: job.RunID, JobID: job.ID})

	if _, inc, _, err := h.d.signals.record(sig, true, time.Now()); err == nil && inc != nil {
		h.d.signals.attachRun(inc.ID, b.ID, "")
		h.d.bus.publish("signals", nil)
	}
	h.d.log.add(Activity{Kind: "hub.claimed", Title: "Claimed an AutoFix run from CSM Hub: " + sig.Title, Refs: map[string]string{"build": b.ID}})

	return nil
}

// sync reports every Hub-linked run: new events since the last report, its lifecycle state and verdict. Hub validates the
// state change and answers whether the run was cancelled there.
func (h *hubRunner) sync(ctx context.Context, c hubClient) error {
	for _, link := range h.d.builds.hubLinks() {
		rep, terminal, ok := h.d.builds.hubReport(link.local, link.synced)
		if !ok {
			continue
		}

		var resp struct {
			Accepted        int  `json:"accepted"`
			CancelRequested bool `json:"cancel_requested"`
		}

		if _, err := c.do(ctx, "POST", "/api/v1/runner/runs/"+url.PathEscape(link.hub.RunID)+"/report", rep, &resp); err != nil {
			return err
		}

		h.d.builds.markSynced(link.local, link.synced+len(rep.Events), len(rep.States), string(rep.State))

		if resp.CancelRequested {
			h.d.builds.act(link.local, "cancel", actionRequest{})
		}

		if link.hub.JobID != "" {
			path := "/api/v1/runner/jobs/" + url.PathEscape(link.hub.JobID)

			if terminal {
				c.do(ctx, "POST", path+"/complete", map[string]any{"state": rep.State}, nil)
			} else {
				c.do(ctx, "POST", path+"/heartbeat", nil, nil)
			}
		}
	}

	return nil
}

type hubLinkRef struct {
	local  string
	hub    HubLink
	synced int
}

func (e *buildEngine) byHubRun(runID string) string {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, b := range e.builds {
		if b.Hub != nil && b.Hub.RunID == runID {
			return b.ID
		}
	}

	return ""
}

func (e *buildEngine) linkHub(id string, link HubLink) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if b := e.builds[id]; b != nil {
		b.Hub = &link
		e.event(b, "hub.linked", "", "Linked to CSM Hub run "+link.RunID)
		e.save(b)
	}
}

func (e *buildEngine) hubLinks() []hubLinkRef {
	e.mu.Lock()
	defer e.mu.Unlock()

	var out []hubLinkRef

	for _, b := range e.builds {
		if b.Hub != nil && !(b.Status.Terminal() && b.Hub.State == string(engrunState(b))) {
			out = append(out, hubLinkRef{local: b.ID, hub: *b.Hub, synced: b.Hub.Synced})
		}
	}

	return out
}

func (e *buildEngine) hubReport(id string, synced int) (hubReport, bool, bool) {
	e.mu.Lock()
	b := e.builds[id]

	if b == nil {
		e.mu.Unlock()

		return hubReport{}, false, false
	}

	rep := hubReport{State: engrunState(b), Branch: b.Integration.Branch, PRURL: b.PR.URL, Attention: b.Attention, LocalRunID: b.ID}

	if b.Hub != nil && b.Hub.SyncedStates < len(b.StateLog) {
		rep.States = append(rep.States, b.StateLog[b.Hub.SyncedStates:]...)
	}

	if v := b.Verification; v != nil {
		rep.Reproduction, rep.Verdict = v.Reproduction, v.Verdict
	}

	terminal := b.Status.Terminal()
	e.mu.Unlock()

	events := e.store.events(id)

	for i := synced; i < len(events) && len(rep.Events) < hubEventBatch; i++ {
		ev := events[i]
		rep.Events = append(rep.Events, hubEvent{Seq: i + 1, At: ev.At, Kind: ev.Kind, TaskID: ev.TaskID, Message: ev.Message})
	}

	return rep, terminal && synced+len(rep.Events) >= len(events), true
}

func (e *buildEngine) markSynced(id string, n, states int, state string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if b := e.builds[id]; b != nil && b.Hub != nil {
		b.Hub.Synced, b.Hub.SyncedStates, b.Hub.State = n, b.Hub.SyncedStates+states, state
		e.store.save(b)
	}
}

// cmdHub connects this machine to CSM Hub as a runner: csm hub connect <url> --token <runner token>, csm hub status,
// csm hub disconnect. The service picks the change up on its next poll.
func cmdHub(log *Logger, home string, args []string) error {
	path := filepath.Join(home, autofixConfigFile)

	var cfg autofixConfig

	readJSON(path, &cfg)

	switch {
	case len(args) >= 1 && args[0] == "setup":
		return hubSetup(log, home, args[1:])

	case len(args) >= 2 && args[0] == "connect":
		u, err := validHubURL(args[1])
		if err != nil {
			return err
		}

		token := os.Getenv("CSM_HUB_TOKEN")

		for i := 2; i+1 < len(args); i++ {
			if args[i] == "--token" {
				token = args[i+1]
			}
		}

		if token == "" {
			return errors.New("give the runner token with --token or CSM_HUB_TOKEN")
		}

		name, _ := os.Hostname()
		cfg.Hub = &hubConfig{URL: u, Token: token, RunnerName: name}

		if err := writeJSON(path, cfg); err != nil {
			return err
		}

		os.Chmod(path, 0o600)
		log.Info("Connected to %s as runner %s. csm serve claims AutoFix runs for the repositories mapped here.", u, name)

		return nil

	case len(args) == 1 && args[0] == "disconnect":
		cfg.Hub = nil

		return writeJSON(path, cfg)

	case len(args) == 0 || args[0] == "status":
		if cfg.Hub == nil {
			log.Info("Not connected to CSM Hub. Connect with: csm hub connect <url> --token <runner token>")

			return nil
		}

		log.Info("CSM Hub  %s\nRunner   %s\nMapped   %d project(s)", cfg.Hub.URL, cfg.Hub.RunnerName, len(cfg.Mappings))

		return nil
	}

	return errors.New("usage: csm hub connect <url> --token <token> | csm hub status | csm hub disconnect")
}
