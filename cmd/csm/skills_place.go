package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onoja123/csm/internal/provider"
	"github.com/onoja123/csm/internal/skills"
)

func skillAgents() map[string]skills.Agent {
	out := map[string]skills.Agent{}

	for _, id := range provider.IDs {
		out[id] = provider.SkillsAgent(id)
	}

	return out
}

func resolvedDir(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}

	if parent, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(parent, filepath.Base(p))
	}

	return p
}

func userHome() string {
	home, _ := os.UserHomeDir()

	return home
}

// accountConsumer is where one account of an agent loads user skills from and where csm places them.
func accountConsumer(a *Account, agent skills.Agent) (skills.Consumer, bool) {
	c := skills.Consumer{Provider: agent.Provider, Account: a.Name, Scope: skills.ScopeGlobal}
	home := userHome()

	for _, d := range agent.UserDirs {
		c.Read = append(c.Read, resolvedDir(filepath.Join(a.ConfigDir, filepath.FromSlash(d))))
	}

	for _, d := range agent.HomeDirs {
		c.Read = append(c.Read, resolvedDir(filepath.Join(home, filepath.FromSlash(d))))
	}

	switch {
	case len(agent.UserDirs) > 0:
		c.Write = resolvedDir(filepath.Join(a.ConfigDir, filepath.FromSlash(agent.UserDirs[0])))
	case len(agent.HomeDirs) > 0:
		c.Write = resolvedDir(filepath.Join(home, filepath.FromSlash(agent.HomeDirs[0])))
	default:
		return c, false
	}

	return c, true
}

func projectConsumer(dir string, agent skills.Agent) (skills.Consumer, bool) {
	if len(agent.ProjectDirs) == 0 {
		return skills.Consumer{}, false
	}

	c := skills.Consumer{Provider: agent.Provider, Scope: skills.ScopeProject}

	for _, d := range agent.ProjectDirs {
		c.Read = append(c.Read, resolvedDir(filepath.Join(dir, filepath.FromSlash(d))))
	}

	c.Write = c.Read[0]

	return c, true
}

// skillConsumers lists every account and project a skill should reach, in a fixed order so placement is stable.
func skillConsumers(s *State, rec skills.Record, agents map[string]skills.Agent, onlyProvider, onlyAccount, onlyProject string) []skills.Consumer {
	var out []skills.Consumer

	for _, id := range provider.IDs {
		agent := agents[id]

		if !agent.Supported() || onlyProvider != "" && id != onlyProvider {
			continue
		}

		if rec.Global {
			for i := range s.Accounts {
				a := &s.Accounts[i]

				if a.providerID() != id || onlyAccount != "" && a.Name != onlyAccount {
					continue
				}

				if c, ok := accountConsumer(a, agent); ok {
					out = append(out, c)
				}
			}
		}

		for _, dir := range rec.Projects {
			if onlyProject != "" && dir != onlyProject {
				continue
			}

			if c, ok := projectConsumer(dir, agent); ok {
				out = append(out, c)
			}
		}
	}

	return out
}

// syncSkills brings the placements of the matching records up to date and saves them.
func syncSkills(lib *skills.Library, s *State, match func(skills.Record) bool) (map[string][]skills.Outcome, error) {
	agents := skillAgents()
	out := map[string][]skills.Outcome{}

	err := lib.Update(func(f *skills.LibraryFile) error {
		for i := range f.Skills {
			rec := &f.Skills[i]

			if match != nil && !match(*rec) {
				continue
			}

			out[rec.ID] = lib.Sync(rec, skillConsumers(s, *rec, agents, "", "", ""), agents, time.Now())
		}

		return nil
	})

	return out, err
}

// placeForLaunch makes the installed skills reach the account and project an agent is about to start in. It only
// adds what is missing; removals happen when the user changes a skill.
func placeForLaunch(home string, s *State, providerID, account, projectDir string) []string {
	lib := skills.Open(home)
	records, err := lib.Records()

	if err != nil || len(records) == 0 {
		return nil
	}

	agents := skillAgents()
	var notes []string

	lib.Update(func(f *skills.LibraryFile) error {
		for i := range f.Skills {
			rec := &f.Skills[i]

			if !rec.Enabled || !rec.For(providerID) || !rec.InProject(projectDir) {
				continue
			}

			for _, o := range lib.Sync(rec, skillConsumers(s, *rec, agents, providerID, account, projectDir), agents, time.Now(), skills.SyncOptions{Partial: true}) {
				if o.Status == skills.StatusConflict || o.Status == skills.StatusFailed {
					notes = append(notes, rec.Name+": "+o.Detail)
				}
			}
		}

		return nil
	})

	return notes
}

// placementLabel says whose folder a placement is in, so a user sees when csm wrote into their own agent folder.
func placementLabel(dir string) string {
	home := userHome()
	csmHomeDir, _ := csmHome()

	switch {
	case csmHomeDir != "" && strings.HasPrefix(dir, resolvedDir(csmHomeDir)+string(filepath.Separator)):
		return displayPath(dir)
	case home != "" && strings.HasPrefix(dir, home+string(filepath.Separator)):
		return displayPath(dir) + " (your own folder)"
	}

	return displayPath(dir)
}
