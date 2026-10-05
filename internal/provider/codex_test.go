package provider

import (
	"encoding/json"
	"slices"
	"testing"
)

func codexUsage(t *testing.T, payload string) Usage {
	t.Helper()
	var limits codexRateLimits

	if err := json.Unmarshal([]byte(payload), &limits); err != nil {
		t.Fatal(err)
	}

	return limits.usage()
}

func TestCodexUsage(t *testing.T) {
	u := codexUsage(t, `{"rateLimits":{"limitId":"codex","primary":{"usedPercent":23,"resetsAt":1790309400,"windowDurationMins":300},"secondary":{"usedPercent":41,"resetsAt":1790467200,"windowDurationMins":10080},"credits":null,"rateLimitReachedType":null},"rateLimitsByLimitId":null}`)
	if u.FiveHour.UsedPercent != 23 || u.FiveHour.ResetsAt.Unix() != 1790309400 || u.SevenDay.UsedPercent != 41 || u.SevenDay.ResetsAt.Unix() != 1790467200 {
		t.Fatalf("got %+v", u)
	}

	if u.Limited || !u.SpendLimit.ResetsAt.IsZero() {
		t.Fatalf("got %+v", u)
	}

	u = codexUsage(t, `{"rateLimits":{"primary":{"usedPercent":60,"resetsAt":1790467200,"windowDurationMins":10080},"secondary":null}}`)
	if u.SevenDay.UsedPercent != 60 || !u.FiveHour.ResetsAt.IsZero() {
		t.Fatalf("a weekly primary window was not reported as 7-day: %+v", u)
	}

	u = codexUsage(t, `{"rateLimits":{"primary":{"usedPercent":10,"resetsAt":1790309400},"secondary":{"usedPercent":20,"resetsAt":1790467200},"individualLimit":{"limit":"100","used":"25","remainingPercent":75,"resetsAt":1790467200}}}`)
	if u.FiveHour.UsedPercent != 10 || u.SevenDay.UsedPercent != 20 || u.SpendLimit.UsedPercent != 25 {
		t.Fatalf("got %+v", u)
	}

	if u := codexUsage(t, `{"rateLimits":{"primary":{"usedPercent":100,"resetsAt":null,"windowDurationMins":300}}}`); !u.Empty() || u.Limited {
		t.Fatalf("a window without a reset time should be dropped: %+v", u)
	}
}

func TestCodexLimited(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{"limit reached", `{"rateLimits":{"rateLimitReachedType":"rate_limit_reached"}}`, true},
		{"workspace limit reached", `{"rateLimits":{"rateLimitReachedType":"workspace_member_usage_limit_reached","credits":{"hasCredits":false,"unlimited":false}}}`, true},
		{"full window without a reached type", `{"rateLimits":{"primary":{"usedPercent":100,"resetsAt":1790309400,"windowDurationMins":300}}}`, false},
		{"limit reached but running on credits", `{"rateLimits":{"rateLimitReachedType":"rate_limit_reached","credits":{"hasCredits":true,"unlimited":false}}}`, false},
		{"nothing reported", `{"rateLimits":{}}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexUsage(t, tt.payload).Limited; got != tt.want {
				t.Fatalf("limited = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCodexArgs(t *testing.T) {
	c := Codex{}

	if got := c.NewSessionArgs(Features{SessionID: true, SystemPrompt: true}, "sid", "note"); len(got) != 0 {
		t.Fatalf("Codex cannot take a session ID or a handoff note, got %v", got)
	}

	if !c.HasSessionFlag([]string{"resume", "--last"}) || c.HasSessionFlag([]string{"--model", "gpt"}) {
		t.Fatal("session flag detection is wrong")
	}

	t.Setenv("CODEX_HOME", "/somewhere/else")
	env := c.Env("/profiles/work", "CSM_ACCOUNT=work")
	if !slices.Contains(env, "CODEX_HOME=/profiles/work") || slices.Contains(env, "CODEX_HOME=/somewhere/else") || !slices.Contains(env, "CSM_ACCOUNT=work") {
		t.Fatal("profile directory not applied to the environment")
	}

	t.Setenv("CODEX_ACCESS_TOKEN", "x")

	if err := c.CheckEnv(); err == nil {
		t.Fatal("an access token in the environment was accepted")
	}
}
