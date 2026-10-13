package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/onoja123/csm/engrun"
)

// setupHub implements the parts of CSM Hub's user and runner API that the Sentry setup uses.
type setupHub struct {
	mu        sync.Mutex
	srv       *httptest.Server
	secret    string
	created   int
	runners   int
	mappings  []hubMapping
	delivered string
}

func (f *setupHub) handler() http.Handler {
	mux := http.NewServeMux()
	user := func(fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if c, err := r.Cookie("autofixer_session"); err != nil || c.Value != "s1" {
				http.Error(w, `{"error":{"message":"Sign in first."}}`, 401)

				return
			}

			f.mu.Lock()
			defer f.mu.Unlock()
			fn(w, r)
		}
	}
	runner := func(fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer csmr_issued" {
				http.Error(w, `{"error":{"message":"Unknown or revoked runner token."}}`, 401)

				return
			}

			f.mu.Lock()
			defer f.mu.Unlock()
			fn(w, r)
		}
	}
	integ := func() []hubIntegration {
		if f.created == 0 {
			return []hubIntegration{}
		}

		return []hubIntegration{{ID: "i1", Kind: "sentry", Name: "Sentry", WebhookPath: "/api/v1/integrations/i1/sentry", HasSecret: f.secret != "", LastDeliveryResult: f.delivered}}
	}
	reply := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }

	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string

		json.NewDecoder(r.Body).Decode(&in)

		if in["password"] != "correct-horse" {
			http.Error(w, `{"error":{"message":"Invalid email or password."}}`, 401)

			return
		}

		http.SetCookie(w, &http.Cookie{Name: "autofixer_session", Value: "s1", Path: "/"})
		reply(w, map[string]string{"email": in["email"]})
	})
	mux.HandleFunc("GET /api/v1/workspaces", user(func(w http.ResponseWriter, r *http.Request) {
		reply(w, []hubWorkspace{{ID: "w1", Name: "Acme", Role: "owner"}})
	}))
	mux.HandleFunc("GET /api/v1/workspaces/w1", user(func(w http.ResponseWriter, r *http.Request) {
		reply(w, hubWorkspace{ID: "w1", Name: "Acme"})
	}))
	mux.HandleFunc("GET /api/v1/workspaces/w1/integrations", user(func(w http.ResponseWriter, r *http.Request) { reply(w, integ()) }))
	mux.HandleFunc("POST /api/v1/workspaces/w1/integrations", user(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string

		json.NewDecoder(r.Body).Decode(&in)
		f.created++
		f.secret = in["secret"]
		reply(w, map[string]any{"integration": integ()[0]})
	}))
	mux.HandleFunc("PUT /api/v1/workspaces/w1/integrations/i1/secret", user(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string

		json.NewDecoder(r.Body).Decode(&in)
		f.secret = in["secret"]
		reply(w, map[string]string{"status": "saved"})
	}))
	mux.HandleFunc("PUT /api/v1/workspaces/w1/repo-mappings", user(func(w http.ResponseWriter, r *http.Request) {
		var m hubMapping

		json.NewDecoder(r.Body).Decode(&m)
		f.mappings = append(f.mappings, m)
		reply(w, m)
	}))
	mux.HandleFunc("POST /api/v1/workspaces/w1/runners", user(func(w http.ResponseWriter, r *http.Request) {
		f.runners++
		reply(w, map[string]string{"token": "csmr_issued"})
	}))
	mux.HandleFunc("GET /api/v1/runner/setup", runner(func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"workspace": hubWorkspace{ID: "w1", Name: "Acme"}, "runner": map[string]string{"name": "laptop"}, "integrations": integ(), "mappings": f.mappings})
	}))
	mux.HandleFunc("POST /api/v1/integrations/i1/sentry", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()

		if f.secret == "" || !engrun.VerifySentrySignature(f.secret, body, r.Header.Get("Sentry-Hook-Signature")) {
			f.delivered = "invalid_signature"
			http.Error(w, `{"error":{"message":"The webhook signature is invalid."}}`, 401)

			return
		}

		f.delivered = "test"
		reply(w, map[string]string{"status": "test", "reason": "The Hub verified the signature. No incident was created."})
	})

	return mux
}

func (h *autofixHarness) call(method, path string, body any, out any) int {
	h.t.Helper()

	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, h.srv.URL+path, bytes.NewReader(data))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if out != nil {
		json.Unmarshal(raw, out)
	}

	return resp.StatusCode
}

func TestSentrySetupThroughHub(t *testing.T) {
	h := newAutofixHarness(t)
	hub := &setupHub{}
	hub.srv = httptest.NewServer(hub.handler())
	t.Cleanup(hub.srv.Close)

	setup := map[string]string{"workspace_id": "w1", "project": "orders-api", "repo": h.repo, "repository": "acme/orders-api", "base_branch": "main"}

	if code := h.call("POST", "/v1/autofix/hub/setup", setup, nil); code != 401 {
		t.Fatalf("setup needs a Hub sign-in first: %d", code)
	}

	var bad struct{ Error string }

	if code := h.call("POST", "/v1/autofix/hub/signin", map[string]string{"url": hub.srv.URL, "email": "me@acme.test", "password": "wrong"}, &bad); code != 401 || !strings.Contains(bad.Error, "Invalid email") {
		t.Fatalf("a wrong password is reported: %d %+v", code, bad)
	}

	var signin struct {
		Workspaces []hubWorkspace `json:"workspaces"`
		Public     bool           `json:"public"`
	}

	if code := h.call("POST", "/v1/autofix/hub/signin", map[string]string{"url": hub.srv.URL, "email": "me@acme.test", "password": "correct-horse"}, &signin); code != 200 || len(signin.Workspaces) != 1 || signin.Public {
		t.Fatalf("sign-in lists workspaces and flags a local Hub: %d %+v", code, signin)
	}

	var st hubStatusView

	if code := h.call("POST", "/v1/autofix/hub/setup", setup, &st); code != 200 {
		t.Fatalf("setup: %d", code)
	}

	if len(st.Integrations) != 1 || st.Integrations[0].WebhookURL != hub.srv.URL+"/api/v1/integrations/i1/sentry" || st.Integrations[0].HasSecret || st.Workspace.Name != "Acme" {
		t.Fatalf("setup answers with the webhook URL before Sentry has given a secret: %+v", st)
	}

	if len(hub.mappings) != 1 || hub.mappings[0].Repository != "acme/orders-api" || hub.mappings[0].Project != "orders-api" {
		t.Fatalf("the project is mapped on Hub: %+v", hub.mappings)
	}

	cfg := h.d.autofixConfig()

	if cfg.Hub == nil || cfg.Hub.Token != "csmr_issued" || cfg.Hub.WorkspaceID != "w1" {
		t.Fatalf("this machine is connected as the runner: %+v", cfg.Hub)
	}

	if m, ok := cfg.mapping("sentry", "orders-api"); !ok || m.Repo != h.repo {
		t.Fatalf("the project is mapped locally: %+v", cfg.Mappings)
	}

	var test struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}

	h.d.hub.setSession(nil)
	secret := "0123456789abcdef0123456789abcdef"

	if code := h.call("PUT", "/v1/autofix/hub/secret", map[string]string{"secret": secret}, nil); code != 401 {
		t.Fatal("changing the secret needs a live Hub session")
	}

	h.call("POST", "/v1/autofix/hub/signin", map[string]string{"url": hub.srv.URL, "email": "me@acme.test", "password": "correct-horse"}, nil)

	if code := h.call("PUT", "/v1/autofix/hub/secret", map[string]string{"secret": secret}, &st); code != 200 || !st.Integrations[0].HasSecret || hub.secret != secret {
		t.Fatalf("the client secret reaches Hub: %d %+v", code, st)
	}

	if code := h.call("POST", "/v1/autofix/hub/test", nil, &test); code != 200 || !test.OK || hub.delivered != "test" {
		t.Fatalf("a signed test reaches Hub: %d %+v", code, test)
	}

	cfg = h.d.autofixConfig()
	cfg.SentrySecret = "a-different-secret-0123456789"
	h.d.saveAutofixConfig(cfg)

	if h.call("POST", "/v1/autofix/hub/test", nil, &test); test.OK || !strings.Contains(test.Message, "signature") {
		t.Fatalf("a mismatched secret is reported: %+v", test)
	}

	if code := h.call("POST", "/v1/autofix/hub/setup", setup, &st); code != 200 || hub.created != 1 || hub.runners != 1 {
		t.Fatalf("running the setup again reuses the integration and the runner token: %d created=%d runners=%d", code, hub.created, hub.runners)
	}

	var view autofixConfigView

	h.call("GET", "/v1/autofix", nil, &view)

	if raw, _ := json.Marshal(view); strings.Contains(string(raw), "csmr_issued") || strings.Contains(string(raw), secret) || view.Hub.Workspace != "Acme" {
		t.Fatalf("settings never return the runner token or the client secret: %s", raw)
	}
}

func TestRepoSlugReadsTheOriginRemote(t *testing.T) {
	h := newBuildHarness(t, nil, nil)
	h.git("remote", "add", "origin", "/tmp/none")

	for remote, want := range map[string]string{
		"git@github.com:acme/orders-api.git":       "acme/orders-api",
		"https://github.com/acme/orders-api":       "acme/orders-api",
		"ssh://git@github.com/acme/orders-api.git": "acme/orders-api",
		"/srv/git/orders":                          "",
	} {
		h.git("remote", "set-url", "origin", remote)

		if got := repoSlug(h.repo); got != want {
			t.Errorf("%s: got %q, want %q", remote, got, want)
		}
	}
}
