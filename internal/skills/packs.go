package skills

import (
	"fmt"
	"time"
)

// Pack is a versioned list of skills for one kind of work. Members are fetched from their own repositories at
// install time; csm has not reviewed or tested any pack as a set, and says so wherever packs are shown.
type Pack struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Version     string       `json:"version"`
	Description string       `json:"description"`
	Members     []PackMember `json:"members"`
}

type PackMember struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	// Optional members may be skipped without failing the pack.
	Optional bool `json:"optional,omitempty"`
}

const PackDisclaimer = "Packs group public skills by topic. csm has not reviewed or tested them as a set; inspect each skill before you rely on it."

// Packs only name skills that exist in the catalog's repositories; there is no Go backend pack because none of
// those repositories has a Go skill.
var CuratedPacks = []Pack{
	{
		ID: "frontend", Name: "Frontend Implementation", Version: "1",
		Description: "Design, build and test web interfaces.",
		Members: []PackMember{
			{Name: "frontend-design", Source: "github.com/anthropics/skills/skills/frontend-design"},
			{Name: "react-best-practices", Source: "github.com/vercel-labs/agent-skills/skills/react-best-practices"},
			{Name: "web-design-guidelines", Source: "github.com/vercel-labs/agent-skills/skills/web-design-guidelines"},
			{Name: "webapp-testing", Source: "github.com/anthropics/skills/skills/webapp-testing", Optional: true},
		},
	},
	{
		ID: "react-native", Name: "React Native Engineer", Version: "1",
		Description: "React Native and Expo apps: patterns, navigation and upgrades.",
		Members: []PackMember{
			{Name: "react-native-best-practices", Source: "github.com/callstackincubator/agent-skills/skills/react-native-best-practices"},
			{Name: "react-navigation", Source: "github.com/callstackincubator/agent-skills/skills/react-navigation"},
			{Name: "upgrading-react-native", Source: "github.com/callstackincubator/agent-skills/skills/upgrading-react-native"},
			{Name: "expo-router", Source: "github.com/expo/skills/plugins/expo/skills/expo-router", Optional: true},
		},
	},
	{
		ID: "code-review", Name: "Code Review", Version: "1",
		Description: "Ask for, give and act on code review.",
		Members: []PackMember{
			{Name: "requesting-code-review", Source: "github.com/obra/superpowers/skills/requesting-code-review"},
			{Name: "receiving-code-review", Source: "github.com/obra/superpowers/skills/receiving-code-review"},
			{Name: "differential-review", Source: "github.com/trailofbits/skills/plugins/differential-review/skills/differential-review", Optional: true},
		},
	},
	{
		ID: "testing", Name: "Testing and Quality", Version: "1",
		Description: "Test first, debug methodically and verify before calling work done.",
		Members: []PackMember{
			{Name: "test-driven-development", Source: "github.com/obra/superpowers/skills/test-driven-development"},
			{Name: "systematic-debugging", Source: "github.com/obra/superpowers/skills/systematic-debugging"},
			{Name: "verification-before-completion", Source: "github.com/obra/superpowers/skills/verification-before-completion"},
			{Name: "property-based-testing", Source: "github.com/trailofbits/skills/plugins/property-based-testing/skills/property-based-testing", Optional: true},
		},
	},
	{
		ID: "security-review", Name: "Security Review", Version: "1",
		Description: "Security-focused review of changes and attack surface.",
		Members: []PackMember{
			{Name: "differential-review", Source: "github.com/trailofbits/skills/plugins/differential-review/skills/differential-review"},
			{Name: "entry-point-analyzer", Source: "github.com/trailofbits/skills/plugins/entry-point-analyzer/skills/entry-point-analyzer"},
			{Name: "agentic-actions-auditor", Source: "github.com/trailofbits/skills/plugins/agentic-actions-auditor/skills/agentic-actions-auditor", Optional: true},
		},
	},
	{
		ID: "documents", Name: "Office Documents", Version: "1",
		Description: "Read and write PDF, Word, Excel and PowerPoint files.",
		Members: []PackMember{
			{Name: "pdf", Source: "github.com/anthropics/skills/skills/pdf"},
			{Name: "docx", Source: "github.com/anthropics/skills/skills/docx"},
			{Name: "xlsx", Source: "github.com/anthropics/skills/skills/xlsx"},
			{Name: "pptx", Source: "github.com/anthropics/skills/skills/pptx"},
		},
	},
}

func FindPack(id string) (Pack, error) {
	for _, p := range CuratedPacks {
		if p.ID == id || p.Name == id {
			return p, nil
		}
	}

	return Pack{}, fmt.Errorf("no pack %q: %w", id, ErrNotFound)
}

// RecordPack remembers which installed skills a pack brought in.
func (l *Library) RecordPack(p Pack, members []string, now time.Time) error {
	return l.Update(func(f *LibraryFile) error {
		for i := range f.Packs {
			if f.Packs[i].ID == p.ID {
				for _, m := range members {
					if !contains(f.Packs[i].Members, m) {
						f.Packs[i].Members = append(f.Packs[i].Members, m)
					}
				}

				f.Packs[i].Version = p.Version

				return nil
			}
		}

		f.Packs = append(f.Packs, PackRecord{ID: p.ID, Version: p.Version, Members: members, InstalledAt: now})

		return nil
	})
}
