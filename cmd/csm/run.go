package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/onoja123/csm/internal/handoff"
	"github.com/onoja123/csm/internal/mcp"
	"github.com/onoja123/csm/internal/provider"
)

const (
	stopGracePeriod     = 10 * time.Second
	usageCooldown       = time.Hour
	exitCheckTimeout    = 10 * time.Second
	providerSessionFile = "provider-session.json"
)

type providerSession struct {
	SessionID string `json:"session_id"`
}

type runner struct {
	state        *State
	provider     provider.Provider
	csmPath      string
	features     provider.Features
	pollInterval time.Duration
	cwd          string
	project      Project
	stateDir     string
	log          *Logger
	in           *bufio.Reader
	interactive  bool
	unavailable  map[string]bool
}

type usageCheck struct {
	usage provider.Usage
	err   error
}

type outcome struct {
	startedAt    time.Time
	exitCode     int
	reason       provider.StopReason
	failure      provider.Failure
	switchTo     string
	crossTo      string
	sessionID    string
	noneLeft     bool
	autoDisabled bool
}

func (r *runner) step(label string, ok bool) {
	mark := "✓"

	if !ok {
		mark = "–"
	}

	r.log.Info("  %-34s %s", label, mark)
}

// run drives one agent until it exits for good, or until a switch to another agent asks cmdRun to start a new runner.
func (r *runner) run(userArgs []string) (int, *nextRun, error) {
	if sess, live := r.state.loadLiveSession(r.provider.ID(), r.project.Dir); live {
		return 1, nil, fmt.Errorf("csm is already managing %s for this project (csm pid %d, account %s).\n\nSwitch it with `csm use <name>` or `csm next`, or stop that session first.", r.provider.Name(), sess.CSMPID, sess.Account)
	}

	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2)

	defer signal.Stop(sigs)

	args := userArgs
	sessionID := ""

	if !r.provider.HasSessionFlag(userArgs) && r.features.SessionID {
		sessionID = provider.NewSessionID()
		args = append(r.provider.NewSessionArgs(r.features, sessionID, ""), userArgs...)
	}

	recovered, err := r.recoverTransition()
	if err != nil {
		return 1, nil, err
	}

	if recovered != nil {
		args, sessionID = recovered.args, recovered.sessionID
	}

	for {
		acct, err := r.state.resolveAccount(r.state.active(r.provider.ID()))
		if err != nil {
			return 1, nil, fmt.Errorf("no active %s account; add one with: csm account add <name> --provider %s", r.provider.Name(), r.provider.ID())
		}

		if _, err := verifyProfile(r.provider, acct); err != nil {
			return 1, nil, fmt.Errorf("cannot start %s as %s.\n\n%w", r.provider.Name(), acct.Name, err)
		}

		res, err := r.launch(acct, args, sessionID, sigs)
		if err != nil {
			return 1, nil, err
		}

		if res.crossTo != "" {
			next, err := r.crossSwitch(acct, res)
			if err != nil {
				return 1, nil, err
			}

			return 0, next, nil
		}

		if res.switchTo == "" {
			r.report(acct, res)

			if res.reason == provider.StopReasonUsageLimit && !res.noneLeft && r.interactive && r.state.Config.policy() == policyManual {
				next, err := r.state.bestAccount(r.provider.ID(), acct.Name, r.unavailable, time.Now())
				if err == nil && r.confirm(fmt.Sprintf("Switch to %s and resume this session? [Y/n] ", next.Name)) {
					args, sessionID, err = r.switchAccount(acct, next.Name, res)
					if err != nil {
						return 1, nil, err
					}

					continue
				}
			}

			return res.exitCode, nil, nil
		}

		args, sessionID, err = r.switchAccount(acct, res.switchTo, res)
		if err != nil {
			return 1, nil, err
		}
	}
}

func (r *runner) launch(acct *Account, args []string, sessionID string, sigs chan os.Signal) (outcome, error) {
	os.Remove(filepath.Join(r.stateDir, eventFile))
	os.Remove(filepath.Join(r.stateDir, requestFile))
	os.Remove(filepath.Join(r.stateDir, providerSessionFile))

	args = append(r.provider.LaunchArgs(r.csmPath), args...)
	cmd := exec.Command(r.provider.Executable(), args...)
	cmd.Dir = r.cwd
	cmd.Env = r.provider.Env(acct.ConfigDir,
		"CSM_STATE_DIR="+r.stateDir,
		"CSM_PID="+strconv.Itoa(os.Getpid()),
		"CSM_HOME="+r.state.Home,
		"CSM_ACCOUNT="+acct.Name,
		"CSM_STATUS_LINE_CHAIN="+r.provider.UserStatusLine(r.project.Dir, acct.ConfigDir),
	)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	slog.Debug("launching agent", "provider", r.provider.Name(), "account", acct.Name, "profile_dir", acct.ConfigDir, "args", r.provider.RedactArgs(args))

	if err := cmd.Start(); err != nil {
		return outcome{}, fmt.Errorf("start %s: %w", r.provider.Name(), err)
	}

	startedAt := time.Now()
	err := r.state.update(func(state *State) error {
		a, err := state.resolveAccount(acct.Name)
		if err != nil {
			return err
		}

		a.LastUsedAt = startedAt

		return nil
	})
	if err != nil {
		return outcome{}, err
	}

	sess := Session{Version: stateVersion, Provider: r.provider.ID(), ProjectDir: r.project.Dir, Account: acct.Name, SessionID: sessionID, CSMPID: os.Getpid(), ProcessPID: cmd.Process.Pid, StartedAt: startedAt}

	if err := writeJSON(filepath.Join(r.stateDir, sessionFile), sess); err != nil {
		return outcome{}, err
	}

	defer os.Remove(filepath.Join(r.stateDir, sessionFile))

	os.Remove(filepath.Join(r.stateDir, transitionFile))

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	res := outcome{startedAt: startedAt, reason: provider.StopReasonProcessExited, sessionID: sessionID}

	var pollTick <-chan time.Time

	if r.pollInterval > 0 {
		ticker := time.NewTicker(r.pollInterval)

		defer ticker.Stop()

		pollTick = ticker.C
	}

	polled := make(chan usageCheck, 1)
	polling := false
	profileDir, accountName := acct.ConfigDir, acct.Name

	var killTimer *time.Timer

	stop := func() {
		if killTimer != nil {
			return
		}

		cmd.Process.Signal(syscall.SIGTERM)
		killTimer = time.AfterFunc(stopGracePeriod, func() { cmd.Process.Kill() })
	}
	interrupted, userStopped := false, false

	var waitErr error

wait:
	for {
		select {
		case waitErr = <-done:
			break wait

		case <-pollTick:
			if polling {
				continue
			}

			polling = true
			go func() { polled <- r.fetchUsage(profileDir, usageFetchTimeout) }()

		case check := <-polled:
			polling = false

			if r.recordUsage(accountName, check) && res.reason != provider.StopReasonUsageLimit && res.switchTo == "" {
				r.handleFailure(acct, &res, r.limitFailure(), userStopped, stop)
			}

		case sig := <-sigs:
			switch sig {
			case syscall.SIGINT:
				interrupted = true

			case syscall.SIGTERM, syscall.SIGHUP:
				userStopped = true
				cmd.Process.Signal(sig)

			case syscall.SIGUSR1:
				var f provider.Failure

				if err := readJSON(filepath.Join(r.stateDir, eventFile), &f); err != nil {
					slog.Debug("failure event unreadable", "err", err)
					continue
				}

				r.handleFailure(acct, &res, f, userStopped, stop)

			case syscall.SIGUSR2:
				r.handleSwitchRequest(acct, &res, stop)
			}
		}
	}

	if killTimer != nil && !killTimer.Stop() {

		sane := exec.Command("stty", "sane")
		sane.Stdin = os.Stdin
		sane.Run()
	}

	res.exitCode = exitCode(waitErr)

	if res.reason == provider.StopReasonProcessExited && res.switchTo == "" {
		switch {
		case interrupted || userStopped:
			res.reason = provider.StopReasonUserInterrupted
		case res.exitCode != 0:
			res.reason = provider.StopReasonUnknown
		}
	}

	if r.pollInterval > 0 && r.interactive && !userStopped && res.switchTo == "" && res.reason != provider.StopReasonUsageLimit {
		if r.recordUsage(accountName, r.fetchUsage(profileDir, exitCheckTimeout)) {
			r.handleFailure(acct, &res, r.limitFailure(), true, func() {})
		}
	}

	if res.failure.Error == "" && (res.reason == provider.StopReasonProcessExited || res.reason == provider.StopReasonUserInterrupted || res.reason == provider.StopReasonManualSwitch) {
		if err := r.state.recordAccountSuccess(acct.Name, time.Now()); err != nil {
			slog.Debug("record success", "account", acct.Name, "err", err)
		}
	}

	slog.Debug("agent exited", "account", acct.Name, "exit_code", res.exitCode, "reason", res.reason, "switch_to", res.switchTo)

	return res, nil
}

func (r *runner) fetchUsage(profileDir string, timeout time.Duration) usageCheck {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	defer cancel()

	usage, err := r.provider.FetchUsage(ctx, profileDir)

	return usageCheck{usage: usage, err: err}
}

func (r *runner) recordUsage(account string, check usageCheck) bool {
	if check.err != nil {
		slog.Debug("usage check failed", "account", account, "err", check.err)

		return false
	}

	if err := r.state.saveUsage(UsageRecord{Version: stateVersion, Account: account, UpdatedAt: time.Now(), Usage: check.usage}); err != nil {
		slog.Debug("save usage", "account", account, "err", err)
	}

	return check.usage.Limited
}

func (r *runner) limitFailure() provider.Failure {
	return provider.Failure{
		Error:   "usage_limit",
		Message: r.provider.Name() + " reports that this account has reached its usage limit.",
		Reason:  provider.StopReasonUsageLimit,
	}
}

func (r *runner) currentSessionID(acct *Account, since time.Time, fallback string) string {
	var cs providerSession

	if readJSON(filepath.Join(r.stateDir, providerSessionFile), &cs) == nil && cs.SessionID != "" {
		return cs.SessionID
	}

	id, err := r.provider.CurrentSession(acct.ConfigDir, r.cwd, since)
	if err != nil {
		slog.Debug("find current session", "err", err)
	}

	if id != "" {
		return id
	}

	return fallback
}

func (r *runner) handleFailure(acct *Account, res *outcome, f provider.Failure, userStopped bool, stop func()) {
	res.failure = f
	res.reason = f.Reason

	if f.SessionID != "" {
		res.sessionID = f.SessionID
	}

	slog.Debug("agent reported failure", "account", acct.Name, "error", f.Error, "classification", f.Reason)

	if f.Reason != provider.StopReasonUserInterrupted {
		if err := r.state.recordAccountFailure(acct.Name, f.Reason, time.Now()); err != nil {
			slog.Debug("record failure", "account", acct.Name, "err", err)
		}
	}

	if res.reason != provider.StopReasonUsageLimit || res.switchTo != "" {
		return
	}

	err := r.state.update(func(state *State) error {
		a, err := state.resolveAccount(acct.Name)
		if err != nil {
			return err
		}

		a.CooldownUntil = time.Now().Add(usageCooldown)

		return nil
	})
	if err != nil {
		slog.Debug("save cooldown", "account", acct.Name, "err", err)

		return
	}

	r.unavailable[acct.Name] = true

	if r.state.Config.policy() != policyAutomatic {
		res.autoDisabled = true

		return
	}

	if userStopped {
		return
	}

	next, err := r.state.bestAccount(r.provider.ID(), acct.Name, r.unavailable, time.Now())
	if err != nil {
		res.noneLeft = true

		return
	}

	res.switchTo = next.Name
	r.beginSwitch(acct, next.Name, res)
	stop()
}

func (r *runner) handleSwitchRequest(acct *Account, res *outcome, stop func()) {
	var req SwitchRequest

	path := filepath.Join(r.stateDir, requestFile)

	if err := readJSON(path, &req); err != nil {
		return
	}

	os.Remove(path)

	if res.switchTo != "" || req.To == acct.Name {
		return
	}

	fresh, err := loadState(r.state.Home)
	if err != nil {
		return
	}

	*r.state = *fresh
	target, err := r.state.resolveAccount(req.To)

	if err != nil {
		return
	}

	res.reason = provider.StopReasonManualSwitch

	if target.providerID() != r.provider.ID() {
		if _, err := provider.Find(target.providerID()); err != nil {
			slog.Debug("cross-agent switch refused", "to", req.To, "err", err)

			return
		}

		res.crossTo = req.To
		r.beginSwitch(acct, req.To, res)
		stop()

		return
	}

	res.switchTo = req.To
	r.beginSwitch(acct, req.To, res)
	stop()
}

func (r *runner) beginSwitch(from *Account, to string, res *outcome) {
	res.sessionID = r.currentSessionID(from, res.startedAt, res.sessionID)
	now := time.Now()
	cp := Checkpoint{
		Version:        stateVersion,
		Provider:       r.provider.ID(),
		ProjectDir:     r.project.Dir,
		Account:        from.Name,
		SessionID:      res.sessionID,
		StartedAt:      res.startedAt,
		CheckpointedAt: now,
		Branch:         detectProject(r.cwd).Branch,
		Reason:         res.reason,
		Detail:         res.failure.Message,
	}

	if err := r.state.saveCheckpoint(cp); err != nil {
		slog.Debug("save checkpoint", "err", err)
	}

	r.snapshotMCP(from)

	t := Transition{Version: stateVersion, Provider: r.provider.ID(), ProjectDir: r.project.Dir, From: from.Name, To: to, SessionID: res.sessionID, Reason: res.reason, CSMPID: os.Getpid(), StartedAt: now}
	transitionDir := r.stateDir

	if target, err := r.state.resolveAccount(to); err == nil && target.providerID() != r.provider.ID() {
		t.Provider = target.providerID()
		transitionDir = r.state.projectStateDir(t.Provider, r.project.Dir)
		os.MkdirAll(transitionDir, 0o700)
	}

	if err := writeJSON(filepath.Join(transitionDir, transitionFile), t); err != nil {
		slog.Debug("save transition", "err", err)
	}
}

func (r *runner) switchAccount(from *Account, to string, res outcome) ([]string, string, error) {
	if res.switchTo == "" {
		r.beginSwitch(from, to, &res)
	}

	r.log.Blank()
	r.log.Info("Code Session Manager")
	r.log.Blank()

	if res.reason == provider.StopReasonManualSwitch {
		r.log.Info("Switching %s → %s.\n", from.Name, to)
	} else {
		r.log.Info("%s account became unavailable.\nReason: %s\n", from.Name, res.reason.Describe())
	}

	r.step("Saving checkpoint", true)
	r.step("Stopping "+r.provider.Name(), true)

	target, err := r.state.resolveAccount(to)

	if err != nil {
		return nil, "", err
	}

	err = r.state.update(func(state *State) error {
		state.setActive(r.provider.ID(), to)

		return nil
	})
	if err != nil {
		return nil, "", err
	}

	r.step("Selecting "+to, true)
	args, sessionID, resumed, st := r.handoffArgs(from, target, res.sessionID, res.reason)
	r.printRestore(to, resumed)
	r.printMCP(mcp.Report{Results: st.MCP})

	return args, sessionID, nil
}

// handoffArgs carries the native session when the agent can resume it, builds the handoff package either way, and adds the agent's way of receiving the instruction.
func (r *runner) handoffArgs(from, to *Account, sessionID string, reason provider.StopReason) (args []string, sessionIDOut string, resumed bool, st *handoff.State) {
	source := r.provider

	if from.providerID() != r.provider.ID() {
		source, _ = provider.New(from.providerID(), "")
	}

	if sessionID != "" && r.features.CanContinue() && source.ID() == r.provider.ID() {
		resumeArgs, newSessionID, err := r.provider.CarrySession(from.ConfigDir, to.ConfigDir, sessionID)
		if err != nil {
			slog.Debug("carry session", "err", err)
		}

		if len(resumeArgs) > 0 {
			args, sessionIDOut, resumed = resumeArgs, newSessionID, true
		}
	}

	if !resumed && r.features.SessionID {
		sessionIDOut = provider.NewSessionID()
		args = r.provider.NewSessionArgs(r.features, sessionIDOut, "")
	}

	target := handoffTarget{provider: r.provider, features: r.features, account: to}
	st, dir := r.buildHandoff(source, from, sessionID, reason, target, resumed)
	args = append(args, injectionArgs(target, st, dir)...)
	markStarted(st, dir)

	return args, sessionIDOut, resumed, st
}

func (r *runner) printRestore(to string, resumed bool) {

	if resumed {
		r.step("Restoring session", true)
	} else {
		r.step("Restoring session (not available)", false)
	}

	r.step("Starting "+r.provider.Name()+" as "+to, true)
	r.log.Blank()

	if resumed {
		r.log.Info("The request that hit the limit was not retried; send it again or type \"continue\".")
	} else {
		r.log.Info("The conversation could not be carried over. Starting a new %s session\nin the same project with a task handoff.", r.provider.Name())
	}

	r.log.Blank()
}

type recovery struct {
	args      []string
	sessionID string
}

func (r *runner) recoverTransition() (*recovery, error) {
	t, ok, err := r.state.loadTransition(r.provider.ID(), r.project.Dir)

	if err != nil || !ok || processAlive(t.CSMPID) {
		return nil, err
	}

	r.log.Info("Previous switch did not complete: %s → %s\n", t.From, t.To)

	if r.state.Config.policy() != policyAutomatic && !r.confirm("Resume the handoff? [Y/n] ") {
		os.Remove(filepath.Join(r.stateDir, transitionFile))

		return nil, nil
	}

	r.log.Info("Recovering previous account switch...")
	from, err := r.state.resolveAccount(t.From)

	if err != nil {
		return nil, err
	}

	to, err := r.state.resolveAccount(t.To)

	if err != nil {
		return nil, fmt.Errorf("the switch target %q no longer exists; run: csm accounts", t.To)
	}

	r.step("Checkpoint found", true)

	if _, err := os.Stat(r.project.Dir); err != nil {
		return nil, fmt.Errorf("project directory %s is missing", r.project.Dir)
	}

	r.step("Project verified", true)

	if _, err := verifyProfile(r.provider, to); err != nil {
		return nil, err
	}

	r.step("Target account "+to.Name+" verified", true)
	err = r.state.update(func(state *State) error {
		state.setActive(r.provider.ID(), to.Name)

		return nil
	})
	if err != nil {
		return nil, err
	}

	args, sessionID, resumed, st := r.handoffArgs(from, to, t.SessionID, t.Reason)
	r.printRestore(to.Name, resumed)
	r.printMCP(mcp.Report{Results: st.MCP})

	return &recovery{args: args, sessionID: sessionID}, nil
}

func (r *runner) report(acct *Account, res outcome) {
	switch res.reason {
	case provider.StopReasonProcessExited, provider.StopReasonUserInterrupted:
		return

	case provider.StopReasonUsageLimit:
		r.log.Info("\n%s reported a usage limit on %s.", r.provider.Name(), acct.Name)

		switch {
		case res.noneLeft:
			r.log.Info("All configured accounts are currently unavailable. Automatic failover paused.")
		case res.autoDisabled && r.state.Config.policy() == policyDisabled:
			r.log.Info("Failover is disabled (enable with: csm auto automatic).")
		case res.autoDisabled:
			r.log.Info("Automatic failover is off (enable with: csm auto automatic).")
		}

	case provider.StopReasonUnknown:
		if res.failure.Error == "" {
			r.log.Info("\n%s exited with status %d. No account switch was made.", r.provider.Name(), res.exitCode)

			return
		}

		fallthrough

	default:
		r.log.Info("\n%s returned an error on %s: %s", r.provider.Name(), acct.Name, res.reason.Describe())

		if res.failure.Message != "" {
			r.log.Info("  %s", res.failure.Message)
		}

		r.log.Info("Automatic account switching only happens for a known usage limit.")

		if res.reason == provider.StopReasonAuthentication {
			r.log.Info("\nRun:\n\n    csm account login %s", acct.Name)

			return
		}

		r.log.Info("\nRun: csm status")
	}
}

func (r *runner) confirm(prompt string) bool {
	if !r.interactive {
		return false
	}

	r.log.Prompt(prompt)
	line, err := r.in.ReadString('\n')

	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}

	answer := strings.ToLower(strings.TrimSpace(line))

	return answer == "" || answer == "y" || answer == "yes"
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}

	var exitErr *exec.ExitError

	if errors.As(err, &exitErr) {

		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}

		return exitErr.ExitCode()
	}

	return 1
}
