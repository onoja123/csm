package main

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

type Health string

const (
	healthHealthy      Health = "healthy"
	healthWarning      Health = "warning"
	healthRateLimited  Health = "rate_limited"
	healthAuthRequired Health = "authentication_required"
	healthUnavailable  Health = "unavailable"
	healthUnknown      Health = "unknown"
)

const (
	policyAutomatic = "automatic"
	policyManual    = "manual"
	policyDisabled  = "disabled"

	usageWarningPercent = 80.0
	failureWindow       = 24 * time.Hour
	maxRecentFailures   = 20
	usageFreshFor       = time.Hour
)

var errBadPolicy = errors.New("usage: csm auto automatic|manual|disabled|status (on and off are aliases of automatic and manual)")

var healthRank = map[Health]int{healthHealthy: 0, healthUnknown: 1, healthWarning: 2, healthRateLimited: 3, healthAuthRequired: 4, healthUnavailable: 5}

func (h Health) usable() bool {
	return h == healthHealthy || h == healthWarning || h == healthUnknown
}

// policy is the configured failover behaviour; older state files only have the on/off flag.
func (c Config) policy() string {
	switch c.FailoverPolicy {
	case policyAutomatic, policyManual, policyDisabled:
		return c.FailoverPolicy
	}

	if c.AutoFailover {
		return policyAutomatic
	}

	return policyManual
}

func (c *Config) setPolicy(policy string) error {
	switch policy {
	case "on":
		policy = policyAutomatic
	case "off":
		policy = policyManual
	case policyAutomatic, policyManual, policyDisabled:
	default:
		return errBadPolicy
	}

	c.FailoverPolicy = policy
	c.AutoFailover = policy == policyAutomatic

	return nil
}

// recordFailure keeps the latest failure and a short window of recent ones; it never stores the agent's message.
func (a *Account) recordFailure(reason provider.StopReason, now time.Time) {
	a.LastFailureAt = now
	a.LastFailure = reason
	a.RecentFailures = append(a.recentFailures(now), now)

	if len(a.RecentFailures) > maxRecentFailures {
		a.RecentFailures = a.RecentFailures[len(a.RecentFailures)-maxRecentFailures:]
	}
}

func (a *Account) recordSuccess(now time.Time) {
	a.LastSuccessAt = now
}

func (a *Account) recentFailures(now time.Time) []time.Time {
	var kept []time.Time

	for _, t := range a.RecentFailures {
		if now.Sub(t) < failureWindow {
			kept = append(kept, t)
		}
	}

	return kept
}

// health is the provider-neutral view of an account; usage only counts while it is fresh.
func (a *Account) health(now time.Time, usage UsageRecord, hasUsage bool) Health {
	switch {
	case !a.Enabled:
		return healthUnavailable
	case a.VerifiedAt.IsZero():
		return healthAuthRequired
	case a.LastFailure == provider.StopReasonAuthentication && a.LastFailureAt.After(a.VerifiedAt):
		return healthAuthRequired
	case now.Before(a.CooldownUntil):
		return healthRateLimited
	}

	fresh := hasUsage && now.Sub(usage.UpdatedAt) < usageFreshFor

	if fresh && usage.Usage.Limited {
		return healthRateLimited
	}

	if (a.LastFailure == provider.StopReasonNetwork || a.LastFailure == provider.StopReasonUnknown) && a.LastFailureAt.After(a.LastSuccessAt) && a.LastFailureAt.After(a.LastUsedAt) {
		return healthUnknown
	}

	if hasUsage && usageWarning(usage.Usage, now) {
		return healthWarning
	}

	return healthHealthy
}

func usageWarning(u provider.Usage, now time.Time) bool {
	for _, w := range []provider.UsageWindow{u.FiveHour, u.SevenDay, u.SpendLimit} {
		if w.ResetsAt.After(now) && w.UsedPercent >= usageWarningPercent {
			return true
		}
	}

	return false
}

func (s *State) accountHealth(a *Account, now time.Time) Health {
	rec, ok := s.loadUsage(a.Name)

	return a.health(now, rec, ok)
}

// usageLabel never invents a figure: it shows saved usage while a window is open, otherwise "unknown".
func (s *State) usageLabel(name string, now time.Time) string {
	rec, ok := s.loadUsage(name)

	if !ok {
		return "unknown"
	}

	if summary := shortUsage(rec.Usage, now); summary != "" {
		return summary
	}

	return "unknown"
}

func (s *State) priority(a *Account) int {
	return slices.Index(s.accountNames(a.providerID()), a.Name) + 1
}

// setPriority moves an account to the given position among its provider's accounts; other providers keep their order.
func (s *State) setPriority(name string, position int) error {
	a, err := s.resolveAccount(name)
	if err != nil {
		return err
	}

	group := s.accountNames(a.providerID())

	if position < 1 || position > len(group) {
		return fmt.Errorf("priority for %s must be between 1 and %d", name, len(group))
	}

	group = slices.DeleteFunc(group, func(n string) bool { return n == name })
	group = slices.Insert(group, position-1, name)
	next := 0
	order := make([]string, 0, len(s.Config.AccountOrder))

	for _, n := range s.Config.AccountOrder {
		other, err := s.resolveAccount(n)

		if err == nil && other.providerID() == a.providerID() {
			order = append(order, group[next])
			next++

			continue
		}

		order = append(order, n)
	}

	s.Config.AccountOrder = order

	return nil
}

type candidate struct {
	account  *Account
	health   Health
	failures int
	priority int
}

// rankAccounts orders a provider's usable accounts for a switch: best health first, then fewer failures in the last day, then the user's priority.
func (s *State) rankAccounts(providerID string, skip map[string]bool, now time.Time) []candidate {
	var ranked []candidate

	for i, name := range s.accountNames(providerID) {
		a, err := s.resolveAccount(name)
		if err != nil || skip[name] {
			continue
		}

		h := s.accountHealth(a, now)

		if !h.usable() || a.status(now) != statusReady {
			continue
		}

		ranked = append(ranked, candidate{account: a, health: h, failures: len(a.recentFailures(now)), priority: i + 1})
	}

	slices.SortStableFunc(ranked, func(x, y candidate) int {
		if d := healthRank[x.health] - healthRank[y.health]; d != 0 {
			return d
		}

		if d := x.failures - y.failures; d != 0 {
			return d
		}

		return x.priority - y.priority
	})

	return ranked
}

func (s *State) bestAccount(providerID, current string, skip map[string]bool, now time.Time) (*Account, error) {
	for _, c := range s.rankAccounts(providerID, skip, now) {
		if c.account.Name != current {
			return c.account, nil
		}
	}

	return nil, errNoAccountAvailable
}

func (s *State) recordAccountFailure(name string, reason provider.StopReason, now time.Time) error {
	return s.update(func(state *State) error {
		a, err := state.resolveAccount(name)
		if err != nil {
			return err
		}

		a.recordFailure(reason, now)

		return nil
	})
}

func (s *State) recordAccountSuccess(name string, now time.Time) error {
	return s.update(func(state *State) error {
		a, err := state.resolveAccount(name)
		if err != nil {
			return err
		}

		a.recordSuccess(now)

		return nil
	})
}

func ago(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}

	d := now.Sub(t).Round(time.Second)

	if d < 0 {
		return "in " + (-d).String()
	}

	return d.String() + " ago"
}

func cmdAccountHealth(log *Logger, s *State, name string) error {
	a, err := s.resolveAccount(name)
	if err != nil {
		return err
	}

	now := time.Now()
	rec, hasUsage := s.loadUsage(a.Name)
	h := a.health(now, rec, hasUsage)
	active := ""

	if s.active(a.providerID()) == a.Name {
		active = " (active)"
	}

	log.Info("%s%s", a.Name, active)
	log.Blank()
	log.Row("  Provider", providerName(a.providerID()))
	log.Row("  Health", string(h))
	log.Row("  Status", string(a.status(now)))
	log.Row("  Priority", strconv.Itoa(s.priority(a)))
	log.Row("  Usage", s.usageLabel(a.Name, now))

	if hasUsage {
		log.Row("  Usage checked", ago(rec.UpdatedAt, now))
	}

	log.Row("  Last used", ago(a.LastUsedAt, now))
	log.Row("  Last success", ago(a.LastSuccessAt, now))

	if a.LastFailureAt.IsZero() {
		log.Row("  Last failure", "never")
	} else {
		log.Row("  Last failure", fmt.Sprintf("%s (%s)", ago(a.LastFailureAt, now), a.LastFailure.Describe()))
	}

	log.Row("  Failures (24h)", strconv.Itoa(len(a.recentFailures(now))))

	if now.Before(a.CooldownUntil) {
		log.Row("  Cooldown until", formatReset(a.CooldownUntil, now))
	}

	log.Row("  Profile", displayPath(a.ConfigDir))

	if advice := healthAdvice(h, a); advice != "" {
		log.Blank()
		log.Info("%s", advice)
	}

	return nil
}

func healthAdvice(h Health, a *Account) string {
	switch h {
	case healthAuthRequired:
		return "Run:\n\n    csm account login " + a.Name
	case healthUnavailable:
		return "Run:\n\n    csm account enable " + a.Name
	case healthRateLimited:
		return "The account is in cooldown; failover skips it until the cooldown ends."
	case healthWarning:
		return "Usage is above " + strconv.Itoa(int(usageWarningPercent)) + "%; failover prefers other accounts while it stays there."
	case healthUnknown:
		return "The last run ended with an error that was not a usage limit; the account is tried after healthy ones."
	}

	return ""
}

func describePolicy(policy string) string {
	switch policy {
	case policyAutomatic:
		return "automatic (switch on a usage limit)"
	case policyDisabled:
		return "disabled (report the limit and stop)"
	default:
		return "manual (ask before switching)"
	}
}

func joinNames(cands []candidate) string {
	names := make([]string, 0, len(cands))

	for _, c := range cands {
		names = append(names, c.account.Name)
	}

	return strings.Join(names, " → ")
}
