// Package mcp moves MCP server configuration between agent profiles; a Server keeps variable and header names, never their values.
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

type Transport string

const (
	Stdio     Transport = "stdio"
	HTTP      Transport = "http"
	SSE       Transport = "sse"
	WebSocket Transport = "ws"
)

type State string

const (
	Stateless    State = "stateless"
	Stateful     State = "stateful"
	StateUnknown State = "unknown"
)

type Status string

const (
	Migrated     Status = "migrated"
	Available    Status = "available"
	RequiresAuth Status = "requires_auth"
	Unsupported  Status = "unsupported"
	Failed       Status = "failed"
	Unknown      Status = "unknown"
)

const (
	ScopeUser    = "user"
	ScopeLocal   = "local"
	ScopeProject = "project"
)

type Server struct {
	Name        string          `json:"name"`
	Transport   Transport       `json:"transport"`
	Command     string          `json:"command,omitempty"`
	Args        []string        `json:"args,omitempty"`
	URL         string          `json:"url,omitempty"`
	EnvNames    []string        `json:"env,omitempty"`
	HeaderNames []string        `json:"headers,omitempty"`
	EnvRefs     []string        `json:"env_refs,omitempty"`
	Provider    string          `json:"provider"`
	Scope       string          `json:"scope"`
	Source      string          `json:"source"`
	Approved    bool            `json:"approved,omitempty"`
	Disabled    bool            `json:"disabled,omitempty"`
	OAuth       bool            `json:"oauth,omitempty"`
	SecretArgs  bool            `json:"secret_args,omitempty"`
	State       State           `json:"state"`
	Native      json.RawMessage `json:"-"`
}

type Snapshot struct {
	Version    int       `json:"version"`
	Provider   string    `json:"provider"`
	ProfileDir string    `json:"profile_dir"`
	ProjectDir string    `json:"project_dir"`
	TakenAt    time.Time `json:"taken_at"`
	Servers    []Server  `json:"servers"`
}

type Result struct {
	Server       string `json:"server"`
	Status       Status `json:"status"`
	Reason       string `json:"reason,omitempty"`
	RequiresAuth bool   `json:"requires_auth,omitempty"`
	Stateful     bool   `json:"stateful,omitempty"`
}

type Report struct {
	Version   int       `json:"version"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	DryRun    bool      `json:"dry_run,omitempty"`
	Results   []Result  `json:"results"`
	CreatedAt time.Time `json:"created_at"`
}

// EnvPresent reports whether an environment variable is set. It deliberately returns no value.
type EnvPresent func(name string) bool

type Adapter interface {
	Provider() string
	Parse(profileDir, projectDir string) ([]Server, error)
	Validate(s Server) error
	// Prepare writes a same-provider server natively and rebuilds any other from its portable fields, with no secret values.
	Prepare(profileDir, projectDir string, servers []Server) error
	// EnvByName reports whether the agent can take a server's environment variables by name from its own environment, so no value has to be written.
	EnvByName() bool
}

var secretNamePattern = regexp.MustCompile(`(?i)(TOKEN|SECRET|PASS|CRED|AUTH|COOKIE|API[_-]?KEY|ACCESS[_-]?KEY|PRIVATE|DSN|DATABASE_URL|CONNECTION)`)

func SecretName(name string) bool {
	return secretNamePattern.MatchString(name)
}

const redacted = "<redacted>"

var (
	userinfoPattern   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://[^/@\s]+:[^/@\s]+@`)
	secretFlagPattern = regexp.MustCompile(`(?i)^--?[A-Za-z0-9-]*(token|secret|password|passwd|api-?key|access-?key|auth|credential)[A-Za-z0-9-]*$`)
)

// RedactArgs hides URLs with a password, values of flags such as --token, and NAME=value pairs with a secret-looking name.
func RedactArgs(args []string) ([]string, bool) {
	out := make([]string, len(args))
	hidden := false
	hide := func(i int, v string) {
		out[i] = v
		hidden = true
	}

	for i, a := range args {
		out[i] = a
		name, value, isPair := strings.Cut(a, "=")

		switch {
		case i > 0 && secretFlagPattern.MatchString(args[i-1]):
			hide(i, redacted)
		case userinfoPattern.MatchString(a):
			hide(i, redacted)
		case isPair && secretFlagPattern.MatchString(name) && value != "":
			hide(i, name+"="+redacted)
		case isPair && !strings.HasPrefix(name, "-") && SecretName(name) && value != "" && !strings.ContainsAny(name, " /"):
			hide(i, name+"="+redacted)
		}
	}

	return out, hidden
}

var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-[^}]*)?\}`)

func EnvRefs(values ...string) []string {
	var refs []string

	for _, v := range values {
		for _, m := range envRefPattern.FindAllStringSubmatch(v, -1) {
			if m[2] == "" && !slices.Contains(refs, m[1]) {
				refs = append(refs, m[1])
			}
		}
	}

	sort.Strings(refs)

	return refs
}

var (
	localStorePattern = regexp.MustCompile(`(?i)\.(db|sqlite3?|duckdb|jsonl?|ndjson|parquet|lmdb|leveldb)$|(^|[/_-])(memory|sqlite|postgres|postgresql|mysql|redis|duckdb|knowledge[_-]?graph)([/_-]|$)`)
	storeEnvPattern   = regexp.MustCompile(`(?i)(_PATH|_DIR|_FILE|DATABASE|MEMORY|STORE|DSN)`)
)

// ClassifyState guesses from the configuration alone whether a server keeps local runtime state.
func ClassifyState(s Server) State {
	if s.Transport != Stdio {
		return Stateless
	}

	for _, v := range append([]string{s.Command}, s.Args...) {
		if localStorePattern.MatchString(v) {
			return Stateful
		}
	}

	for _, name := range s.EnvNames {
		if storeEnvPattern.MatchString(name) {
			return Stateful
		}
	}

	return StateUnknown
}

func Names[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))

	for name := range m {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

func ValidName(name string) error {
	if name == "" || name != strings.TrimSpace(name) || strings.ContainsAny(name, "/\\\x00\n") || name != filepath.Base(name) {
		return fmt.Errorf("invalid MCP server name %q", name)
	}

	return nil
}

func Take(a Adapter, profileDir, projectDir string, now time.Time) (Snapshot, error) {
	servers, err := a.Parse(profileDir, projectDir)
	snap := Snapshot{Version: 1, Provider: a.Provider(), ProfileDir: profileDir, ProjectDir: projectDir, TakenAt: now, Servers: servers}

	for i := range snap.Servers {
		s := &snap.Servers[i]
		s.Provider = a.Provider()

		if s.State == "" {
			s.State = ClassifyState(*s)
		}

		s.Args, s.SecretArgs = RedactArgs(s.Args)
	}

	slices.SortStableFunc(snap.Servers, func(a, b Server) int { return strings.Compare(a.Name, b.Name) })

	return snap, err
}

func Inspect(a Adapter, profileDir, projectDir string, env EnvPresent) ([]Result, error) {
	snap, err := Take(a, profileDir, projectDir, time.Time{})
	if err != nil {
		return nil, err
	}

	results := make([]Result, 0, len(snap.Servers))

	for _, s := range snap.Servers {
		r := Result{Server: s.Name, Status: Available, Stateful: s.State == Stateful}

		switch missing := missingRefs(s, env); {
		case s.Disabled:
			r.Reason = "disabled for this project"
		case len(missing) > 0:
			r.Status, r.RequiresAuth = RequiresAuth, true
			r.Reason = "MCP requires missing environment variable: " + strings.Join(missing, ", ")
		case s.OAuth:
			r.Status, r.RequiresAuth = RequiresAuth, true
			r.Reason = "needs a login in this profile"
		}

		results = append(results, r)
	}

	return results, nil
}

func missingRefs(s Server, env EnvPresent) []string { return absent(s.EnvRefs, env) }

func absent(names []string, env EnvPresent) []string {
	var missing []string

	for _, name := range names {
		if env == nil || !env(name) {
			missing = append(missing, name)
		}
	}

	return missing
}

type Target struct {
	Adapter    Adapter
	ProfileDir string
	ProjectDir string
	Label      string
}

// Handoff never returns an error: every problem becomes a per-server result, so a switch is never blocked by MCP.
func Handoff(snap Snapshot, t Target, env EnvPresent, dryRun bool, now time.Time) Report {
	rep := Report{Version: 1, From: snap.Provider, To: t.Adapter.Provider(), DryRun: dryRun, CreatedAt: now}
	existing, err := t.Adapter.Parse(t.ProfileDir, t.ProjectDir)

	if err != nil {
		for _, s := range snap.Servers {
			rep.Results = append(rep.Results, Result{Server: s.Name, Status: Failed, Reason: "target MCP configuration is unreadable: " + err.Error(), Stateful: s.State == Stateful})
		}

		return rep
	}

	present := byName(existing)

	var pending []Server
	var pendingIdx []int

	for _, s := range snap.Servers {
		r, write := decide(s, t, present, missingRefs(s, env), env)

		if write {
			pending, pendingIdx = append(pending, s), append(pendingIdx, len(rep.Results))
		}

		rep.Results = append(rep.Results, r)
	}

	if dryRun || len(pending) == 0 {
		return rep
	}

	if err := t.Adapter.Prepare(t.ProfileDir, t.ProjectDir, pending); err != nil {
		for _, i := range pendingIdx {
			rep.Results[i].Status = Failed
			rep.Results[i].Reason = "could not write " + t.Label + " configuration: " + err.Error()
		}

		return rep
	}

	after, err := t.Adapter.Parse(t.ProfileDir, t.ProjectDir)

	if err != nil {
		for _, i := range pendingIdx {
			rep.Results[i].Status = Failed
			rep.Results[i].Reason = t.Label + " configuration is unreadable after writing: " + err.Error()
		}

		return rep
	}

	written := byName(after)

	for n, i := range pendingIdx {
		s := pending[n]
		got, ok := written[s.Name]

		switch {
		case !ok:
			rep.Results[i].Status = Failed
			rep.Results[i].Reason = "not found in " + t.Label + " after writing"

		case s.Scope == ScopeProject && s.Provider == t.Adapter.Provider() && rep.Results[i].Status == Unknown && (got.Approved || got.Disabled):
			rep.Results[i].Status = Available
			rep.Results[i].Reason = "shared through the project configuration"
		}
	}

	return rep
}

func byName(servers []Server) map[string]Server {
	m := make(map[string]Server, len(servers))

	for _, s := range servers {
		m[s.Name] = s
	}

	return m
}

func decide(s Server, t Target, present map[string]Server, missing []string, env EnvPresent) (Result, bool) {
	r := Result{Server: s.Name, Stateful: s.State == Stateful}
	sameProvider := s.Provider == t.Adapter.Provider()

	needAuth := func(reason string) {
		r.Status, r.RequiresAuth = RequiresAuth, true
		r.Reason = strings.TrimPrefix(r.Reason+"; "+reason, "; ")
	}

	write := false

	switch {
	case ValidName(s.Name) != nil:
		r.Status, r.Reason = Unsupported, "invalid server name"

	case t.Adapter.Validate(s) != nil:
		r.Status, r.Reason = Unsupported, "unsupported by "+t.Label+": "+t.Adapter.Validate(s).Error()

	case sameProvider && s.Scope == ScopeProject:
		r.Status, r.Reason = Available, "shared through the project configuration"
		existing, known := present[s.Name]

		if !known || (!existing.Approved && !existing.Disabled) {
			r.Status, r.Reason = Unknown, "shared through the project configuration; approve it in "+t.Label+" when the agent asks"
			write = s.Approved || s.Disabled
		}

	case present[s.Name].Name != "":
		r.Status, r.Reason = Available, "already configured in "+t.Label

	case sameProvider:
		r.Status = Migrated
		write = true

		if s.OAuth {
			needAuth("needs a login in " + t.Label)
		}

	case s.SecretArgs:
		needAuth("its arguments contain a credential; add it to " + t.Label + " yourself")

	default:
		r.Status = Migrated
		write = true
		missing = nil

		if t.Adapter.EnvByName() {
			missing = absent(s.EnvNames, env)

			if len(s.EnvNames) > 0 && len(missing) == 0 {
				r.Reason = "reads " + strings.Join(s.EnvNames, ", ") + " from the environment"
			}
		}

		if reason := crossProviderReason(s, t.Label, t.Adapter.EnvByName()); reason != "" {
			needAuth(reason)
		}
	}

	if len(missing) > 0 && r.Status != Unsupported {
		slices.Sort(missing)
		needAuth("MCP requires missing environment variable: " + strings.Join(slices.Compact(missing), ", "))
	}

	if r.Stateful && r.Status != Unsupported {
		r.Reason = strings.TrimPrefix(r.Reason+"; runtime state stays with the server", "; ")
	}

	return r, write
}

func crossProviderReason(s Server, target string, envByName bool) string {
	var secret, plain []string

	names := slices.Clone(s.HeaderNames)

	if !envByName {
		names = append(names, s.EnvNames...)
	}

	for _, name := range names {
		if SecretName(name) {
			secret = append(secret, name)
		} else {
			plain = append(plain, name)
		}
	}

	var parts []string

	if len(secret) > 0 {
		parts = append(parts, "set "+strings.Join(secret, ", ")+" in "+target)
	}

	if len(plain) > 0 {
		parts = append(parts, "values are not copied between agents; set "+strings.Join(plain, ", ")+" in "+target)
	}

	if s.OAuth || (s.Transport != Stdio && len(s.HeaderNames) == 0 && len(parts) == 0) {
		parts = append(parts, "may need a login in "+target)
	}

	return strings.Join(parts, "; ")
}

func (r Result) Mark() string {
	switch r.Status {
	case Migrated, Available:
		return "✓"
	case Unsupported, Failed:
		return "✗"
	default:
		return "⚠"
	}
}

func (r Result) Line() string {
	line := r.Mark() + " " + r.Server

	switch {
	case r.Status == RequiresAuth && r.Reason == "":
		line += " requires authentication"
	case r.Status == RequiresAuth:
		line += " requires authentication: " + r.Reason
	case r.Reason != "":
		line += " " + r.Reason
	}

	return line
}

// Lines renders a report for the terminal; it contains server names, variable names and reasons only.
func (rep Report) Lines() []string {
	lines := make([]string, 0, len(rep.Results))

	for _, r := range rep.Results {
		lines = append(lines, r.Line())
	}

	return lines
}

func (rep Report) Summary() string {
	if len(rep.Results) == 0 {
		return ""
	}

	var parts []string

	for _, r := range rep.Results {
		part := r.Server + " " + string(r.Status)

		if r.Reason != "" {
			part += " (" + r.Reason + ")"
		}

		parts = append(parts, part)
	}

	return "MCP handoff: " + strings.Join(parts, "; ") + "."
}

func (rep Report) Count(statuses ...Status) int {
	n := 0

	for _, r := range rep.Results {
		if slices.Contains(statuses, r.Status) {
			n++
		}
	}

	return n
}

var ErrNoMCP = errors.New("this agent has no MCP configuration csm understands")
