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
	"slices"
	"strings"
	"time"
)

const (
	codexRPCTimeout = 20 * time.Second

	codexInitializeID = 1
	codexCallID       = 2
)

var codexAuthOverrideEnv = []string{"CODEX_ACCESS_TOKEN", "CODEX_API_KEY"}

type Codex struct {
	Path string
}

func FindCodex() (Provider, error) {
	if path := os.Getenv("CSM_CODEX"); path != "" {
		return Codex{Path: path}, nil
	}

	path, err := exec.LookPath("codex")
	if err != nil {
		return nil, errors.New("Codex is not installed or not on PATH.\n\nInstall it with `npm install -g @openai/codex`, then run: csm doctor")
	}

	return Codex{Path: path}, nil
}

func (c Codex) ID() string { return CodexID }

func (c Codex) Name() string { return "Codex" }

func (c Codex) Executable() string { return c.Path }

func (c Codex) CheckEnv() error { return checkAuthEnv(codexAuthOverrideEnv) }

func (c Codex) Env(profileDir string, extra ...string) []string {
	return profileEnv("CODEX_HOME", profileDir, extra)
}

func (c Codex) Version() (string, error) {
	out, err := probeOutput(c, "--version")

	if err != nil {
		return "", fmt.Errorf("run %s --version: %w", c.Path, err)
	}

	return strings.TrimPrefix(strings.TrimSpace(string(out)), "codex-cli "), nil
}

func (c Codex) Features() (Features, error) {
	out, err := probeOutput(c, "--help")

	if err != nil {
		return Features{}, fmt.Errorf("run %s --help: %w", c.Path, err)
	}

	help := string(out)
	appServer := strings.Contains(help, "\n  app-server ")

	return Features{
		Auth:          strings.Contains(help, "\n  login "),
		Supervise:     appServer && strings.Contains(help, "--no-daemon"),
		Resume:        strings.Contains(help, "\n  resume "),
		InitialPrompt: strings.Contains(help, "[PROMPT]"),
		UsageProbe:    appServer,
	}, nil
}

type codexRequest struct {
	ID     int    `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type codexNotification struct {
	Method string `json:"method"`
}

type codexResponse struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *codexError     `json:"error"`
}

type codexError struct {
	Message string `json:"message"`
}

type codexInitializeParams struct {
	ClientInfo codexClientInfo `json:"clientInfo"`
}

type codexClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

type codexInitialized struct {
	CodexHome string `json:"codexHome"`
}

// rpc asks one short-lived `codex app-server` over stdio for one thing; it decodes the answer into result and returns the Codex home the server reports.
func (c Codex) rpc(ctx context.Context, profileDir, method string, params, result any) (string, error) {
	ctx, cancel := context.WithCancel(ctx)

	defer cancel()

	cmd := exec.CommandContext(ctx, c.Path, "app-server")
	cmd.Env = c.Env(profileDir)
	cmd.Dir = os.TempDir()
	stdin, err := cmd.StdinPipe()

	if err != nil {
		return "", err
	}

	stdout, err := cmd.StdoutPipe()

	if err != nil {
		return "", err
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start codex app-server: %w", err)
	}

	defer func() {
		stdin.Close()
		cancel()
		cmd.Wait()
	}()

	enc := json.NewEncoder(stdin)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)

	reply := func(id int) (codexResponse, error) {

		for sc.Scan() {
			var msg codexResponse

			if json.Unmarshal(sc.Bytes(), &msg) == nil && msg.ID != nil && *msg.ID == id && msg.Method == "" {
				return msg, nil
			}
		}

		if err := ctx.Err(); err != nil {
			return codexResponse{}, fmt.Errorf("codex app-server did not answer: %w", err)
		}

		return codexResponse{}, errors.New("codex app-server exited before answering")
	}

	clientInfo := codexClientInfo{Name: "csm", Title: "Code Session Manager", Version: "1"}

	if err := enc.Encode(codexRequest{ID: codexInitializeID, Method: "initialize", Params: codexInitializeParams{ClientInfo: clientInfo}}); err != nil {
		return "", err
	}

	msg, err := reply(codexInitializeID)

	if err != nil {
		return "", err
	}

	if msg.Error != nil {
		return "", fmt.Errorf("codex app-server: %s", msg.Error.Message)
	}

	var init codexInitialized

	if err := json.Unmarshal(msg.Result, &init); err != nil {
		return "", fmt.Errorf("codex app-server returned unexpected output: %w", err)
	}

	if err := enc.Encode(codexNotification{Method: "initialized"}); err != nil {
		return "", err
	}

	if err := enc.Encode(codexRequest{ID: codexCallID, Method: method, Params: params}); err != nil {
		return "", err
	}

	msg, err = reply(codexCallID)

	if err != nil {
		return "", err
	}

	if msg.Error != nil {
		return init.CodexHome, fmt.Errorf("codex %s: %s", method, msg.Error.Message)
	}

	if err := json.Unmarshal(msg.Result, result); err != nil {
		return init.CodexHome, fmt.Errorf("codex %s returned unexpected output: %w", method, err)
	}

	return init.CodexHome, nil
}

func (c Codex) call(profileDir, method string, params, result any) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), codexRPCTimeout)

	defer cancel()

	return c.rpc(ctx, profileDir, method, params, result)
}

type codexAccountReadParams struct {
	RefreshToken bool `json:"refreshToken"`
}
type codexAccount struct {
	Account *codexAccountInfo `json:"account"`
}

type codexAccountInfo struct {
	Type  string `json:"type"`
	Email string `json:"email"`
}

func (c Codex) Identity(profileDir string) (Identity, error) {
	var res codexAccount

	home, err := c.call(profileDir, "account/read", codexAccountReadParams{RefreshToken: false}, &res)

	if err != nil {
		return Identity{}, err
	}

	if res.Account == nil {
		return Identity{ProfileDir: home}, nil
	}

	if res.Account.Type != "chatgpt" {
		return Identity{}, fmt.Errorf("this Codex profile is signed in with %q credentials.\n\ncsm manages ChatGPT sign-ins only. Sign out and log in with a ChatGPT account:\n\n    %s", res.Account.Type, c.LogoutCommand(profileDir))
	}

	if res.Account.Email == "" {
		return Identity{}, errors.New("Codex did not report which account this profile is signed in to")
	}

	return Identity{LoggedIn: true, Email: res.Account.Email, ProfileDir: home}, nil
}

func (c Codex) LogoutCommand(profileDir string) string {
	return "CODEX_HOME=" + profileDir + " codex logout"
}

func (c Codex) Login(profileDir string) error {
	cmd := exec.Command(c.Path, "login")
	cmd.Env = c.Env(profileDir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	return cmd.Run()
}

func (c Codex) LinkUserConfig(profileDir string) (string, []string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil, err
	}

	userDir := filepath.Join(home, ".codex")
	linked, err := linkUserConfig(userDir, profileDir, []string{"config.toml", "AGENTS.md", "hooks.json", "prompts", "rules"})

	return userDir, linked, err
}

// Without --no-daemon a background server keeps the session running after csm stops the terminal UI.
func (c Codex) LaunchArgs(csmPath string) []string {
	return []string{"--no-daemon"}
}

func (c Codex) UserStatusLine(projectDir, profileDir string) string { return "" }

func (c Codex) RedactArgs(args []string) []string { return slices.Clone(args) }

func (c Codex) HasSessionFlag(args []string) bool {
	return slices.Contains(args, "resume") || slices.Contains(args, "fork")
}

func (c Codex) NewSessionArgs(f Features, sessionID, handoffNote string) []string { return nil }

type codexThread struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	HistoryMode string `json:"historyMode"`
	UpdatedAt   int64  `json:"updatedAt"`
}

type codexThreadListParams struct {
	Cwd     []string `json:"cwd"`
	SortKey string   `json:"sortKey"`
	Limit   int      `json:"limit"`
}

type codexThreadList struct {
	Data []codexThread `json:"data"`
}

type codexThreadReadParams struct {
	ThreadID     string `json:"threadId"`
	IncludeTurns bool   `json:"includeTurns"`
}

type codexThreadRead struct {
	Thread codexThread `json:"thread"`
}

// CurrentSession is the interactive thread most recently updated in cwd since the agent started.
func (c Codex) CurrentSession(profileDir, cwd string, since time.Time) (string, error) {
	cwds := []string{cwd}

	if real, err := filepath.EvalSymlinks(cwd); err == nil && real != cwd {
		cwds = append(cwds, real)
	}

	var res codexThreadList

	params := codexThreadListParams{Cwd: cwds, SortKey: "updated_at", Limit: 1}

	if _, err := c.call(profileDir, "thread/list", params, &res); err != nil {
		return "", err
	}

	if len(res.Data) == 0 || res.Data[0].UpdatedAt < since.Unix() {
		return "", nil
	}

	return res.Data[0].ID, nil
}

// CarrySession copies a session's rollout file, kept under <profile>/sessions, to the same place in another profile.
func (c Codex) CarrySession(fromProfile, toProfile, sessionID string) ([]string, string, error) {
	var res codexThreadRead

	if _, err := c.call(fromProfile, "thread/read", codexThreadReadParams{ThreadID: sessionID, IncludeTurns: false}, &res); err != nil {
		return nil, "", err
	}

	// Paginated history lives in a database, so the rollout file alone would resume as an empty session.
	if res.Thread.Path == "" || res.Thread.HistoryMode == "paginated" {
		return nil, "", fmt.Errorf("codex session %s is not stored as a single rollout file", sessionID)
	}

	root, err := filepath.EvalSymlinks(fromProfile)

	if err != nil {
		return nil, "", err
	}

	src, err := filepath.EvalSymlinks(res.Thread.Path)

	if err != nil {
		return nil, "", err
	}

	rel, err := filepath.Rel(root, src)

	if err != nil || !filepath.IsLocal(rel) {
		return nil, "", fmt.Errorf("codex session %s is stored outside its profile at %s", sessionID, src)
	}

	if err := copyFile(src, filepath.Join(toProfile, rel)); err != nil {
		return nil, "", err
	}

	return []string{"resume", sessionID}, sessionID, nil
}

func (c Codex) UsageCheckNote() string {
	return "A Codex live check reads the account's limits from OpenAI; it sends no message and uses no tokens."
}

func (c Codex) LimitPollInterval() time.Duration { return time.Minute }

func (c Codex) FetchUsage(ctx context.Context, profileDir string) (Usage, error) {
	var res codexRateLimits

	if _, err := c.rpc(ctx, profileDir, "account/rateLimits/read", nil, &res); err != nil {
		return Usage{}, err
	}

	return res.usage(), nil
}

type codexRateWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	ResetsAt           int64   `json:"resetsAt"`
	WindowDurationMins int64   `json:"windowDurationMins"`
}

type codexRateLimits struct {
	RateLimits struct {
		Primary         codexRateWindow `json:"primary"`
		Secondary       codexRateWindow `json:"secondary"`
		IndividualLimit struct {
			RemainingPercent float64 `json:"remainingPercent"`
			ResetsAt         int64   `json:"resetsAt"`
		} `json:"individualLimit"`
		RateLimitReachedType string `json:"rateLimitReachedType"`
		Credits              struct {
			HasCredits bool `json:"hasCredits"`
			Unlimited  bool `json:"unlimited"`
		} `json:"credits"`
	} `json:"rateLimits"`
}

func (r codexRateLimits) usage() Usage {
	var u Usage

	limits := r.RateLimits

	for i, w := range []codexRateWindow{limits.Primary, limits.Secondary} {
		if w.ResetsAt == 0 {
			continue
		}

		window := UsageWindow{UsedPercent: w.UsedPercent, ResetsAt: time.Unix(w.ResetsAt, 0)}
		weekly := w.WindowDurationMins > 24*60 || (w.WindowDurationMins == 0 && i == 1)
		if weekly {
			u.SevenDay = window
		} else {
			u.FiveHour = window
		}
	}

	if l := limits.IndividualLimit; l.ResetsAt != 0 {
		u.SpendLimit = UsageWindow{UsedPercent: 100 - l.RemainingPercent, ResetsAt: time.Unix(l.ResetsAt, 0)}
	}

	onCredits := limits.Credits.HasCredits || limits.Credits.Unlimited
	u.Limited = limits.RateLimitReachedType != "" && !onCredits

	return u
}
