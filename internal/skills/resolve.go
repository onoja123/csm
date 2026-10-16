package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	ModeManual      = "manual"
	ModeRecommended = "recommended"
	ModeAutomatic   = "automatic"
)

const (
	OriginExplicit    = "explicit"
	OriginDefault     = "default"
	OriginAutomatic   = "automatic"
	OriginRecommended = "recommended"
	OriginHandoff     = "handoff"
)

const (
	recommendScore = 4
	automaticScore = 5
	defaultMaxAuto = 3
)

// Policy is the user's skill selection settings, kept in csm's config.
type Policy struct {
	Mode string `json:"mode,omitempty"`
	// Defaults are skill IDs selected for every task the agent can use them in.
	Defaults []string `json:"defaults,omitempty"`
	MaxAuto  int      `json:"max_auto,omitempty"`
	// Excluded maps a project folder to the skill IDs never used there.
	Excluded map[string][]string `json:"excluded,omitempty"`
}

func (p Policy) EffectiveMode() string {
	switch p.Mode {
	case ModeManual, ModeAutomatic:
		return p.Mode
	}

	return ModeRecommended
}

func (p Policy) maxAuto() int {
	if p.MaxAuto > 0 {
		return p.MaxAuto
	}

	return defaultMaxAuto
}

// Request is what the resolver may use about a task: its text and the project's setup, never its source code.
type Request struct {
	Text     string
	Provider string
	Project  string
	Explicit []string
	Facts    Facts
}

type Choice struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Origin      string  `json:"origin"`
	Reason      string  `json:"reason"`
	Score       float64 `json:"score,omitempty"`
	Compat      Compat  `json:"compat"`
}

type Skip struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Conflict is two skills, or a skill and the project, that want different things.
type Conflict struct {
	Facet         string   `json:"facet"`
	Skills        []string `json:"skills"`
	Values        []string `json:"values"`
	Project       string   `json:"project,omitempty"`
	Resolution    string   `json:"resolution"`
	NeedsDecision bool     `json:"needs_decision,omitempty"`
}

func (c Conflict) String() string {
	if c.Project != "" {
		return fmt.Sprintf("%s wants %s %s, but the project uses %s; %s", c.Skills[0], c.Facet, c.Values[0], c.Project, c.Resolution)
	}

	return fmt.Sprintf("%s and %s disagree about the %s (%s vs %s); %s", c.Skills[0], c.Skills[1], c.Facet, c.Values[0], c.Values[1], c.Resolution)
}

type Result struct {
	Mode        string     `json:"mode"`
	Selected    []Choice   `json:"selected"`
	Recommended []Choice   `json:"recommended,omitempty"`
	Skipped     []Skip     `json:"skipped,omitempty"`
	Conflicts   []Conflict `json:"conflicts,omitempty"`
	// Missing are skills asked for that are not installed; csm never installs them on its own.
	Missing []string `json:"missing,omitempty"`
}

func (r Result) NeedsDecision() bool {
	for _, c := range r.Conflicts {
		if c.NeedsDecision {
			return true
		}
	}

	return false
}

func originRank(origin string) int {
	switch origin {
	case OriginExplicit, OriginHandoff:
		return 0
	case OriginDefault:
		return 1
	}

	return 2
}

// Resolve picks the installed skills a task should use. It never installs anything and never selects a skill that
// is disabled, excluded from the project, limited to other agents or unusable by the task's agent.
func Resolve(records []Record, agent Agent, req Request, pol Policy) Result {
	res := Result{Mode: pol.EffectiveMode()}
	chosen := map[string]bool{}
	excluded := pol.Excluded[req.Project]

	eligible := func(r Record, origin string) (Compat, string) {
		c := Assess(&r.Skill, agent, r.PermissionsApproved)

		switch {
		case !r.Enabled:
			return c, "disabled; enable it with: csm skills enable " + r.Name
		case contains(excluded, r.ID):
			return c, "excluded in this project"
		case !r.For(req.Provider):
			return c, "not enabled for " + agent.Name
		case !c.Level.Usable():
			return c, fmt.Sprintf("%s for %s: %s", c.Level, agent.Name, strings.Join(c.Reasons, "; "))
		case origin != OriginExplicit && origin != OriginDefault && !r.InProject(req.Project):
			return c, "installed for other projects only"
		}

		return c, ""
	}

	add := func(ref, origin, reason string) {
		r, err := FindIn(records, ref)
		if err != nil {
			if origin == OriginExplicit {
				res.Missing = append(res.Missing, ref)
			}

			return
		}

		if chosen[r.ID] {
			return
		}

		c, why := eligible(*r, origin)

		if why != "" {
			res.Skipped = append(res.Skipped, Skip{ID: r.ID, Name: r.Name, Reason: why})

			return
		}

		chosen[r.ID] = true
		res.Selected = append(res.Selected, Choice{ID: r.ID, Name: r.Name, Description: r.Description, Origin: origin, Reason: reason, Compat: c})
	}

	for _, ref := range req.Explicit {
		add(ref, OriginExplicit, "selected for this task")
	}

	for _, id := range pol.Defaults {
		add(id, OriginDefault, "one of your default skills")
	}

	if res.Mode != ModeManual {
		terms := Terms(req.Text)
		var matches []Choice

		for _, r := range records {
			if chosen[r.ID] || len(terms) == 0 {
				continue
			}

			score := relevance(Item{Name: r.Name, Description: r.Description, Tags: append([]string{r.Category}, r.Tags...)}, terms)

			if score < recommendScore {
				continue
			}

			c, why := eligible(r, OriginRecommended)

			if why != "" {
				continue
			}

			matches = append(matches, Choice{ID: r.ID, Name: r.Name, Description: r.Description, Origin: OriginRecommended, Reason: matchReason(r, terms), Score: score, Compat: c})
		}

		sort.SliceStable(matches, func(i, j int) bool {
			if matches[i].Score != matches[j].Score {
				return matches[i].Score > matches[j].Score
			}

			return matches[i].Name < matches[j].Name
		})

		auto := 0

		for _, m := range matches {
			if res.Mode == ModeAutomatic && m.Score >= automaticScore && auto < pol.maxAuto() {
				m.Origin = OriginAutomatic
				res.Selected = append(res.Selected, m)
				chosen[m.ID] = true
				auto++

				continue
			}

			res.Recommended = append(res.Recommended, m)
		}
	}

	res.resolveConflicts(records, req.Facts)

	return res
}

func matchReason(r Record, terms []string) string {
	name := set(Terms(strings.ReplaceAll(r.Name, "-", " ")))
	tags := set(Terms(strings.Join(append([]string{r.Category}, r.Tags...), " ")))
	desc := set(Terms(r.Description))
	var hits []string

	for _, t := range terms {
		switch {
		case name[t] || strings.EqualFold(r.Name, t):
			hits = append(hits, fmt.Sprintf("%q matches its name", t))
		case tags[t]:
			hits = append(hits, fmt.Sprintf("%q matches a tag", t))
		case desc[t]:
			hits = append(hits, fmt.Sprintf("%q is in its description", t))
		}
	}

	if len(hits) > 3 {
		hits = hits[:3]
	}

	if len(hits) == 0 {
		return "related words in its description"
	}

	return strings.Join(hits, ", ")
}

// resolveConflicts applies the precedence order: the project's own setup outranks every skill, an explicit selection
// outranks a default, and a default outranks an automatic pick. Equal ranks that disagree need the user's decision.
func (res *Result) resolveConflicts(records []Record, facts Facts) {
	byID := map[string]Record{}

	for _, r := range records {
		byID[r.ID] = r
	}

	drop := map[string]bool{}

	for i := 0; i < len(res.Selected); i++ {
		a := res.Selected[i]
		fa := byID[a.ID].Facets

		for facet, want := range fa {
			if have := facts[facet]; have != "" && have != want {
				c := Conflict{Facet: facet, Skills: []string{a.Name}, Values: []string{want}, Project: have}

				if a.Origin == OriginAutomatic {
					drop[a.ID] = true
					c.Resolution = "csm left " + a.Name + " out because the project's setup comes first"
				} else {
					c.Resolution = "the agent is told to follow the project where they differ"
				}

				res.Conflicts = append(res.Conflicts, c)
			}
		}

		for j := i + 1; j < len(res.Selected); j++ {
			b := res.Selected[j]

			for facet, wa := range fa {
				wb := byID[b.ID].Facets[facet]

				if wb == "" || wb == wa || drop[a.ID] || drop[b.ID] {
					continue
				}

				c := Conflict{Facet: facet, Skills: []string{a.Name, b.Name}, Values: []string{wa, wb}}
				ra, rb := originRank(a.Origin), originRank(b.Origin)

				switch {
				case ra < rb:
					drop[b.ID] = true
					c.Resolution = fmt.Sprintf("kept %s (%s); left out %s (%s)", a.Name, a.Origin, b.Name, b.Origin)
				case rb < ra:
					drop[a.ID] = true
					c.Resolution = fmt.Sprintf("kept %s (%s); left out %s (%s)", b.Name, b.Origin, a.Name, a.Origin)
				case a.Origin == OriginAutomatic:
					loser := b

					if b.Score > a.Score {
						loser = a
					}

					drop[loser.ID] = true
					c.Resolution = "left out the weaker automatic match, " + loser.Name
				default:
					c.NeedsDecision = true
					c.Resolution = "choose one of them for this task"
				}

				res.Conflicts = append(res.Conflicts, c)
			}
		}
	}

	if len(drop) == 0 {
		return
	}

	kept := res.Selected[:0]

	for _, c := range res.Selected {
		if drop[c.ID] {
			res.Skipped = append(res.Skipped, Skip{ID: c.ID, Name: c.Name, Reason: "conflicts with a higher-priority instruction"})

			continue
		}

		kept = append(kept, c)
	}

	res.Selected = kept
}

// Facts are settings a project shows in its files: which package manager, formatter, test runner and deploy target.
type Facts map[string]string

// directive matches a sentence that tells the agent to use a tool, not one that only shows it in an example.
func directive(tool string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b(always use|must use|only use|use|prefer|switch to|stick to)\s+` + tool + `\b|\b` + tool + `\s+only\b`)
}

var facetPatterns = map[string]map[string]*regexp.Regexp{
	"package manager": {"npm": directive("npm"), "pnpm": directive("pnpm"), "yarn": directive("yarn"), "bun": directive("bun")},
	"formatter":       {"prettier": directive("prettier"), "biome": directive("biome"), "dprint": directive("dprint")},
	"test runner":     {"jest": directive("jest"), "vitest": directive("vitest"), "mocha": directive("mocha")},
	"deploy target": {
		"vercel":     regexp.MustCompile(`(?i)\bdeploy\s+(it\s+)?to\s+vercel\b|\bvercel\s+deploy\b`),
		"netlify":    regexp.MustCompile(`(?i)\bdeploy\s+(it\s+)?to\s+netlify\b|\bnetlify\s+deploy\b`),
		"fly.io":     regexp.MustCompile(`(?i)\bdeploy\s+(it\s+)?to\s+fly(\.io)?\b|\bfly(ctl)?\s+deploy\b`),
		"cloudflare": regexp.MustCompile(`(?i)\bdeploy\s+(it\s+)?to\s+cloudflare\b|\bwrangler\s+(deploy|publish)\b`),
	},
}

// FacetsOf finds what a skill insists on: a facet counts only when its text tells the agent to use exactly one choice
// for it; commands shown as examples do not count.
func FacetsOf(body string) map[string]string {
	out := map[string]string{}

	for facet, values := range facetPatterns {
		var hits []string

		for value, re := range values {
			if re.MatchString(body) {
				hits = append(hits, value)
			}
		}

		if len(hits) == 1 {
			out[facet] = hits[0]
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// ProjectFacts reads a project's choices from its lockfiles and config files.
func ProjectFacts(dir string) Facts {
	f := Facts{}
	has := func(names ...string) bool {
		for _, n := range names {
			if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
				return true
			}
		}

		return false
	}

	switch {
	case has("pnpm-lock.yaml"):
		f["package manager"] = "pnpm"
	case has("yarn.lock"):
		f["package manager"] = "yarn"
	case has("bun.lockb", "bun.lock"):
		f["package manager"] = "bun"
	case has("package-lock.json"):
		f["package manager"] = "npm"
	}

	switch {
	case has("biome.json", "biome.jsonc"):
		f["formatter"] = "biome"
	case has(".prettierrc", ".prettierrc.json", ".prettierrc.js", ".prettierrc.cjs", ".prettierrc.mjs", ".prettierrc.yaml", ".prettierrc.yml", "prettier.config.js", "prettier.config.mjs", "prettier.config.cjs"):
		f["formatter"] = "prettier"
	case has("dprint.json", ".dprint.json"):
		f["formatter"] = "dprint"
	}

	switch {
	case has("vercel.json"):
		f["deploy target"] = "vercel"
	case has("netlify.toml"):
		f["deploy target"] = "netlify"
	case has("fly.toml"):
		f["deploy target"] = "fly.io"
	case has("wrangler.toml", "wrangler.json", "wrangler.jsonc"):
		f["deploy target"] = "cloudflare"
	}

	if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		var pkg struct {
			Dev  map[string]any `json:"devDependencies"`
			Deps map[string]any `json:"dependencies"`
		}

		if json.Unmarshal(data, &pkg) == nil {
			var runners []string

			for _, name := range []string{"jest", "vitest", "mocha"} {
				_, dev := pkg.Dev[name]
				_, dep := pkg.Deps[name]

				if dev || dep {
					runners = append(runners, name)
				}
			}

			if len(runners) == 1 {
				f["test runner"] = runners[0]
			}
		}
	}

	return f
}
