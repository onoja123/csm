package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fakeSentryToken = "sntryu_test_token"

// fakeSentry serves the parts of Sentry's REST API that CSM reads, for one organization with one issue.
func fakeSentry(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	issue := `{"id":"7001","shortId":"DEMO-API-1","title":"runtime error: integer divide by zero","culprit":"stats.go in Average","level":"error",
	"platform":"go","permalink":"https://acme.sentry.io/issues/7001/","count":"37","userCount":4,"firstSeen":"2026-10-12T09:00:00Z",
	"lastSeen":"2026-10-13T08:30:00Z","project":{"id":"11","slug":"demo-api"},"metadata":{"type":"runtime.Error","value":"integer divide by zero"}}`
	event := `{"eventID":"abc123","groupID":"7001","dateCreated":"2026-10-13T08:30:00Z","platform":"go",
	"tags":[{"key":"environment","value":"production"}],
	"entries":[{"type":"exception","data":{"values":[{"type":"runtime.Error","value":"integer divide by zero",
	"stacktrace":{"frames":[{"filename":"stats.go","function":"Average","lineNo":11,"inApp":true,"context":[[11,"return sum / len(xs)"]]}]}}]}}]}`
	auth := func(fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+fakeSentryToken {
				http.Error(w, `{"detail":"Invalid token"}`, 401)

				return
			}

			fn(w, r)
		}
	}

	mux.HandleFunc("GET /api/0/organizations/", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"slug":"acme","name":"Acme"}]`))
	}))
	mux.HandleFunc("GET /api/0/organizations/acme/projects/", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":"11","slug":"demo-api","name":"demo-api"},{"id":"12","slug":"billing","name":"billing"}]`))
	}))
	mux.HandleFunc("GET /api/0/organizations/acme/issues/", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") != "is:unresolved" || r.URL.Query().Get("project") != "11" {
			http.Error(w, "bad query "+r.URL.RawQuery, 400)

			return
		}

		w.Write([]byte("[" + issue + "]"))
	}))
	mux.HandleFunc("GET /api/0/organizations/acme/shortids/DEMO-API-1/", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"groupId":"7001","group":` + issue + `}`))
	}))
	mux.HandleFunc("GET /api/0/organizations/acme/shortids/DEMO-API-404/", auth(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"not found"}`, 404)
	}))
	mux.HandleFunc("GET /api/0/organizations/acme/issues/7001/events/latest/", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(event))
	}))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv
}

func TestSentryFromTheCLI(t *testing.T) {
	h := newAutofixHarness(t)
	sentry := fakeSentry(t)

	var out bytes.Buffer

	log := &Logger{out: &out, err: &out}

	cfg := loadAutofixConfig(h.home)
	cfg.Mappings = nil
	saveAutofixConfigAt(h.home, cfg)

	wd, _ := os.Getwd()
	os.Chdir(h.repo)
	t.Cleanup(func() { os.Chdir(wd) })

	t.Setenv("SENTRY_AUTH_TOKEN", "wrong")

	if err := sentryConnect(log, h.home, []string{"--url", sentry.URL, "--project", "demo-api"}); err == nil || !strings.Contains(err.Error(), "rejected the auth token") {
		t.Fatalf("a bad token is reported: %v", err)
	}

	t.Setenv("SENTRY_AUTH_TOKEN", fakeSentryToken)

	if err := sentryConnect(log, h.home, []string{"--url", sentry.URL, "--project", "demo-api"}); err != nil {
		t.Fatalf("connect: %v\n%s", err, out.String())
	}

	cfg = loadAutofixConfig(h.home)

	if cfg.Sentry == nil || cfg.Sentry.Org != "acme" || cfg.Sentry.Projects["demo-api"] != "11" || cfg.Sentry.Token != fakeSentryToken {
		t.Fatalf("connect saves the org, the project id and the token: %+v", cfg.Sentry)
	}

	if m, ok := cfg.mapping("sentry", "demo-api"); !ok || !sameDir(m.Repo, h.repo) {
		t.Fatalf("the project is mapped to the repository it was connected in: %+v", cfg.Mappings)
	}

	if fi, _ := os.Stat(autofixConfigPath(h.home)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("the token file is private: %v", fi.Mode())
	}

	if strings.Contains(out.String(), fakeSentryToken) {
		t.Fatal("the token is never printed")
	}

	out.Reset()

	if err := cmdIssues(log, h.home, nil); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "DEMO-API-1") || !strings.Contains(out.String(), "37") || !strings.Contains(out.String(), "runtime error: integer divide by zero") {
		t.Fatalf("issues are listed: %s", out.String())
	}

	var fixed struct {
		Run      buildSummary `json:"run"`
		Existing bool         `json:"existing"`
	}

	post := func(issue string, out any) int {
		resp, err := http.Post(h.srv.URL+"/v1/sentry/fix", "application/json", strings.NewReader(`{"issue":"`+issue+`"}`))
		if err != nil {
			t.Fatal(err)
		}

		defer resp.Body.Close()
		json.NewDecoder(resp.Body).Decode(out)

		return resp.StatusCode
	}

	if code := post("DEMO-API-1", &fixed); code != 200 || fixed.Run.ID == "" || fixed.Existing {
		t.Fatalf("fix starts a run: %d %+v", code, fixed)
	}

	b, err := newBuildStore(h.home).load(fixed.Run.ID)
	if err != nil || b.Kind != "autofix" || b.Signal == nil || b.Signal.ExternalID != "event:abc123" || b.Signal.Evidence.Sentry.Count != 37 || b.Signal.Environment != "production" {
		t.Fatalf("the run carries the pulled evidence: %+v %v", b, err)
	}

	var again struct {
		Run      buildSummary `json:"run"`
		Existing bool         `json:"existing"`
	}

	if code := post("demo-api-1", &again); code != 200 || !again.Existing || again.Run.ID != fixed.Run.ID {
		t.Fatalf("fixing it again while the run is active returns that run: %d %+v", code, again)
	}

	var e apiError

	if code := post("DEMO-API-404", &e); code != 404 || !strings.Contains(e.Error, "no issue") {
		t.Fatalf("an unknown issue is reported: %d %+v", code, e)
	}

	if code := post("not an id", &e); code == 200 {
		t.Fatal("a malformed id is refused")
	}

	time.Sleep(200 * time.Millisecond)
	out.Reset()
	cmdIssues(log, h.home, nil)

	if line := issueLine(out.String(), "DEMO-API-1"); strings.Contains(line, "—") {
		t.Fatalf("the issue list shows CSM's run: %q", line)
	}

	cfg = loadAutofixConfig(h.home)
	cfg.Mappings = nil
	saveAutofixConfigAt(h.home, cfg)

	if code := post("DEMO-API-1", &e); code != 409 || !strings.Contains(e.Error, "not mapped") {
		t.Fatalf("an unmapped project starts nothing: %d %+v", code, e)
	}

	if err := cmdSentry(log, h.home, []string{"disconnect"}); err != nil || loadAutofixConfig(h.home).Sentry != nil {
		t.Fatal("disconnect removes the token")
	}
}

func issueLine(out, id string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, id) {
			return l
		}
	}

	return ""
}

func TestSentryURLAndTokenStayPrivate(t *testing.T) {
	for raw, ok := range map[string]bool{"": true, "https://sentry.io": true, "https://de.sentry.io/": true, "http://localhost:9000": true, "http://sentry.acme.dev": false, "https://u:p@sentry.io": false} {
		if _, err := validSentryURL(raw); (err == nil) != ok {
			t.Errorf("%q: %v", raw, err)
		}
	}

	t.Setenv("SENTRY_AUTH_TOKEN", "sntryu_secret")

	if strings.Contains(strings.Join(childEnv(), "\n"), "sntryu_secret") {
		t.Fatal("the Sentry token never reaches an agent's environment")
	}
}

func sameDir(a, b string) bool {
	ra, _ := filepath.EvalSymlinks(a)
	rb, _ := filepath.EvalSymlinks(b)

	return ra == rb
}
