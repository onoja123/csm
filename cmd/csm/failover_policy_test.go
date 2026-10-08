package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

func TestDisabledFailoverReportsAndStops(t *testing.T) {
	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha", policy: policyDisabled})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}

	if res.active != "alpha" || strings.Contains(res.output, "became unavailable") {
		t.Fatalf("a disabled policy switched accounts:\n%s", res.output)
	}

	for _, want := range []string{"reported a usage limit on alpha", "Failover is disabled (enable with: csm auto automatic)"} {
		if !strings.Contains(res.output, want) {
			t.Errorf("missing %q:\n%s", want, res.output)
		}
	}

	alpha := res.accounts["alpha"]

	if alpha.LastFailure != provider.StopReasonUsageLimit || alpha.LastFailureAt.IsZero() || len(alpha.RecentFailures) != 1 {
		t.Fatalf("the limit was not recorded against alpha: %+v", alpha)
	}

	if alpha.CooldownUntil.Before(time.Now()) {
		t.Fatal("alpha should be in cooldown even when failover is disabled")
	}
}

func TestManualFailoverWithoutTerminalDoesNotSwitch(t *testing.T) {
	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha", policy: policyManual})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}

	if res.active != "alpha" || !strings.Contains(res.output, "Automatic failover is off (enable with: csm auto automatic)") {
		t.Fatalf("manual policy output:\n%s", res.output)
	}
}

func TestFailoverRecordsHealthHistory(t *testing.T) {
	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha"})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}

	alpha, beta := res.accounts["alpha"], res.accounts["beta"]

	if alpha.LastFailure != provider.StopReasonUsageLimit || len(alpha.recentFailures(time.Now())) != 1 {
		t.Fatalf("alpha history = %+v", alpha)
	}

	if beta.LastSuccessAt.IsZero() || !beta.LastFailureAt.IsZero() {
		t.Fatalf("beta ran cleanly and should record a success only: %+v", beta)
	}

	if alpha.health(time.Now(), UsageRecord{}, false) != healthRateLimited || beta.health(time.Now(), UsageRecord{}, false) != healthHealthy {
		t.Fatalf("health after failover: alpha %s, beta %s", alpha.health(time.Now(), UsageRecord{}, false), beta.health(time.Now(), UsageRecord{}, false))
	}
}

func TestFailoverPicksTheHealthiestAccount(t *testing.T) {
	// gamma is next in order but has been failing; delta is healthy, so failover should skip gamma.
	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha", before: func(s *State, projectDir string) {
		s.update(func(state *State) error {
			for _, name := range []string{"gamma", "delta"} {
				a, err := state.addAccount(provider.ClaudeID, name, time.Now())
				if err != nil {
					return err
				}

				os.MkdirAll(a.ConfigDir, 0o700)
				os.WriteFile(filepath.Join(a.ConfigDir, "fake-auth"), []byte(name+"@example.test"), 0o600)
				a.Email, a.VerifiedAt = name+"@example.test", time.Now()
			}

			beta, _ := state.resolveAccount("beta")
			beta.Enabled = false
			gamma, _ := state.resolveAccount("gamma")
			gamma.recordFailure(provider.StopReasonNetwork, time.Now())

			return nil
		})
	}})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}

	if res.active != "delta" || !strings.Contains(res.output, "Selecting delta") {
		t.Fatalf("active = %s; a healthy account must beat one with a recent unexplained failure\n%s", res.active, res.output)
	}
}

func TestNetworkFailureIsRecordedWithoutSwitching(t *testing.T) {
	res, err := runFailoverScenario(testBinary(t), scenario{limited: "alpha", fakeError: "overloaded"})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}

	alpha := res.accounts["alpha"]

	if alpha.LastFailure != provider.StopReasonNetwork || !alpha.CooldownUntil.IsZero() {
		t.Fatalf("a network error should be recorded but never cool the account down: %+v", alpha)
	}

	if alpha.health(time.Now(), UsageRecord{}, false) != healthUnknown {
		t.Fatalf("health = %s", alpha.health(time.Now(), UsageRecord{}, false))
	}
}
