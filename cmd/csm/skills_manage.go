package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onoja123/csm/internal/provider"
	"github.com/onoja123/csm/internal/skills"
)

func joinPath(base string, parts ...string) string {
	all := []string{base}

	for _, p := range parts {
		all = append(all, filepath.FromSlash(p))
	}

	return filepath.Join(all...)
}

type updateReport struct {
	Name     string             `json:"name"`
	Status   string             `json:"status"`
	Error    string             `json:"error,omitempty"`
	Plan     *skills.UpdatePlan `json:"plan,omitempty"`
	Outcomes []skills.Outcome   `json:"placements,omitempty"`
	Upstream *skills.Upstream   `json:"upstream,omitempty"`
}

func skillsUpdate(log *Logger, s *State, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, []string{"all", "yes", "unpin", "accept-risk", "diff"})
	if err != nil {
		return err
	}

	ids := a.pos

	if a.has("all") {
		ids = nil
	}

	for _, ref := range ids {
		if _, err := lib.Find(ref); err != nil {
			return err
		}
	}

	reports := checkSkillUpdates(s, lib, ids, a.has("yes"), skills.UpdateOptions{AcceptRisk: a.has("accept-risk"), Unpin: a.has("unpin")})

	if a.has("json") {
		return printJSON(log, reports)
	}

	if len(reports) == 0 {
		log.Info("No skills installed.")

		return nil
	}

	pending := 0

	for _, rep := range reports {
		mark := "✓"

		switch {
		case rep.Error != "":
			mark = "✗"
		case rep.Status == "update available":
			mark = "↑"
			pending++
		}

		log.Info("%s %-28s %s", mark, rep.Name, rep.Status)

		if rep.Error != "" {
			log.Info("    %s", rep.Error)
		}

		if p := rep.Plan; p != nil && !p.UpToDate {
			printPlan(log, *p, a.has("diff"))
		}

		if len(rep.Outcomes) > 0 {
			printOutcomes(log, rep.Outcomes)
		}
	}

	if pending > 0 {
		log.Info("\nReview the changes (add --diff to see them), then apply with: csm skills update <name> --yes")
	}

	return nil
}

func printPlan(log *Logger, p skills.UpdatePlan, diff bool) {
	if p.Record.Pinned {
		log.Info("    pinned to %s; applying needs --unpin", p.Record.VersionLabel())
	}

	from, to := p.Record.VersionLabel(), p.Candidate.Revision

	if to == "" && p.Candidate.Skill != nil {
		to = "content " + shortHash(p.Candidate.Skill.Hash)
	}

	log.Info("    %s -> %s, %d file(s) changed", from, shortHash(to), len(p.Changes))

	for _, c := range p.Changes {
		log.Info("      %-9s %s", c.Change, c.Path)
	}

	if p.RiskIncrease {
		log.Info("    ! the new version's inspection is worse: %s (apply needs --accept-risk if serious)", p.Candidate.Inspection.Summary())
	}

	if p.NewPermissions {
		log.Info("    ! the new version asks for different tool permissions; they stay removed until you approve them again")
	}

	for _, d := range p.LocalChanges {
		log.Info("    ! your edits in %s are kept; that copy will not get the update until you choose: csm skills keep|revert", placementLabel(d))
	}

	if diff {
		for _, c := range p.Changes {
			for _, line := range strings.Split(strings.TrimRight(skills.RedactSecrets(c.Diff), "\n"), "\n") {
				log.Info("      %s", line)
			}
		}
	}
}

func skillsRollback(log *Logger, s *State, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, nil)
	if err != nil {
		return err
	}

	if len(a.pos) != 1 {
		return errors.New("usage: csm skills rollback <name>")
	}

	r, err := lib.Rollback(a.pos[0], time.Now())
	if err != nil {
		return err
	}

	outcomes, err := syncSkills(lib, s, func(x skills.Record) bool { return x.ID == r.ID })
	if err != nil {
		return err
	}

	if a.has("json") {
		return printJSON(log, map[string]any{"record": r, "placements": outcomes[r.ID]})
	}

	log.Info("✓ %s is back at %s and pinned there; csm skills update %s --unpin moves it again", r.Name, r.VersionLabel(), r.Name)
	printOutcomes(log, outcomes[r.ID])

	return nil
}

// expectedCopy is what csm would place for a projection now: the installed revision adapted for its agent.
func expectedCopy(lib *skills.Library, r skills.Record, p skills.Projection) (map[string][]byte, error) {
	s, contents, _, err := lib.LoadRevision(r.ID, r.Hash)
	if err != nil {
		return nil, err
	}

	out, _, err := skills.Adapt(s, contents, provider.SkillsAgent(p.Provider), skills.AdaptOptions{Dir: p.Dir, PermissionsApproved: r.PermissionsApproved})

	return out, err
}

func skillsDiff(log *Logger, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, []string{"upstream"})
	if err != nil {
		return err
	}

	if len(a.pos) != 1 {
		return errors.New("usage: csm skills diff <name> [--upstream]")
	}

	r, err := lib.Find(a.pos[0])
	if err != nil {
		return err
	}

	type diffReport struct {
		Dir     string              `json:"dir"`
		Changes []skills.FileChange `json:"changes"`
	}

	var reports []diffReport

	if a.has("upstream") {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		cand, up := newSkillFetcher(lib).Check(ctx, *r)
		if cand == nil {
			return fmt.Errorf("could not read the source: %s", up.Error)
		}

		_, current, _, err := lib.LoadRevision(r.ID, r.Hash)
		if err != nil {
			return err
		}

		reports = append(reports, diffReport{Dir: r.Source.String(), Changes: skills.Changes(current, cand.Contents)})
	} else {
		for _, p := range r.Projections {
			if _, err := skills.CheckOwned(p.Dir, r.ID); !errors.Is(err, skills.ErrModified) {
				continue
			}

			want, err := expectedCopy(lib, *r, p)
			if err != nil {
				return err
			}

			have, _, err := skills.ReadTree(p.Dir, []string{".gitignore"})
			if err != nil {
				return err
			}

			reports = append(reports, diffReport{Dir: p.Dir, Changes: skills.Changes(want, have)})
		}
	}

	for i := range reports {
		for j := range reports[i].Changes {
			reports[i].Changes[j].Diff = skills.RedactSecrets(reports[i].Changes[j].Diff)
		}
	}

	if a.has("json") {
		return printJSON(log, reports)
	}

	if len(reports) == 0 {
		log.Info("No placed copy of %s was edited.", r.Name)

		return nil
	}

	for _, rep := range reports {
		log.Info("%s", placementLabel(rep.Dir))

		if len(rep.Changes) == 0 {
			log.Info("  no differences")
		}

		for _, c := range rep.Changes {
			log.Info("%s", strings.TrimRight(c.Diff, "\n"))
		}
	}

	if !a.has("upstream") {
		log.Info("\nKeep your edits as the installed version with: csm skills keep %s <folder>\nOr restore csm's copy (yours is backed up) with: csm skills revert %s <folder>", r.Name, r.Name)
	}

	return nil
}

func projectionFor(r skills.Record, dir string) (skills.Projection, bool) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return skills.Projection{}, false
	}

	for _, p := range r.Projections {
		if resolvedDir(p.Dir) == resolvedDir(abs) {
			return p, true
		}
	}

	return skills.Projection{}, false
}

func skillsKeep(log *Logger, s *State, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, nil)
	if err != nil {
		return err
	}

	if len(a.pos) != 2 {
		return errors.New("usage: csm skills keep <name> <folder>")
	}

	r, err := lib.Find(a.pos[0])
	if err != nil {
		return err
	}

	p, ok := projectionFor(*r, a.pos[1])
	if !ok {
		return fmt.Errorf("%s is not a copy csm placed for %s; see: csm skills show %s", a.pos[1], r.Name, r.Name)
	}

	updated, err := lib.Adopt(r.ID, p.Dir, time.Now())
	if err != nil {
		return err
	}

	if err := skills.Reclaim(p.Dir, r.ID, updated.Hash, updated.Revision); err != nil {
		return err
	}

	outcomes, err := syncSkills(lib, s, func(x skills.Record) bool { return x.ID == r.ID })
	if err != nil {
		return err
	}

	if a.has("json") {
		return printJSON(log, map[string]any{"record": updated, "placements": outcomes[r.ID]})
	}

	log.Info("✓ Your edits in %s are now the installed version of %s (content %s; upstream %s is kept on record)", placementLabel(p.Dir), r.Name, shortHash(updated.Hash), shortHash(updated.UpstreamHash))
	printOutcomes(log, outcomes[r.ID])

	return nil
}

func skillsRevert(log *Logger, s *State, lib *skills.Library, args []string) error {
	a, err := parseCLI(args, nil)
	if err != nil {
		return err
	}

	if len(a.pos) != 2 {
		return errors.New("usage: csm skills revert <name> <folder>")
	}

	r, err := lib.Find(a.pos[0])
	if err != nil {
		return err
	}

	p, ok := projectionFor(*r, a.pos[1])
	if !ok {
		return fmt.Errorf("%s is not a copy csm placed for %s; csm only restores its own copies", a.pos[1], r.Name)
	}

	if _, err := skills.CheckOwned(p.Dir, r.ID); err == nil {
		log.Info("%s is unchanged; nothing to revert.", placementLabel(p.Dir))

		return nil
	} else if !errors.Is(err, skills.ErrModified) {
		return fmt.Errorf("%s: %w", p.Dir, err)
	}

	backup, err := lib.Backup(p.Dir)
	if err != nil {
		return fmt.Errorf("could not back up your copy, so it was left alone: %w", err)
	}

	if err := os.RemoveAll(p.Dir); err != nil {
		return err
	}

	outcomes, err := syncSkills(lib, s, func(x skills.Record) bool { return x.ID == r.ID })
	if err != nil {
		return err
	}

	if a.has("json") {
		return printJSON(log, map[string]any{"backup": backup, "placements": outcomes[r.ID]})
	}

	log.Info("✓ Restored csm's copy of %s in %s\n  your edited copy is saved at %s", r.Name, placementLabel(p.Dir), displayPath(backup))
	printOutcomes(log, outcomes[r.ID])

	return nil
}
