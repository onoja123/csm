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

type Codex struct {
	Path string
}

// These override per-profile login, which would make every profile the same account.
var codexAuthOverrideEnv = []string{"CODEX_ACCESS_TOKEN", "CODEX_API_KEY"}

const codexRPCTimeout = 20 * time.Second

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

func (c Codex) ID() string         { return CodexID }
func (c Codex) Name() string       { return "Codex" }
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
		Auth:       strings.Contains(help, "\n  login "),
		Supervise:  appServer && strings.Contains(help, "--no-daemon"),
		Resume:     strings.Contains(help, "\n  resume "),
		UsageProbe: appServer,
	}, nil
}

type codexCall struct {
	Method string
	Params any
}

type codexReply struct {
	Result json.RawMessage
	Err    error
}

type codexRequest struct {
	ID     *int   `json:"id,omitempty"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type codexResponse struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// rpc asks one short-lived `codex app-server` over stdio; it returns the Codex home the server reports and one reply per call.
func (c Codex) rpc(ctx context.Context, profileDir string, calls ...codexCall) (string, []codexReply, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Path, "app-server")
	cmd.Env = c.Env(profileDir)
	cmd.Dir = os.TempDir()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start codex app-server: %w", err)
	}
	defer func() {
		stdin.Close()
		cancel()
		cmd.Wait()
	}()

	enc := json.NewEncoder(stdin)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	// Notifications and server requests are skipped; only replies to csm's own numbered requests count.
	next := func() (codexResponse, error) {
		for sc.Scan() {
			var msg codexResponse
			if json.Unmarshal(sc.Bytes(), &msg) == nil && msg.ID != nil && msg.Method == "" {
				return msg, nil
			}
		}
		if err := ctx.Err(); err != nil {
			return codexResponse{}, fmt.Errorf("codex app-server did not answer: %w", err)
		}
		return codexResponse{}, errors.New("codex app-server exited before answering")
	}

	initID := 0
	clientInfo := map[string]string{"name": "csm", "title": "Code Session Manager", "version": "1"}
	if err := enc.Encode(codexRequest{ID: &initID, Method: "initialize", Params: map[string]any{"clientInfo": clientInfo}}); err != nil {
		return "", nil, err
	}
	msg, err := next()
	if err != nil {
		return "", nil, err
	}
	if msg.Error != nil {
		return "", nil, fmt.Errorf("codex app-server: %s", msg.Error.Message)
	}
	var init struct {
		CodexHome string `json:"codexHome"`
	}
	if err := json.Unmarshal(msg.Result, &init); err != nil {
		return "", nil, fmt.Errorf("codex app-server returned unexpected output: %w", err)
	}

	if err := enc.Encode(codexRequest{Method: "initialized"}); err != nil {
		return "", nil, err
	}
	for i, call := range calls {
		id := i + 1
		if err := enc.Encode(codexRequest{ID: &id, Method: call.Method, Params: call.Params}); err != nil {
			return "", nil, err
		}
	}
	replies := make([]codexReply, len(calls))
	for answered := 0; answered < len(calls); {
		msg, err := next()
		if err != nil {
			return "", nil, err
		}
		if *msg.ID < 1 || *msg.ID > len(calls) {
			continue
		}
		reply := codexReply{Result: msg.Result}
		if msg.Error != nil {
			reply.Err = errors.New(msg.Error.Message)
		}
		replies[*msg.ID-1] = reply
		answered++
	}
	return init.CodexHome, replies, nil
}

func (c Codex) call(profileDir, method string, params, result any) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), codexRPCTimeout)
	defer cancel()
	return c.callContext(ctx, profileDir, method, params, result)
}

func (c Codex) callContext(ctx context.Context, profileDir, method string, params, result any) (string, error) {
	home, replies, err := c.rpc(ctx, profileDir, codexCall{Method: method, Params: params})
	if err != nil {
		return "", err
	}
	if replies[0].Err != nil {
		return home, fmt.Errorf("codex %s: %w", method, replies[0].Err)
	}
	if err := json.Unmarshal(replies[0].Result, result); err != nil {
		return home, fmt.Errorf("codex %s returned unexpected output: %w", method, err)
	}
	return home, nil
}

// codexAccount is the subset of `account/read` csm reads; it never contains tokens.
type codexAccount struct {
	Account *struct {
		Type  string `json:"type"`
		Email string `json:"email"`
	} `json:"account"`
}

func (c Codex) Identity(profileDir string) (Identity, error) {
	var res codexAccount
	home, err := c.call(profileDir, "account/read", map[string]any{"refreshToken": false}, &res)
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

// LinkUserConfig symlinks ~/.codex settings into a profile; ~/.codex itself is never modified.
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

func (c Codex) ResumeArgs(sessionID string) []string {
	return []string{"resume", sessionID}
}

type codexThread struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	HistoryMode string `json:"historyMode"`
	UpdatedAt   int64  `json:"updatedAt"`
}

// CurrentSession is the interactive thread most recently updated in cwd since the agent started.
func (c Codex) CurrentSession(profileDir, cwd string, since time.Time) string {
	cwds := []string{cwd}
	if real, err := filepath.EvalSymlinks(cwd); err == nil && real != cwd {
		cwds = append(cwds, real)
	}
	var res struct {
		Data []codexThread `json:"data"`
	}
	params := map[string]any{"cwd": cwds, "sortKey": "updated_at", "limit": 1}
	if _, err := c.call(profileDir, "thread/list", params, &res); err != nil || len(res.Data) == 0 {
		return ""
	}
	if res.Data[0].UpdatedAt < since.Unix() {
		return ""
	}
	return res.Data[0].ID
}

// CarrySession copies a session's rollout file, kept under <profile>/sessions, to the same place in another profile.
func (c Codex) CarrySession(fromProfile, toProfile, sessionID string) (Carried, error) {
	var res struct {
		Thread codexThread `json:"thread"`
	}
	if _, err := c.call(fromProfile, "thread/read", map[string]any{"threadId": sessionID, "includeTurns": false}, &res); err != nil {
		return Carried{}, err
	}
	// Paginated history lives in a database, so the rollout file alone would resume as an empty session.
	if res.Thread.Path == "" || res.Thread.HistoryMode == "paginated" {
		return Carried{}, fmt.Errorf("codex session %s is not stored as a single rollout file", sessionID)
	}
	root, err := filepath.EvalSymlinks(fromProfile)
	if err != nil {
		return Carried{}, err
	}
	src, err := filepath.EvalSymlinks(res.Thread.Path)
	if err != nil {
		return Carried{}, err
	}
	rel, err := filepath.Rel(root, src)
	if err != nil || !filepath.IsLocal(rel) {
		return Carried{}, fmt.Errorf("codex session %s is stored outside its profile at %s", sessionID, src)
	}
	if err := copyFile(src, filepath.Join(toProfile, rel)); err != nil {
		return Carried{}, err
	}
	return Carried{Args: c.ResumeArgs(sessionID), SessionID: sessionID}, nil
}

func (c Codex) UsageCheckNote() string {
	return "A Codex live check reads the account's limits from OpenAI; it sends no message and uses no tokens."
}

func (c Codex) LimitPollInterval() time.Duration { return time.Minute }

func (c Codex) FetchUsage(ctx context.Context, profileDir string) (Usage, error) {
	var res codexRateLimits
	if _, err := c.callContext(ctx, profileDir, "account/rateLimits/read", nil, &res); err != nil {
		return Usage{}, err
	}
	return res.usage(), nil
}

type codexRateWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	ResetsAt           int64   `json:"resetsAt"`
	WindowDurationMins int64   `json:"windowDurationMins"`
}

// codexRateLimits is the subset of `account/rateLimits/read` csm reads.
type codexRateLimits struct {
	RateLimits struct {
		Primary         *codexRateWindow `json:"primary"`
		Secondary       *codexRateWindow `json:"secondary"`
		IndividualLimit *struct {
			RemainingPercent float64 `json:"remainingPercent"`
			ResetsAt         int64   `json:"resetsAt"`
		} `json:"individualLimit"`
		RateLimitReachedType *string `json:"rateLimitReachedType"`
		Credits              *struct {
			HasCredits bool `json:"hasCredits"`
			Unlimited  bool `json:"unlimited"`
		} `json:"credits"`
	} `json:"rateLimits"`
}

func (r codexRateLimits) usage() Usage {
	var u Usage
	limits := r.RateLimits
	for i, w := range []*codexRateWindow{limits.Primary, limits.Secondary} {
		if w == nil || w.ResetsAt == 0 {
			continue
		}
		window := UsageWindow{UsedPercent: w.UsedPercent, ResetsAt: time.Unix(w.ResetsAt, 0)}
		// Plans without a short window report the weekly one as primary.
		weekly := w.WindowDurationMins > 24*60 || (w.WindowDurationMins == 0 && i == 1)
		if weekly {
			u.SevenDay = window
		} else {
			u.FiveHour = window
		}
	}
	if l := limits.IndividualLimit; l != nil && l.ResetsAt != 0 {
		u.SpendLimit = UsageWindow{UsedPercent: 100 - l.RemainingPercent, ResetsAt: time.Unix(l.ResetsAt, 0)}
	}
	// With credits the account keeps working past its plan limit, so it is not treated as limited.
	onCredits := limits.Credits != nil && (limits.Credits.HasCredits || limits.Credits.Unlimited)
	u.Limited = limits.RateLimitReachedType != nil && !onCredits
	return u
}
