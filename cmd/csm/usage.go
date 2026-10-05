package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

const usageFetchTimeout = 90 * time.Second

type UsageRecord struct {
	Version   int            `json:"version"`
	Account   string         `json:"account"`
	UpdatedAt time.Time      `json:"updated_at"`
	Usage     provider.Usage `json:"usage"`
}

func (s *State) usagePath(account string) string {
	return filepath.Join(s.Home, "usage", account+".json")
}

func (s *State) saveUsage(r UsageRecord) error {
	if err := os.MkdirAll(filepath.Join(s.Home, "usage"), 0o700); err != nil {
		return err
	}

	return writeJSON(s.usagePath(r.Account), r)
}

func (s *State) loadUsage(account string) (UsageRecord, bool) {
	var r UsageRecord

	if err := readJSON(s.usagePath(account), &r); err != nil {
		return r, false
	}

	return r, true
}

func cmdUsage(log *Logger, home string, args []string) error {
	s, err := loadState(home)
	if err != nil {
		return err
	}

	cached := false
	var names []string

	for _, arg := range args {
		if arg == "--cached" {
			cached = true
			continue
		}

		a, err := s.resolveAccount(arg)
		if err != nil {
			return err
		}

		names = append(names, a.Name)
	}

	if len(names) == 0 {
		names = s.Config.AccountOrder
	}

	if len(names) == 0 {
		return errors.New("no accounts configured.\n\nRun:\n\n    csm account add personal")
	}

	var errs map[string]error

	if !cached {
		errs = map[string]error{}

		for _, id := range provider.IDs {
			group := s.namesOf(id, names)
			if len(group) == 0 {
				continue
			}

			p, err := usageProvider(id)
			if err != nil {
				for _, name := range group {
					errs[name] = err
				}

				continue
			}

			log.Info("Checking %d account(s) with %s...", len(group), p.Name())
			maps.Copy(errs, refreshUsage(s, p, group, time.Now()))
		}

		log.Blank()
	}

	printUsage(log, s, names, errs, time.Now())

	return nil
}

func usageProvider(id string) (provider.Provider, error) {
	p, err := provider.Find(id)
	if err != nil {
		return nil, err
	}

	if err := p.CheckEnv(); err != nil {
		return nil, err
	}

	features, err := p.Features()
	if err != nil {
		return nil, err
	}

	if !features.UsageProbe {
		return nil, fmt.Errorf("this %s version cannot report usage outside a session.\n\nFigures recorded during a session, if any, are shown by: csm usage --cached", p.Name())
	}

	return p, nil
}

func refreshUsage(s *State, p provider.Provider, names []string, now time.Time) map[string]error {
	errs := make([]error, len(names))
	var wg sync.WaitGroup

	for i, name := range names {
		a, err := s.resolveAccount(name)
		if err != nil {
			errs[i] = err
			continue
		}

		switch a.status(now) {
		case statusDisabled:
			errs[i] = errors.New("disabled; not checked")
			continue

		case statusNotAuthenticated:
			errs[i] = fmt.Errorf("not logged in; run: csm account login %s", name)
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()

			if _, err := verifyProfile(p, a); err != nil {
				errs[i] = err

				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), usageFetchTimeout)

			defer cancel()

			usage, err := p.FetchUsage(ctx, a.ConfigDir)
			if err != nil {
				errs[i] = err

				return
			}

			errs[i] = s.saveUsage(UsageRecord{Version: stateVersion, Account: name, UpdatedAt: time.Now(), Usage: usage})
		}()
	}

	wg.Wait()

	byName := map[string]error{}

	for i, name := range names {
		if errs[i] != nil {
			byName[name] = errs[i]
		}
	}

	return byName
}

func printUsage(log *Logger, s *State, names []string, errs map[string]error, now time.Time) {
	var notes []string

	printed := false

	for _, id := range provider.IDs {
		group := s.namesOf(id, names)
		if len(group) == 0 {
			continue
		}

		if printed {
			log.Blank()
		}

		printed = true
		log.Info("Usage (%s)", providerName(id))
		printProviderUsage(log, s, id, group, errs, now)

		if p, err := provider.New(id, ""); err == nil && p.UsageCheckNote() != "" {
			notes = append(notes, p.UsageCheckNote())
		}
	}

	if errs == nil {
		log.Info("\nLast saved figures only. Run `csm usage` without --cached for a live check.")

		return
	}

	log.Blank()

	for _, note := range notes {
		log.Info("%s", note)
	}
}

func printProviderUsage(log *Logger, s *State, providerID string, names []string, errs map[string]error, now time.Time) {
	for _, name := range names {
		marker := "○"

		if name == s.active(providerID) {
			marker = "●"
		}

		log.Blank()
		rec, ok := s.loadUsage(name)
		fetchErr := errs[name]

		switch {
		case (!ok || rec.Usage.Empty()) && fetchErr != nil:
			log.Info("%s %-12s could not check usage", marker, name)
			log.Info("    %s", indent(fetchErr.Error()))
			continue

		case !ok || rec.Usage.Empty():
			log.Info("%s %-12s no usage seen yet", marker, name)
			log.Info("    Check it live with: csm usage %s", name)
			continue
		}

		log.Info("%s %-12s updated %s ago", marker, name, now.Sub(rec.UpdatedAt).Round(time.Second))

		if rec.Usage.Limited {
			log.Info("    limit reached: requests are currently being rejected")
		}

		printWindow(log, "5-hour", rec.Usage.FiveHour, now)
		printWindow(log, "7-day", rec.Usage.SevenDay, now)
		printWindow(log, "spend", rec.Usage.SpendLimit, now)

		if fetchErr != nil {
			log.Info("    showing last saved figures; live check failed: %s", indent(fetchErr.Error()))
		}

		if a, err := s.resolveAccount(name); err == nil && a.status(now) == statusCooldown {
			log.Info("    csm cooldown until %s", formatReset(a.CooldownUntil, now))
		}
	}
}

func indent(text string) string {
	return strings.ReplaceAll(strings.TrimSpace(text), "\n", "\n    ")
}

func printWindow(log *Logger, label string, win provider.UsageWindow, now time.Time) {
	if win.ResetsAt.IsZero() {
		return
	}

	if !win.ResetsAt.After(now) {
		log.Info("    %-8s window reset at %s (was %.0f%%)", label, formatReset(win.ResetsAt, now), win.UsedPercent)

		return
	}

	log.Info("    %-8s %3.0f%%   resets %s", label, win.UsedPercent, formatReset(win.ResetsAt, now))
}

func formatReset(t, now time.Time) string {
	t, now = t.Local(), now.Local()

	switch {
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return t.Format("15:04")
	case t.Sub(now) < 7*24*time.Hour && now.Sub(t) < 7*24*time.Hour:
		return t.Format("Mon 15:04")
	default:
		return t.Format("Jan 2 15:04")
	}
}

func shortUsage(u provider.Usage, now time.Time) string {
	var parts []string

	if u.FiveHour.ResetsAt.After(now) {
		parts = append(parts, fmt.Sprintf("5h %.0f%%", u.FiveHour.UsedPercent))
	}

	if u.SevenDay.ResetsAt.After(now) {
		parts = append(parts, fmt.Sprintf("7d %.0f%%", u.SevenDay.UsedPercent))
	}

	if u.SpendLimit.ResetsAt.After(now) {
		parts = append(parts, fmt.Sprintf("spend %.0f%%", u.SpendLimit.UsedPercent))
	}

	return strings.Join(parts, " · ")
}
