package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onoja123/csm/engrun"
)

// hubSessionTTL bounds how long a Hub sign-in made from CSM stays usable. The password is never stored; only the session
// cookie is kept, in memory, so Sentry's client secret can be pasted a few minutes after the webhook URL was created.
const hubSessionTTL = 2 * time.Hour

type hubSession struct {
	url     string
	email   string
	http    *http.Client
	expires time.Time
}

type hubWorkspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role,omitempty"`
}

type hubIntegration struct {
	ID                 string     `json:"id"`
	Kind               string     `json:"kind"`
	Name               string     `json:"name"`
	WebhookPath        string     `json:"webhook_path"`
	WebhookURL         string     `json:"webhook_url"`
	HasSecret          bool       `json:"has_secret"`
	LastDeliveryAt     *time.Time `json:"last_delivery_at,omitempty"`
	LastDeliveryResult string     `json:"last_delivery_result,omitempty"`
	LastEventAt        *time.Time `json:"last_event_at,omitempty"`
}

type hubMapping struct {
	Source     string `json:"source"`
	Project    string `json:"project"`
	Repository string `json:"repository"`
	BaseBranch string `json:"base_branch"`
}

type hubStatusView struct {
	URL      string `json:"url"`
	Public   bool   `json:"public"`
	SignedIn string `json:"signed_in,omitempty"`
	// Reachable is false when the runner token could not be used; Error says why.
	Reachable    bool             `json:"reachable"`
	Error        string           `json:"error,omitempty"`
	Workspace    *hubWorkspace    `json:"workspace,omitempty"`
	Runner       string           `json:"runner,omitempty"`
	Integrations []hubIntegration `json:"integrations"`
	Mappings     []hubMapping     `json:"mappings"`
	HasSecret    bool             `json:"has_local_secret"`
}

func (s *hubSession) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader

	if in != nil {
		data, _ := json.Marshal(in)
		body = bytes.NewReader(data)
	}

	ctx, cancel := context.WithTimeout(ctx, hubRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, s.url+path, body)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("CSM Hub could not be reached at %s: %v", s.url, err)
	}

	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode >= 400 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}

		json.Unmarshal(data, &e)

		return &hubError{status: resp.StatusCode, msg: orValue(e.Error.Message, firstLine(string(data)))}
	}

	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}

	return nil
}

type hubError struct {
	status int
	msg    string
}

func (e *hubError) Error() string { return e.msg }

func publicHubURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}

	host := p.Hostname()

	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return false
	}

	ip := net.ParseIP(host)

	return ip == nil || !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

func (h *hubRunner) currentSession() *hubSession {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.session == nil || time.Now().After(h.session.expires) {
		h.session = nil

		return nil
	}

	return h.session
}

func (h *hubRunner) setSession(s *hubSession) {
	h.mu.Lock()
	h.session = s
	h.mu.Unlock()
}

func (d *daemon) postHubSignin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL           string `json:"url"`
		Email         string `json:"email"`
		Password      string `json:"password"`
		Name          string `json:"name"`
		CreateAccount bool   `json:"create_account"`
	}

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	u, err := validHubURL(in.URL)
	if err != nil {
		writeErr(w, 400, err)

		return
	}

	if strings.TrimSpace(in.Email) == "" || in.Password == "" {
		writeErr(w, 400, errors.New("enter the email and password of your CSM Hub account"))

		return
	}

	jar, _ := cookiejar.New(nil)
	s := &hubSession{url: u, email: strings.TrimSpace(in.Email), http: &http.Client{Jar: jar, Timeout: hubRequestTimeout}, expires: time.Now().Add(hubSessionTTL)}

	if in.CreateAccount {
		err = s.do(r.Context(), "POST", "/api/v1/auth/register", map[string]string{"email": s.email, "name": orValue(strings.TrimSpace(in.Name), s.email), "password": in.Password}, nil)
	} else {
		err = s.do(r.Context(), "POST", "/api/v1/auth/login", map[string]string{"email": s.email, "password": in.Password}, nil)
	}

	if err != nil {
		writeErr(w, hubStatus(err), err)

		return
	}

	var ws []hubWorkspace

	if err := s.do(r.Context(), "GET", "/api/v1/workspaces", nil, &ws); err != nil {
		writeErr(w, hubStatus(err), err)

		return
	}

	d.hub.setSession(s)
	writeOK(w, map[string]any{"url": u, "email": s.email, "public": publicHubURL(u), "workspaces": ws})
}

func hubStatus(err error) int {
	var he *hubError

	if errors.As(err, &he) && he.status < 500 {
		return he.status
	}

	return 502
}

func (d *daemon) postHubSignout(w http.ResponseWriter, r *http.Request) {
	d.hub.setSession(nil)
	writeOK(w, map[string]string{"status": "signed_out"})
}

// postHubSetup does what an admin would otherwise do by hand against the Hub API: choose or create the workspace, create
// (or reuse) its Sentry integration, map the Sentry project to the repository, issue a runner token for this machine, and
// connect this service as that runner. The answer carries the webhook URL to paste into Sentry.
func (d *daemon) postHubSetup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WorkspaceID   string `json:"workspace_id"`
		WorkspaceName string `json:"workspace_name"`
		Project       string `json:"project"`
		Repo          string `json:"repo"`
		Repository    string `json:"repository"`
		BaseBranch    string `json:"base_branch"`
		SentrySecret  string `json:"sentry_secret"`
	}

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	s := d.hub.currentSession()
	if s == nil {
		writeErr(w, 401, errors.New("sign in to CSM Hub first"))

		return
	}

	in.Project = strings.TrimSpace(in.Project)

	if in.Project == "" {
		writeErr(w, 400, errors.New("enter the Sentry project slug, as it appears in Sentry's project settings"))

		return
	}

	dir, err := cleanProjectDir(in.Repo)
	if err != nil {
		writeErr(w, 400, err)

		return
	}

	rs, err := inspectRepo(dir)
	if err != nil {
		writeErr(w, 400, err)

		return
	}

	repository := strings.TrimSpace(in.Repository)

	if repository == "" {
		repository = repoSlug(rs.Root)
	}

	if repository == "" {
		writeErr(w, 400, errors.New("this repository has no origin remote; enter its GitHub name as owner/name"))

		return
	}

	base := orValue(strings.TrimSpace(in.BaseBranch), rs.Branch)
	ctx := r.Context()
	ws := hubWorkspace{ID: in.WorkspaceID}

	if ws.ID == "" {
		if strings.TrimSpace(in.WorkspaceName) == "" {
			writeErr(w, 400, errors.New("choose a workspace or name a new one"))

			return
		}

		if err := s.do(ctx, "POST", "/api/v1/workspaces", map[string]string{"name": strings.TrimSpace(in.WorkspaceName)}, &ws); err != nil {
			writeErr(w, hubStatus(err), err)

			return
		}
	}

	wsPath := "/api/v1/workspaces/" + url.PathEscape(ws.ID)

	if err := s.do(ctx, "GET", wsPath, nil, &ws); err != nil {
		writeErr(w, hubStatus(err), err)

		return
	}

	var integs []hubIntegration

	if err := s.do(ctx, "GET", wsPath+"/integrations", nil, &integs); err != nil {
		writeErr(w, hubStatus(err), err)

		return
	}

	var integ *hubIntegration

	for i := range integs {
		if integs[i].Kind == engrun.SourceSentry {
			integ = &integs[i]

			break
		}
	}

	secret := strings.TrimSpace(in.SentrySecret)

	if integ == nil {
		var created struct {
			Integration hubIntegration `json:"integration"`
		}

		if err := s.do(ctx, "POST", wsPath+"/integrations", map[string]string{"kind": engrun.SourceSentry, "name": "Sentry", "secret": secret}, &created); err != nil {
			writeErr(w, hubStatus(err), err)

			return
		}

		integ = &created.Integration
	} else if secret != "" {
		if err := s.do(ctx, "PUT", wsPath+"/integrations/"+integ.ID+"/secret", map[string]string{"secret": secret}, nil); err != nil {
			writeErr(w, hubStatus(err), err)

			return
		}
	}

	if err := s.do(ctx, "PUT", wsPath+"/repo-mappings", map[string]string{"source": engrun.SourceSentry, "project": in.Project, "repository": repository, "base_branch": base}, nil); err != nil {
		writeErr(w, hubStatus(err), err)

		return
	}

	cfg := d.autofixConfig()
	token := ""

	if cfg.Hub != nil && cfg.Hub.URL == s.url && cfg.Hub.WorkspaceID == ws.ID {
		token = cfg.Hub.Token
	}

	if token == "" {
		name, _ := os.Hostname()

		var created struct {
			Token string `json:"token"`
		}

		if err := s.do(ctx, "POST", wsPath+"/runners", map[string]string{"name": orValue(name, "CSM runner")}, &created); err != nil {
			writeErr(w, hubStatus(err), err)

			return
		}

		token = created.Token
	}

	name, _ := os.Hostname()
	cfg.Hub = &hubConfig{URL: s.url, Token: token, RunnerName: name, WorkspaceID: ws.ID, WorkspaceName: ws.Name}

	if secret != "" {
		cfg.SentrySecret = secret
	}

	cfg.Mappings = upsertMapping(cfg.Mappings, RepoMapping{Source: engrun.SourceSentry, Project: in.Project, Repo: rs.Root, BaseBranch: base})

	if err := d.saveAutofixConfig(cfg); err != nil {
		writeErr(w, 500, err)

		return
	}

	d.log.add(Activity{Kind: "autofix.config", Title: "Connected Sentry through CSM Hub", Detail: ws.Name + " · " + in.Project + " → " + displayPath(rs.Root)})
	d.bus.publish("signals", nil)
	writeOK(w, d.hubStatus(ctx))
}

func (d *daemon) putHubSecret(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Secret string `json:"secret"`
	}

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	secret := strings.TrimSpace(in.Secret)
	cfg := d.autofixConfig()

	if cfg.Hub == nil || cfg.Hub.WorkspaceID == "" {
		writeErr(w, 409, errors.New("connect Sentry through CSM Hub first"))

		return
	}

	s := d.hub.currentSession()
	if s == nil || s.url != cfg.Hub.URL {
		writeErr(w, 401, errors.New("sign in to CSM Hub again to change the client secret"))

		return
	}

	st := d.hubStatus(r.Context())

	if len(st.Integrations) == 0 {
		writeErr(w, 409, errors.New("this workspace has no Sentry integration; run the setup again"))

		return
	}

	if err := s.do(r.Context(), "PUT", "/api/v1/workspaces/"+url.PathEscape(cfg.Hub.WorkspaceID)+"/integrations/"+st.Integrations[0].ID+"/secret", map[string]string{"secret": secret}, nil); err != nil {
		writeErr(w, hubStatus(err), err)

		return
	}

	cfg.SentrySecret = secret

	if err := d.saveAutofixConfig(cfg); err != nil {
		writeErr(w, 500, err)

		return
	}

	writeOK(w, d.hubStatus(r.Context()))
}

func (d *daemon) getHubStatus(w http.ResponseWriter, r *http.Request) {
	if d.autofixConfig().Hub == nil {
		writeErr(w, 404, errors.New("not connected to CSM Hub"))

		return
	}

	writeOK(w, d.hubStatus(r.Context()))
}

func (d *daemon) hubStatus(ctx context.Context) hubStatusView {
	cfg := d.autofixConfig()
	v := hubStatusView{Integrations: []hubIntegration{}, Mappings: []hubMapping{}, HasSecret: cfg.SentrySecret != ""}

	if cfg.Hub == nil {
		return v
	}

	v.URL, v.Public = cfg.Hub.URL, publicHubURL(cfg.Hub.URL)

	if s := d.hub.currentSession(); s != nil && s.url == cfg.Hub.URL {
		v.SignedIn = s.email
	}

	c, ok := d.hub.client()
	if !ok {
		v.Error = "no runner token"

		return v
	}

	var out struct {
		Workspace    hubWorkspace     `json:"workspace"`
		Runner       hubWorkspace     `json:"runner"`
		Integrations []hubIntegration `json:"integrations"`
		Mappings     []hubMapping     `json:"mappings"`
	}

	if _, err := c.do(ctx, "GET", "/api/v1/runner/setup", nil, &out); err != nil {
		v.Error = err.Error()

		return v
	}

	v.Reachable, v.Workspace, v.Runner = true, &out.Workspace, out.Runner.Name

	for _, i := range out.Integrations {
		i.WebhookURL = cfg.Hub.URL + i.WebhookPath
		v.Integrations = append(v.Integrations, i)
	}

	if out.Mappings != nil {
		v.Mappings = out.Mappings
	}

	return v
}

// postHubTest sends Hub a delivery signed with the client secret saved here, marked as a CSM test so Hub verifies it
// and creates nothing. It proves the webhook URL and the secret match; only a real Sentry event proves Sentry is set up.
func (d *daemon) postHubTest(w http.ResponseWriter, r *http.Request) {
	cfg := d.autofixConfig()

	if cfg.SentrySecret == "" {
		writeErr(w, 409, errors.New("paste Sentry's client secret first"))

		return
	}

	st := d.hubStatus(r.Context())

	if !st.Reachable || len(st.Integrations) == 0 {
		writeErr(w, 409, errors.New(orValue(st.Error, "this workspace has no Sentry integration")))

		return
	}

	body := []byte(`{"action":"test","installation":{"uuid":"csm"}}`)
	ctx, cancel := context.WithTimeout(r.Context(), hubRequestTimeout)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "POST", st.Integrations[0].WebhookURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sentry-Hook-Resource", engrun.SentryTestResource)
	req.Header.Set("Sentry-Hook-Signature", engrun.SignSentry(cfg.SentrySecret, body))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeOK(w, map[string]any{"ok": false, "message": "The webhook URL could not be reached: " + err.Error()})

		return
	}

	defer resp.Body.Close()

	var out struct {
		Reason string `json:"reason"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out)

	if resp.StatusCode != 200 {
		writeOK(w, map[string]any{"ok": false, "message": fmt.Sprintf("Hub refused the test (%d): %s", resp.StatusCode, orValue(out.Error.Message, resp.Status))})

		return
	}

	writeOK(w, map[string]any{"ok": true, "message": orValue(out.Reason, "Hub verified the signature.")})
}

func (d *daemon) saveAutofixConfig(cfg autofixConfig) error {
	path := filepath.Join(d.home, autofixConfigFile)

	if err := writeJSON(path, cfg); err != nil {
		return err
	}

	return os.Chmod(path, 0o600)
}

func upsertMapping(ms []RepoMapping, m RepoMapping) []RepoMapping {
	for i := range ms {
		if ms[i].Source == m.Source && ms[i].Project == m.Project {
			m.Agents = ms[i].Agents
			ms[i] = m

			return ms
		}
	}

	return append(ms, m)
}

// repoSlug reads owner/name from the origin remote, in any of the forms git accepts.
func repoSlug(dir string) string {
	remote, err := gitOut(dir, "remote", "get-url", "origin")
	if err != nil || remote == "" {
		return ""
	}

	remote = strings.TrimSuffix(strings.TrimSpace(remote), ".git")

	switch i := strings.Index(remote, "://"); {
	case i >= 0:
		remote = remote[i+3:]
	case strings.Contains(remote, "@") && strings.Contains(remote, ":"):
		remote = "/" + remote[strings.Index(remote, ":")+1:]
	default:
		return ""
	}

	parts := strings.Split(strings.Trim(remote, "/"), "/")

	if len(parts) < 2 {
		return ""
	}

	owner, name := parts[len(parts)-2], parts[len(parts)-1]

	if !validSlugPart(owner) || !validSlugPart(name) {
		return ""
	}

	return owner + "/" + name
}

func validSlugPart(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}

	for _, r := range s {
		if !(r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}

	return true
}
