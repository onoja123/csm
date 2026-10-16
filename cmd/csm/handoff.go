package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	saved, haveSaved := r.savedCheckpoint(root, sessionID)
	var contextOK bool

	st.Context, contextOK = extractContext(source, from, r.cwd, sessionID)

	if haveSaved {
		st.Checkpoint = saved.ID
		st.Edits = saved.Edits

		if !contextOK && !saved.Context.Empty() {
			st.Context, contextOK = saved.Context, true
		}
	}

	st.Stage = handoff.StageContextExtracted
	r.step("Capturing task context", contextOK)

	if commit := r.commitBuildWork(st); commit != "" {
		st.SourceCommit = commit
		r.step("Committing "+st.Source.String()+"'s work", true)
	}

	var diff []byte

	st.Git, diff = handoff.CaptureGit(r.cwd)
	r.step("Capturing git state", st.Git.Root != "")

	rep := r.prepareMCP(source, from, target)
	st.MCP = rep.Results
	st.Stage = handoff.StageMCPPrepared

	if st.Skills = r.carrySkills(source, from, sessionID, target); st.Skills != nil {
		st.Stage = handoff.StageSkillsPrepared
		r.step("Carrying skills", len(st.Skills.NotCarried) == 0)

		if os.Getenv("CSM_TASK_SKILLS") == "" {
			st.Skills.Source, st.Skills.Manifest = st.Skills.Manifest, filepath.Join(dir, skillsManifestFile)
		}
	}

	reference := *st

	if haveSaved {
		reference = saved
	}

	out := &handoff.Outcome{Status: handoff.OutcomeStarting, Delivery: handoff.DeliveryNone, Verification: handoff.Verify(reference, r.cwd, now)}

	if !contextOK && st.Edits == nil {
		out.Problems = append(out.Problems, "No task context could be read from the previous agent; only the repository state was handed over.")
	}

	if n := rep.Count(mcp.Failed); n > 0 {
		out.Problems = append(out.Problems, fmt.Sprintf("%d MCP server(s) could not be carried over.", n))
	}

	if st.Skills != nil && len(st.Skills.NotCarried) > 0 {
		out.Problems = append(out.Problems, fmt.Sprintf("%d skill(s) could not be carried over.", len(st.Skills.NotCarried)))
	}

	out.Problems = append(out.Problems, out.Verification.Problems()...)
	st.Outcome = out

	if haveSaved {
		r.step("Checking the repository against checkpoint "+saved.ID, out.Verification.Clean())
	}

	if err := handoff.Write(dir, st, diff); err != nil {
		slog.Debug("write handoff", "err", err)
		r.step("Saving handoff", false)
		st.Error = err.Error()

		return st, ""
	}

	r.step("Saving handoff", true)

	if st.Skills != nil && st.Skills.Manifest != "" {
		if data, err := os.ReadFile(orText(st.Skills.Source, st.Skills.Manifest)); err == nil {
			if err := os.WriteFile(filepath.Join(dir, skillsManifestFile), data, 0o600); err != nil {
				slog.Debug("copy skill manifest", "err", err)
			}
		}
	}

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

	instruction := handoff.Instruction(*st, filepath.Join(dir, handoff.FileMD))

	if contract := buildContract(); contract != "" {
		if evidence := buildEvidence(); evidence != "" {
			contract = evidence + "\n\n" + contract
		}

		if starter, ok := provider.Starter(target.provider); ok {
			return starter.TaskArgs(target.features, instruction+"\n\n"+contract)
		}
	}

	return injector.HandoffArgs(target.features, instruction)
}

// buildContract is the Build Room completion contract of the task this runner works on, so an agent that takes over still knows where to report and that it should start at once.
func buildContract() string {
	path := os.Getenv("CSM_BUILD_CONTRACT")

	if path == "" {
		return ""
	}

	data, err := os.ReadFile(path)
	if err != nil || len(data) > 64<<10 {
		return ""
	}

	return strings.TrimSpace(string(data))
}

// buildEvidence summarizes the receipt of the run this runner works for, so the receiving agent knows what CSM has
// verified and what it has not; the full receipt stays on disk.
func buildEvidence() string {
	id := os.Getenv("CSM_BUILD_ID")
	if !validBuildID(id) {
		return ""
	}

	home, err := csmHome()
	if err != nil {
		return ""
	}

	b, err := newBuildStore(home).load(id)
	if err != nil {
		return ""
	}

	cwd, _ := os.Getwd()
	head, _ := gitOut(cwd, "rev-parse", "HEAD")

	return receiptBrief(newReceipt(b, time.Now()), head)
}

// armHandoff records how the brief will reach the agent; the runner settles the outcome once the agent has been launched.
func (r *runner) armHandoff(st *handoff.State, dir string, args []string) string {
	if dir == "" {
		return ""
	}

	if st.Outcome == nil {
		st.Outcome = &handoff.Outcome{Status: handoff.OutcomeStarting}
	}

	switch {
	case len(args) > 0:
		st.Outcome.Delivery = handoff.DeliveryInstruction
	default:
		st.Outcome.Delivery = handoff.DeliveryFileOnly
		st.Outcome.Problems = append(st.Outcome.Problems, "This agent version takes no opening instruction; it has to be pointed at handoff.md by hand.")
	}

	if err := handoff.Save(dir, st); err != nil {
		slog.Debug("save handoff stage", "err", err)
	}

	return dir
}

// savedCheckpoint is the checkpoint a handoff continues: the one the user named, else the newest one saved from this session.
func (r *runner) savedCheckpoint(root, sessionID string) (handoff.State, bool) {
	if r.checkpointID != "" {
		st, _, err := r.state.findHandoff(r.checkpointID)
		r.checkpointID = ""

		if err == nil && st.Kind == handoff.KindCheckpoint {
			return st, true
		}

		slog.Debug("named checkpoint unusable", "err", err)
	}

	if sessionID == "" {
		return handoff.State{}, false
	}

	st, _, err := handoff.LatestCheckpoint(root, sessionID)

	return st, err == nil
}

// nextRun is what a cross-agent switch hands back to cmdRun: start this provider's runner with these arguments.
type nextRun struct {
	providerID string
	args       []string
	handoffDir string
	auto       bool
	autoHops   int
	tried      map[string]bool
	target     string
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

	if res.autoHandoff {
		r.log.Info("%s reached its usage limit and no other %s account is ready.", from.Name, r.provider.Name())
		r.log.Info("Automatic handoff: %s (%s) → %s (%s).\n", from.Name, r.provider.Name(), target.Name, p.Name())
	} else {
		r.log.Info("Switching %s (%s) → %s (%s).\n", from.Name, r.provider.Name(), target.Name, p.Name())
	}
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

	if res.autoHandoff && dir == "" {
		r.log.Blank()
		r.log.Info("Automatic handoff stopped: the task context could not be saved (%s).", st.Error)
		r.log.Info("%s was not started, so nothing runs without the task. Your working tree is untouched.", p.Name())
		r.log.Info("Fix the problem and continue with: csm handoff %s", target.Name)

		return nil, errors.New("automatic handoff stopped before starting " + p.Name())
	}

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

	tried := map[string]bool{r.provider.ID(): true}

	for id := range r.tried {
		tried[id] = true
	}

	next := &nextRun{providerID: p.ID(), args: args, handoffDir: r.armHandoff(st, dir, args), auto: res.autoHandoff, autoHops: r.autoHops, tried: tried, target: target.Name}

	if res.autoHandoff {
		next.autoHops++
	}

	return next, nil
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
	return errors.New("usage: csm handoff [--yes] [--checkpoint <id>] <account|number> | csm handoff [--yes] [--checkpoint <id>] --provider " + strings.Join(provider.IDs, "|"))
}

func cmdHandoff(log *Logger, home string, args []string) (int, error) {
	s, err := loadState(home)
	if err != nil {
		return 1, err
	}

	var target *Account

	yes := false
	checkpointID := ""
	args = slices.DeleteFunc(slices.Clone(args), func(a string) bool {
		if a == "--yes" || a == "-y" {
			yes = true

			return true
		}

		return false
	})

	if i := slices.Index(args, "--checkpoint"); i >= 0 {
		if i+1 >= len(args) {
			return 1, handoffUsage()
		}

		checkpointID = args[i+1]
		args = slices.Delete(args, i, i+2)
	}

	var saved handoff.State

	if checkpointID != "" {
		var err error

		if saved, _, err = s.findHandoff(checkpointID); err != nil {
			return 1, err
		}

		if saved.Kind != handoff.KindCheckpoint {
			return 1, fmt.Errorf("%s is a handoff record, not a saved checkpoint", checkpointID)
		}
	}

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

	if checkpointID != "" && saved.Git.Root != "" && detectProject(saved.Git.Root).Dir != proj.Dir {
		return 1, fmt.Errorf("checkpoint %s belongs to %s; run this from that project", checkpointID, displayPath(saved.Git.Root))
	}

	interactive := isTerminal(os.Stdin.Fd()) && !yes
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

		if checkpointID != "" {
			log.Row("Checkpoint", checkpointID)
		}

		log.Blank()

		if interactive && !confirmLine(log, fmt.Sprintf("Continue with %s? [Y/n] ", p.Name())) {
			return 0, nil
		}

		return 0, requestSwitchWith(log, s, sess, target.Name, checkpointID)
	}

	cp, found := s.latestCheckpoint(proj.Dir)

	if checkpointID != "" && saved.Source.Account != "" {
		cp = Checkpoint{Provider: saved.Source.Provider, Account: saved.Source.Account, SessionID: saved.Source.SessionID, ProjectDir: proj.Dir, CheckpointedAt: saved.CreatedAt}
		found = true
	}

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

	switch {
	case checkpointID != "":
		log.Row("Task context", "from checkpoint "+checkpointID)
	case from == nil || source == nil || cp.SessionID == "":
		log.Row("Task context", "none; repository state only")
	default:
		if _, readable := provider.Extractor(source); !readable {
			log.Row("Task context", "repository state only; "+source.Name()+" sessions cannot be read")
		} else {
			log.Row("Task context", "from the last checkpoint")
		}
	}

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

	r.checkpointID = checkpointID

	if from != nil {
		st, dir := r.buildHandoff(source, from, cp.SessionID, provider.StopReasonManualSwitch, t, false)
		runArgs = injectionArgs(t, st, dir)
		r.printMCP(mcp.Report{Results: st.MCP})

		if dir != "" {
			log.Info("Handoff: %s\n", filepath.Join(dir, handoff.FileMD))
		}

		r.pendingHandoff = r.armHandoff(st, dir, runArgs)
	} else {
		st, dir := r.buildHandoff(p, &Account{Name: "none", Provider: p.ID()}, "", provider.StopReasonManualSwitch, t, false)
		st.Source = handoff.Agent{}
		runArgs = injectionArgs(t, st, dir)
		r.pendingHandoff = r.armHandoff(st, dir, runArgs)
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

// inBuildWorktree reports whether this runner works in a worktree Build Room created, the only place csm commits for an agent.
func (r *runner) inBuildWorktree() bool {
	if os.Getenv("CSM_BUILD_ID") == "" {
		return false
	}

	root, err := filepath.EvalSymlinks(filepath.Join(r.state.Home, buildsDir))
	if err != nil {
		return false
	}

	dir, err := filepath.EvalSymlinks(r.project.Dir)

	return err == nil && !outside(root, dir) && dir != root
}

// commitBuildWork records the outgoing agent's changes before another takes over, so the branch history shows which agent wrote what.
func (r *runner) commitBuildWork(st *handoff.State) string {
	if !r.inBuildWorktree() {
		return ""
	}

	msg := fmt.Sprintf("Work by %s before handoff to %s\n\n%s", st.Source, st.Target, agentTrailers(st.Source.String(), st.Source.SessionID, st.ID))

	head, committed, err := commitAll(r.cwd, msg)
	if err != nil {
		slog.Debug("commit build work at handoff", "err", err)

		return ""
	}

	if !committed {
		return ""
	}

	return head
}

func agentTrailers(agent, sessionID, handoffID string) string {
	lines := []string{"CSM-Agent: " + agent}

	if sessionID != "" {
		lines = append(lines, "CSM-Session: "+sessionID)
	}

	if handoffID != "" {
		lines = append(lines, "CSM-Handoff: "+handoffID)
	}

	if id := os.Getenv("CSM_BUILD_ID"); validBuildID(id) {
		lines = append(lines, "CSM-Build: "+id)
	}

	return strings.Join(lines, "\n")
}
