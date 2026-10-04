package provider

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// CSM_FAKE_LIMIT profiles report a reached limit; CSM_FAKE_HOLD profiles run until stopped.
func RunFakeCodex(args []string) int {
	home := os.Getenv("CODEX_HOME")
	authFile := filepath.Join(home, "fake-auth")
	profile := filepath.Base(home)
	limited := slices.Contains(strings.Split(os.Getenv("CSM_FAKE_LIMIT"), ","), profile)

	args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--no-daemon" })
	switch {
	case len(args) > 0 && args[0] == "--version":
		fmt.Println("codex-cli 0.0.0-fake")
		return 0
	case len(args) > 0 && args[0] == "--help":
		fmt.Println("Codex CLI\n\nUsage: codex [OPTIONS] [PROMPT]\n\nCommands:\n  login             Manage login\n  app-server        [experimental] Run the app server or related tooling\n  resume            Resume a previous interactive session\n\nOptions:\n      --no-daemon\n          Run without the shared background server")
		return 0
	case len(args) > 0 && args[0] == "login":
		os.WriteFile(authFile, []byte(profile+"@example.test"), 0o600)
		return 0
	case len(args) > 0 && args[0] == "app-server":
		return fakeCodexAppServer(home, authFile, limited)
	}

	sessionID, resume := NewSessionID(), false
	if len(args) >= 2 && args[0] == "resume" {
		sessionID, resume = args[1], true
	}
	rollout := fakeCodexRollout(home, sessionID)
	if resume {
		if _, err := os.Stat(rollout); err != nil {
			fmt.Fprintf(os.Stderr, "No saved session found with ID %s\n", sessionID)
			return 1
		}
	}
	cwd, _ := os.Getwd()
	os.MkdirAll(filepath.Dir(rollout), 0o700)
	f, err := os.OpenFile(rollout, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 1
	}
	fmt.Fprintf(f, "{\"id\":%q,\"cwd\":%q,\"profile\":%q,\"resumed\":%t}\n", sessionID, cwd, profile, resume)
	f.Close()

	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	wait := time.Duration(0)
	switch {
	case slices.Contains(strings.Split(os.Getenv("CSM_FAKE_HOLD"), ","), profile):
		wait = 30 * time.Second
	case limited:
		wait = 2 * time.Second
	}
	select {
	case <-term:
		return 143
	case <-time.After(wait):
		return 0
	}
}

func fakeCodexRollout(home, sessionID string) string {
	return filepath.Join(home, "sessions", "2026", "01", "01", "rollout-2026-01-01T00-00-00-"+sessionID+".jsonl")
}

type fakeCodexRequest struct {
	ID     *int   `json:"id"`
	Method string `json:"method"`
	Params struct {
		Cwd      []string `json:"cwd"`
		ThreadID string   `json:"threadId"`
	} `json:"params"`
}

func fakeCodexAppServer(home, authFile string, limited bool) int {
	out := json.NewEncoder(os.Stdout)
	reply := func(id int, result any) { out.Encode(map[string]any{"id": id, "result": result}) }
	fail := func(id int, message string) {
		out.Encode(map[string]any{"id": id, "error": map[string]any{"code": -32600, "message": message}})
	}
	email, authErr := os.ReadFile(authFile)

	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req fakeCodexRequest
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		id := *req.ID
		switch req.Method {
		case "initialize":
			reply(id, map[string]any{"codexHome": home})
			out.Encode(map[string]any{"method": "remoteControl/status/changed", "params": map[string]any{"status": "disabled"}})
		case "account/read":
			if authErr != nil {
				reply(id, map[string]any{"account": nil, "requiresOpenaiAuth": true})
				continue
			}
			reply(id, map[string]any{"account": map[string]any{"type": "chatgpt", "email": string(email), "planType": "plus"}, "requiresOpenaiAuth": true})
		case "account/rateLimits/read":
			if authErr != nil {
				fail(id, "codex account authentication required to read rate limits")
				continue
			}
			primary := map[string]any{"usedPercent": 42, "resetsAt": time.Now().Add(2 * time.Hour).Unix(), "windowDurationMins": 300}
			secondary := map[string]any{"usedPercent": 17, "resetsAt": time.Now().Add(72 * time.Hour).Unix(), "windowDurationMins": 10080}
			limits := map[string]any{"primary": primary, "secondary": secondary, "rateLimitReachedType": nil}
			if limited {
				primary["usedPercent"] = 100
				limits["rateLimitReachedType"] = "rate_limit_reached"
			}
			reply(id, map[string]any{"rateLimits": limits})
		case "thread/list":
			reply(id, map[string]any{"data": fakeCodexThreads(home, req.Params.Cwd)})
		case "thread/read":
			path := fakeCodexRollout(home, req.Params.ThreadID)
			if _, err := os.Stat(path); err != nil {
				fail(id, "thread not found: "+req.Params.ThreadID)
				continue
			}
			reply(id, map[string]any{"thread": codexThread{ID: req.Params.ThreadID, Path: path, HistoryMode: "legacy"}})
		default:
			fail(id, "unknown method "+req.Method)
		}
	}
	return 0
}

func fakeCodexThreads(home string, cwds []string) []codexThread {
	matches, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*.jsonl"))
	var newest codexThread
	for _, path := range matches {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		var meta struct {
			ID  string `json:"id"`
			Cwd string `json:"cwd"`
		}
		err = json.NewDecoder(f).Decode(&meta)
		fi, statErr := f.Stat()
		f.Close()
		if err != nil || statErr != nil || !slices.Contains(cwds, meta.Cwd) {
			continue
		}
		if updated := fi.ModTime().Unix(); newest.ID == "" || updated > newest.UpdatedAt {
			newest = codexThread{ID: meta.ID, Path: path, HistoryMode: "legacy", UpdatedAt: updated}
		}
	}
	if newest.ID == "" {
		return []codexThread{}
	}
	return []codexThread{newest}
}
