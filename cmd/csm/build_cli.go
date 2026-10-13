package main

import (
	"errors"
	"fmt"
	"strings"
)

// cmdBuild reads the runs the service keeps on disk; it never changes one, because the service owns their agents.
func cmdBuild(log *Logger, home string, args []string) error {
	store := newBuildStore(home)

	if len(args) == 0 || args[0] == "list" {
		runs := store.all()

		if len(runs) == 0 {
			log.Info("No runs yet. Start a Build Room run from the CSM app, or fix a Sentry issue with csm fix.")

			return nil
		}

		for _, b := range runs {
			s := b.summary()
			log.Info("%s  %-24s %d/%d tasks  %s  %s", b.ID, strings.ReplaceAll(string(b.Status), "_", " "), s.Completed, s.Tasks, b.RepoName, b.Title)
		}

		return nil
	}

	if _, ok := buildActions[args[0]]; (ok || args[0] == "watch") && len(args) == 2 {
		return cmdBuildAction(log, home, args[0], args[1])
	}

	if args[0] != "show" || len(args) != 2 {
		return errors.New("usage: csm build list | show <id> | watch <id> | approve <id> | finish <id> | retry <id> | cancel <id>")
	}

	b, err := store.load(args[1])
	if err != nil {
		return fmt.Errorf("no build run %q", args[1])
	}

	log.Info("%s\n", b.Title)
	log.Info("Status      %s", strings.ReplaceAll(string(b.Status), "_", " "))

	if b.Attention != "" {
		log.Info("Attention   %s", b.Attention)
	}

	log.Info("Repository  %s (%s at %s)", displayPath(b.Repo), b.BaseBranch, shortSHA(b.BaseCommit))

	if b.Integration.Branch != "" {
		log.Info("Branch      %s", b.Integration.Branch)
	}

	if b.PR.URL != "" {
		log.Info("Pull request %s", b.PR.URL)
	}

	log.Blank()

	for _, t := range topoOrder(b.Tasks) {
		deps := ""

		if len(t.DependsOn) > 0 {
			deps = " (after " + strings.Join(t.DependsOn, ", ") + ")"
		}

		log.Info("  %-16s %-16s %-12s %s%s", t.ID, strings.ReplaceAll(string(t.Status), "_", " "), agentLabel(t.Provider, ""), t.Title, deps)
	}

	var final []CheckResult

	for _, c := range b.Checks {
		if c.Stage == "integration" {
			final = append(final, c)
		}
	}

	if len(final) > 0 {
		log.Blank()

		for _, c := range final {
			log.Info("  %-24s %s", c.Name, strings.ReplaceAll(string(c.State), "_", " "))
		}
	}

	return nil
}
