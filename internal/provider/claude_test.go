package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFailureClassification(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    StopReason
	}{
		{"usage limit with reset time", `{"error":"rate_limit","last_assistant_message":"You've hit your limit · resets 5pm (Europe/London)"}`, StopReasonUsageLimit},
		{"usage limit reached", `{"error":"rate_limit","last_assistant_message":"Claude usage limit reached. Your limit will reset at 5pm."}`, StopReasonUsageLimit},
		{"five hour limit", `{"error":"rate_limit","last_assistant_message":"5-hour limit reached ∙ resets 3pm"}`, StopReasonUsageLimit},
		{"bare 429 is not a usage limit", `{"error":"rate_limit","last_assistant_message":"API Error: Rate limit reached","error_details":"429 Too Many Requests"}`, StopReasonRateLimited},
		{"usage wording without rate_limit type", `{"error":"unknown","last_assistant_message":"usage limit reached"}`, StopReasonUnknown},
		{"overloaded", `{"error":"overloaded","last_assistant_message":"API Error: Overloaded"}`, StopReasonNetwork},
		{"server error", `{"error":"server_error"}`, StopReasonNetwork},
		{"auth failed", `{"error":"authentication_failed"}`, StopReasonAuthentication},
		{"account on hold", `{"error":"account_on_hold"}`, StopReasonAuthentication},
		{"billing is not failover", `{"error":"billing_error","last_assistant_message":"credit balance too low; usage limit"}`, StopReasonUnknown},
		{"empty", `{}`, StopReasonUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Claude{}.ParseFailure([]byte(tt.payload))
			if err != nil {
				t.Fatal(err)
			}
			if f.Reason != tt.want {
				t.Fatalf("got %s, want %s", f.Reason, tt.want)
			}
		})
	}
}

func TestParseFailureKeepsSessionAndMessage(t *testing.T) {
	f, err := Claude{}.ParseFailure([]byte(`{"session_id":"s1","error":"rate_limit","last_assistant_message":"limit · resets 5pm","extra":{"x":1}}`))
	if err != nil || f.SessionID != "s1" || f.Message != "limit · resets 5pm" {
		t.Fatalf("got %+v %v", f, err)
	}
	if _, err := (Claude{}).ParseFailure([]byte("{")); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestSettingsArgs(t *testing.T) {
	args := Claude{}.SettingsArgs("/Applications/it's here/csm")
	if len(args) != 2 || args[0] != "--settings" {
		t.Fatalf("args = %v", args)
	}
	var s Settings
	if err := json.Unmarshal([]byte(args[1]), &s); err != nil {
		t.Fatal(err)
	}
	if cmd := s.Hooks["StopFailure"][0].Hooks[0].Command; cmd != `'/Applications/it'\''s here/csm' hook failure` {
		t.Fatalf("command = %s", cmd)
	}
	if len(s.Hooks["SessionStart"]) != 1 {
		t.Fatal("SessionStart hook missing")
	}
	if s.StatusLine.Command != `'/Applications/it'\''s here/csm' hook status-line` {
		t.Fatalf("status line = %+v", s.StatusLine)
	}
}

func TestCarrySession(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	project := filepath.Join(from, "projects", "-Users-x-polishpad")
	os.MkdirAll(filepath.Join(project, "sid", "subagents"), 0o700)
	os.WriteFile(filepath.Join(project, "sid.jsonl"), []byte("line\n"), 0o600)
	os.WriteFile(filepath.Join(project, "sid", "subagents", "a.jsonl"), []byte("sub\n"), 0o600)

	carried, err := Claude{}.CarrySession(from, to, "sid")
	if err != nil || !carried {
		t.Fatalf("carried=%v err=%v", carried, err)
	}
	for _, rel := range []string{"sid.jsonl", "sid/subagents/a.jsonl"} {
		if _, err := os.Stat(filepath.Join(to, "projects", "-Users-x-polishpad", rel)); err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
	}
	if carried, _ := (Claude{}).CarrySession(from, to, "missing"); carried {
		t.Fatal("carried a transcript that does not exist")
	}
}

func TestHasSessionFlag(t *testing.T) {
	for _, args := range [][]string{{"-c"}, {"--resume", "x"}, {"--resume=x"}, {"--model", "opus", "-r"}} {
		if !(Claude{}).HasSessionFlag(args) {
			t.Errorf("missed session flag in %v", args)
		}
	}
	if (Claude{}).HasSessionFlag([]string{"--model", "opus"}) {
		t.Error("false positive")
	}
}

func TestNewSessionArgsRespectsFeatures(t *testing.T) {
	all := Features{SessionID: true, SystemPrompt: true}
	if got := (Claude{}).NewSessionArgs(all, "id", "note"); len(got) != 4 {
		t.Fatalf("got %v", got)
	}
	if got := (Claude{}).NewSessionArgs(Features{}, "id", "note"); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestParseUsage(t *testing.T) {
	u, err := Claude{}.ParseUsage([]byte(`{"model":{"id":"x"},"rate_limits":{"five_hour":{"used_percentage":23.5,"resets_at":1738425600},"seven_day":{"used_percentage":41.2,"resets_at":1738857600}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if u.FiveHour.UsedPercent != 23.5 || u.FiveHour.ResetsAt.Unix() != 1738425600 || u.SevenDay.UsedPercent != 41.2 {
		t.Fatalf("got %+v", u)
	}
	if !u.SpendLimit.ResetsAt.IsZero() || u.Empty() {
		t.Fatalf("absent window handled wrong: %+v", u)
	}
	u, err = Claude{}.ParseUsage([]byte(`{"model":{"id":"x"}}`))
	if err != nil || !u.Empty() {
		t.Fatalf("no rate_limits should be empty: %+v %v", u, err)
	}
}

func TestUserStatusLine(t *testing.T) {
	project, profile := t.TempDir(), t.TempDir()
	if got := (Claude{}).UserStatusLine(project, profile); got != "" {
		t.Fatalf("got %q with no settings", got)
	}
	os.WriteFile(filepath.Join(profile, "settings.json"), []byte(`{"statusLine":{"type":"command","command":"user-line"}}`), 0o600)
	if got := (Claude{}).UserStatusLine(project, profile); got != "user-line" {
		t.Fatalf("got %q", got)
	}
	os.MkdirAll(filepath.Join(project, ".claude"), 0o700)
	os.WriteFile(filepath.Join(project, ".claude", "settings.json"), []byte(`{"statusLine":{"type":"command","command":"project-line"}}`), 0o600)
	if got := (Claude{}).UserStatusLine(project, profile); got != "project-line" {
		t.Fatalf("project settings should win, got %q", got)
	}
}

// Recorded from `claude -p --output-format stream-json --verbose` on Claude Code 2.1.282, ids removed.
const realRateLimitEvent = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1790309400,"rateLimitType":"five_hour","overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0,"resetsAt":1790309400},"seven_day":{"utilization":0.13,"resetsAt":1790467200}}}}`

func TestParseUsageStream(t *testing.T) {
	stream := `{"type":"system","subtype":"init"}` + "\n" + realRateLimitEvent + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"ok"}` + "\n"
	u, err := parseUsageStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if u.FiveHour.UsedPercent != 0 || u.FiveHour.ResetsAt.Unix() != 1790309400 {
		t.Fatalf("five hour = %+v", u.FiveHour)
	}
	if u.SevenDay.UsedPercent != 13 || u.SevenDay.ResetsAt.Unix() != 1790467200 || u.Limited {
		t.Fatalf("got %+v", u)
	}

	u, err = parseUsageStream(strings.NewReader(strings.Replace(realRateLimitEvent, `"status":"allowed"`, `"status":"rejected"`, 1)))
	if err != nil || !u.Limited {
		t.Fatalf("rejected status not reported: %+v %v", u, err)
	}

	_, err = parseUsageStream(strings.NewReader(`{"type":"result","is_error":true,"result":"Not logged in · Please run /login"}`))
	if err == nil || !strings.Contains(err.Error(), "Not logged in") {
		t.Fatalf("got %v", err)
	}
	if _, err := parseUsageStream(strings.NewReader("")); err == nil {
		t.Fatal("expected error with no events")
	}
}
