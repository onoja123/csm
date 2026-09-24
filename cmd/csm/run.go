package main

import (
	"bufio"
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

	"github.com/onoja123/csm/internal/provider"
)

const (
	stopGracePeriod = 10 * time.Second
	usageCooldown   = time.Hour
)

// Written by the session-start hook; the ID changes on /clear or /resume.
const providerSessionFile = "provider-session.json"

type providerSession struct {
	SessionID string `json:"session_id"`
}

type runner struct {
	state    *State
	provider provider.Claude
	csmPath  string
	features provider.Features
	cwd      string
	project  Project
	stateDir string
	out      io.Writer
	in       *bufio.Reader
	// interactive is false under `csm test failover` and when stdin is not a TTY.
	interactive bool
	// Accounts limited during this run, so failover pauses instead of cycling.
	unavailable map[string]bool
}

type outcome struct {
	exitCode     int
	reason       provider.StopReason
	failure      provider.Failure
	switchTo     string
	sessionID    string
	noneLeft     bool
	autoDisabled bool
}

func (r *runner) step(label string, ok bool) {
	mark := "✓"
	if !ok {
		mark = "–"
	}
	fmt.Fprintf(r.out, "  %-34s %s\n", label, mark)
}

func (r *runner) run(userArgs []string) (int, error) {
	if sess, live := r.state.loadLiveSession(r.project.Dir); live {
		return 1, fmt.Errorf("csm is already managing %s for this project (csm pid %d, account %s).\n\nSwitch it with `csm use <name>` or `csm next`, or stop that session first.", r.provider.Name(), sess.CSMPID, sess.Account)
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
		return 1, err
	}
	if recovered != nil {
		args, sessionID = recovered.args, recovered.sessionID
	}

	for {
		acct, err := r.state.resolveAccount(r.state.Config.ActiveAccount)
		if err != nil {
			return 1, fmt.Errorf("no active account; add one with: csm account add <name>")
		}
		if _, err := verifyProfile(r.provider, acct); err != nil {
			return 1, fmt.Errorf("cannot start %s as %s.\n\n%w", r.provider.Name(), acct.Name, err)
		}

		res, err := r.launch(acct, args, sessionID, sigs)
		if err != nil {
			return 1, err
		}
		if res.switchTo == "" {
			r.report(acct, res)
			if res.reason == provider.StopReasonUsageLimit && !res.noneLeft && r.interactive {
				next, err := r.state.nextAccount(acct.Name, r.unavailable, time.Now())
				if err == nil && r.confirm(fmt.Sprintf("Switch to %s and resume this session? [Y/n] ", next.Name)) {
					args, sessionID, err = r.switchAccount(acct, next.Name, res)
					if err != nil {
						return 1, err
					}
					continue
				}
			}
			return res.exitCode, nil
		}
		args, sessionID, err = r.switchAccount(acct, res.switchTo, res)
		if err != nil {
			return 1, err
		}
	}
}

func (r *runner) launch(acct *Account, args []string, sessionID string, sigs chan os.Signal) (outcome, error) {
	os.Remove(filepath.Join(r.stateDir, eventFile))
	os.Remove(filepath.Join(r.stateDir, requestFile))
	os.Remove(filepath.Join(r.stateDir, providerSessionFile))

	if r.features.Hooks {
		args = append(r.provider.SettingsArgs(r.csmPath), args...)
	}
	cmd := exec.Command(r.provider.Path, args...)
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
	acct.LastUsedAt = startedAt
	if err := r.state.save(); err != nil {
		return outcome{}, err
	}
	sess := Session{Version: stateVersion, ProjectDir: r.project.Dir, Account: acct.Name, SessionID: sessionID, CSMPID: os.Getpid(), ProcessPID: cmd.Process.Pid, StartedAt: startedAt}
	if err := writeJSON(filepath.Join(r.stateDir, sessionFile), sess); err != nil {
		return outcome{}, err
	}
	defer os.Remove(filepath.Join(r.stateDir, sessionFile))
	os.Remove(filepath.Join(r.stateDir, transitionFile))

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	res := outcome{reason: provider.StopReasonProcessExited, sessionID: sessionID}
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
		case sig := <-sigs:
			switch sig {
			case syscall.SIGINT:
				// The terminal already delivered it to the agent, which shares our process group.
				interrupted = true
			case syscall.SIGTERM, syscall.SIGHUP:
				userStopped = true
				cmd.Process.Signal(sig)
			case syscall.SIGUSR1:
				r.handleFailure(acct, &res, startedAt, userStopped, stop)
			case syscall.SIGUSR2:
				r.handleSwitchRequest(acct, &res, startedAt, stop)
			}
		}
	}
	if killTimer != nil && !killTimer.Stop() {
		// The agent was killed without restoring the terminal.
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
	slog.Debug("agent exited", "account", acct.Name, "exit_code", res.exitCode, "reason", res.reason, "switch_to", res.switchTo)
	return res, nil
}

func (r *runner) currentSessionID(fallback string) string {
	var cs providerSession
	if readJSON(filepath.Join(r.stateDir, providerSessionFile), &cs) == nil && cs.SessionID != "" {
		return cs.SessionID
	}
	return fallback
}

func (r *runner) handleFailure(acct *Account, res *outcome, startedAt time.Time, userStopped bool, stop func()) {
	var f provider.Failure
	if err := readJSON(filepath.Join(r.stateDir, eventFile), &f); err != nil {
		slog.Debug("failure event unreadable", "err", err)
		return
	}
	res.failure = f
	res.reason = f.Reason
	if f.SessionID != "" {
		res.sessionID = f.SessionID
	}
	slog.Debug("agent reported failure", "account", acct.Name, "error", f.Error, "classification", f.Reason)
	if res.reason != provider.StopReasonUsageLimit || res.switchTo != "" {
		return
	}

	fresh, err := loadState(r.state.Home)
	if err != nil {
		slog.Debug("reload state", "err", err)
		return
	}
	*r.state = *fresh
	acct, err = r.state.resolveAccount(acct.Name)
	if err != nil {
		return
	}
	acct.CooldownUntil = time.Now().Add(usageCooldown)
	r.state.save()
	r.unavailable[acct.Name] = true

	if !r.state.Config.AutoFailover {
		res.autoDisabled = true
		return
	}
	if userStopped {
		return
	}
	next, err := r.state.nextAccount(acct.Name, r.unavailable, time.Now())
	if err != nil {
		res.noneLeft = true
		return
	}
	res.switchTo = next.Name
	r.beginSwitch(acct, next.Name, res, startedAt)
	stop()
}

func (r *runner) handleSwitchRequest(acct *Account, res *outcome, startedAt time.Time, stop func()) {
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
	if _, err := r.state.resolveAccount(req.To); err != nil {
		return
	}
	res.reason = provider.StopReasonManualSwitch
	res.switchTo = req.To
	r.beginSwitch(acct, req.To, res, startedAt)
	stop()
}

// Runs while the old agent is still alive, before it is stopped.
func (r *runner) beginSwitch(from *Account, to string, res *outcome, startedAt time.Time) {
	res.sessionID = r.currentSessionID(res.sessionID)
	now := time.Now()
	cp := Checkpoint{
		Version:        stateVersion,
		ProjectDir:     r.project.Dir,
		Account:        from.Name,
		SessionID:      res.sessionID,
		StartedAt:      startedAt,
		CheckpointedAt: now,
		Branch:         detectProject(r.cwd).Branch,
		Reason:         res.reason,
		Detail:         res.failure.Message,
	}
	if err := r.state.saveCheckpoint(cp); err != nil {
		slog.Debug("save checkpoint", "err", err)
	}
	t := Transition{Version: stateVersion, ProjectDir: r.project.Dir, From: from.Name, To: to, SessionID: res.sessionID, Reason: res.reason, CSMPID: os.Getpid(), StartedAt: now}
	if err := writeJSON(filepath.Join(r.stateDir, transitionFile), t); err != nil {
		slog.Debug("save transition", "err", err)
	}
}

func (r *runner) switchAccount(from *Account, to string, res outcome) ([]string, string, error) {
	if res.switchTo == "" {
		// Switch confirmed after exit, so nothing was checkpointed yet.
		res.sessionID = r.currentSessionID(res.sessionID)
		r.beginSwitch(from, to, &res, time.Now())
	}

	fmt.Fprintln(r.out)
	fmt.Fprintln(r.out, "Code Session Manager")
	fmt.Fprintln(r.out)
	if res.reason == provider.StopReasonManualSwitch {
		fmt.Fprintf(r.out, "Switching %s → %s.\n\n", from.Name, to)
	} else {
		fmt.Fprintf(r.out, "%s account became unavailable.\nReason: %s\n\n", from.Name, res.reason.Describe())
	}
	r.step("Saving checkpoint", true)
	r.step("Stopping "+r.provider.Name(), true)

	target, err := r.state.resolveAccount(to)
	if err != nil {
		return nil, "", err
	}
	r.state.Config.ActiveAccount = to
	if err := r.state.save(); err != nil {
		return nil, "", err
	}
	r.step("Selecting "+to, true)
	args, sessionID, resumed := r.handoffArgs(from, target, res.sessionID)
	r.printRestore(to, resumed)
	return args, sessionID, nil
}

// Falls back to a fresh session with a handoff note when the transcript is missing.
func (r *runner) handoffArgs(from, to *Account, sessionID string) (args []string, sessionIDOut string, resumed bool) {
	if sessionID != "" && r.features.CanContinue() {
		carried, err := r.provider.CarrySession(from.ConfigDir, to.ConfigDir, sessionID)
		if err != nil {
			slog.Debug("carry session", "err", err)
		}
		if carried {
			return r.provider.ResumeArgs(sessionID), sessionID, true
		}
	}
	sessionIDOut = provider.NewSessionID()
	return r.provider.NewSessionArgs(r.features, sessionIDOut, r.handoffNote(from.Name)), sessionIDOut, false
}

func (r *runner) printRestore(to string, resumed bool) {
	if resumed {
		r.step("Restoring session", true)
	} else {
		r.step("Restoring session (not available)", false)
	}
	r.step("Starting "+r.provider.Name()+" as "+to, true)
	fmt.Fprintln(r.out)
	if resumed {
		fmt.Fprintln(r.out, "The request that hit the limit was not retried; send it again or type \"continue\".")
	} else {
		fmt.Fprintf(r.out, "The conversation could not be carried over. Starting a new %s session\nin the same project with a handoff note.\n", r.provider.Name())
	}
	fmt.Fprintln(r.out)
}

func (r *runner) handoffNote(fromAccount string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This session continues work that was in progress in a previous %s session (account %q) in this directory. ", r.provider.Name(), fromAccount)
	b.WriteString("That conversation could not be transferred, so ask the user what they were working on if it is unclear. ")
	if status := gitStatusShort(r.project.Dir); status != "" {
		fmt.Fprintf(&b, "Current git status:\n%s", status)
	}
	return b.String()
}

type recovery struct {
	args      []string
	sessionID string
}

func (r *runner) recoverTransition() (*recovery, error) {
	t, ok, err := r.state.loadTransition(r.project.Dir)
	if err != nil || !ok || processAlive(t.CSMPID) {
		return nil, err
	}
	fmt.Fprintf(r.out, "Previous switch did not complete: %s → %s\n\n", t.From, t.To)
	if !r.state.Config.AutoFailover && !r.confirm("Resume the handoff? [Y/n] ") {
		os.Remove(filepath.Join(r.stateDir, transitionFile))
		return nil, nil
	}
	fmt.Fprintln(r.out, "Recovering previous account switch...")
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
	r.state.Config.ActiveAccount = to.Name
	if err := r.state.save(); err != nil {
		return nil, err
	}
	args, sessionID, resumed := r.handoffArgs(from, to, t.SessionID)
	r.printRestore(to.Name, resumed)
	return &recovery{args: args, sessionID: sessionID}, nil
}

func (r *runner) report(acct *Account, res outcome) {
	switch res.reason {
	case provider.StopReasonProcessExited, provider.StopReasonUserInterrupted:
		return
	case provider.StopReasonUsageLimit:
		fmt.Fprintf(r.out, "\n%s reported a usage limit on %s.\n", r.provider.Name(), acct.Name)
		switch {
		case res.noneLeft:
			fmt.Fprintln(r.out, "All configured accounts are currently unavailable. Automatic failover paused.")
		case res.autoDisabled:
			fmt.Fprintln(r.out, "Automatic failover is off (enable with: csm auto on).")
		}
	case provider.StopReasonUnknown:
		if res.failure.Error == "" {
			fmt.Fprintf(r.out, "\n%s exited with status %d. No account switch was made.\n", r.provider.Name(), res.exitCode)
			return
		}
		fallthrough
	default:
		fmt.Fprintf(r.out, "\n%s returned an error on %s: %s\n", r.provider.Name(), acct.Name, res.reason.Describe())
		if res.failure.Message != "" {
			fmt.Fprintf(r.out, "  %s\n", res.failure.Message)
		}
		fmt.Fprintln(r.out, "Automatic account switching only happens for a known usage limit.")
		if res.reason == provider.StopReasonAuthentication {
			fmt.Fprintf(r.out, "\nRun:\n\n    csm account login %s\n", acct.Name)
			return
		}
		fmt.Fprintln(r.out, "\nRun: csm status")
	}
}

func (r *runner) confirm(prompt string) bool {
	if !r.interactive {
		return false
	}
	fmt.Fprint(r.out, prompt)
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
