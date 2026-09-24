package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type Claude struct {
	Path string
}

// These override per-profile login, which would make every profile the same account.
var claudeAuthOverrideEnv = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}

func FindClaude() (Claude, error) {
	if path := os.Getenv("CSM_CLAUDE"); path != "" {
		return Claude{Path: path}, nil
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		return Claude{}, errors.New("Claude Code is not installed or not on PATH.\n\nInstall it from https://code.claude.com, then run: csm doctor")
	}
	return Claude{Path: path}, nil
}

func (c Claude) Name() string { return "Claude Code" }

func (c Claude) CheckEnv() error {
	for _, name := range claudeAuthOverrideEnv {
		if os.Getenv(name) != "" {
			return fmt.Errorf("%s is set in your environment.\n\nIt overrides per-profile login, so every csm account would use the same credentials.\nUnset it before using csm:\n\n    unset %s", name, name)
		}
	}
	return nil
}

func (c Claude) Env(profileDir string, extra ...string) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=")
	})
	env = append(env, "CLAUDE_CONFIG_DIR="+profileDir)
	return append(env, extra...)
}

func (c Claude) Version() (string, error) {
	out, err := exec.Command(c.Path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("run %s --version: %w", c.Path, err)
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(string(out)), "(Claude Code)")), nil
}

func (c Claude) Features() (Features, error) {
	out, err := exec.Command(c.Path, "--help").Output()
	if err != nil {
		return Features{}, fmt.Errorf("run %s --help: %w", c.Path, err)
	}
	help := string(out)
	return Features{
		Auth:         strings.Contains(help, "\n  auth "),
		Hooks:        strings.Contains(help, "--settings"),
		Resume:       strings.Contains(help, "--resume"),
		SessionID:    strings.Contains(help, "--session-id"),
		SystemPrompt: strings.Contains(help, "--append-system-prompt"),
		UsageProbe:   strings.Contains(help, "stream-json") && strings.Contains(help, "--tools"),
	}, nil
}

// claudeAuthStatus is the subset of `claude auth status --json` csm reads; it never contains tokens.
type claudeAuthStatus struct {
	LoggedIn        bool   `json:"loggedIn"`
	AuthMethod      string `json:"authMethod"`
	Email           string `json:"email"`
	ConfigDirectory string `json:"configDirectory"`
}

func (c Claude) Identity(profileDir string) (Identity, error) {
	cmd := exec.Command(c.Path, "auth", "status", "--json")
	cmd.Env = c.Env(profileDir)
	out, err := cmd.Output()
	var st claudeAuthStatus
	// A logged-out profile exits non-zero but still prints valid JSON.
	if jsonErr := json.Unmarshal(out, &st); jsonErr != nil {

		if err != nil {
			return Identity{}, fmt.Errorf("claude auth status failed: %w", err)
		}

		return Identity{}, fmt.Errorf("claude auth status returned unexpected output: %w", jsonErr)

	}

	return Identity{LoggedIn: st.LoggedIn, Email: st.Email, ProfileDir: st.ConfigDirectory}, nil
}

func (c Claude) LogoutCommand(profileDir string) string {
	return "CLAUDE_CONFIG_DIR=" + profileDir + " claude auth logout"
}

func (c Claude) Login(profileDir string) error {
	cmd := exec.Command(c.Path, "auth", "login")
	cmd.Env = c.Env(profileDir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

type HookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type HookGroup struct {
	Hooks []HookCommand `json:"hooks"`
}

type StatusLine struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// Settings is the subset of Claude Code's settings.json that csm writes or reads.
type Settings struct {
	Hooks      map[string][]HookGroup `json:"hooks,omitempty"`
	StatusLine StatusLine             `json:"statusLine,omitzero"`
}

// SettingsArgs registers csm's hooks and status line for one launch without touching settings files.
func (c Claude) SettingsArgs(csmPath string) []string {
	quoted := "'" + strings.ReplaceAll(csmPath, "'", `'\''`) + "'"
	s := Settings{
		Hooks: map[string][]HookGroup{
			"StopFailure":  {{Hooks: []HookCommand{{Type: "command", Command: quoted + " hook failure"}}}},
			"SessionStart": {{Hooks: []HookCommand{{Type: "command", Command: quoted + " hook session-start"}}}},
		},
		StatusLine: StatusLine{Type: "command", Command: quoted + " hook status-line"},
	}
	data, _ := json.Marshal(s)
	return []string{"--settings", string(data)}
}

// UserStatusLine finds the status line the user configured, which csm's --settings would otherwise hide.
func (c Claude) UserStatusLine(projectDir, profileDir string) string {
	paths := []string{
		filepath.Join(projectDir, ".claude", "settings.local.json"),
		filepath.Join(projectDir, ".claude", "settings.json"),
		filepath.Join(profileDir, "settings.json"),
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s Settings
		if json.Unmarshal(data, &s) == nil && s.StatusLine.Type == "command" && s.StatusLine.Command != "" {
			return s.StatusLine.Command
		}
	}
	return ""
}

type claudeRateWindow struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at"`
}

// Status line input, per https://code.claude.com/docs/en/statusline.md#available-data.
type claudeStatusLineInput struct {
	RateLimits struct {
		FiveHour   claudeRateWindow `json:"five_hour"`
		SevenDay   claudeRateWindow `json:"seven_day"`
		SpendLimit claudeRateWindow `json:"spend_limit"`
	} `json:"rate_limits"`
}

func (w claudeRateWindow) usage() UsageWindow {
	if w.ResetsAt == 0 {
		return UsageWindow{}
	}
	return UsageWindow{UsedPercent: w.UsedPercentage, ResetsAt: time.Unix(w.ResetsAt, 0)}
}

func (c Claude) ParseUsage(data []byte) (Usage, error) {
	var in claudeStatusLineInput
	if err := json.Unmarshal(data, &in); err != nil {
		return Usage{}, err
	}
	return Usage{
		FiveHour:   in.RateLimits.FiveHour.usage(),
		SevenDay:   in.RateLimits.SevenDay.usage(),
		SpendLimit: in.RateLimits.SpendLimit.usage(),
	}, nil
}

func (c Claude) ResumeArgs(sessionID string) []string {
	return []string{"--resume", sessionID}
}

func (c Claude) NewSessionArgs(f Features, sessionID, handoffNote string) []string {
	var args []string
	if f.SessionID {
		args = append(args, "--session-id", sessionID)
	}
	if f.SystemPrompt && handoffNote != "" {
		args = append(args, "--append-system-prompt", handoffNote)
	}
	return args
}

func (c Claude) HasSessionFlag(args []string) bool {
	for _, a := range args {
		switch {
		case a == "-c", a == "--continue", a == "-r", a == "--resume", a == "--session-id", a == "--from-pr",
			strings.HasPrefix(a, "--resume="), strings.HasPrefix(a, "--session-id="), strings.HasPrefix(a, "--from-pr="):
			return true
		}
	}
	return false
}

func (c Claude) RedactArgs(args []string) []string {
	out := slices.Clone(args)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "--settings" || out[i] == "--append-system-prompt" {
			out[i+1] = "<omitted>"
		}
	}
	return out
}

// StopFailure payload, per https://code.claude.com/docs/en/hooks.md#stopfailure.
type claudeStopFailure struct {
	SessionID            string `json:"session_id"`
	Error                string `json:"error"`
	ErrorDetails         string `json:"error_details"`
	LastAssistantMessage string `json:"last_assistant_message"`
}

func (c Claude) ParseFailure(data []byte) (Failure, error) {
	var ev claudeStopFailure
	if err := json.Unmarshal(data, &ev); err != nil {
		return Failure{}, err
	}
	return Failure{
		SessionID: ev.SessionID,
		Error:     ev.Error,
		Details:   ev.ErrorDetails,
		Message:   ev.LastAssistantMessage,
		Reason:    classifyClaudeFailure(ev.Error, ev.LastAssistantMessage+" "+ev.ErrorDetails),
	}, nil
}

func (c Claude) ParseSessionStart(data []byte) (string, error) {
	var ev struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return "", err
	}
	return ev.SessionID, nil
}

// Only a rate_limit error with usage-limit wording counts; a bare 429 is transient.
func classifyClaudeFailure(errorType, text string) StopReason {
	switch errorType {
	case "rate_limit":
		text = strings.ToLower(text)
		if strings.Contains(text, "limit") && (strings.Contains(text, "usage") || strings.Contains(text, "reset")) {
			return StopReasonUsageLimit
		}
		return StopReasonRateLimited
	case "authentication_failed", "oauth_org_not_allowed", "account_on_hold":
		return StopReasonAuthentication
	case "overloaded", "server_error":
		return StopReasonNetwork
	default:
		return StopReasonUnknown
	}
}

// CarrySession copies a transcript, kept at <profile>/projects/<project>/<id>.jsonl plus an optional <id>/ dir.
func (c Claude) CarrySession(fromProfile, toProfile, sessionID string) (bool, error) {
	matches, err := filepath.Glob(filepath.Join(fromProfile, "projects", "*", sessionID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return false, err
	}
	src := matches[0]
	dstProject := filepath.Join(toProfile, "projects", filepath.Base(filepath.Dir(src)))
	if err := copyFile(src, filepath.Join(dstProject, sessionID+".jsonl")); err != nil {
		return false, err
	}
	srcExtra := filepath.Join(filepath.Dir(src), sessionID)
	if _, err := os.Stat(srcExtra); err == nil {
		if err := copyTree(srcExtra, filepath.Join(dstProject, sessionID)); err != nil {
			return false, err
		}
	}
	return true, nil
}

// LinkUserConfig symlinks ~/.claude settings into a profile; ~/.claude itself is never modified.
func (c Claude) LinkUserConfig(profileDir string) (string, []string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil, err
	}
	userDir := filepath.Join(home, ".claude")
	var linked []string
	for _, name := range []string{"settings.json", "CLAUDE.md", "agents", "commands", "skills"} {
		src := filepath.Join(userDir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Symlink(src, filepath.Join(profileDir, name)); err != nil {
			return userDir, linked, err
		}
		linked = append(linked, name)
	}
	return userDir, linked, nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".csm-tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(path, target)
	})
}

// FetchUsage spends one ~500-token Haiku request to read the rate_limit_event from stream-json output.
func (c Claude) FetchUsage(ctx context.Context, profileDir string) (Usage, error) {
	cmd := exec.CommandContext(ctx, c.Path, "-p", "ok",
		"--model", "haiku",
		"--tools", "",
		"--system-prompt", "Reply with: ok",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--setting-sources", "",
		"--no-session-persistence",
		"--output-format", "stream-json",
		"--verbose",
	)
	cmd.Env = c.Env(profileDir)
	cmd.Dir = os.TempDir()
	out, runErr := cmd.Output()
	usage, err := parseUsageStream(strings.NewReader(string(out)))
	if err != nil && runErr != nil {
		return Usage{}, fmt.Errorf("%w (%v)", err, runErr)
	}
	return usage, err
}

type claudeUnifiedWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resetsAt"`
}

func (w claudeUnifiedWindow) usage() UsageWindow {
	if w.ResetsAt == 0 {
		return UsageWindow{}
	}
	return UsageWindow{UsedPercent: w.Utilization * 100, ResetsAt: time.Unix(w.ResetsAt, 0)}
}

type claudeStreamEvent struct {
	Type          string `json:"type"`
	IsError       bool   `json:"is_error"`
	Result        string `json:"result"`
	RateLimitInfo struct {
		Status         string `json:"status"`
		UnifiedWindows struct {
			FiveHour claudeUnifiedWindow `json:"five_hour"`
			SevenDay claudeUnifiedWindow `json:"seven_day"`
		} `json:"unifiedWindows"`
	} `json:"rate_limit_info"`
}

func parseUsageStream(r io.Reader) (Usage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var usage Usage
	found := false
	resultErr := ""
	for sc.Scan() {
		var ev claudeStreamEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "rate_limit_event":
			windows := ev.RateLimitInfo.UnifiedWindows
			usage = Usage{
				FiveHour: windows.FiveHour.usage(),
				SevenDay: windows.SevenDay.usage(),
				Limited:  ev.RateLimitInfo.Status == "rejected",
			}
			found = true
		case "result":
			if ev.IsError {
				resultErr = ev.Result
			}
		}
	}
	if err := sc.Err(); err != nil {
		return Usage{}, err
	}
	if found {
		return usage, nil
	}
	if resultErr != "" {
		return Usage{}, fmt.Errorf("Claude Code reported: %s", resultErr)
	}
	return Usage{}, errors.New("Claude Code did not report usage for this account")
}
