package handoff

import (
	"fmt"
	"strings"
)

// Skills records which skills a handoff carried to the next agent, which it could not, and why. It names skills,
// revisions and folders; it never holds a skill's contents.
type Skills struct {
	Manifest string `json:"manifest,omitempty"`
	// Source is the manifest the previous agent worked with, when the handoff carries a copy of it.
	Source     string         `json:"source,omitempty"`
	Carried    []CarriedSkill `json:"carried,omitempty"`
	NotCarried []CarriedSkill `json:"not_carried,omitempty"`
	// Brief is the part of the receiving agent's instruction that names the skills and how to load them.
	Brief string `json:"brief,omitempty"`
}

type CarriedSkill struct {
	ID          string   `json:"id,omitempty"`
	Name        string   `json:"name"`
	Version     string   `json:"version,omitempty"`
	Hash        string   `json:"hash,omitempty"`
	Source      string   `json:"source,omitempty"`
	Origin      string   `json:"origin,omitempty"`
	From        string   `json:"from,omitempty"`
	To          string   `json:"to,omitempty"`
	Level       string   `json:"level,omitempty"`
	Adaptations []string `json:"adaptations,omitempty"`
	// Used is what the previous agent's own session record shows: "invoked", "unverified" or empty.
	Used   string `json:"used,omitempty"`
	Use    string `json:"use,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (s *Skills) Empty() bool { return len(s.Carried)+len(s.NotCarried) == 0 }

func (s *Skills) Render() string {
	var b strings.Builder

	for _, c := range s.Carried {
		fmt.Fprintf(&b, "- %s", c.Name)

		if c.Version != "" {
			fmt.Fprintf(&b, " (%s)", c.Version)
		}

		fmt.Fprintf(&b, ": available at %s", c.To)

		if c.Use != "" {
			fmt.Fprintf(&b, "; %s", c.Use)
		}

		if c.Level != "" && c.Level != "compatible" {
			fmt.Fprintf(&b, "; %s", c.Level)
		}

		for _, a := range c.Adaptations {
			fmt.Fprintf(&b, "; adapted: %s", a)
		}

		if c.Used == "invoked" {
			b.WriteString("; the previous agent loaded it")
		}

		b.WriteString("\n")
	}

	for _, c := range s.NotCarried {
		fmt.Fprintf(&b, "- %s: not carried: %s\n", c.Name, c.Reason)
	}

	b.WriteString("\nA skill being available does not mean it was loaded or followed; check the work itself.\n")

	return b.String()
}
