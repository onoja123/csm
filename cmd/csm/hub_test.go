package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onoja123/csm/engrun"
)

// fakeHub implements the runner side of CSM Hub's API: one job to hand out, and the reports it receives.
type fakeHub struct {
	mu       sync.Mutex
	job      *hubJob
	claimed  int
	reports  []hubReport
	states   []engrun.State
	auth     []string
	complete bool
	cancel   bool
	seqs     map[int]bool
}

func (f *fakeHub) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/runner/claim", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		f.auth = append(f.auth, r.Header.Get("Authorization"))

		if f.job == nil || f.claimed > 0 {
			w.WriteHeader(204)

			return
		}

		f.claimed++
		json.NewEncoder(w).Encode(f.job)
	})

	mux.HandleFunc("POST /api/v1/runner/runs/{id}/report", func(w http.ResponseWriter, r *http.Request) {
		var rep hubReport

		json.NewDecoder(r.Body).Decode(&rep)
		f.mu.Lock()
		defer f.mu.Unlock()

		for _, ev := range rep.Events {
			if f.seqs[ev.Seq] {
				http.Error(w, "duplicate seq", 409)

				return
			}

			f.seqs[ev.Seq] = true
		}

		path := rep.States

		if len(path) == 0 {
			path = []engrun.State{rep.State}
		}

		for _, st := range path {
			if n := len(f.states); n == 0 || f.states[n-1] != st {
				if n > 0 && !engrun.CanTransition(f.states[n-1], st) {
					http.Error(w, "invalid transition "+string(f.states[n-1])+" → "+string(st), 409)

					return
				}

				f.states = append(f.states, st)
			}
		}

		f.reports = append(f.reports, rep)
		json.NewEncoder(w).Encode(map[string]any{"accepted": len(rep.Events), "cancel_requested": f.cancel})
	})

	mux.HandleFunc("POST /api/v1/runner/jobs/{id}/heartbeat", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("POST /api/v1/runner/jobs/{id}/complete", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.complete = true
		f.mu.Unlock()
	})

	return mux
}

func TestHubRunnerClaimsRunsAndReportsBack(t *testing.T) {
	h := newAutofixHarness(t)
	body := sentryEvent("evt-hub")
	sig, err := engrun.ParseSentry(engrun.SentryError, body, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	hub := &fakeHub{job: &hubJob{ID: "job-1", RunID: "run-1", IncidentID: "inc-1", Attempt: 1, Repository: "acme/demo-api", Signal: sig}, seqs: map[int]bool{}}
	srv := httptest.NewServer(hub.handler())
	t.Cleanup(srv.Close)

	cfg := h.d.autofixConfig()
	cfg.Hub = &hubConfig{URL: srv.URL, Token: "csmr_test", RunnerName: "test-runner"}
	writeJSON(filepath.Join(h.home, autofixConfigFile), cfg)

	runner := &hubRunner{d: h.d}
	ctx := context.Background()
	runner.tick(ctx)

	local := h.d.builds.byHubRun("run-1")
	if local == "" {
		_, lastErr := runner.status()
		t.Fatalf("the claimed job did not become a local run (%s)", lastErr)
	}

	runner.tick(ctx)

	if local2 := h.d.builds.byHubRun("run-1"); local2 != local || hub.claimed != 1 {
		t.Fatal("a job is executed once")
	}

	if incs := h.d.signals.list(); len(incs) != 1 || len(incs[0].Runs) != 1 || incs[0].Runs[0] != local {
		t.Fatalf("a claimed run appears with its incident: %+v", incs)
	}

	deadline := time.Now().Add(120 * time.Second)

	for {
		runner.tick(ctx)
		hub.mu.Lock()
		last := engrun.State("")

		if n := len(hub.states); n > 0 {
			last = hub.states[n-1]
		}

		hub.mu.Unlock()

		if last == engrun.StateAwaitPR || time.Now().After(deadline) {
			break
		}

		time.Sleep(2 * time.Second)
	}

	hub.mu.Lock()
	defer hub.mu.Unlock()

	if _, lastErr := runner.status(); lastErr != "" {
		t.Fatalf("runner error: %s", lastErr)
	}

	if n := len(hub.states); n == 0 || hub.states[n-1] != engrun.StateAwaitPR {
		t.Fatalf("states reported: %v", hub.states)
	}

	for _, want := range []engrun.State{engrun.StateInvestigating, engrun.StateReproducing, engrun.StateImplementing, engrun.StateVerifying, engrun.StateReviewing} {
		found := false

		for _, s := range hub.states {
			found = found || s == want
		}

		if !found {
			t.Errorf("%s was never reported: %v", want, hub.states)
		}
	}

	last := hub.reports[len(hub.reports)-1]

	if last.Reproduction != engrun.ReproReproduced || last.Verdict != engrun.VerdictVerified || last.LocalRunID != local {
		t.Fatalf("last report %+v", last)
	}

	for _, a := range hub.auth {
		if a != "Bearer csmr_test" {
			t.Fatalf("auth header %q", a)
		}
	}
}

func TestHubCancellationReachesTheLocalRun(t *testing.T) {
	h := newAutofixHarness(t)
	h.fixture(investigateStep, nil, nil)
	sig, _ := engrun.ParseSentry(engrun.SentryError, sentryEvent("evt-cancel"), "", time.Now())
	hub := &fakeHub{job: &hubJob{ID: "job-2", RunID: "run-2", Signal: sig}, seqs: map[int]bool{}, cancel: true}
	srv := httptest.NewServer(hub.handler())
	t.Cleanup(srv.Close)

	cfg := h.d.autofixConfig()
	cfg.Hub = &hubConfig{URL: srv.URL, Token: "csmr_test"}
	writeJSON(filepath.Join(h.home, autofixConfigFile), cfg)

	runner := &hubRunner{d: h.d}
	runner.tick(context.Background())
	local := h.d.builds.byHubRun("run-2")
	runner.tick(context.Background())

	got := h.waitFor(local, 30*time.Second, statusIs(BuildCancelled))

	if got.Status != BuildCancelled {
		t.Fatal(got.Status)
	}
}

func TestHubURLMustBeSecure(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://hub.example.com":     true,
		"http://localhost:8080":       true,
		"http://hub.example.com":      false,
		"https://user:pw@hub.example": false,
		"ftp://hub.example.com":       false,
		"hub.example.com":             false,
	} {
		if _, err := validHubURL(raw); (err == nil) != ok {
			t.Errorf("%s: %v", raw, err)
		}
	}

	t.Setenv("CSM_HUB_TOKEN", "csmr_secret")

	if strings.Contains(strings.Join(childEnv(), "\n"), "csmr_secret") {
		t.Fatal("the Hub token must never reach an agent's environment")
	}
}
