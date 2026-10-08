package provider

import (
	"context"
	"crypto/sha256"
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

// The first three pick another login than the profile's; the last moves the login into the shared Keychain.
var geminiAuthOverrideEnv = []string{"GEMINI_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GEMINI_BASE_URL", "GEMINI_FORCE_ENCRYPTED_FILE_STORAGE"}

type Gemini struct {
	Path string
}

func FindGemini() (Provider, error) {
	if path := os.Getenv("CSM_GEMINI"); path != "" {
		return Gemini{Path: path}, nil
	}

	path, err := exec.LookPath("gemini")

	if err != nil {
		return nil, errors.New("Gemini CLI is not installed or not on PATH.\n\nInstall it with `npm install -g @google/gemini-cli`, then run: csm doctor")
	}

	return Gemini{Path: path}, nil
}

func (g Gemini) ID() string { return GeminiID }

func (g Gemini) Name() string { return "Gemini CLI" }

func (g Gemini) Executable() string { return g.Path }

func (g Gemini) CheckEnv() error { return checkAuthEnv(geminiAuthOverrideEnv) }

func (g Gemini) Env(profileDir string, extra ...string) []string {
	return profileEnv("GEMINI_CLI_HOME", profileDir, extra)
}

// Gemini CLI keeps everything in a .gemini directory under the home it is given.
func geminiDir(profileDir string) string { return filepath.Join(profileDir, ".gemini") }

func (g Gemini) Version() (string, error) {
	out, err := probeOutput(g, "--version")

	if err != nil {
		return "", fmt.Errorf("run %s --version: %w", g.Path, err)
	}

	return strings.TrimSpace(string(out)), nil
}

func (g Gemini) Features() (Features, error) {
	out, err := probeOutput(g, "--help")

	if err != nil {
		return Features{}, fmt.Errorf("run %s --help: %w", g.Path, err)
	}

	help := string(out)
	sessionID := strings.Contains(help, "--session-id")

	return Features{
		Auth:          true,
		Supervise:     sessionID,
		Resume:        strings.Contains(help, "--session-file"),
		SessionID:     sessionID,
		InitialPrompt: strings.Contains(help, "--prompt-interactive"),
	}, nil
}

// Identity reads the account email Gemini CLI records at sign-in; Gemini CLI has no command that reports it.
func (g Gemini) Identity(profileDir string) (Identity, error) {
	id := Identity{ProfileDir: profileDir}

	if _, err := os.Stat(filepath.Join(geminiDir(profileDir), "oauth_creds.json")); err != nil {
		return id, nil
	}

	var accounts struct {
		Active string `json:"active"`
	}

	data, err := os.ReadFile(filepath.Join(geminiDir(profileDir), "google_accounts.json"))

	if err == nil {
		err = json.Unmarshal(data, &accounts)
	}

	if err != nil || accounts.Active == "" {
		return id, errors.New("Gemini CLI did not record which Google account this profile is signed in to")
	}

	id.LoggedIn, id.Email = true, accounts.Active

	return id, nil
}

// Gemini CLI has no sign-out command; its login is the oauth_creds.json file in the profile.
func (g Gemini) LogoutCommand(profileDir string) string {
	return "rm -f " + filepath.Join(geminiDir(profileDir), "oauth_creds.json")
}

// Login opens Gemini CLI itself, because signing in only happens inside it.
func (g Gemini) Login(profileDir string) error {
	fmt.Println("Gemini CLI will open. Choose \"Sign in with Google\", finish in the browser,\nthen type /quit to come back here.")
	fmt.Println()
	cmd := exec.Command(g.Path)
	cmd.Env = g.Env(profileDir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	return cmd.Run()
}

// LinkUserConfig symlinks ~/.gemini settings into a profile; ~/.gemini itself is never modified.
func (g Gemini) LinkUserConfig(profileDir string) (string, []string, error) {
	home, err := os.UserHomeDir()

	if err != nil {
		return "", nil, err
	}

	userDir := filepath.Join(home, ".gemini")

	if err := os.MkdirAll(geminiDir(profileDir), 0o700); err != nil {
		return userDir, nil, err
	}

	linked, err := linkUserConfig(userDir, geminiDir(profileDir), []string{"settings.json", "GEMINI.md", "commands", "extensions", "skills"})

	return userDir, linked, err
}

func (g Gemini) LaunchArgs(csmPath string) []string { return nil }

func (g Gemini) UserStatusLine(projectDir, profileDir string) string { return "" }

func (g Gemini) RedactArgs(args []string) []string { return slices.Clone(args) }

func (g Gemini) HasSessionFlag(args []string) bool {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")

		switch name {
		case "-r", "--resume", "--session-id", "--session-file":
			return true
		}
	}

	return false
}

func (g Gemini) NewSessionArgs(f Features, sessionID, handoffNote string) []string {
	if !f.SessionID {
		return nil
	}

	return []string{"--session-id", sessionID}
}

type geminiSession struct {
	path    string
	updated time.Time
	meta    struct {
		SessionID   string `json:"sessionId"`
		ProjectHash string `json:"projectHash"`
		Kind        string `json:"kind"`
	}
}

// geminiSessions reads the first record of each session file, kept at <profile>/.gemini/tmp/<project>/chats/, newest first.
func geminiSessions(profileDir string) ([]geminiSession, error) {
	paths, err := filepath.Glob(filepath.Join(geminiDir(profileDir), "tmp", "*", "chats", "session-*.json*"))

	if err != nil {
		return nil, err
	}

	var sessions []geminiSession

	for _, path := range paths {
		f, err := os.Open(path)

		if err != nil {
			continue
		}

		s := geminiSession{path: path}
		err = json.NewDecoder(f).Decode(&s.meta)
		fi, statErr := f.Stat()
		f.Close()

		if err != nil || statErr != nil || s.meta.SessionID == "" || s.meta.Kind == "subagent" {
			continue
		}

		s.updated = fi.ModTime()
		sessions = append(sessions, s)
	}

	slices.SortFunc(sessions, func(a, b geminiSession) int { return b.updated.Compare(a.updated) })

	return sessions, nil
}

// Gemini CLI names a project by the SHA-256 of the directory it was started in.
func geminiProjectHash(dir string) string {
	sum := sha256.Sum256([]byte(dir))

	return hex.EncodeToString(sum[:])
}

// CurrentSession is the session most recently written for cwd since the agent started.
func (g Gemini) CurrentSession(profileDir, cwd string, since time.Time) (string, error) {
	hashes := []string{geminiProjectHash(cwd)}

	if real, err := filepath.EvalSymlinks(cwd); err == nil && real != cwd {
		hashes = append(hashes, geminiProjectHash(real))
	}

	sessions, err := geminiSessions(profileDir)

	if err != nil {
		return "", err
	}

	for _, s := range sessions {
		if s.updated.Before(since.Truncate(time.Second)) {
			break
		}

		if slices.Contains(hashes, s.meta.ProjectHash) {
			return s.meta.SessionID, nil
		}
	}

	return "", nil
}

// CarrySession copies nothing: the target profile imports the session file with --session-file, under a new ID.
func (g Gemini) CarrySession(fromProfile, toProfile, sessionID string) ([]string, string, error) {
	sessions, err := geminiSessions(fromProfile)

	if err != nil {
		return nil, "", err
	}

	for _, s := range sessions {
		if s.meta.SessionID == sessionID {
			return []string{"--session-file", s.path}, "", nil
		}
	}

	return nil, "", nil
}

func (g Gemini) FetchUsage(ctx context.Context, profileDir string) (Usage, error) {
	return Usage{}, errors.New("Gemini CLI cannot report usage outside a session")
}

func (g Gemini) UsageCheckNote() string { return "" }

func (g Gemini) LimitPollInterval() time.Duration { return 0 }
