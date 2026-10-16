package provider

import (
	"context"
	"crypto/md5"
	"encoding/hex"
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

var cursorProbeTimeout = 20 * time.Second

type Cursor struct {
	Path string
}

// cursor-agent first: only Cursor installs it, while "agent" is a generic name another tool may own.
func FindCursor() (Provider, error) {
	if path := os.Getenv("CSM_CURSOR"); path != "" {
		return Cursor{Path: path}, nil
	}

	if path, err := exec.LookPath("cursor-agent"); err == nil {
		return Cursor{Path: path}, nil
	}

	path, err := exec.LookPath("agent")
	if err == nil && isCursorInstall(path) {
		return Cursor{Path: path}, nil
	}

	if err == nil {
		return nil, fmt.Errorf("Cursor CLI is not installed: %s on PATH is not Cursor's agent.\n\nInstall it with `curl https://cursor.com/install -fsS | bash`, then run: csm doctor", path)
	}

	return nil, errors.New("Cursor CLI is not installed or not on PATH.\n\nInstall it with `curl https://cursor.com/install -fsS | bash`, then run: csm doctor")
}

func isCursorInstall(path string) bool {
	real, err := filepath.EvalSymlinks(path)

	return err == nil && filepath.Base(real) == "cursor-agent"
}

func (c Cursor) ID() string { return CursorID }

func (c Cursor) Name() string { return "Cursor CLI" }

func (c Cursor) Executable() string { return c.Path }

// CheckEnv accepts CURSOR_API_KEY: the login is shared by every profile anyway, so a key cannot mix up csm accounts.
func (c Cursor) CheckEnv() error { return nil }

func (c Cursor) Env(profileDir string, extra ...string) []string {
	return profileEnv("CURSOR_CONFIG_DIR", profileDir, extra)
}

func (c Cursor) Version() (string, error) {
	out, err := probeOutput(c, "--version")

	if err != nil {
		return "", fmt.Errorf("run %s --version: %w", c.Path, err)
	}

	v := strings.TrimSpace(string(out))

	if v == "" {
		return "", fmt.Errorf("%s --version printed nothing", c.Path)
	}

	return v, nil
}

func (c Cursor) Features() (Features, error) {
	out, err := probeOutput(c, "--help")

	if err != nil {
		return Features{}, fmt.Errorf("run %s --help: %w", c.Path, err)
	}

	help := string(out)
	resume := strings.Contains(help, "--resume")

	return Features{
		Auth:          strings.Contains(help, "\n  login ") && strings.Contains(help, "\n  status"),
		Supervise:     resume,
		Resume:        resume,
		InitialPrompt: strings.Contains(help, "[prompt...]"),
		SharedLogin:   true,
	}, nil
}

// status exits 0 signed in or not, so only isAuthenticated decides.
type cursorStatus struct {
	Status          string `json:"status"`
	IsAuthenticated bool   `json:"isAuthenticated"`
	UserInfo        struct {
		Email string `json:"email"`
	} `json:"userInfo"`
}

func (c Cursor) probe(profileDir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cursorProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Path, args...)
	cmd.Env = c.Env(profileDir, "NO_OPEN_BROWSER=1")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()

	if ctx.Err() != nil {
		return out, fmt.Errorf("%s %s did not answer within %s", c.Name(), strings.Join(args, " "), cursorProbeTimeout)
	}

	return out, err
}

// The login lives in Cursor's own store (the Keychain on macOS), which csm never reads, so the CLI is asked.
func (c Cursor) Identity(profileDir string) (Identity, error) {
	id := Identity{ProfileDir: profileDir}
	out, err := c.probe(profileDir, "status", "--format", "json")

	var st cursorStatus

	if jsonErr := json.Unmarshal(out, &st); jsonErr != nil {
		if err != nil {
			return id, fmt.Errorf("cursor status failed: %w", err)
		}

		return id, fmt.Errorf("cursor status returned unexpected output: %w", jsonErr)
	}

	if !st.IsAuthenticated {
		return id, nil
	}

	id.LoggedIn, id.Email = true, st.UserInfo.Email

	if id.Email == "" {
		var about struct {
			UserEmail string `json:"userEmail"`
		}

		if out, err := c.probe(profileDir, "about", "--format", "json"); err == nil && json.Unmarshal(out, &about) == nil {
			id.Email = about.UserEmail
		}
	}

	return id, nil
}

func (c Cursor) LogoutCommand(profileDir string) string {
	return "agent logout    # signs the Cursor CLI out for this user, not just this profile"
}

func (c Cursor) Login(profileDir string) error {
	fmt.Println("Cursor keeps one sign-in for your user account, shared by every Cursor CLI session.\ncsm uses that sign-in and never stores or reads it.")
	fmt.Println()
	cmd := exec.Command(c.Path, "login")
	cmd.Env = c.Env(profileDir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	return cmd.Run()
}

func cursorUserDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); strings.TrimSpace(xdg) != "" {
		return filepath.Join(xdg, "cursor"), nil
	}

	home, err := os.UserHomeDir()

	return filepath.Join(home, ".cursor"), err
}

// LinkUserConfig shares the user's own settings with a fresh profile; the user's directory is never modified.
func (c Cursor) LinkUserConfig(profileDir string) (string, []string, error) {
	userDir, links, err := c.userLinks(profileDir)
	if err != nil {
		return "", nil, err
	}

	linked, _, err := applyLinks(links)

	return userDir, linked, err
}

func (c Cursor) LaunchArgs(csmPath string) []string { return nil }

func (c Cursor) UserStatusLine(projectDir, profileDir string) string { return "" }

var cursorSecretFlags = []string{"--api-key", "--auth-token", "-H", "--header"}

func (c Cursor) RedactArgs(args []string) []string {
	out := slices.Clone(args)

	for i := 0; i < len(out); i++ {
		name, _, hasValue := strings.Cut(out[i], "=")

		if !slices.Contains(cursorSecretFlags, name) {
			continue
		}

		if hasValue {
			out[i] = name + "=[redacted]"
		} else if i+1 < len(out) {
			out[i+1] = "[redacted]"
			i++
		}
	}

	return out
}

func (c Cursor) HasSessionFlag(args []string) bool {
	for i, a := range args {
		name, _, _ := strings.Cut(a, "=")

		switch name {
		case "--resume", "--continue":
			return true
		}

		if i == 0 && (a == "resume" || a == "ls") {
			return true
		}
	}

	return false
}

// NewSessionArgs is empty: the CLI cannot be given a chat ID up front, so csm finds the chat afterwards.
func (c Cursor) NewSessionArgs(f Features, sessionID, handoffNote string) []string { return nil }

// Mirrors the CLI's layout: <config>/chats/<md5 of the working directory>/<chat id>/store.db.
func cursorChatsDir(profileDir, cwd string) string {
	sum := md5.Sum([]byte(cwd))

	return filepath.Join(profileDir, "chats", hex.EncodeToString(sum[:]))
}

// Only chat folder names are read, never the chats themselves.
func (c Cursor) CurrentSession(profileDir, cwd string, since time.Time) (string, error) {
	dirs := []string{cursorChatsDir(profileDir, cwd)}

	if real, err := filepath.EvalSymlinks(cwd); err == nil && real != cwd {
		dirs = append(dirs, cursorChatsDir(profileDir, real))
	}

	newest, newestAt := "", since.Truncate(time.Second)

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)

		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return "", err
		}

		for _, e := range entries {
			if !e.IsDir() {
				continue
			}

			chat := filepath.Join(dir, e.Name())
			updated := modTime(chat)

			if db := modTime(filepath.Join(chat, "store.db")); db.After(updated) {
				updated = db
			}

			if updated.Before(newestAt) {
				continue
			}

			newest, newestAt = e.Name(), updated
		}
	}

	return newest, nil
}

// CarrySession carries nothing: a chat belongs to the signed-in Cursor user, and csm manages one Cursor account.
func (c Cursor) CarrySession(fromProfile, toProfile, sessionID string) ([]string, string, error) {
	return nil, "", nil
}

func (c Cursor) FetchUsage(ctx context.Context, profileDir string) (Usage, error) {
	return Usage{}, errors.New("Cursor CLI does not report plan usage")
}

func (c Cursor) UsageCheckNote() string { return "" }

func (c Cursor) LimitPollInterval() time.Duration { return 0 }

func (c Cursor) HandoffArgs(f Features, instruction string) []string {
	if !f.InitialPrompt || instruction == "" {
		return nil
	}

	return []string{instruction}
}

func (c Cursor) TaskArgs(f Features, instruction string) []string {
	return c.HandoffArgs(f, instruction)
}
