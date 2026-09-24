package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// CSM_FAKE_LIMIT profiles report a usage limit; CSM_FAKE_HOLD profiles run until stopped.
func RunFakeClaude(args []string) int {
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	authFile := filepath.Join(configDir, "fake-auth")
	profile := filepath.Base(configDir)

	switch {
	case len(args) > 0 && args[0] == "--version":
		fmt.Println("0.0.0-fake (Claude Code)")
		return 0
	case len(args) > 0 && args[0] == "--help":
		fmt.Println("Usage: claude [options] [command] [prompt]\n\nOptions:\n  -r, --resume [value]\n  --session-id <uuid>\n  --settings <file-or-json>\n  --append-system-prompt <prompt>\n  --tools <tools...>\n  --output-format <format>  (choices: \"text\", \"json\", \"stream-json\")\n\nCommands:\n  auth                                  Manage authentication")
		return 0
	case len(args) >= 2 && args[0] == "auth" && args[1] == "login":
		os.WriteFile(authFile, []byte(profile+"@example.test"), 0o600)
		return 0
	case len(args) >= 2 && args[0] == "auth" && args[1] == "status":
		email, err := os.ReadFile(authFile)
		st := claudeAuthStatus{LoggedIn: err == nil, Email: string(email), ConfigDirectory: configDir, AuthMethod: "claude.ai"}
		data, _ := json.Marshal(st)
		fmt.Println(string(data))
		if !st.LoggedIn {
			return 1
		}
		return 0
	}

	if slices.Contains(args, "-p") {
		return fakePrint(profile)
	}

	var settings Settings
	sessionID, resume := "", false
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--settings":
			json.Unmarshal([]byte(args[i+1]), &settings)
		case "--session-id":
			sessionID = args[i+1]
		case "--resume":
			sessionID, resume = args[i+1], true
		}
	}
	if sessionID == "" {
		sessionID = NewSessionID()
	}

	cwd, _ := os.Getwd()
	transcript := filepath.Join(configDir, "projects", strings.ReplaceAll(cwd, "/", "-"), sessionID+".jsonl")
	if resume {
		if _, err := os.Stat(transcript); err != nil {
			fmt.Fprintf(os.Stderr, "No conversation found with session ID: %s\n", sessionID)
			return 1
		}
	}
	os.MkdirAll(filepath.Dir(transcript), 0o700)
	f, err := os.OpenFile(transcript, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 1
	}
	fmt.Fprintf(f, "{\"profile\":%q,\"resumed\":%t}\n", profile, resume)
	f.Close()

	runFakeHooks(settings, "SessionStart", map[string]string{"session_id": sessionID, "hook_event_name": "SessionStart"})
	runFakeStatusLine(settings, profile)

	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	if slices.Contains(strings.Split(os.Getenv("CSM_FAKE_HOLD"), ","), profile) {
		select {
		case <-term:
			return 143
		case <-time.After(30 * time.Second):
			return 0
		}
	}
	if !slices.Contains(strings.Split(os.Getenv("CSM_FAKE_LIMIT"), ","), profile) {
		return 0
	}

	failure := map[string]string{
		"session_id":             sessionID,
		"hook_event_name":        "StopFailure",
		"error":                  "rate_limit",
		"last_assistant_message": "You've hit your usage limit · resets 5pm",
	}
	if os.Getenv("CSM_FAKE_ERROR") == "overloaded" {
		failure["error"] = "overloaded"
		failure["last_assistant_message"] = "API Error: Overloaded"
	}
	runFakeHooks(settings, "StopFailure", failure)
	select {
	case <-term:
		return 143
	case <-time.After(2 * time.Second):
		return 0
	}
}

func runFakeHooks(settings Settings, event string, payload map[string]string) {
	data, _ := json.Marshal(payload)
	for _, group := range settings.Hooks[event] {
		for _, h := range group.Hooks {
			cmd := exec.Command("/bin/sh", "-c", h.Command)
			cmd.Stdin = bytes.NewReader(data)
			cmd.Run()
		}
	}
}

// The fake reports 100% of the 5-hour window for profiles in CSM_FAKE_LIMIT and 42% otherwise.
func runFakeStatusLine(settings Settings, profile string) {
	if settings.StatusLine.Command == "" {
		return
	}
	var in claudeStatusLineInput
	in.RateLimits.FiveHour = claudeRateWindow{UsedPercentage: 42, ResetsAt: time.Now().Add(2 * time.Hour).Unix()}
	in.RateLimits.SevenDay = claudeRateWindow{UsedPercentage: 17, ResetsAt: time.Now().Add(72 * time.Hour).Unix()}
	if slices.Contains(strings.Split(os.Getenv("CSM_FAKE_LIMIT"), ","), profile) {
		in.RateLimits.FiveHour.UsedPercentage = 100
	}
	data, _ := json.Marshal(in)
	cmd := exec.Command("/bin/sh", "-c", settings.StatusLine.Command)
	cmd.Stdin = bytes.NewReader(data)
	cmd.Run()
}

// fakePrint emits the stream-json rate_limit_event a real `claude -p` produces; logged-out profiles fail.
func fakePrint(profile string) int {
	if _, err := os.Stat(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "fake-auth")); err != nil {
		fmt.Println(`{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login"}`)
		return 1
	}
	var ev claudeStreamEvent
	ev.Type = "rate_limit_event"
	ev.RateLimitInfo.Status = "allowed"
	ev.RateLimitInfo.UnifiedWindows.FiveHour = claudeUnifiedWindow{Utilization: 0.42, ResetsAt: time.Now().Add(2 * time.Hour).Unix()}
	ev.RateLimitInfo.UnifiedWindows.SevenDay = claudeUnifiedWindow{Utilization: 0.17, ResetsAt: time.Now().Add(72 * time.Hour).Unix()}
	if slices.Contains(strings.Split(os.Getenv("CSM_FAKE_LIMIT"), ","), profile) {
		ev.RateLimitInfo.Status = "rejected"
		ev.RateLimitInfo.UnifiedWindows.FiveHour.Utilization = 1
	}
	data, _ := json.Marshal(ev)
	fmt.Println(`{"type":"system","subtype":"init"}`)
	fmt.Println(string(data))
	fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`)
	return 0
}
