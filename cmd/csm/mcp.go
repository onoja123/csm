package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onoja123/csm/internal/mcp"
	"github.com/onoja123/csm/internal/provider"
)

const (
	mcpSnapshotFile = "mcp-snapshot.json"
	mcpHandoffFile  = "mcp-handoff.json"
)

// envPresent answers whether a variable is set without ever handing its value on.
func envPresent(name string) bool {
	_, ok := os.LookupEnv(name)

	return ok
}

// The snapshot on disk is a record of what the agent ran with; it holds names only, never values.
func (r *runner) snapshotMCP(from *Account) {
	adapter, ok := provider.MCPAdapter(r.provider)
	if !ok {
		return
	}

	snap, err := mcp.Take(adapter, from.ConfigDir, r.cwd, time.Now())
	if err != nil {
		slog.Debug("mcp snapshot", "account", from.Name, "err", err)

		return
	}

	if err := writeJSON(filepath.Join(r.stateDir, mcpSnapshotFile), snap); err != nil {
		slog.Debug("save mcp snapshot", "err", err)
	}
}

func (r *runner) printMCP(rep mcp.Report) {
	if len(rep.Results) == 0 {
		return
	}

	r.log.Info("MCP handoff:")

	for _, line := range rep.Lines() {
		r.log.Info("  %s", line)
	}

	if rep.Count(mcp.RequiresAuth) > 0 {
		r.log.Info("\nServers marked ⚠ need their credentials or login in the new profile; csm never copies secret values.")
	}

	r.log.Blank()
}

func mcpUsage() error {
	return errors.New("usage: csm mcp [account] | csm mcp handoff <from> <to> [--apply]")
}

func cmdMCP(log *Logger, home string, args []string) error {
	s, err := loadState(home)
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	if len(args) > 0 && args[0] == "handoff" {
		return mcpHandoffCommand(log, s, cwd, args[1:])
	}

	if len(args) > 1 {
		return mcpUsage()
	}

	var accounts []*Account

	if len(args) == 1 {
		a, err := s.resolveAccount(args[0])
		if err != nil {
			return err
		}

		accounts = append(accounts, a)
	} else {
		for i := range s.Accounts {
			if s.Accounts[i].Enabled {
				accounts = append(accounts, &s.Accounts[i])
			}
		}
	}

	if len(accounts) == 0 {
		log.Info("No accounts yet. Add one with:\n\n    csm account add personal")

		return nil
	}

	log.Info("MCP servers for %s", displayPath(cwd))

	for _, a := range accounts {
		log.Blank()
		results, err := inspectAccountMCP(a, cwd)

		switch {
		case errors.Is(err, mcp.ErrNoMCP):
			log.Info("%s (%s): %v", a.Name, providerName(a.providerID()), err)
		case err != nil:
			log.Info("%s (%s): ✗ %v", a.Name, providerName(a.providerID()), err)
		case len(results) == 0:
			log.Info("%s (%s): no MCP servers configured", a.Name, providerName(a.providerID()))
		default:
			log.Info("%s (%s)", a.Name, providerName(a.providerID()))

			for _, r := range results {
				log.Info("  %s", r.Line())
			}
		}
	}

	return nil
}

func inspectAccountMCP(a *Account, cwd string) ([]mcp.Result, error) {
	p, err := provider.New(a.providerID(), "")
	if err != nil {
		return nil, err
	}

	adapter, ok := provider.MCPAdapter(p)
	if !ok {
		return nil, mcp.ErrNoMCP
	}

	return mcp.Inspect(adapter, a.ConfigDir, cwd, envPresent)
}

func mcpHandoffCommand(log *Logger, s *State, cwd string, args []string) error {
	apply := false
	var names []string

	for _, a := range args {
		if a == "--apply" {
			apply = true

			continue
		}

		names = append(names, a)
	}

	if len(names) != 2 {
		return mcpUsage()
	}

	from, err := s.resolveAccount(names[0])
	if err != nil {
		return err
	}

	to, err := s.resolveAccount(names[1])
	if err != nil {
		return err
	}

	if from.Name == to.Name {
		return errors.New("the source and target accounts are the same")
	}

	rep, err := handoffBetween(from, to, cwd, !apply)
	if err != nil {
		return err
	}

	log.Info("MCP handoff %s (%s) → %s (%s)", from.Name, providerName(from.providerID()), to.Name, providerName(to.providerID()))
	log.Blank()

	if len(rep.Results) == 0 {
		log.Info("  %s has no MCP servers configured for %s.", from.Name, displayPath(cwd))

		return nil
	}

	for _, line := range rep.Lines() {
		log.Info("  %s", line)
	}

	log.Blank()

	switch {
	case !apply:
		log.Info("Nothing was written. Add --apply to write the compatible servers into %s.", to.Name)
	case rep.Count(mcp.Failed) > 0:
		log.Info("Some servers could not be written to %s; see above.", to.Name)
	case rep.Count(mcp.Migrated) > 0:
		log.Info("Written to the %s profile. Servers marked ⚠ still need their credentials or login there.", to.Name)
	default:
		log.Info("Nothing needed writing.")
	}

	return nil
}

func handoffBetween(from, to *Account, cwd string, dryRun bool) (mcp.Report, error) {
	source, err := provider.New(from.providerID(), "")
	if err != nil {
		return mcp.Report{}, err
	}

	target, err := provider.New(to.providerID(), "")
	if err != nil {
		return mcp.Report{}, err
	}

	sourceAdapter, ok := provider.MCPAdapter(source)
	if !ok {
		return mcp.Report{}, fmt.Errorf("%s: %w", source.Name(), mcp.ErrNoMCP)
	}

	targetAdapter, ok := provider.MCPAdapter(target)
	if !ok {
		return mcp.Report{}, fmt.Errorf("%s: %w", target.Name(), mcp.ErrNoMCP)
	}

	snap, err := mcp.Take(sourceAdapter, from.ConfigDir, cwd, time.Now())
	if err != nil {
		return mcp.Report{}, fmt.Errorf("read the MCP configuration of %s: %w", from.Name, err)
	}

	t := mcp.Target{Adapter: targetAdapter, ProfileDir: to.ConfigDir, ProjectDir: cwd, Label: to.Name}

	return mcp.Handoff(snap, t, envPresent, dryRun, time.Now()), nil
}

func doctorMCP(log *Logger, s *State, usable map[string]provider.Provider, cwd string, fail func(label, reason string)) {
	for i := range s.Accounts {
		a := &s.Accounts[i]

		if !a.Enabled {
			continue
		}

		p, ok := usable[a.providerID()]
		if !ok {
			continue
		}

		adapter, ok := provider.MCPAdapter(p)
		if !ok {
			continue
		}

		label := "MCP " + a.Name
		results, err := mcp.Inspect(adapter, a.ConfigDir, cwd, envPresent)

		if err != nil {
			fail(label, err.Error())

			continue
		}

		if len(results) == 0 {
			log.Row(label, "none")

			continue
		}

		var names []string

		for _, r := range results {
			names = append(names, r.Mark()+" "+r.Server)
		}

		log.Row(label, strings.Join(names, "  "))

		for _, r := range results {
			if r.Status != mcp.Available && r.Reason != "" {
				log.Info("  %s: %s", r.Server, r.Reason)
			}
		}
	}
}
