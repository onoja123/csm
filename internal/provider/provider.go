// Package provider holds everything specific to a coding agent.
package provider

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	ClaudeID  = "claude"
	CodexID   = "codex"
	GeminiID  = "gemini"
	CopilotID = "copilot"
)

// IDs lists the supported providers; each ID is also the csm command that runs the agent.
var IDs = []string{ClaudeID, CodexID, GeminiID, CopilotID}

type Provider interface {
	ID() string
	Name() string
	Executable() string
	CheckEnv() error
	Env(profileDir string, extra ...string) []string
	Version() (string, error)
	Features() (Features, error)
	Identity(profileDir string) (Identity, error)
	Login(profileDir string) error
	LogoutCommand(profileDir string) string
	LinkUserConfig(profileDir string) (string, []string, error)
	LaunchArgs(csmPath string) []string
	UserStatusLine(projectDir, profileDir string) string
	RedactArgs(args []string) []string
	HasSessionFlag(args []string) bool
	NewSessionArgs(f Features, sessionID, handoffNote string) []string
	// CurrentSession is for agents that cannot push their session ID to csm; the others return "".
	CurrentSession(profileDir, cwd string, since time.Time) (string, error)
	// CarrySession returns no resumeArgs when the session cannot be carried, and no newSessionID when the agent resumes it under an ID csm cannot know.
	CarrySession(fromProfile, toProfile, sessionID string) (resumeArgs []string, newSessionID string, err error)
	FetchUsage(ctx context.Context, profileDir string) (Usage, error)
	UsageCheckNote() string
	// LimitPollInterval is non-zero for agents that have no failure hook, so limits are found by polling usage.
	LimitPollInterval() time.Duration
}

// New returns a provider without looking for its executable; an empty ID means Claude Code.
func New(id, path string) (Provider, error) {
	switch id {
	case "", ClaudeID:
		return Claude{Path: path}, nil
	case CodexID:
		return Codex{Path: path}, nil
	case GeminiID:
		return Gemini{Path: path}, nil
	case CopilotID:
		return Copilot{Path: path}, nil
	}

	return nil, fmt.Errorf("unknown provider %q; supported: %s", id, strings.Join(IDs, ", "))
}

func Find(id string) (Provider, error) {
	switch id {
	case "", ClaudeID:
		return FindClaude()
	case CodexID:
		return FindCodex()
	case GeminiID:
		return FindGemini()
	case CopilotID:
		return FindCopilot()
	}

	return New(id, "")
}

// probeOutput runs an agent command with a throwaway profile, because some agents write to their home even for --version.
func probeOutput(p Provider, args ...string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "csm-probe-")
	if err != nil {
		return nil, err
	}

	defer os.RemoveAll(dir)

	cmd := exec.Command(p.Executable(), args...)
	cmd.Env = p.Env(dir)

	return cmd.Output()
}

func checkAuthEnv(names []string) error {
	for _, name := range names {
		if os.Getenv(name) != "" {
			return fmt.Errorf("%s is set in your environment.\n\nIt overrides per-profile login, so every csm account would use the same credentials.\nUnset it before using csm:\n\n    unset %s", name, name)
		}
	}

	return nil
}

func profileEnv(key, profileDir string, extra []string) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, key+"=")
	})
	env = append(env, key+"="+profileDir)

	return append(env, extra...)
}

// linkUserConfig symlinks the user's own agent settings into a profile; the user's directory is never modified.
func linkUserConfig(userDir, profileDir string, names []string) ([]string, error) {
	var linked []string

	for _, name := range names {
		src := filepath.Join(userDir, name)

		if _, err := os.Stat(src); err != nil {
			continue
		}

		if err := os.Symlink(src, filepath.Join(profileDir, name)); err != nil {
			return linked, err
		}

		linked = append(linked, name)
	}

	return linked, nil
}

func NewSessionID() string {
	var b [16]byte

	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type StopReason string

const (
	StopReasonUsageLimit      StopReason = "usage_limit"
	StopReasonRateLimited     StopReason = "rate_limited"
	StopReasonAuthentication  StopReason = "authentication"
	StopReasonNetwork         StopReason = "network"
	StopReasonUserInterrupted StopReason = "user_interrupted"
	StopReasonProcessExited   StopReason = "process_exited"
	StopReasonManualSwitch    StopReason = "manual_switch"
	StopReasonUnknown         StopReason = "unknown"
)

func (r StopReason) Describe() string {
	switch r {
	case StopReasonUsageLimit:
		return "known usage limit"
	case StopReasonRateLimited:
		return "temporary API rate limit (not a usage limit)"
	case StopReasonAuthentication:
		return "authentication required"
	case StopReasonNetwork:
		return "network or service error"
	case StopReasonUserInterrupted:
		return "stopped by user"
	case StopReasonProcessExited:
		return "the agent exited"
	case StopReasonManualSwitch:
		return "manual switch"
	default:
		return "unknown error"
	}
}

// Identity is who the agent says it is logged in as for one profile directory.
type Identity struct {
	LoggedIn   bool
	Email      string
	ProfileDir string
}

type Features struct {
	Auth bool
	// Supervise means csm can observe and own a session: hooks for Claude Code, the app-server and --no-daemon for Codex, --session-id for Gemini CLI and Copilot CLI.
	Supervise bool
	Resume    bool
	// SessionID means csm can choose the ID of a new session.
	SessionID    bool
	SystemPrompt bool
	UsageProbe   bool
}

func (f Features) CanIsolate() bool { return f.Auth && f.Supervise }

func (f Features) CanContinue() bool { return f.Resume }

// Failure is an API error the agent reported through its failure hook.
type Failure struct {
	SessionID string     `json:"session_id"`
	Error     string     `json:"error"`
	Details   string     `json:"details,omitempty"`
	Message   string     `json:"message"`
	Reason    StopReason `json:"reason"`
}

// UsageWindow is one plan limit window; a zero ResetsAt means the agent did not report it.
type UsageWindow struct {
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at"`
}

// Usage is the plan usage the agent last reported for the account it runs as.
type Usage struct {
	FiveHour   UsageWindow `json:"five_hour,omitzero"`
	SevenDay   UsageWindow `json:"seven_day,omitzero"`
	SpendLimit UsageWindow `json:"spend_limit,omitzero"`
	// Limited is true when the agent reported that requests are currently being rejected.
	Limited bool `json:"limited,omitempty"`
}

func (u Usage) Empty() bool {
	return u.FiveHour.ResetsAt.IsZero() && u.SevenDay.ResetsAt.IsZero() && u.SpendLimit.ResetsAt.IsZero()
}
