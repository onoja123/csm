package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onoja123/csm/internal/handoff"
	"github.com/onoja123/csm/internal/mcp"
	"github.com/onoja123/csm/internal/provider"
)

const handoffsDir = "handoffs"

func (s *State) handoffRoot(projectDir string) string {
	return filepath.Join(s.Home, handoffsDir, projectID(projectDir))
}

type handoffTarget struct {
	provider provider.Provider
	features provider.Features
	account  *Account
}

// buildHandoff captures the task context, the repository state and the MCP verdicts, writes the package and returns it. It never stops a switch: every problem becomes a note in the package.
func (r *runner) buildHandoff(source provider.Provider, from *Account, sessionID string, reason provider.StopReason, target handoffTarget, resumed bool) (*handoff.State, string) {
	now := time.Now()
	st := &handoff.State{
		Version:   handoff.Version,
		CreatedAt: now,
		Stage:     handoff.StageCheckpointed,
		Reason:    string(reason),
		Source:    handoff.Agent{Provider: source.ID(), Account: from.Name, ProfileDir: from.ConfigDir, SessionID: sessionID},
		Target:    handoff.Agent{Provider: target.provider.ID(), Account: target.account.Name, ProfileDir: target.account.ConfigDir},
		Resumed:   resumed,
	}
	st.ID = handoff.NewID(now, st.Source, st.Target)
	root := r.state.handoffRoot(r.project.Dir)

	if prev, _, err := handoff.Latest(root); err == nil {
		st.Previous = prev.ID
	}

	dir := filepath.Join(root, st.ID)
	contextOK := false

	if extractor, ok := provider.Extractor(source); ok && sessionID != "" {
		ctx, err := extractor.SessionContext(from.ConfigDir, r.cwd, sessionID)

		if err != nil {
			slog.Debug("extract session context", "account", from.Name, "err", err)
			ctx.Note = "The previous agent's session could not be read, so only the repository state is known."
		} else {
			contextOK = !ctx.Empty()
		}

		st.Context = handoff.Clean(ctx)
	} else if sessionID == "" {
		st.Context.Note = "No session was recorded for the previous agent."
	}

	st.Stage = handoff.StageContextExtracted
	r.step("Capturing task context", contextOK)

	var diff []byte

	st.Git, diff = handoff.CaptureGit(r.cwd)
	r.step("Capturing git state", st.Git.Root != "")

	rep := r.prepareMCP(source, from, target)
	st.MCP = rep.Results
	st.Stage = handoff.StageMCPPrepared

	if err := handoff.Write(dir, st, diff); err != nil {
		slog.Debug("write handoff", "err", err)
		r.step("Saving handoff", false)
		st.Error = err.Error()

		return st, ""
	}

	r.step("Saving handoff", true)

	return st, dir
}

func (r *runner) prepareMCP(source provider.Provider, from *Account, target handoffTarget) mcp.Report {
	srcAdapter, ok := provider.MCPAdapter(source)
	if !ok {
		return mcp.Report{}
	}

	dstAdapter, ok := provider.MCPAdapter(target.provider)
	if !ok {
		return mcp.Report{}
	}

	snap, err := mcp.Take(srcAdapter, from.ConfigDir, r.cwd, time.Now())
	if err != nil {
		slog.Debug("mcp snapshot unreadable", "account", from.Name, "err", err)
		r.step("Carrying MCP configuration (unreadable)", false)

		return mcp.Report{}
	}

	if len(snap.Servers) == 0 {
		os.Remove(filepath.Join(r.stateDir, mcpHandoffFile))

		return mcp.Report{}
	}

	rep := mcp.Handoff(snap, mcp.Target{Adapter: dstAdapter, ProfileDir: target.account.ConfigDir, ProjectDir: r.cwd, Label: target.account.Name}, envPresent, false, time.Now())

	if err := writeJSON(filepath.Join(r.stateDir, mcpHandoffFile), rep); err != nil {
		slog.Debug("save mcp handoff", "err", err)
	}

	r.step("Carrying MCP configuration", rep.Count(mcp.Failed, mcp.Unsupported) == 0)

	return rep
}

func injectionArgs(target handoffTarget, st *handoff.State, dir string) []string {
	injector, ok := provider.Injector(target.provider)
	if !ok || dir == "" {
		return nil
	}

	return injector.HandoffArgs(target.features, handoff.Instruction(*st, filepath.Join(dir, handoff.FileMD)))
}

func markStarted(st *handoff.State, dir string) {
	if dir == "" {
		return
	}

	st.Stage = handoff.StageTargetStarted

	if err := handoff.Save(dir, st); err != nil {
		slog.Debug("save handoff stage", "err", err)
	}
}

// nextRun is what a cross-agent switch hands back to cmdRun: start this provider's runner with these arguments.
type nextRun struct {
	providerID string
	args       []string
}

func (r *runner) crossSwitch(from *Account, res outcome) (*nextRun, error) {
	target, err := r.state.resolveAccount(res.crossTo)
	if err != nil {
		return nil, err
	}

	p, err := provider.Find(target.providerID())
	if err != nil {
		return nil, err
	}

	features, err := p.Features()
	if err != nil {
		return nil, err
	}

	r.log.Blank()
	r.log.Info("Code Session Manager")
	r.log.Blank()
	r.log.Info("Switching %s (%s) → %s (%s).\n", from.Name, r.provider.Name(), target.Name, p.Name())
	r.step("Saving checkpoint", true)
	r.step("Stopping "+r.provider.Name(), true)

	err = r.state.update(func(state *State) error {
		state.setActive(p.ID(), target.Name)

		return nil
	})
	if err != nil {
		return nil, err
	}

	r.step("Selecting "+target.Name, true)
	t := handoffTarget{provider: p, features: features, account: target}
	st, dir := r.buildHandoff(r.provider, from, res.sessionID, res.reason, t, false)
	args := injectionArgs(t, st, dir)
	r.step("Handing the task to "+p.Name(), len(args) > 0)
	r.step("Starting "+p.Name()+" as "+target.Name, true)
	r.log.Blank()

	switch {
	case dir == "":
		r.log.Info("The handoff could not be saved; %s starts without task context.", p.Name())
	case len(args) == 0:
		r.log.Info("This %s version cannot take an opening instruction. Ask it to read:\n\n    %s", p.Name(), filepath.Join(dir, handoff.FileMD))
	default:
		r.log.Info("%s cannot read a %s conversation. It starts with a task handoff instead:\n\n    %s", p.Name(), r.provider.Name(), filepath.Join(dir, handoff.FileMD))
	}

	r.log.Blank()
	r.printMCP(mcp.Report{Results: st.MCP})
	markStarted(st, dir)

	return &nextRun{providerID: p.ID(), args: args}, nil
}

func (s *State) latestCheckpoint(projectDir string) (Checkpoint, bool) {
	var best Checkpoint
	found := false

	for _, id := range provider.IDs {
		cp, err := s.loadCheckpoint(id, projectDir)

		if err == nil && (!found || cp.CheckpointedAt.After(best.CheckpointedAt)) {
			best, found = cp, true
		}
	}

	return best, found
}

func handoffUsage() error {
	return errors.New("usage: csm handoff <account|number> | csm handoff --provider " + strings.Join(provider.IDs, "|"))
}

func cmdHandoff(log *Logger, home string, args []string) (int, error) {
	s, err := loadState(home)
	if err != nil {
		return 1, err
	}

	var target *Account

	switch {
	case len(args) == 2 && args[0] == "--provider":
		if _, err := provider.New(args[1], ""); err != nil {
			return 1, err
		}

		name := s.active(args[1])
		if name == "" {
			return 1, fmt.Errorf("no active %s account; run: csm account add <name> --provider %s", providerName(args[1]), args[1])
		}

		target, err = s.resolveAccount(name)

	case len(args) == 1:
		target, err = s.resolveAccount(args[0])

	default:
		return 1, handoffUsage()
	}

	if err != nil {
		return 1, err
	}

	switch target.status(time.Now()) {
	case statusDisabled:
		return 1, fmt.Errorf("cannot hand off to %q: it is disabled.\n\nRun:\n\n    csm account enable %s", target.Name, target.Name)
	case statusNotAuthenticated:
		return 1, fmt.Errorf("cannot hand off to %q: the profile is not authenticated.\n\nRun:\n\n    csm account login %s", target.Name, target.Name)
	}

	p, err := provider.Find(target.providerID())
	if err != nil {
		return 1, err
	}

	if _, err := verifyProfile(p, target); err != nil {
		return 1, fmt.Errorf("cannot hand off to %q.\n\n%w", target.Name, err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return 1, err
	}

	proj := detectProject(cwd)
	interactive := isTerminal(os.Stdin.Fd())
	sess, live, err := findManagedSession(s, "")

	if err != nil {
		return 1, err
	}

	log.Info("CSM handoff")
	log.Blank()

	if live {
		if sess.Account == target.Name {
			log.Info("The running session is already using %s.", target.Name)

			return 0, nil
		}

		log.Row("Current agent", providerName(sess.Provider)+" / "+sess.Account)
		log.Row("Target agent", p.Name()+" / "+target.Name)

		if sess.SessionID != "" {
			log.Row("Session", sess.SessionID)
		}

		log.Row("Task context", "captured at the switch")
		log.Row("Git state", "captured at the switch")
		log.Blank()

		if interactive && !confirmLine(log, fmt.Sprintf("Continue with %s? [Y/n] ", p.Name())) {
			return 0, nil
		}

		return 0, requestSwitch(log, s, sess, target.Name)
	}

	cp, found := s.latestCheckpoint(proj.Dir)
	var source provider.Provider
	var from *Account

	if found {
		if from, err = s.resolveAccount(cp.Account); err == nil {
			source, err = provider.Find(providerOrDefault(cp.Provider))

			if err != nil {
				source, _ = provider.New(providerOrDefault(cp.Provider), "")
			}
		}
	}

	if from == nil {
		log.Row("Current agent", "none recorded for this project")
	} else {
		log.Row("Current agent", fmt.Sprintf("%s / %s (checkpoint %s ago)", source.Name(), from.Name, time.Since(cp.CheckpointedAt).Round(time.Second)))
	}

	log.Row("Target agent", p.Name()+" / "+target.Name)
	log.Row("Task context", map[bool]string{true: "from the last checkpoint", false: "none; repository state only"}[from != nil && cp.SessionID != ""])
	log.Row("Git state", map[bool]string{true: proj.Branch, false: "not a git repository"}[proj.IsGit])
	log.Blank()

	if interactive && !confirmLine(log, fmt.Sprintf("Start %s here? [Y/n] ", p.Name())) {
		return 0, nil
	}

	r, err := newRunner(log, s, p.ID())
	if err != nil {
		return 1, err
	}

	err = s.update(func(state *State) error {
		state.setActive(p.ID(), target.Name)

		return nil
	})
	if err != nil {
		return 1, err
	}

	t := handoffTarget{provider: p, features: r.features, account: target}
	var runArgs []string

	if from != nil {
		st, dir := r.buildHandoff(source, from, cp.SessionID, provider.StopReasonManualSwitch, t, false)
		runArgs = injectionArgs(t, st, dir)
		r.printMCP(mcp.Report{Results: st.MCP})

		if dir != "" {
			log.Info("Handoff: %s\n", filepath.Join(dir, handoff.FileMD))
		}

		markStarted(st, dir)
	} else {
		st, dir := r.buildHandoff(p, &Account{Name: "none", Provider: p.ID()}, "", provider.StopReasonManualSwitch, t, false)
		st.Source = handoff.Agent{}
		runArgs = injectionArgs(t, st, dir)
		markStarted(st, dir)
	}

	code, _, err := r.run(runArgs)

	return code, err
}

func confirmLine(log *Logger, prompt string) bool {
	log.Prompt(prompt)
	var line string

	fmt.Fscanln(os.Stdin, &line)
	answer := strings.ToLower(strings.TrimSpace(line))

	return answer == "" || answer == "y" || answer == "yes"
}

func doctorHandoff(log *Logger, s *State, usable map[string]provider.Provider, cwd string, fail func(label, reason string)) {
	root := filepath.Join(s.Home, handoffsDir)

	if err := os.MkdirAll(root, 0o700); err != nil {
		fail("Handoff directory", err.Error())
	} else if probe, err := os.CreateTemp(root, ".probe-*"); err != nil {
		fail("Handoff directory", err.Error())
	} else {
		probe.Close()
		os.Remove(probe.Name())
		log.Check("Handoff directory writable", true)
	}

	log.Row("Git repository here", map[bool]string{true: "yes", false: "no"}[detectProject(cwd).IsGit])

	for _, id := range provider.IDs {
		p, ok := usable[id]
		if !ok {
			continue
		}

		if _, ok := provider.Extractor(p); ok {
			log.Check(p.Name()+" task context extraction", true)
		} else {
			log.Row(p.Name()+" task context extraction", "not supported")
		}

		features, err := p.Features()
		if err != nil {
			continue
		}

		if provider.CanInject(p, features) {
			log.Check(p.Name()+" handoff injection", true)
		} else {
			fail(p.Name()+" handoff injection", "this version takes no opening instruction; a handed-over task must be pointed at handoff.md by hand")
		}
	}
}

func printLatestHandoff(log *Logger, s *State, projectDir string, now time.Time) {
	st, dir, err := handoff.Latest(s.handoffRoot(projectDir))
	if err != nil {
		return
	}

	log.Blank()
	log.Info("Last handoff")
	log.Info("  %s → %s, %s, %s ago", st.Source, st.Target, st.Stage, now.Sub(st.CreatedAt).Round(time.Second))
	log.Info("  %s", displayPath(filepath.Join(dir, handoff.FileMD)))
}
