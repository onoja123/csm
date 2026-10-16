package skills

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// AdapterVersion changes whenever Adapt starts transforming skills differently, so receipts can say which rules applied.
const AdapterVersion = "1"

// Agent is what csm verified about how one coding agent finds and runs skills.
type Agent struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	// UserDirs are skill folders relative to the agent's profile directory (CLAUDE_CONFIG_DIR, CODEX_HOME, ...).
	UserDirs []string `json:"user_dirs,omitempty"`
	// HomeDirs are skill folders relative to the user's home that the agent reads whatever profile it runs with.
	HomeDirs []string `json:"home_dirs,omitempty"`
	// DefaultProfile is the profile folder, relative to the home, the agent uses outside csm ("." for the home itself).
	DefaultProfile string `json:"default_profile,omitempty"`
	// ProjectDirs are skill folders relative to the project root.
	ProjectDirs []string `json:"project_dirs,omitempty"`
	// Fields are the frontmatter fields the agent acts on beyond name and description.
	Fields []string `json:"fields,omitempty"`
	// Variables are the placeholders the agent substitutes in SKILL.md.
	Variables []string `json:"variables,omitempty"`
	// Invoke formats how a person asks for a skill, e.g. "/%s"; empty when the agent has no syntax for it.
	Invoke string `json:"invoke,omitempty"`
	// Request formats how an instruction asks the agent to load a skill, e.g. a mention the CLI expands itself.
	Request string `json:"request,omitempty"`
	// Surface is the value of metadata.surfaces the agent requires when a skill sets it.
	Surface string `json:"surface,omitempty"`
	// TrustedProjectsOnly means the agent ignores project skills in folders the user has not trusted.
	TrustedProjectsOnly bool `json:"trusted_projects_only,omitempty"`
	// AutoSelect means the agent decides by itself, from the description, when to load a skill.
	AutoSelect bool `json:"auto_select"`
	// Scripts means the agent can run a skill's bundled scripts with its own shell tool, under its own approvals.
	Scripts bool `json:"scripts"`
	// Evidence says how csm can tell that the agent loaded a skill; empty means it cannot.
	Evidence string `json:"evidence,omitempty"`
	// Verified records what these facts were checked against; empty means nothing was checked and compatibility is unknown.
	Verified string `json:"verified,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

func (a Agent) Supported() bool { return len(a.UserDirs)+len(a.HomeDirs)+len(a.ProjectDirs) > 0 }

func (a Agent) honors(field string) bool { return slices.Contains(a.Fields, field) }

func (a Agent) substitutes(v string) bool { return slices.Contains(a.Variables, v) }

// InvokeText is how a person asks the agent for a skill, or "" when it has no syntax for that.
func (a Agent) InvokeText(name string) string {
	if a.Invoke == "" {
		return ""
	}

	return fmt.Sprintf(a.Invoke, name)
}

// RequestText is how an instruction asks the agent to load a skill, or "" when the agent has no reliable way.
func (a Agent) RequestText(name string) string {
	if a.Request == "" {
		return ""
	}

	return fmt.Sprintf(a.Request, name)
}

type Level string

const (
	Compatible  Level = "compatible"
	Adapted     Level = "adapted"
	Partial     Level = "partial"
	Unsupported Level = "unsupported"
	Unknown     Level = "unknown"
)

func (l Level) rank() int {
	switch l {
	case Compatible:
		return 0
	case Adapted:
		return 1
	case Partial:
		return 2
	case Unknown:
		return 3
	}

	return 4
}

// Usable reports whether csm can make the skill available to the agent.
func (l Level) Usable() bool { return l == Compatible || l == Adapted || l == Partial }

// Compat is the result of checking one skill against one agent; every level other than compatible says why.
type Compat struct {
	Provider    string   `json:"provider"`
	Level       Level    `json:"level"`
	Reasons     []string `json:"reasons,omitempty"`
	Adaptations []string `json:"adaptations,omitempty"`
	Notes       []string `json:"notes,omitempty"`
}

func (c *Compat) raise(l Level, reason string) {
	if l.rank() > c.Level.rank() {
		c.Level = l
	}

	if reason != "" {
		c.Reasons = append(c.Reasons, reason)
	}
}

type fieldKind int

const (
	fieldCosmetic fieldKind = iota
	fieldBehavior
	fieldPermission
)

type fieldInfo struct {
	kind   fieldKind
	effect string
}

// knownFields are agent-specific frontmatter extensions whose effect csm understands.
var knownFields = map[string]fieldInfo{
	"allowed-tools":            {fieldPermission, "pre-approves tools while the skill is active"},
	"hooks":                    {fieldPermission, "runs commands on agent events while the skill is active"},
	"context":                  {fieldBehavior, "runs the skill in a separate subagent"},
	"agent":                    {fieldBehavior, "picks the subagent that runs the skill"},
	"model":                    {fieldBehavior, "switches the model while the skill runs"},
	"effort":                   {fieldBehavior, "changes the reasoning effort while the skill runs"},
	"disable-model-invocation": {fieldBehavior, "keeps the agent from loading it on its own"},
	"disallowed-tools":         {fieldBehavior, "blocks tools while the skill runs"},
	"arguments":                {fieldBehavior, "names the arguments the skill takes"},
	"background":               {fieldBehavior, "runs the skill in the background"},
	"globs":                    {fieldBehavior, "limits automatic use to matching files"},
	"alwaysApply":              {fieldBehavior, "applies the skill to every request"},
	"user-invocable":           {fieldBehavior, "controls whether the user can invoke it by name"},
	"paths":                    {fieldBehavior, "limits automatic use to matching files"},
	"shell":                    {fieldBehavior, "chooses the shell for its commands"},
	"argument-hint":            {fieldCosmetic, "shows a hint for arguments"},
	"when_to_use":              {fieldCosmetic, "adds guidance on when to use it"},
	"version":                  {fieldCosmetic, "records a version"},
}

var (
	skillDirVar    = "${CLAUDE_SKILL_DIR}"
	projectDirVar  = "${CLAUDE_PROJECT_DIR}"
	argumentsVar   = regexp.MustCompile(`\$ARGUMENTS\b`)
	preprocessExpr = regexp.MustCompile("!`[^`\n]+`")
	// sessionVars only have a value inside a running Claude Code session, so no copy can carry them.
	sessionVars = []string{"${CLAUDE_SESSION_ID}", "${CLAUDE_EFFORT}", "${CLAUDE_PLUGIN_ROOT}", "${CLAUDE_PLUGIN_DATA}"}
)

// bodyFeatures lists what in a SKILL.md body only some agents understand.
func bodyFeatures(body string) []string {
	var out []string

	for _, v := range append([]string{skillDirVar, projectDirVar}, sessionVars...) {
		if strings.Contains(body, v) {
			out = append(out, v)
		}
	}

	if argumentsVar.MatchString(body) {
		out = append(out, "$ARGUMENTS")
	}

	if preprocessExpr.MatchString(body) {
		out = append(out, "!`command`")
	}

	return out
}

func (s *Skill) uses(feature string) bool {
	if s.Body != "" {
		return slices.Contains(bodyFeatures(s.Body), feature)
	}

	return slices.Contains(s.Features, feature)
}

// AssessOptions describe the copy being judged.
type AssessOptions struct {
	PermissionsApproved bool
	// Handoff means the copy is for an agent taking over a task; it never carries tool pre-approval, hooks or
	// commands that run on load.
	Handoff bool
	// ProjectDir is set when the copy belongs to one project, so ${CLAUDE_PROJECT_DIR} has a fixed value.
	ProjectDir string
}

// Assess checks a skill against what an agent supports. It reads only the skill; Adapt makes the changes it names.
func Assess(s *Skill, a Agent, permissionsApproved bool) Compat {
	return AssessWith(s, a, AssessOptions{PermissionsApproved: permissionsApproved})
}

func AssessWith(s *Skill, a Agent, opts AssessOptions) Compat {
	c := Compat{Provider: a.Provider, Level: Compatible}
	permissionsApproved := opts.PermissionsApproved && !opts.Handoff

	switch {
	case !a.Supported():
		c.raise(Unsupported, a.Name+" does not load Agent Skills")

		return c
	case a.Verified == "":
		c.raise(Unknown, "csm has not verified how "+a.Name+" loads skills")
	}

	extra := make([]string, 0, len(s.Extra)+1)

	for k := range s.Extra {
		extra = append(extra, k)
	}

	if len(s.AllowedTools) > 0 {
		extra = append(extra, "allowed-tools")
	}

	sort.Strings(extra)

	if surfaces := s.Metadata["surfaces"]; a.Surface != "" && surfaces != "" && !strings.Contains(surfaces, a.Surface) {
		c.raise(Unsupported, fmt.Sprintf("its metadata.surfaces (%s) leaves out %s, so %s skips it", surfaces, a.Surface, a.Name))
	}

	if _, hooks := s.Extra["hooks"]; opts.Handoff && hooks && a.honors("hooks") {
		c.raise(Unsupported, "it defines hooks, which "+a.Name+" would run on its own; csm never activates hooks in a handoff")
	}

	if opts.Handoff && s.uses("!`command`") && a.substitutes("!`command`") {
		c.raise(Unsupported, "it has !`command` lines "+a.Name+" would run on load; csm never carries that in a handoff")
	}

	for _, field := range extra {
		info, known := knownFields[field]

		switch {
		case a.honors(field) && info.kind == fieldPermission && opts.Handoff:
			c.raise(Adapted, "")
			c.Adaptations = append(c.Adaptations, fmt.Sprintf("%s removed: a handoff never carries it (it %s)", field, info.effect))
		case a.honors(field) && info.kind == fieldPermission && !permissionsApproved:
			c.raise(Adapted, "")
			c.Adaptations = append(c.Adaptations, fmt.Sprintf("%s removed until you approve it (it %s)", field, info.effect))
		case a.honors(field):
		case !known:
			c.Notes = append(c.Notes, fmt.Sprintf("%s ignores the field %q", a.Name, field))
		case info.kind == fieldBehavior:
			c.raise(Partial, fmt.Sprintf("%s ignores %s, which %s", a.Name, field, info.effect))
		case info.kind == fieldPermission:
			c.Notes = append(c.Notes, fmt.Sprintf("%s ignores %s, so its own approval rules apply", a.Name, field))
		default:
			c.Notes = append(c.Notes, fmt.Sprintf("%s ignores %s", a.Name, field))
		}
	}

	if s.uses(skillDirVar) && !a.substitutes(skillDirVar) {
		c.raise(Adapted, "")
		c.Adaptations = append(c.Adaptations, skillDirVar+" replaced with the skill's installed folder")
	}

	if s.uses(projectDirVar) && !a.substitutes(projectDirVar) {
		if opts.ProjectDir != "" {
			c.raise(Adapted, "")
			c.Adaptations = append(c.Adaptations, projectDirVar+" replaced with the project folder")
		} else {
			c.raise(Partial, projectDirVar+" stays literal in a copy shared by every project")
		}
	}

	for _, v := range sessionVars {
		if s.uses(v) && !a.substitutes(v) {
			c.raise(Partial, a.Name+" has no value for "+v+", so it stays literal")
		}
	}

	if s.uses("$ARGUMENTS") && !a.substitutes("$ARGUMENTS") {
		c.raise(Partial, a.Name+" does not pass arguments into skills, so $ARGUMENTS stays literal; put the details in the request")
	}

	if s.uses("!`command`") && !a.substitutes("!`command`") {
		c.raise(Partial, a.Name+" does not run !`command` lines before loading a skill; it sees the command text instead of its output")
	}

	if hasScripts(s) && !a.Scripts {
		c.raise(Partial, a.Name+" cannot run the skill's bundled scripts")
	}

	return c
}

func hasScripts(s *Skill) bool {
	for _, f := range s.Files {
		if f.Executable || scriptExt[strings.ToLower(pathExt(f.Path))] {
			return true
		}
	}

	return false
}

func pathExt(p string) string {
	if i := strings.LastIndex(p, "."); i > strings.LastIndex(p, "/") {
		return p[i:]
	}

	return ""
}

// AdaptOptions describe the place a projection is written to.
type AdaptOptions struct {
	// Dir is the absolute folder the projection will live in.
	Dir                 string
	PermissionsApproved bool
	Handoff             bool
	ProjectDir          string
}

// Adapt returns the files to write for an agent: the original bytes, or a copy with the documented changes Assess
// reported. The result is parsed again so a change can never produce a skill the agent would reject.
func Adapt(s *Skill, contents map[string][]byte, a Agent, opts AdaptOptions) (map[string][]byte, []string, error) {
	c := AssessWith(s, a, AssessOptions{PermissionsApproved: opts.PermissionsApproved, Handoff: opts.Handoff, ProjectDir: opts.ProjectDir})

	if !c.Level.Usable() && c.Level != Unknown {
		return nil, nil, fmt.Errorf("%s: %s", a.Name, strings.Join(c.Reasons, "; "))
	}

	if len(c.Adaptations) == 0 {
		return contents, nil, nil
	}

	manifest := contents[ManifestFile]
	front, body, ok := splitFrontmatter(strings.TrimPrefix(strings.ReplaceAll(string(manifest), "\r\n", "\n"), "\ufeff"))

	if !ok {
		return nil, nil, fmt.Errorf("%s has no frontmatter", ManifestFile)
	}

	var drop []string

	for _, field := range []string{"allowed-tools", "hooks"} {
		if a.honors(field) && (!opts.PermissionsApproved || opts.Handoff) {
			drop = append(drop, field)
		}
	}

	if len(drop) > 0 {
		rewritten, err := dropFields(front, drop)
		if err != nil {
			return nil, nil, err
		}

		front = rewritten
	}

	if strings.Contains(body, skillDirVar) && !a.substitutes(skillDirVar) {
		if opts.Dir == "" {
			return nil, nil, fmt.Errorf("cannot replace %s without the skill's folder", skillDirVar)
		}

		body = strings.ReplaceAll(body, skillDirVar, opts.Dir)
	}

	if strings.Contains(body, projectDirVar) && !a.substitutes(projectDirVar) && opts.ProjectDir != "" {
		body = strings.ReplaceAll(body, projectDirVar, opts.ProjectDir)
	}

	out := make(map[string][]byte, len(contents))

	for k, v := range contents {
		out[k] = v
	}

	out[ManifestFile] = []byte("---\n" + strings.TrimRight(front, "\n") + "\n---\n" + body)

	check, _, err := FromFiles(s.Name, out, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("the adapted skill is not valid: %w", err)
	}

	if check.Name != s.Name || check.Description != s.Description {
		return nil, nil, fmt.Errorf("adapting the skill changed its name or description")
	}

	for _, field := range drop {
		if _, still := check.Extra[field]; still || field == "allowed-tools" && len(check.AllowedTools) > 0 {
			return nil, nil, fmt.Errorf("could not remove %s", field)
		}
	}

	return out, c.Adaptations, nil
}

// dropFields removes top-level keys from YAML frontmatter, keeping everything else, comments included.
func dropFields(front string, fields []string) (string, error) {
	var doc yaml.Node

	if err := yaml.Unmarshal([]byte(front), &doc); err != nil {
		return "", err
	}

	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("the frontmatter is not a mapping")
	}

	m := doc.Content[0]
	kept := m.Content[:0]
	removed := false

	for i := 0; i+1 < len(m.Content); i += 2 {
		if slices.Contains(fields, m.Content[i].Value) {
			removed = true

			continue
		}

		kept = append(kept, m.Content[i], m.Content[i+1])
	}

	if !removed {
		return front, nil
	}

	m.Content = kept

	var buf bytes.Buffer

	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)

	if err := enc.Encode(&doc); err != nil {
		return "", err
	}

	enc.Close()

	return buf.String(), nil
}
