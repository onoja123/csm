package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testDaemon(t *testing.T) (*daemon, *httptest.Server) {
	t.Helper()
	home := t.TempDir()

	if _, err := initState(home); err != nil {
		t.Fatal(err)
	}

	d, err := newDaemon(home, "/bin/false", "test")
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)

	return d, srv
}

func call(t *testing.T, srv *httptest.Server, method, path string, body any) (int, map[string]any, []byte) {
	t.Helper()
	var buf bytes.Buffer

	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}

	req, _ := http.NewRequest(method, srv.URL+path, &buf)
	resp, err := http.DefaultClient.Do(req)

	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	var raw bytes.Buffer
	raw.ReadFrom(resp.Body)
	var out map[string]any
	json.Unmarshal(raw.Bytes(), &out)

	return resp.StatusCode, out, raw.Bytes()
}

func TestStatusAndSettings(t *testing.T) {
	_, srv := testDaemon(t)
	code, out, _ := call(t, srv, "GET", "/v1/status", nil)

	if code != 200 || out["failover_policy"] != policyManual || out["approvals_enabled"] != true {
		t.Fatalf("status: %d %v", code, out)
	}

	off := false
	code, out, _ = call(t, srv, "PUT", "/v1/settings", map[string]any{"approvals_enabled": off})

	if code != 200 || out["approvals_enabled"] != false {
		t.Fatalf("settings: %d %v", code, out)
	}

	code, out, _ = call(t, srv, "GET", "/v1/status", nil)

	if out["approvals_enabled"] != false {
		t.Fatalf("status after settings: %d %v", code, out)
	}
}

func TestAccountsEmptyAndUnknown(t *testing.T) {
	_, srv := testDaemon(t)
	code, _, raw := call(t, srv, "GET", "/v1/accounts", nil)

	if code != 200 || strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("accounts: %d %s", code, raw)
	}

	code, out, _ := call(t, srv, "GET", "/v1/accounts/nobody", nil)

	if code != 404 || out["error"] == nil {
		t.Fatalf("unknown account: %d %v", code, out)
	}
}

func TestAccountActionsOnDisk(t *testing.T) {
	d, srv := testDaemon(t)
	s, _ := loadState(d.home)
	now := time.Now()
	s.addAccount("claude", "work", now)
	s.addAccount("claude", "personal", now)
	s.Accounts[0].VerifiedAt, s.Accounts[1].VerifiedAt = now, now
	s.save()

	code, out, _ := call(t, srv, "POST", "/v1/accounts/personal/priority", map[string]int{"position": 1})

	if code != 200 || out["priority"] != float64(1) {
		t.Fatalf("priority: %d %v", code, out)
	}

	code, out, _ = call(t, srv, "POST", "/v1/accounts/work/disable", nil)

	if code != 200 || out["enabled"] != false || out["health"] != string(healthUnavailable) {
		t.Fatalf("disable: %d %v", code, out)
	}

	code, out, _ = call(t, srv, "PUT", "/v1/failover", map[string]string{"policy": "automatic"})

	if code != 200 || out["policy"] != policyAutomatic {
		t.Fatalf("failover: %d %v", code, out)
	}

	code, out, _ = call(t, srv, "PUT", "/v1/failover", map[string]string{"policy": "sometimes"})

	if code != 400 {
		t.Fatalf("bad policy accepted: %d %v", code, out)
	}

	code, _, raw := call(t, srv, "GET", "/v1/activity", nil)

	if code != 200 || !strings.Contains(string(raw), "Failover policy set to automatic") {
		t.Fatalf("activity: %d %s", code, raw)
	}
}

func TestApprovalFlow(t *testing.T) {
	d, srv := testDaemon(t)
	payload := map[string]any{"session_id": "s1", "tool_name": "Bash", "tool_input": map[string]string{"command": "rm -rf build", "description": "clean"}}
	req := map[string]any{"csm_pid": 12345, "account": "work", "state_dir": "/tmp/x", "payload": payload}

	// Nobody is listening, so the agent keeps its own prompt.
	code, out, _ := call(t, srv, "POST", "/v1/hooks/pre-tool-use", req)

	if code != 200 || out["decision"] != "" {
		t.Fatalf("hook without subscribers: %d %v", code, out)
	}

	ch, cancel := d.bus.subscribe()
	defer cancel()

	type reply struct {
		code int
		out  map[string]any
	}

	replies := make(chan reply, 1)
	go func() {
		code, out, _ := call(t, srv, "POST", "/v1/hooks/pre-tool-use", req)
		replies <- reply{code, out}
	}()

	var requested Event

	for {
		ev := <-ch

		if ev.Type == "approval.requested" {
			requested = ev
			break
		}
	}

	var a Approval
	json.Unmarshal(requested.Data, &a)

	if !a.Destructive || a.Command != "rm -rf build" {
		t.Fatalf("approval view: %+v", a)
	}

	code, out, _ = call(t, srv, "POST", "/v1/approvals/"+a.ID+"/decision", map[string]string{"decision": "maybe"})

	if code != 400 {
		t.Fatalf("bad decision accepted: %d %v", code, out)
	}

	code, out, _ = call(t, srv, "POST", "/v1/approvals/"+a.ID+"/decision", map[string]string{"decision": "deny"})

	if code != 200 || out["decision"] != "deny" {
		t.Fatalf("decision: %d %v", code, out)
	}

	r := <-replies

	if r.code != 200 || r.out["decision"] != "deny" {
		t.Fatalf("hook reply: %+v", r)
	}

	if string(hookResponse("deny")) == "" || hookResponse("") != nil {
		t.Fatal("hook response shape")
	}
}

func TestProjectsAndRuns(t *testing.T) {
	d, srv := testDaemon(t)
	code, out, _ := call(t, srv, "POST", "/v1/projects", map[string]string{"dir": d.home})

	if code != 200 || out["dir"] != d.home {
		t.Fatalf("project: %d %v", code, out)
	}

	code, _, raw := call(t, srv, "GET", "/v1/projects", nil)

	if code != 200 || !strings.Contains(string(raw), filepath.Base(d.home)) {
		t.Fatalf("projects: %d %s", code, raw)
	}

	code, out, _ = call(t, srv, "POST", "/v1/runs", map[string]any{"provider": "claude", "project_dir": d.home})

	if code != 409 || !strings.Contains(out["error"].(string), "no Claude Code accounts") {
		t.Fatalf("run without accounts: %d %v", code, out)
	}

	code, out, _ = call(t, srv, "POST", "/v1/runs/nope/input", map[string]string{"text": "hi"})

	if code != 404 {
		t.Fatalf("input to unknown run: %d %v", code, out)
	}

	code, _, raw = call(t, srv, "GET", "/v1/runs", nil)

	if code != 200 || !strings.Contains(string(raw), `"runs":[]`) {
		t.Fatalf("runs: %d %s", code, raw)
	}
}

func TestDestructiveDetection(t *testing.T) {
	for _, cmd := range []string{"rm -rf node_modules", "git push --force origin main", "git reset --hard HEAD~1", "DROP TABLE users;"} {
		if !looksDestructive(cmd) {
			t.Errorf("%q should look destructive", cmd)
		}
	}

	for _, cmd := range []string{"npm install stripe@latest", "git status", "ls -la", "rm build/output.txt"} {
		if looksDestructive(cmd) {
			t.Errorf("%q should not look destructive", cmd)
		}
	}
}
