package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/onoja123/csm/internal/skills"
)

// SkillsAgent returns what csm verified about how an agent finds and runs Agent Skills. Directories are in the
// order the agent reads them; the first of each list is where csm places copies.
func SkillsAgent(id string) skills.Agent {
	switch id {
	case ClaudeID:
		return skills.Agent{
			Provider: ClaudeID, Name: "Claude Code", DefaultProfile: ".claude",
			UserDirs:    []string{"skills"},
			ProjectDirs: []string{".claude/skills"},
			Fields: []string{"when_to_use", "argument-hint", "arguments", "disable-model-invocation", "user-invocable", "allowed-tools",
				"disallowed-tools", "model", "effort", "context", "agent", "background", "hooks", "paths", "shell"},
			Variables:  []string{"$ARGUMENTS", "${CLAUDE_SKILL_DIR}", "${CLAUDE_SESSION_ID}", "${CLAUDE_PROJECT_DIR}", "${CLAUDE_EFFORT}", "${CLAUDE_PLUGIN_ROOT}", "${CLAUDE_PLUGIN_DATA}", "!`command`"},
			Invoke:     "/%s",
			Request:    `load it with your Skill tool (skill: "%s")`,
			AutoSelect: true, Scripts: true,
			Evidence: "Skill tool calls and /skill commands in the session transcript",
			Verified: "Claude Code 2.1.299: binary, documentation and scratch-profile runs (2026-10-14)",
			Notes:    "Reads $CLAUDE_CONFIG_DIR/skills and .claude/skills from the working folder up to the repository root, not ~/.agents/skills. The folder name is the slash command.",
		}
	case CodexID:
		return skills.Agent{
			Provider: CodexID, Name: "Codex", DefaultProfile: ".codex",
			UserDirs:    []string{"skills"},
			HomeDirs:    []string{".agents/skills"},
			ProjectDirs: []string{".agents/skills", ".codex/skills"},
			Invoke:      "$%s",
			Request:     "$%s",
			AutoSelect:  true, Scripts: true,
			Evidence: "explicit $skill injections and shell reads of SKILL.md in the session rollout",
			Verified: "Codex 0.162.1: source at rust-v0.162.1 and scratch-profile runs of codex debug prompt-input (2026-10-14)",
			Notes:    "Reads $CODEX_HOME/skills (deprecated, still read) and the real ~/.agents/skills whatever CODEX_HOME is, plus .agents/skills from the working folder up to the repository root and .codex/skills. Only name and description are read from the frontmatter.",
		}
	case GeminiID:
		return skills.Agent{
			Provider: GeminiID, Name: "Gemini CLI", DefaultProfile: ".",
			UserDirs:    []string{".gemini/skills", ".agents/skills"},
			ProjectDirs: []string{".gemini/skills", ".agents/skills"},
			Invoke:      "/%s",
			Request:     `activate it with the activate_skill tool (name: "%s")`,
			AutoSelect:  true, Scripts: true,
			Evidence:            "activate_skill tool calls in the chat record",
			TrustedProjectsOnly: true,
			Verified:            "Gemini CLI 0.64.0: bundle and scratch-profile runs of gemini skills list (2026-10-14)",
			Notes:               "Reads skills under GEMINI_CLI_HOME, and project skills only in the working folder itself and only when that folder is trusted. It asks before activating a skill that is not built in.",
		}
	case CopilotID:
		return skills.Agent{
			Provider: CopilotID, Name: "GitHub Copilot CLI", DefaultProfile: ".copilot",
			UserDirs:    []string{"skills"},
			ProjectDirs: []string{".github/skills", ".agents/skills", ".claude/skills"},
			Fields:      []string{"allowed-tools", "user-invocable", "disable-model-invocation", "argument-hint"},
			Invoke:      "/%s",
			Request:     `load it with your skill tool (skill: "%s")`,
			AutoSelect:  true, Scripts: true,
			Evidence: "skill.invoked events in the session record",
			Verified: "GitHub Copilot CLI 1.0.97: package, event schema and scratch-profile runs of copilot skill list (2026-10-14)",
			Notes:    "Reads $COPILOT_HOME/skills, folders named in COPILOT_SKILLS_DIRS, and .github, .agents and .claude skills from the working folder up to the repository root. It reads ~/.agents/skills only with its default home.",
		}
	case CursorID:
		return skills.Agent{
			Provider: CursorID, Name: "Cursor CLI", DefaultProfile: ".cursor",
			HomeDirs:    []string{".cursor/skills", ".agents/skills", ".claude/skills", ".codex/skills"},
			ProjectDirs: []string{".cursor/skills", ".agents/skills", ".claude/skills", ".codex/skills"},
			Fields:      []string{"disable-model-invocation", "user-invocable", "paths", "globs", "alwaysApply"},
			Invoke:      "/%s",
			Surface:     "cli",
			AutoSelect:  true, Scripts: true,
			Verified: "Cursor CLI 2026.10.14: bundle and documentation; csm could not list its skills without a model call (2026-10-14)",
			Notes:    "Reads skills from your real home whatever CURSOR_CONFIG_DIR is, including ~/.claude/skills and ~/.agents/skills. csm cannot read Cursor sessions, so use of a skill is never verified.",
		}
	}

	return skills.Agent{Provider: id, Name: id}
}

// SkillEvidenceReader reads, from an agent's own session record, which skills it loaded.
type SkillEvidenceReader interface {
	SkillEvidence(profileDir, cwd, sessionID string) ([]skills.Evidence, error)
}

func SkillEvidence(p Provider) (SkillEvidenceReader, bool) {
	r, ok := p.(SkillEvidenceReader)

	return r, ok
}

func (c Claude) SkillEvidence(profileDir, cwd, sessionID string) ([]skills.Evidence, error) {
	path, err := claudeTranscriptPath(profileDir, sessionID)
	if err != nil {
		return nil, err
	}

	return scanLines(path, func(line []byte) []skills.Evidence {
		var rec struct {
			Type      string    `json:"type"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}

		if json.Unmarshal(line, &rec) != nil {
			return nil
		}

		var out []skills.Evidence
		blocks, text := claudeBlocks(rec.Message.Content)

		if rec.Type == "assistant" {
			for _, b := range blocks {
				var in struct {
					Skill string `json:"skill"`
				}

				if b.Type == "tool_use" && b.Name == "Skill" && json.Unmarshal(b.Input, &in) == nil && in.Skill != "" {
					out = append(out, skills.Evidence{Skill: skillBase(in.Skill), Provider: ClaudeID, Session: sessionID, Kind: "Skill tool call", At: rec.Timestamp})
				}
			}
		}

		if rec.Type == "user" {
			for _, m := range commandName.FindAllStringSubmatch(text, -1) {
				out = append(out, skills.Evidence{Skill: skillBase(m[1]), Provider: ClaudeID, Session: sessionID, Kind: "/" + m[1] + " command", At: rec.Timestamp})
			}
		}

		return out
	})
}

var (
	commandName  = regexp.MustCompile(`<command-name>/([A-Za-z0-9_.:-]+)</command-name>`)
	skillBlock   = regexp.MustCompile(`<skill>\s*<name>([^<]+)</name>`)
	skillFileRef = regexp.MustCompile(`([^\s"'\\]+)/SKILL\.md`)
)

// skillBase drops a plugin prefix such as "plugin:name".
func skillBase(name string) string {
	if i := strings.LastIndex(name, ":"); i >= 0 {
		return name[i+1:]
	}

	return name
}

func (c Codex) SkillEvidence(profileDir, cwd, sessionID string) ([]skills.Evidence, error) {
	if sessionID == "" || sessionID != filepath.Base(sessionID) {
		return nil, errors.New("unexpected Codex session ID")
	}

	matches, _ := filepath.Glob(filepath.Join(profileDir, "sessions", "*", "*", "*", "rollout-*"+sessionID+".jsonl"))

	if len(matches) == 0 {
		return nil, os.ErrNotExist
	}

	return scanLines(matches[0], func(line []byte) []skills.Evidence {
		var rec struct {
			Timestamp time.Time `json:"timestamp"`
			Type      string    `json:"type"`
			Payload   struct {
				Type      string          `json:"type"`
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				Arguments string          `json:"arguments"`
				Input     string          `json:"input"`
				Action    json.RawMessage `json:"action"`
			} `json:"payload"`
		}

		if json.Unmarshal(line, &rec) != nil || rec.Type != "response_item" {
			return nil
		}

		var out []skills.Evidence
		p := rec.Payload

		switch {
		case p.Type == "message" && p.Role == "user":
			for _, m := range skillBlock.FindAllStringSubmatch(textOf(p.Content), -1) {
				out = append(out, skills.Evidence{Skill: strings.TrimSpace(m[1]), Provider: CodexID, Session: sessionID, Kind: "$skill instructions injected", At: rec.Timestamp})
			}
		case strings.Contains(p.Type, "call"):
			for _, m := range skillFileRef.FindAllStringSubmatch(p.Arguments+p.Input+string(p.Action), -1) {
				out = append(out, skills.Evidence{Skill: filepath.Base(m[1]), Provider: CodexID, Session: sessionID, Kind: "read SKILL.md with a shell command", At: rec.Timestamp})
			}
		}

		return out
	})
}

func (g Gemini) SkillEvidence(profileDir, cwd, sessionID string) ([]skills.Evidence, error) {
	sessions, err := geminiSessions(profileDir)
	if err != nil {
		return nil, err
	}

	for _, s := range sessions {
		if s.meta.SessionID != sessionID {
			continue
		}

		return scanLines(s.path, func(line []byte) []skills.Evidence {
			var rec struct {
				Messages  []geminiToolMessage `json:"messages"`
				ToolCalls []geminiToolCall    `json:"toolCalls"`
			}

			if json.Unmarshal(line, &rec) != nil {
				return nil
			}

			calls := rec.ToolCalls

			for _, m := range rec.Messages {
				calls = append(calls, m.ToolCalls...)
			}

			var out []skills.Evidence

			for _, call := range calls {
				var args struct {
					Name string `json:"name"`
				}

				if call.Name == "activate_skill" && call.Status != "error" && call.Status != "cancelled" && json.Unmarshal(call.Args, &args) == nil && args.Name != "" {
					out = append(out, skills.Evidence{Skill: args.Name, Provider: GeminiID, Session: sessionID, Kind: "activate_skill call", At: call.Timestamp})
				}
			}

			return out
		})
	}

	return nil, os.ErrNotExist
}

type geminiToolCall struct {
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args"`
	Status    string          `json:"status"`
	Timestamp time.Time       `json:"timestamp"`
}

type geminiToolMessage struct {
	ToolCalls []geminiToolCall `json:"toolCalls"`
}

func (c Copilot) SkillEvidence(profileDir, cwd, sessionID string) ([]skills.Evidence, error) {
	if sessionID == "" || sessionID != filepath.Base(sessionID) {
		return nil, errors.New("unexpected Copilot CLI session ID")
	}

	return scanLines(filepath.Join(copilotSessionDir(profileDir, sessionID), "events.jsonl"), func(line []byte) []skills.Evidence {
		var rec struct {
			Type      string    `json:"type"`
			Timestamp time.Time `json:"timestamp"`
			Data      struct {
				Name    string `json:"name"`
				Trigger string `json:"trigger"`
			} `json:"data"`
		}

		if json.Unmarshal(line, &rec) != nil || (rec.Type != "skill.invoked" && rec.Type != "skill.invoked_ref") || rec.Data.Name == "" {
			return nil
		}

		kind := "skill invoked"

		if rec.Data.Trigger != "" {
			kind += " (" + rec.Data.Trigger + ")"
		}

		return []skills.Evidence{{Skill: rec.Data.Name, Provider: CopilotID, Session: sessionID, Kind: kind, At: rec.Timestamp}}
	})
}

// scanLines reads a JSON-lines file, or one JSON document, and collects what fn finds in each record.
func scanLines(path string, fn func([]byte) []skills.Evidence) ([]skills.Evidence, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	defer f.Close()

	var out []skills.Evidence

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)

	for sc.Scan() {
		out = append(out, fn(sc.Bytes())...)
	}

	if err := sc.Err(); err != nil {
		return out, err
	}

	if len(out) == 0 {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 && data[0] == '{' && !strings.Contains(string(data[:min(len(data), 4096)]), "\n{") {
			out = fn(data)
		}
	}

	return dedupeEvidence(out), nil
}

func dedupeEvidence(in []skills.Evidence) []skills.Evidence {
	seen := map[string]bool{}
	var out []skills.Evidence

	for _, e := range in {
		key := e.Skill + "\x00" + e.Kind + "\x00" + e.At.String()

		if !seen[key] {
			seen[key] = true
			out = append(out, e)
		}
	}

	return out
}

// SkillLister asks an agent which skills it can load, using a command that makes no model request.
type SkillLister interface {
	SkillListing(ctx context.Context, profileDir, cwd string) (string, error)
}

func SkillListing(p Provider) (SkillLister, bool) {
	l, ok := p.(SkillLister)

	return l, ok
}

func runListing(ctx context.Context, p Provider, profileDir, cwd string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.Executable(), args...)
	cmd.Dir = cwd
	cmd.Env = p.Env(profileDir)
	cmd.Stdin = nil
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()

	if ctx.Err() != nil {
		return "", fmt.Errorf("%s gave no answer within %s", p.Name(), probeTimeout)
	}

	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w", p.Name(), strings.Join(args[:min(len(args), 2)], " "), err)
	}

	return string(out), nil
}

// SkillListing runs a local /cost turn, which Claude Code answers without the API, and keeps the skill list from its
// start-up record.
func (c Claude) SkillListing(ctx context.Context, profileDir, cwd string) (string, error) {
	out, err := runListing(ctx, c, profileDir, cwd, "-p", "--no-session-persistence", "--output-format", "stream-json", "--verbose", "/cost")
	if err != nil {
		return "", err
	}

	for _, line := range strings.Split(out, "\n") {
		var rec struct {
			Type    string          `json:"type"`
			Subtype string          `json:"subtype"`
			Skills  json.RawMessage `json:"skills"`
		}

		if json.Unmarshal([]byte(line), &rec) == nil && rec.Type == "system" && rec.Subtype == "init" {
			return string(rec.Skills), nil
		}
	}

	return "", errors.New("Claude Code did not report its skills")
}

func (c Codex) SkillListing(ctx context.Context, profileDir, cwd string) (string, error) {
	out, err := runListing(ctx, c, profileDir, cwd, "debug", "prompt-input", "x")
	if err != nil {
		return "", err
	}

	if i := strings.Index(out, "<skills_instructions>"); i >= 0 {
		end := strings.Index(out[i:], "</skills_instructions>")

		if end > 0 {
			return out[i : i+end], nil
		}

		return out[i:], nil
	}

	return "", nil
}

func (g Gemini) SkillListing(ctx context.Context, profileDir, cwd string) (string, error) {
	return runListing(ctx, g, profileDir, cwd, "skills", "list", "--all")
}

func (c Copilot) SkillListing(ctx context.Context, profileDir, cwd string) (string, error) {
	return runListing(ctx, c, profileDir, cwd, "skill", "list", "--json")
}

// ListingHas reports whether a listing names a skill as a whole word.
func ListingHas(listing, name string) bool {
	re := regexp.MustCompile(`(^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(name) + `($|[^A-Za-z0-9_-])`)

	return re.MatchString(listing)
}

// FolderTruster reports whether an agent trusts a folder enough to load its project skills.
type FolderTruster interface {
	FolderTrusted(profileDir, dir string) bool
}

// TrustsFolder is true for agents that load project skills anywhere, and the agent's own answer otherwise.
func TrustsFolder(p Provider, profileDir, dir string) bool {
	if t, ok := p.(FolderTruster); ok {
		return t.FolderTrusted(profileDir, dir)
	}

	return true
}

// FolderTrusted follows Gemini CLI's own rules: its environment overrides, the security.folderTrust.enabled setting,
// then the longest matching rule in trustedFolders.json. csm only reads them; it never trusts a folder itself.
func (g Gemini) FolderTrusted(profileDir, dir string) bool {
	switch {
	case os.Getenv("GEMINI_RESTRICTED_MODE") == "true" || os.Getenv("GEMINI_CLI_TRUST_WORKSPACE") == "false":
		return false
	case os.Getenv("GEMINI_CLI_TRUST_WORKSPACE") == "true":
		return true
	}

	var settings struct {
		Security struct {
			FolderTrust struct {
				Enabled *bool `json:"enabled"`
			} `json:"folderTrust"`
		} `json:"security"`
	}

	if data, err := os.ReadFile(filepath.Join(geminiDir(profileDir), "settings.json")); err == nil && json.Unmarshal(data, &settings) == nil {
		if e := settings.Security.FolderTrust.Enabled; e != nil && !*e {
			return true
		}
	}

	path := os.Getenv("GEMINI_CLI_TRUSTED_FOLDERS_PATH")

	if path == "" {
		path = filepath.Join(geminiDir(profileDir), "trustedFolders.json")
	}

	var rules map[string]string

	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &rules) != nil {
		return false
	}

	target := realPath(dir)
	best, level := -1, ""

	for rule, l := range rules {
		effective := rule

		if l == "TRUST_PARENT" {
			effective = filepath.Dir(rule)
		}

		if within := realPath(effective); (target == within || strings.HasPrefix(target, within+string(filepath.Separator))) && len(rule) > best {
			best, level = len(rule), l
		}
	}

	return level == "TRUST_FOLDER" || level == "TRUST_PARENT"
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}

	return filepath.Clean(p)
}
