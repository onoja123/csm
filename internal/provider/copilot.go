package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type Copilot struct {
	Path string
}

// The tokens take precedence over a profile's stored login; the base URL replaces GitHub sign-in altogether.
var copilotAuthOverrideEnv = []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "COPILOT_PROVIDER_BASE_URL"}

const copilotDefaultHost = "https://github.com"

func FindCopilot() (Provider, error) {
	if path := os.Getenv("CSM_COPILOT"); path != "" {
		return Copilot{Path: path}, nil
	}
	path, err := exec.LookPath("copilot")
	if err != nil {
		return nil, errors.New("GitHub Copilot CLI is not installed or not on PATH.\n\nInstall it with `npm install -g @github/copilot`, then run: csm doctor")
	}
	return Copilot{Path: path}, nil
}

func (c Copilot) ID() string         { return CopilotID }
func (c Copilot) Name() string       { return "GitHub Copilot CLI" }
func (c Copilot) Executable() string { return c.Path }

func (c Copilot) CheckEnv() error { return checkAuthEnv(copilotAuthOverrideEnv) }

func (c Copilot) Env(profileDir string, extra ...string) []string {
	return profileEnv("COPILOT_HOME", profileDir, extra)
}

func (c Copilot) Version() (string, error) {
	out, err := probeOutput(c, "--version")
	if err != nil {
		return "", fmt.Errorf("run %s --version: %w", c.Path, err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSuffix(strings.TrimPrefix(line, "GitHub Copilot CLI "), "."), nil
}

func (c Copilot) Features() (Features, error) {
	out, err := probeOutput(c, "--help")
	if err != nil {
		return Features{}, fmt.Errorf("run %s --help: %w", c.Path, err)
	}
	help := string(out)
	sessionID := strings.Contains(help, "--session-id")
	return Features{
		Auth:      strings.Contains(help, "\n  login "),
		Supervise: sessionID,
		Resume:    strings.Contains(help, "--resume"),
		SessionID: sessionID,
	}, nil
}

type copilotUser struct {
	Host  string `json:"host"`
	Login string `json:"login"`
}

// copilotConfig is the part of <profile>/config.json that names the signed-in user; the token itself is kept in the Keychain.
type copilotConfig struct {
	LastLoggedInUser    *copilotUser `json:"lastLoggedInUser"`
	LastLoggedInUserOld *copilotUser `json:"last_logged_in_user"`
}

// Identity reads the user Copilot CLI records at sign-in; Copilot CLI has no command that reports it.
func (c Copilot) Identity(profileDir string) (Identity, error) {
	id := Identity{ProfileDir: profileDir}
	data, err := os.ReadFile(filepath.Join(profileDir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return id, nil
	}
	if err != nil {
		return id, err
	}
	var cfg copilotConfig
	if err := json.Unmarshal(stripLineComments(data), &cfg); err != nil {
		return id, fmt.Errorf("read Copilot CLI config: %w", err)
	}
	user := cfg.LastLoggedInUser
	if user == nil {
		user = cfg.LastLoggedInUserOld
	}
	if user == nil || user.Login == "" {
		return id, nil
	}
	id.LoggedIn, id.Email = true, user.Login
	if user.Host != "" && user.Host != copilotDefaultHost {
		id.Email += "@" + strings.TrimPrefix(user.Host, "https://")
	}
	return id, nil
}

// Copilot CLI writes config.json with // comment lines above the JSON.
func stripLineComments(data []byte) []byte {
	var out [][]byte
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if !bytes.HasPrefix(bytes.TrimSpace(line), []byte("//")) {
			out = append(out, line)
		}
	}
	return bytes.Join(out, []byte("\n"))
}

// Copilot CLI only signs out from inside a session.
func (c Copilot) LogoutCommand(profileDir string) string {
	return "COPILOT_HOME=" + profileDir + " copilot    # then type /logout"
}

func (c Copilot) Login(profileDir string) error {
	cmd := exec.Command(c.Path, "login")
	cmd.Env = c.Env(profileDir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// LinkUserConfig symlinks ~/.copilot settings into a profile; config.json is left out because it names the signed-in user.
func (c Copilot) LinkUserConfig(profileDir string) (string, []string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil, err
	}
	userDir := filepath.Join(home, ".copilot")
	linked, err := linkUserConfig(userDir, profileDir, []string{"settings.json", "mcp-config.json", "copilot-instructions.md", "agents", "skills"})
	return userDir, linked, err
}

func (c Copilot) LaunchArgs(csmPath string) []string { return nil }

func (c Copilot) UserStatusLine(projectDir, profileDir string) string { return "" }

func (c Copilot) RedactArgs(args []string) []string { return slices.Clone(args) }

func (c Copilot) HasSessionFlag(args []string) bool {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		switch name {
		case "-r", "--resume", "--continue", "--session-id", "--connect":
			return true
		}
	}
	return false
}

func (c Copilot) NewSessionArgs(f Features, sessionID, handoffNote string) []string {
	if !f.SessionID {
		return nil
	}
	return []string{"--session-id", sessionID}
}

func copilotSessionDir(profileDir, sessionID string) string {
	return filepath.Join(profileDir, "session-state", sessionID)
}

// CurrentSession is the session folder most recently written since the agent started whose workspace.yaml names cwd.
func (c Copilot) CurrentSession(profileDir, cwd string, since time.Time) (string, error) {
	cwds := []string{cwd}
	if real, err := filepath.EvalSymlinks(cwd); err == nil && real != cwd {
		cwds = append(cwds, real)
	}
	dirs, err := filepath.Glob(copilotSessionDir(profileDir, "*"))
	if err != nil {
		return "", err
	}
	newest, newestAt := "", since.Truncate(time.Second)
	for _, dir := range dirs {
		updated := modTime(dir)
		if events := modTime(filepath.Join(dir, "events.jsonl")); events.After(updated) {
			updated = events
		}
		if updated.Before(newestAt) || !copilotSessionIn(dir, cwds) {
			continue
		}
		newest, newestAt = filepath.Base(dir), updated
	}
	return newest, nil
}

func modTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func copilotSessionIn(sessionDir string, cwds []string) bool {
	data, err := os.ReadFile(filepath.Join(sessionDir, "workspace.yaml"))
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "cwd:")
		if ok && slices.Contains(cwds, strings.Trim(strings.TrimSpace(value), `"'`)) {
			return true
		}
	}
	return false
}

// CarrySession copies a session's folder, kept at <profile>/session-state/<id>, to the same place in another profile.
func (c Copilot) CarrySession(fromProfile, toProfile, sessionID string) ([]string, string, error) {
	if sessionID != filepath.Base(sessionID) {
		return nil, "", fmt.Errorf("unexpected Copilot CLI session ID %q", sessionID)
	}
	src := copilotSessionDir(fromProfile, sessionID)
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return nil, "", nil
	}
	if err := copyTree(src, copilotSessionDir(toProfile, sessionID)); err != nil {
		return nil, "", err
	}
	return []string{"--resume", sessionID}, sessionID, nil
}

func (c Copilot) FetchUsage(ctx context.Context, profileDir string) (Usage, error) {
	return Usage{}, errors.New("GitHub Copilot CLI cannot report usage outside a session")
}

func (c Copilot) UsageCheckNote() string { return "" }

func (c Copilot) LimitPollInterval() time.Duration { return 0 }
