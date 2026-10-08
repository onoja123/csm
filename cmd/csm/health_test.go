package main

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

func usageAt(percent float64, updated time.Time, limited bool) UsageRecord {
	return UsageRecord{Version: stateVersion, UpdatedAt: updated, Usage: provider.Usage{
		FiveHour: provider.UsageWindow{UsedPercent: percent, ResetsAt: updated.Add(3 * time.Hour)},
		Limited:  limited,
	}}
}

func TestAccountHealthStates(t *testing.T) {
	ready := Account{Name: "a", Enabled: true, VerifiedAt: testNow.Add(-time.Hour)}

	tests := []struct {
		name  string
		setup func(a *Account)
		usage UsageRecord
		has   bool
		want  Health
	}{
		{name: "healthy without usage", want: healthHealthy},
		{name: "healthy with low usage", usage: usageAt(18, testNow, false), has: true, want: healthHealthy},
		{name: "warning near the limit", usage: usageAt(85, testNow, false), has: true, want: healthWarning},
		{name: "stale usage does not warn as limited", usage: usageAt(100, testNow.Add(-2*time.Hour), true), has: true, want: healthWarning},
		{name: "fresh limited usage", usage: usageAt(100, testNow.Add(-time.Minute), true), has: true, want: healthRateLimited},
		{name: "cooldown", setup: func(a *Account) { a.CooldownUntil = testNow.Add(time.Hour) }, want: healthRateLimited},
		{name: "disabled wins", setup: func(a *Account) { a.Enabled = false; a.CooldownUntil = testNow.Add(time.Hour) }, want: healthUnavailable},
		{name: "never logged in", setup: func(a *Account) { a.VerifiedAt = time.Time{} }, want: healthAuthRequired},
		{name: "auth failure after login", setup: func(a *Account) { a.recordFailure(provider.StopReasonAuthentication, testNow) }, want: healthAuthRequired},
		{name: "auth failure before a new login", setup: func(a *Account) {
			a.recordFailure(provider.StopReasonAuthentication, testNow.Add(-2*time.Hour))
		}, want: healthHealthy},
		{name: "unexplained failure is unknown", setup: func(a *Account) { a.recordFailure(provider.StopReasonNetwork, testNow) }, want: healthUnknown},
		{name: "success after failure is healthy", setup: func(a *Account) {
			a.recordFailure(provider.StopReasonNetwork, testNow.Add(-time.Minute))
			a.recordSuccess(testNow)
		}, want: healthHealthy},
		{name: "usage limit failure without cooldown is not unknown", setup: func(a *Account) { a.recordFailure(provider.StopReasonUsageLimit, testNow) }, want: healthHealthy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := ready

			if tt.setup != nil {
				tt.setup(&a)
			}

			if got := a.health(testNow, tt.usage, tt.has); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestRecentFailuresWindow(t *testing.T) {
	a := Account{Enabled: true, VerifiedAt: testNow.Add(-48 * time.Hour)}
	a.recordFailure(provider.StopReasonNetwork, testNow.Add(-30*time.Hour))
	a.recordFailure(provider.StopReasonNetwork, testNow.Add(-2*time.Hour))
	a.recordFailure(provider.StopReasonUsageLimit, testNow)

	if got := a.recentFailures(testNow); len(got) != 2 {
		t.Fatalf("failures in the last day = %d, want 2", len(got))
	}

	if a.LastFailure != provider.StopReasonUsageLimit || !a.LastFailureAt.Equal(testNow) {
		t.Fatalf("last failure = %s at %s", a.LastFailure, a.LastFailureAt)
	}

	for range maxRecentFailures * 2 {
		a.recordFailure(provider.StopReasonNetwork, testNow)
	}

	if len(a.RecentFailures) != maxRecentFailures {
		t.Fatalf("history grew to %d", len(a.RecentFailures))
	}
}

func TestPolicyBackwardCompatible(t *testing.T) {
	var c Config

	if c.policy() != policyManual {
		t.Fatalf("an old state without the flag should ask before switching, got %s", c.policy())
	}

	c.AutoFailover = true

	if c.policy() != policyAutomatic {
		t.Fatalf("auto_failover=true must read as automatic, got %s", c.policy())
	}

	c.FailoverPolicy = "nonsense"

	if c.policy() != policyAutomatic {
		t.Fatal("a corrupted policy value must fall back to the flag")
	}

	for in, want := range map[string]string{"on": policyAutomatic, "off": policyManual, "disabled": policyDisabled, "manual": policyManual, "automatic": policyAutomatic} {
		if err := c.setPolicy(in); err != nil || c.policy() != want || c.AutoFailover != (want == policyAutomatic) {
			t.Fatalf("setPolicy(%q) = %v, policy %s, flag %v", in, err, c.policy(), c.AutoFailover)
		}
	}

	if err := c.setPolicy("sometimes"); !errors.Is(err, errBadPolicy) {
		t.Fatalf("got %v", err)
	}
}

func TestRankAccountsIsDeterministic(t *testing.T) {
	s := newTestState(t, "work", "personal", "backup", "spare")
	personal, _ := s.resolveAccount("personal")
	personal.recordFailure(provider.StopReasonNetwork, testNow.Add(-time.Minute))
	backup, _ := s.resolveAccount("backup")
	backup.CooldownUntil = testNow.Add(time.Hour)

	if err := s.saveUsage(usageAt(90, testNow, false).withAccount("work")); err != nil {
		t.Fatal(err)
	}

	ranked := s.rankAccounts(provider.ClaudeID, nil, testNow)
	names := joinNames(ranked)

	if names != "spare → personal → work" {
		t.Fatalf("ranked = %s (healthy spare, then unknown personal, then work at 90%%; backup is in cooldown)", names)
	}

	best, err := s.bestAccount(provider.ClaudeID, "spare", nil, testNow)
	if err != nil || best.Name != "personal" {
		t.Fatalf("best after spare = %v, %v", best, err)
	}

	if _, err := s.bestAccount(provider.ClaudeID, "spare", map[string]bool{"personal": true, "work": true}, testNow); !errors.Is(err, errNoAccountAvailable) {
		t.Fatalf("got %v, want errNoAccountAvailable", err)
	}

	work, _ := s.resolveAccount("work")
	work.recordFailure(provider.StopReasonUsageLimit, testNow)
	work.recordFailure(provider.StopReasonUsageLimit, testNow)
	spare, _ := s.resolveAccount("spare")
	spare.recordFailure(provider.StopReasonUsageLimit, testNow)
	os.Remove(s.usagePath("work"))

	if got := joinNames(s.rankAccounts(provider.ClaudeID, nil, testNow)); got != "spare → work → personal" {
		t.Fatalf("among healthy accounts fewer recent failures win before priority, and an unexplained failure sorts last; got %s", got)
	}
}

func (r UsageRecord) withAccount(name string) UsageRecord {
	r.Account = name

	return r
}

func TestRankAccountsPrefersPriorityWhenEqual(t *testing.T) {
	s := newTestState(t, "work", "personal")

	if got := joinNames(s.rankAccounts(provider.ClaudeID, nil, testNow)); got != "work → personal" {
		t.Fatalf("got %s", got)
	}

	if err := s.setPriority("personal", 1); err != nil {
		t.Fatal(err)
	}

	if got := joinNames(s.rankAccounts(provider.ClaudeID, nil, testNow)); got != "personal → work" {
		t.Fatalf("after reprioritising got %s", got)
	}
}

func TestSetPriorityKeepsOtherProvidersInPlace(t *testing.T) {
	s := newTestState(t, "work", "personal")

	for _, name := range []string{"cx-a", "cx-b"} {
		a, err := s.addAccount(provider.CodexID, name, testNow)
		if err != nil {
			t.Fatal(err)
		}

		a.VerifiedAt = testNow
	}

	if err := s.setPriority("cx-b", 1); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(s.Config.AccountOrder, []string{"work", "personal", "cx-b", "cx-a"}) {
		t.Fatalf("order = %v", s.Config.AccountOrder)
	}

	if err := s.setPriority("personal", 1); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(s.Config.AccountOrder, []string{"personal", "work", "cx-b", "cx-a"}) {
		t.Fatalf("order = %v", s.Config.AccountOrder)
	}

	if s.priority(mustAccount(t, s, "work")) != 2 || s.priority(mustAccount(t, s, "cx-a")) != 2 {
		t.Fatal("priority must count within the provider")
	}

	if err := s.setPriority("work", 3); err == nil {
		t.Fatal("a position past the provider's accounts was accepted")
	}

	if err := s.setPriority("nobody", 1); err == nil {
		t.Fatal("an unknown account was accepted")
	}
}

func mustAccount(t *testing.T, s *State, name string) *Account {
	t.Helper()
	a, err := s.resolveAccount(name)
	if err != nil {
		t.Fatal(err)
	}

	return a
}

func TestUsageLabelNeverInventsFigures(t *testing.T) {
	s := newTestState(t, "work")

	if got := s.usageLabel("work", testNow); got != "unknown" {
		t.Fatalf("got %q", got)
	}

	if err := s.saveUsage(usageAt(42, testNow, false).withAccount("work")); err != nil {
		t.Fatal(err)
	}

	if got := s.usageLabel("work", testNow); got != "5h 42%" {
		t.Fatalf("got %q", got)
	}

	if got := s.usageLabel("work", testNow.Add(4*time.Hour)); got != "unknown" {
		t.Fatalf("a window that has reset is not current usage, got %q", got)
	}
}

func TestAccountCommandsShowHealthAndPriority(t *testing.T) {
	s := newTestState(t, "work", "personal")
	personal := mustAccount(t, s, "personal")
	personal.recordFailure(provider.StopReasonAuthentication, testNow.Add(time.Minute))

	if err := s.save(); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	log := &Logger{out: &out, err: &out}

	if err := cmdAccounts(log, s.Home); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"● work", "healthy", "unknown", "priority 1", "○ personal", "authentication_required", "priority 2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("accounts missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()

	if err := cmdAccount(log, s.Home, []string{"health", "personal"}); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"Health", "authentication_required", "Last failure", "authentication required", "Failures (24h)", "csm account login personal"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("health missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()

	if err := cmdAccount(log, s.Home, []string{"priority", "personal", "1"}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "personal is now priority 1") || !strings.Contains(out.String(), "personal → work") {
		t.Fatalf("priority output:\n%s", out.String())
	}

	if err := cmdAccount(log, s.Home, []string{"priority", "personal", "x"}); err == nil {
		t.Fatal("a non-numeric priority was accepted")
	}

	out.Reset()

	if err := cmdAuto(log, s.Home, []string{"disabled"}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "Failover: disabled") || !strings.Contains(out.String(), "Would switch to (Claude Code): work") || strings.Contains(out.String(), "Would switch to (Claude Code): personal") {
		t.Fatalf("auto output:\n%s", out.String())
	}

	if err := cmdAuto(log, s.Home, []string{"sometimes"}); !errors.Is(err, errBadPolicy) {
		t.Fatalf("got %v", err)
	}

	reloaded, err := loadState(s.Home)
	if err != nil || reloaded.Config.policy() != policyDisabled || reloaded.Config.AutoFailover {
		t.Fatalf("policy not saved: %+v %v", reloaded.Config, err)
	}
}

func TestStatusShowsPolicyAndHealth(t *testing.T) {
	s := newTestState(t, "work")
	s.Config.setPolicy(policyAutomatic)

	if err := s.save(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CSM_HOME", s.Home)
	var out strings.Builder

	if err := cmdStatus(&Logger{out: &out, err: &out}, s.Home); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"Failover", "automatic (switch on a usage limit)", "● work", "healthy", "unknown", "priority 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status missing %q:\n%s", want, out.String())
		}
	}
}
