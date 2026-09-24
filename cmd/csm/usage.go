package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

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

const usageFetchTimeout = 90 * time.Second

func cmdUsage(home string, args []string) error {
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
		p, err := provider.FindClaude()
		if err != nil {
			return err
		}
		if err := p.CheckEnv(); err != nil {
			return err
		}
		features, err := p.Features()
		if err != nil {
			return err
		}
		if !features.UsageProbe {
			return fmt.Errorf("this %s version cannot report usage outside a session.\n\nShow the last recorded figures with: csm usage --cached", p.Name())
		}
		fmt.Printf("Checking %d account(s) with %s...\n\n", len(names), p.Name())
		errs = refreshUsage(s, p, names, time.Now())
	}
	printUsage(os.Stdout, s, names, errs, time.Now())
	return nil
}

func refreshUsage(s *State, p provider.Claude, names []string, now time.Time) map[string]error {
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

func printUsage(w io.Writer, s *State, names []string, errs map[string]error, now time.Time) {
	fmt.Fprintln(w, "Usage (Claude Code)")
	for _, name := range names {
		marker := "○"
		if name == s.Config.ActiveAccount {
			marker = "●"
		}
		fmt.Fprintln(w)
		rec, ok := s.loadUsage(name)
		fetchErr := errs[name]
		switch {
		case (!ok || rec.Usage.Empty()) && fetchErr != nil:
			fmt.Fprintf(w, "%s %-12s could not check usage\n", marker, name)
			fmt.Fprintf(w, "    %s\n", indent(fetchErr.Error()))
			continue
		case !ok || rec.Usage.Empty():
			fmt.Fprintf(w, "%s %-12s no usage seen yet\n", marker, name)
			fmt.Fprintf(w, "    Check it live with: csm usage %s\n", name)
			continue
		}
		fmt.Fprintf(w, "%s %-12s updated %s ago\n", marker, name, now.Sub(rec.UpdatedAt).Round(time.Second))
		if rec.Usage.Limited {
			fmt.Fprintln(w, "    limit reached: requests are currently being rejected")
		}
		printWindow(w, "5-hour", rec.Usage.FiveHour, now)
		printWindow(w, "7-day", rec.Usage.SevenDay, now)
		printWindow(w, "spend", rec.Usage.SpendLimit, now)
		if fetchErr != nil {
			fmt.Fprintf(w, "    showing last saved figures; live check failed: %s\n", indent(fetchErr.Error()))
		}
		if a, err := s.resolveAccount(name); err == nil && a.status(now) == statusCooldown {
			fmt.Fprintf(w, "    csm cooldown until %s\n", formatReset(a.CooldownUntil, now))
		}
	}
	if errs == nil {
		fmt.Fprintln(w, "\nLast saved figures only. Run `csm usage` without --cached for a live check.")
	} else {
		fmt.Fprintln(w, "\nA live check sends one tiny Haiku message per account (about 500 tokens).")
	}
}

func indent(text string) string {
	return strings.ReplaceAll(strings.TrimSpace(text), "\n", "\n    ")
}

func printWindow(w io.Writer, label string, win provider.UsageWindow, now time.Time) {
	if win.ResetsAt.IsZero() {
		return
	}
	if !win.ResetsAt.After(now) {
		fmt.Fprintf(w, "    %-8s window reset at %s (was %.0f%%)\n", label, formatReset(win.ResetsAt, now), win.UsedPercent)
		return
	}
	fmt.Fprintf(w, "    %-8s %3.0f%%   resets %s\n", label, win.UsedPercent, formatReset(win.ResetsAt, now))
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
