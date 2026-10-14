package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/onoja123/csm/internal/handoff"
	"github.com/onoja123/csm/internal/provider"
	"github.com/onoja123/csm/internal/sandbox"
)

const (
	buildTick        = time.Second
	reportStopGrace  = 5 * time.Second
	buildBranchRoot  = "csm/build-"
	opIntegrate      = "integrate"
	opValidate       = "validate"
	opBaseline       = "baseline"
	opPR             = "pr"
	opPush           = "push"
	findingOpen      = "open"
	findingFixAsked  = "fix_requested"
	findingRechecked = "rechecked"
	findingAccepted  = "accepted"
)

// buildEngine drives every Build Run from the service. All state changes happen under mu and are saved before the lock is released; slow git and check work runs in ops whose results are applied under the lock.
type buildEngine struct {
	mu     sync.Mutex
	store  buildStore
	home   string
	builds map[string]*Build
	runs   *runManager
	log    *activityLog
	bus    *eventBus
	ops    map[string]map[string]bool
	wake   chan struct{}
	done   chan struct{}
	// sandbox runs AutoFix reproductions and checks; tests replace it with a deterministic runner.
	sandbox sandbox.Runner
	closed  bool
	wg      sync.WaitGroup
}

func newBuildEngine(home string, runs *runManager, log *activityLog, bus *eventBus) *buildEngine {
	e := &buildEngine{store: newBuildStore(home), home: home, builds: map[string]*Build{}, runs: runs, log: log, bus: bus, ops: map[string]map[string]bool{}, wake: make(chan struct{}, 1), done: make(chan struct{}), sandbox: sandbox.Docker{}}

	for _, b := range e.store.all() {
		e.builds[b.ID] = b
	}

	return e
}

func (e *buildEngine) start() {
	e.wg.Add(1)

	go func() {
		defer e.wg.Done()

		t := time.NewTicker(buildTick)
		defer t.Stop()

		for {
			e.step()

			select {
			case <-e.done:
				return
			case <-t.C:
			case <-e.wake:
			}
		}
	}()
}

func (e *buildEngine) close() {
	e.mu.Lock()

	if e.closed {
		e.mu.Unlock()

		return
	}

	e.closed = true
	close(e.done)
	e.mu.Unlock()
	e.wg.Wait()
}

func (e *buildEngine) poke() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *buildEngine) state() (*State, error) { return loadState(e.home) }

func (e *buildEngine) step() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return
	}

	ids := make([]string, 0, len(e.builds))

	for id := range e.builds {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	for _, id := range ids {
		b := e.builds[id]

		if b.Status.Terminal() {
			continue
		}

		before, _ := json.Marshal(b)

		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("build step panicked", "build", b.ID, "panic", r)
					e.attention(b, fmt.Sprintf("CSM hit an internal error while advancing this run: %v", r), "retry")
				}
			}()

			e.advance(b)
		}()

		if after, _ := json.Marshal(b); string(before) != string(after) {
			e.save(b)
		}
	}
}

func (e *buildEngine) save(b *Build) {
	if err := e.store.save(b); err != nil {
		slog.Error("save build", "build", b.ID, "err", err)
	}

	e.store.saveReceipt(b)

	e.bus.publish("build", map[string]string{"id": b.ID, "status": string(b.Status)})
}

// event records one thing that happened to a run, in its own history and in the shared activity log.
func (e *buildEngine) event(b *Build, kind, taskID, msg string) {
	msg = clean(msg, 600)
	e.store.appendEvent(b.ID, BuildEvent{At: time.Now(), Kind: kind, TaskID: taskID, Message: msg})

	refs := map[string]string{"build": b.ID}

	if taskID != "" {
		refs["task"] = taskID
	}

	a := Activity{Kind: "build." + kind, Title: msg, Detail: b.Title, Refs: refs}

	if t := b.task(taskID); t != nil {
		a.Provider, a.Account = t.Provider, t.Account

		if n := len(t.Attempts); n > 0 {
			a.RunID = t.Attempts[n-1].RunID
		}
	}

	e.log.add(a)
}

func (e *buildEngine) setStatus(b *Build, to BuildStatus) bool {
	if b.Status == to {
		return true
	}

	from := b.Status

	if err := b.transition(to); err != nil {
		slog.Error("build transition", "build", b.ID, "err", err)

		return false
	}

	slog.Debug("build status", "build", b.ID, "from", from, "to", to)
	b.logState()

	return true
}

func (e *buildEngine) attention(b *Build, reason string, recovery ...string) {
	if b.Status == BuildNeedsAttention && b.Attention == reason {
		return
	}

	if !e.setStatus(b, BuildNeedsAttention) {
		return
	}

	b.Attention, b.Recovery = reason, recovery
	e.event(b, "attention", "", reason)
}

func (e *buildEngine) busy(b *Build, op string) bool { return e.ops[b.ID][op] }

func (e *buildEngine) anyBusy(b *Build) bool { return len(e.ops[b.ID]) > 0 }

// async runs slow work outside the lock; the closure it returns applies the result to the run under the lock.
func (e *buildEngine) async(b *Build, op string, work func() func(*Build)) {
	if e.ops[b.ID] == nil {
		e.ops[b.ID] = map[string]bool{}
	}

	if e.ops[b.ID][op] {
		return
	}

	e.ops[b.ID][op] = true
	id := b.ID
	e.wg.Add(1)

	go func() {
		defer e.wg.Done()

		var apply func(*Build)

		func() {
			defer func() {
				if r := recover(); r != nil {
					msg := fmt.Sprintf("%s failed: %v", op, r)
					apply = func(b *Build) { e.attention(b, msg, "retry") }
				}
			}()

			apply = work()
		}()

		e.mu.Lock()
		delete(e.ops[id], op)

		if b := e.builds[id]; b != nil && apply != nil {
			apply(b)
			e.save(b)
		}

		e.mu.Unlock()
		e.poke()
	}()
}

func (e *buildEngine) advance(b *Build) {
	if b.CancelAsked {
		e.cancelStep(b)

		return
	}

	e.pollAttempts(b)

	switch b.Status {
	case BuildDraft:
		if b.autofix() {
			e.startInvestigation(b)
		} else {
			e.startPlanning(b)
		}
	case BuildReproducing:
		e.reproduce(b)
	case BuildReady:
		if e.setStatus(b, BuildRunning) {
			b.StartedAt = time.Now()
			e.event(b, "started", "", "Run started: tasks are being scheduled")
			e.baseline(b)
		}
	case BuildRunning:
		e.schedule(b)

		if b.runningAttempts() == 0 && len(b.implementTasks()) > 0 && b.allDone(TaskKindImplement) {
			b.Integration.Overlaps = overlaps(b)
			e.setStatus(b, BuildIntegrating)
			e.event(b, "integration.started", "", "All tasks completed; integrating their branches")
		} else if b.runningAttempts() == 0 {
			e.stuck(b)
		}
	case BuildFixing:
		e.schedule(b)

		if b.runningAttempts() == 0 && b.allDone("") {
			e.setStatus(b, BuildIntegrating)
			e.event(b, "integration.started", "", "Fixes completed; integrating them")
		} else if b.runningAttempts() == 0 {
			e.stuck(b)
		}
	case BuildIntegrating:
		if b.Integration.Pending == "" {
			e.integrate(b)
		}
	case BuildValidating:
		e.validate(b)
	case BuildReviewing:
		e.review(b)
	case BuildAwaitingFinal:
		if b.PR.State == "" {
			e.preparePR(b)
		}
	}
}

// stuck explains why nothing can move: a paused run waits quietly, a failed task asks for a decision.
func (e *buildEngine) stuck(b *Build) {
	if b.Paused {
		return
	}

	for _, t := range b.Tasks {
		if t.Status == TaskNeedsAttention || t.Status == TaskFailed {
			e.attention(b, fmt.Sprintf("Task %q needs attention: %s", t.Title, orDash(t.Problem)), "retry_task")

			return
		}
	}

	if len(b.schedulable()) == 0 {
		for _, t := range b.Tasks {
			if t.Status == TaskBlocked {
				e.attention(b, fmt.Sprintf("Task %q is blocked by a dependency that cannot finish", t.Title), "retry_task")

				return
			}
		}
	}
}

func (e *buildEngine) cancelStep(b *Build) {
	for i := range b.Attempts {
		a := &b.Attempts[i]

		if a.Status != AttemptRunning && a.Status != AttemptReported {
			continue
		}

		if r, ok := e.runs.get(a.RunID); ok {
			if status, _ := r.exitState(); status == runStatusLive {
				r.stop(stopGracePeriod)

				continue
			}
		}

		a.Status, a.EndedAt, a.Note = AttemptStopped, time.Now(), "stopped when the run was cancelled"
	}

	if b.runningAttempts() > 0 || e.anyBusy(b) {
		return
	}

	for _, t := range b.Tasks {
		if t.Status != TaskCompleted {
			t.Status = TaskCancelled
		}
	}

	if e.setStatus(b, BuildCancelled) {
		e.event(b, "cancelled", "", "Run cancelled; worktrees and branches were kept")
	}
}

// eligible finds an installed agent with a ready, signed-in account and a way to start a task on its own.
func eligible(s *State, id string, now time.Time) (*Account, provider.Provider, provider.Features, error) {
	var f provider.Features

	p, err := provider.Find(id)
	if err != nil {
		return nil, nil, f, fmt.Errorf("%s is not installed", providerName(id))
	}

	if f, err = p.Features(); err != nil {
		return nil, nil, f, err
	}

	if _, ok := provider.Starter(p); !ok {
		return nil, nil, f, fmt.Errorf("%s cannot be given a task", providerName(id))
	}

	acct, err := s.bestAccount(id, "", nil, now)
	if err != nil {
		return nil, nil, f, fmt.Errorf("no %s account is ready", providerName(id))
	}

	if _, err := verifyProfile(p, acct); err != nil {
		return nil, nil, f, fmt.Errorf("%s account %s is not signed in", providerName(id), acct.Name)
	}

	return acct, p, f, nil
}

func (b *Build) candidates(first string) []string {
	var out []string

	for _, id := range append(append([]string{first}, b.Policy.Agents...), provider.IDs...) {
		if id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}

	return out
}

func (e *buildEngine) pickAgent(s *State, b *Build, first string, avoid []string) (string, error) {
	var reasons []string

	for _, id := range b.candidates(first) {
		if slices.Contains(avoid, id) {
			continue
		}

		if _, _, _, err := eligible(s, id, time.Now()); err != nil {
			reasons = append(reasons, err.Error())

			continue
		}

		return id, nil
	}

	if len(reasons) == 0 {
		return "", errors.New("no agent is available")
	}

	return "", errors.New(strings.Join(reasons, "; "))
}

func (e *buildEngine) worktreePath(b *Build, name string) string {
	return filepath.Join(e.store.dir(b.ID), "worktrees", name)
}

func (b *Build) branch(name string) string {
	return buildBranchRoot + b.ID[len(b.ID)-6:] + "/" + name
}

// launch starts one agent through the ordinary service run path, so the runner applies account failover, automatic handoff and approvals exactly as for any other session.
func (e *buildEngine) launch(b *Build, role, taskID, providerID, dir, step, label, instruction string) (*Attempt, error) {
	s, err := e.state()
	if err != nil {
		return nil, err
	}

	acct, p, f, err := eligible(s, providerID, time.Now())
	if err != nil {
		return nil, err
	}

	if cur, err := s.resolveAccount(s.active(providerID)); err != nil || cur.status(time.Now()) != statusReady {
		s.update(func(st *State) error {
			st.setActive(providerID, acct.Name)

			return nil
		})
	}

	a := Attempt{ID: b.nextID("a"), Role: role, TaskID: taskID, Provider: providerID, Account: acct.Name, Dir: dir, Status: AttemptRunning, StartedAt: time.Now()}
	reports := filepath.Join(e.store.dir(b.ID), "reports")
	a.Report = filepath.Join(reports, a.ID+".json")
	os.MkdirAll(reports, 0o700)
	os.Remove(a.Report)

	contract := contractText(role, a.Report)
	brief := filepath.Join(reports, a.ID+".brief.txt")

	if err := os.WriteFile(brief, []byte("CSM Build Room task brief:\n"+instruction+"\n\n"+contract+"\n"), 0o600); err != nil {
		return nil, err
	}

	starter, _ := provider.Starter(p)
	args := starter.TaskArgs(f, instruction+"\n\n"+contract)
	env := []string{"CSM_BUILD_ID=" + b.ID, "CSM_BUILD_STEP=" + step, "CSM_TASK_REPORT=" + a.Report, "CSM_BUILD_CONTRACT=" + brief}

	run, err := e.runs.spawn(s, runKindAgent, providerID, dir, append([]string{providerID}, args...), nil, 0, 0, spawnOpts{env: env, buildID: b.ID, task: taskID, label: label})
	if err != nil {
		return nil, err
	}

	a.RunID, a.CSMPID = run.ID, run.CSMPID

	if run.Account != "" {
		a.Account = run.Account
	}

	b.Attempts = append(b.Attempts, a)
	e.event(b, "agent.started", taskID, fmt.Sprintf("%s started %s", agentLabel(providerID, a.Account), label))

	return b.attempt(a.ID), nil
}

func reportReady(path string) bool {
	data, err := os.ReadFile(path)

	return err == nil && len(data) > 0 && len(data) <= maxReportBytes && json.Valid([]byte(strings.TrimSpace(string(data))))
}

func (e *buildEngine) pollAttempts(b *Build) {
	for i := range b.Attempts {
		a := &b.Attempts[i]

		if a.Status != AttemptRunning && a.Status != AttemptReported {
			continue
		}

		run, known := e.runs.get(a.RunID)

		var exit *int

		live := false

		if known {
			var status string

			status, exit = run.exitState()
			live = status == runStatusLive
		}

		e.noteSwitches(b, a)

		if a.Status == AttemptRunning && reportReady(a.Report) {
			a.Status, a.ReportedAt = AttemptReported, time.Now()
			e.event(b, "agent.reported", a.TaskID, agentLabel(a.Provider, a.Account)+" wrote its completion report; stopping the session")

			if live {
				run.stop(reportStopGrace)
			}
		}

		if live {
			continue
		}

		a.EndedAt = time.Now()

		if known {
			a.ExitCode = exit
		}

		switch {
		case a.Status == AttemptReported || reportReady(a.Report):
			a.Status = AttemptCompleted
		case !known && processAlive(a.CSMPID):
			a.Status, a.Note = AttemptInterrupted, fmt.Sprintf("the agent process from before CSM restarted (pid %d) is still running outside the service; stop it before retrying", a.CSMPID)
		case !known:
			a.Status, a.Note = AttemptInterrupted, "the CSM service stopped while this agent was running"
		default:
			a.Status, a.Note = AttemptFailed, e.exitReason(a, run)
		}

		e.event(b, "agent.exited", a.TaskID, fmt.Sprintf("%s finished: %s", agentLabel(a.Provider, a.Account), describeAttempt(*a)))
		e.finish(b, a)
	}
}

func describeAttempt(a Attempt) string {
	switch a.Status {
	case AttemptCompleted:
		return "report received"
	case AttemptInterrupted:
		return "interrupted, " + a.Note
	default:
		return a.Note
	}
}

func (e *buildEngine) exitReason(a *Attempt, run *agentRun) string {
	code := 0

	if _, exit := run.exitState(); exit != nil {
		code = *exit
	}

	reason := fmt.Sprintf("the session ended without a completion report (exit status %d)", code)

	if s, err := e.state(); err == nil {
		if _, err := s.bestAccount(a.Provider, "", nil, time.Now()); err != nil {
			reason += "; no " + providerName(a.Provider) + " account is ready, most likely because of usage limits"
		}
	}

	return reason
}

// noteSwitches reads the runner's handoff records for the attempt's directory, so account failover and cross-agent handoffs inside one attempt show up in the run's history.
func (e *buildEngine) noteSwitches(b *Build, a *Attempt) {
	s, err := e.state()
	if err != nil {
		return
	}

	root := s.handoffRoot(detectProject(a.Dir).Dir)
	entries, _ := os.ReadDir(root)

	var seen []AgentSwitch

	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}

		st, err := handoff.Load(filepath.Join(root, ent.Name()))
		if err != nil || st.Kind == "checkpoint" || st.CreatedAt.Before(a.StartedAt) {
			continue
		}

		sw := AgentSwitch{At: st.CreatedAt, From: st.Source.String(), To: st.Target.String(), Reason: st.Reason, Handoff: st.ID, Commit: st.SourceCommit, FromSession: st.Source.SessionID, ToSession: st.Target.SessionID}
		sw.Started = st.Outcome != nil && st.Outcome.Started
		seen = append(seen, sw)
	}

	sort.Slice(seen, func(i, j int) bool { return seen[i].At.Before(seen[j].At) })

	for _, sw := range seen {
		if i := slices.IndexFunc(a.Switches, func(o AgentSwitch) bool { return o.Handoff == sw.Handoff }); i >= 0 {
			a.Switches[i].Started, a.Switches[i].ToSession = sw.Started, sw.ToSession

			if t := b.task(a.TaskID); t != nil {
				for j := range t.Switches {
					if t.Switches[j].Handoff == sw.Handoff {
						t.Switches[j].Started, t.Switches[j].ToSession = sw.Started, sw.ToSession
					}
				}
			}

			continue
		}

		a.Switches = append(a.Switches, sw)

		if a.StartedAs == "" {
			a.StartedAs = agentLabel(a.Provider, a.Account)
		}

		if parts := strings.SplitN(sw.To, "/", 2); len(parts) == 2 {
			a.Provider, a.Account = parts[0], parts[1]

			if r, ok := e.runs.get(a.RunID); ok {
				r.mu.Lock()
				r.Provider, r.Account = parts[0], parts[1]
				r.mu.Unlock()
			}
		}

		reason := "manual switch"

		if sw.Reason == string(provider.StopReasonUsageLimit) {
			reason = "usage limit detected"
		}

		kind, what := "fallback.account", "account fallback"

		if strings.SplitN(sw.From, "/", 2)[0] != strings.SplitN(sw.To, "/", 2)[0] {
			kind, what = "fallback.handoff", "cross-agent handoff"
		}

		e.event(b, kind, a.TaskID, fmt.Sprintf("%s: %s %s → %s", reason, what, sw.From, sw.To))

		if t := b.task(a.TaskID); t != nil {
			t.Switches = append(t.Switches, sw)
			parts := strings.SplitN(sw.To, "/", 2)
			t.Provider = parts[0]

			if len(parts) == 2 {
				t.Account = parts[1]
			}
		}
	}
}

func (e *buildEngine) finish(b *Build, a *Attempt) {
	switch a.Role {
	case RolePlan:
		e.finishPlan(b, a)
	case RoleInvestigate:
		e.finishInvestigation(b, a)
	case RoleImplement, RoleFix:
		e.finishWork(b, a)
	case RoleResolve:
		e.finishResolve(b, a)
	case RoleReview:
		e.finishReview(b, a)
	}
}

// Planning

func (e *buildEngine) startPlanning(b *Build) {
	if b.Paused {
		return
	}

	s, err := e.state()
	if err != nil {
		e.attention(b, err.Error(), "retry_plan")

		return
	}

	planner, err := e.pickAgent(s, b, b.Policy.Planner, nil)
	if err != nil {
		e.attention(b, "No agent can plan this run: "+err.Error(), "retry_plan", "write_plan")

		return
	}

	dir := e.worktreePath(b, "plan")

	if err := addDetachedWorktree(b.Repo, dir, b.BaseCommit); err != nil {
		e.attention(b, "Could not prepare a read-only copy for planning: "+err.Error(), "retry_plan", "write_plan")

		return
	}

	if !e.setStatus(b, BuildPlanning) {
		return
	}

	if _, err := e.launch(b, RolePlan, "", planner, dir, "plan", "planning", planInstruction(b, dir)); err != nil {
		e.attention(b, "Could not start the planner: "+err.Error(), "retry_plan", "write_plan")

		return
	}

	e.event(b, "plan.started", "", providerName(planner)+" is inspecting the repository and writing a plan")
}

func (e *buildEngine) finishPlan(b *Build, a *Attempt) {
	if a.Status != AttemptCompleted {
		e.attention(b, "Planning did not finish: "+a.Note, "retry_plan", "write_plan")

		return
	}

	var rep planReport

	if err := readReport(a.Report, &rep); err != nil {
		e.attention(b, "The planner's report could not be read: "+err.Error(), "retry_plan", "write_plan")

		return
	}

	tasks := rep.tasks()

	if err := validatePlan(tasks); err != nil {
		e.event(b, "plan.invalid", "", "The generated plan was rejected: "+err.Error())
		e.attention(b, "The generated plan was rejected: "+err.Error(), "retry_plan", "write_plan")

		return
	}

	if changes := worktreeChanges(a.Dir); len(changes) > 0 {
		e.event(b, "plan.ignored_edits", "", fmt.Sprintf("The planner changed %d file(s) in its read-only copy; the changes were ignored", len(changes)))
	}

	b.Tasks = tasks
	b.Plan = Plan{
		Summary:         clean(rep.Summary, 2000),
		Layers:          cleanAll(rep.Layers, 20, 200),
		Considerations:  cleanAll(rep.Considerations, 20, 400),
		SuggestedChecks: cleanAll(rep.SuggestedChecks, 12, 200),
		Source:          agentLabel(a.Provider, a.Account),
	}
	b.refreshTaskStates()
	e.event(b, "plan.generated", "", fmt.Sprintf("Plan generated by %s with %d task(s)", b.Plan.Source, len(tasks)))

	if b.Policy.RequirePlanApproval {
		e.setStatus(b, BuildAwaitingPlan)

		return
	}

	e.setStatus(b, BuildReady)
	e.event(b, "plan.approved", "", "Plan approved automatically by the run's policy")
}

// Tasks

func (e *buildEngine) schedule(b *Build) {
	for _, t := range b.schedulable() {
		e.startTask(b, t)
	}
}

func (b *Build) lastAttempt(taskID string) *Attempt {
	for i := len(b.Attempts) - 1; i >= 0; i-- {
		if b.Attempts[i].TaskID == taskID {
			return &b.Attempts[i]
		}
	}

	return nil
}

func (e *buildEngine) startTask(b *Build, t *BuildTask) {
	s, err := e.state()
	if err != nil {
		return
	}

	agent, err := e.pickAgent(s, b, t.Provider, nil)
	if err != nil {
		e.taskProblem(b, t, "No agent is available: "+err.Error(), "Sign in to an agent or wait for an account to become ready, then retry the task.")

		return
	}

	if t.Worktree == "" {
		if err := e.prepareWorktree(b, t); err != nil {
			e.taskProblem(b, t, err.Error(), "Fix the repository state described above, then retry the task.")

			return
		}
	}

	t.Status = TaskAssigned
	prev := b.lastAttempt(t.ID)

	if prev != nil && prev.Status == AttemptCompleted {
		prev = nil
	}

	role, step := RoleImplement, t.ID

	if t.Kind == TaskKindFix {
		role = RoleFix
	}

	a, err := e.launch(b, role, t.ID, agent, t.Worktree, step, t.Title, taskInstruction(b, t, prev))
	if err != nil {
		e.taskProblem(b, t, "Could not start "+providerName(agent)+": "+err.Error(), "Retry the task, optionally with another agent.")

		return
	}

	t.Provider, t.Account, t.Status, t.Problem, t.Recovery = agent, a.Account, TaskRunning, "", ""
	t.Attempts = append(t.Attempts, *a)

	if t.StartedAt.IsZero() {
		t.StartedAt = time.Now()
	}

	e.event(b, "task.scheduled", t.ID, fmt.Sprintf("Task %q assigned to %s in %s", t.Title, agentLabel(agent, a.Account), t.Branch))
}

func (e *buildEngine) taskProblem(b *Build, t *BuildTask, problem, recovery string) {
	t.Status, t.Problem, t.Recovery = TaskNeedsAttention, problem, recovery
	e.event(b, "task.failed", t.ID, fmt.Sprintf("Task %q needs attention: %s", t.Title, problem))
}

// prepareWorktree gives a task its own branch and worktree: an implementation task starts at the base commit with its dependencies merged in, a fix starts at the integrated result.
func (e *buildEngine) prepareWorktree(b *Build, t *BuildTask) error {
	path, branch, base := e.worktreePath(b, t.ID), b.branch(t.ID), b.BaseCommit

	if t.BaseRef != "" {
		base = t.BaseRef
	}

	if t.Kind == TaskKindFix {
		if b.Integration.Head == "" {
			return errors.New("there is no integrated result to fix yet")
		}

		base = b.Integration.Head
	}

	if err := addWorktree(b.Repo, path, branch, base); err != nil {
		return err
	}

	t.Worktree, t.Branch, t.BaseCommit = path, branch, base

	if t.Kind != TaskKindFix {
		for _, dep := range topoOrder(b.Tasks) {
			if !slices.Contains(t.DependsOn, dep.ID) || dep.Branch == "" {
				continue
			}

			conflicts, err := mergeInto(path, dep.Branch, "Merge dependency "+dep.ID)
			if err != nil || len(conflicts) > 0 {
				gitIn(path, "merge", "--abort")

				return fmt.Errorf("the changes of dependencies conflict in %s; resolve them in %s", strings.Join(conflicts, ", "), displayPath(path))
			}
		}
	}

	if head, err := gitOut(path, "rev-parse", "HEAD"); err == nil {
		t.BaseCommit = head
	}

	if note := linkDependencies(b.Repo, path); note != "" {
		e.event(b, "worktree.note", t.ID, note)
	}

	return nil
}

func (e *buildEngine) finishWork(b *Build, a *Attempt) {
	t := b.task(a.TaskID)
	if t == nil {
		return
	}

	for i := range t.Attempts {
		if t.Attempts[i].ID == a.ID {
			t.Attempts[i] = *a
		}
	}

	if a.Status != AttemptCompleted {
		e.taskFailed(b, t, a)

		return
	}

	var rep workReport

	parseErr := readReport(a.Report, &rep)
	head, committed, err := commitAll(t.Worktree, fmt.Sprintf("%s\n\nCSM Build Room task %s (run %s)\n\nCSM-Agent: %s/%s\nCSM-Build: %s", t.Title, t.ID, b.ID, a.Provider, a.Account, b.ID))

	if err != nil {
		e.taskProblem(b, t, "CSM could not commit the task's changes: "+err.Error(), "Inspect the worktree, fix the problem and retry or accept the task.")

		return
	}

	c := &Completion{
		FilesChanged:    changedSince(t.Worktree, t.BaseCommit),
		DiffStat:        diffStatSince(t.Worktree, t.BaseCommit),
		Commit:          head,
		Summary:         clean(rep.Summary, 2000),
		ClaimedCommands: cleanAll(rep.Commands, 30, 200),
		ClaimedTests:    cleanAll(rep.Tests, 30, 300),
		Limitations:     cleanAll(rep.Limitations, 20, 400),
	}

	if parseErr != nil {
		c.ReportUnparseable = true
	}

	for _, f := range rep.FilesChanged {
		if p := cleanPath(f); p != "" {
			c.ClaimedFiles = append(c.ClaimedFiles, p)
		}
	}

	for _, ac := range rep.Acceptance {
		if len(c.Acceptance) < 20 {
			c.Acceptance = append(c.Acceptance, AcceptanceClaim{Criterion: clean(ac.Criterion, 300), Addressed: ac.Addressed, Note: clean(ac.Note, 400)})
		}
	}

	for _, f := range c.ClaimedFiles {
		if !slices.Contains(c.FilesChanged, f) {
			c.Discrepancies = append(c.Discrepancies, "reported changing "+f+", but the branch does not change it")
		}
	}

	for _, f := range c.FilesChanged {
		if len(c.ClaimedFiles) > 0 && !slices.Contains(c.ClaimedFiles, f) {
			c.Discrepancies = append(c.Discrepancies, f+" changed but was not mentioned in the report")
		}
	}

	t.Completion, t.Head = c, head

	if len(c.FilesChanged) == 0 {
		e.taskProblem(b, t, "The agent reported completion, but the branch has no changes", "Retry the task, or accept it if no change was needed.")

		return
	}

	verb := "committed"

	if !committed {
		verb = "found"
	}

	t.Status, t.FinishedAt, t.Integrated = TaskCompleted, time.Now(), false
	e.event(b, "task.completed", t.ID, fmt.Sprintf("Task %q completed: CSM %s %d changed file(s) at %s", t.Title, verb, len(c.FilesChanged), shortSHA(head)))
}

// taskFailed keeps the worktree as it is and either hands the task to another agent once, when the agent it had has no usable account left, or asks for a decision.
func (e *buildEngine) taskFailed(b *Build, t *BuildTask, a *Attempt) {
	t.Problem = a.Note

	if a.Status == AttemptStopped {
		t.Status, t.Recovery = TaskNeedsAttention, "Retry the task to continue in the same worktree."

		return
	}

	attempts := 0
	var tried []string

	for _, at := range b.Attempts {
		if at.TaskID == t.ID {
			attempts++
			tried = append(tried, at.Provider)

			for _, sw := range at.Switches {
				tried = append(tried, strings.SplitN(sw.To, "/", 2)[0])
			}
		}
	}

	if s, err := e.state(); err == nil && a.Status == AttemptFailed && attempts < maxTaskAttempts && !b.Paused {
		if _, err := s.bestAccount(t.Provider, "", nil, time.Now()); err != nil {
			if next, err := e.pickAgent(s, b, "", tried); err == nil {
				t.Status, t.Provider, t.Account = TaskReady, next, ""
				e.event(b, "task.retry", t.ID, fmt.Sprintf("Retrying task %q with %s; the worktree and its changes are kept", t.Title, providerName(next)))

				return
			}
		}
	}

	t.Status = TaskNeedsAttention
	t.Recovery = "The worktree and its changes are kept. Retry the task, optionally with another agent."
	e.event(b, "task.failed", t.ID, fmt.Sprintf("Task %q needs attention: %s", t.Title, a.Note))
}

// Integration

func overlaps(b *Build) []Overlap {
	byFile := map[string][]string{}

	for _, t := range b.Tasks {
		if t.Completion == nil {
			continue
		}

		for _, f := range t.Completion.FilesChanged {
			byFile[f] = append(byFile[f], t.ID)
		}
	}

	var out []Overlap

	for f, ids := range byFile {
		if len(ids) > 1 {
			out = append(out, Overlap{File: f, Tasks: ids})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })

	return out
}

type mergeItem struct {
	id, branch, title string
}

func (e *buildEngine) integrate(b *Build) {
	if e.busy(b, opIntegrate) {
		return
	}

	if b.Integration.Branch == "" {
		b.Integration.Branch, b.Integration.Worktree = b.branch("integration"), e.worktreePath(b, "integration")
	}

	var items []mergeItem

	for _, t := range topoOrder(b.Tasks) {
		if t.Status == TaskCompleted && !t.Integrated && t.Branch != "" {
			items = append(items, mergeItem{t.ID, t.Branch, t.Title})
		}
	}

	repo, path, branch, base := b.Repo, b.Integration.Worktree, b.Integration.Branch, b.BaseCommit

	e.async(b, opIntegrate, func() func(*Build) {
		if err := addWorktree(repo, path, branch, base); err != nil {
			return func(b *Build) { e.attention(b, "Could not prepare the integration worktree: "+err.Error(), "retry") }
		}

		if mergeInProgress(path) {
			return func(b *Build) {
				e.attention(b, "A merge is in progress in the integration worktree "+displayPath(path)+"; finish or resolve it, then continue", "continue_integration")
			}
		}

		var merged []string
		var conflict *Conflict
		var mergeErr error

		for _, it := range items {
			files, err := mergeInto(path, it.branch, fmt.Sprintf("Merge task %s: %s", it.id, it.title))

			if err != nil {
				mergeErr = fmt.Errorf("merging task %s: %w", it.id, err)

				break
			}

			if len(files) > 0 {
				conflict = &Conflict{TaskID: it.id, Files: files, Status: "detected", At: time.Now()}

				break
			}

			merged = append(merged, it.id)
		}

		head, _ := gitOut(path, "rev-parse", "HEAD")
		files := changedSince(path, base)
		stat := diffStatSince(path, base)

		return func(b *Build) {
			for _, id := range merged {
				if t := b.task(id); t != nil {
					t.Integrated = true
				}

				if !slices.Contains(b.Integration.Merged, id) {
					b.Integration.Merged = append(b.Integration.Merged, id)
				}

				e.event(b, "integration.merged", id, "Merged task "+id+" into "+branch)
			}

			b.Integration.Head, b.Integration.Files, b.Integration.DiffStat = head, files, stat

			switch {
			case b.CancelAsked:
			case mergeErr != nil:
				e.attention(b, "Integration failed: "+mergeErr.Error(), "retry")
			case conflict != nil:
				e.conflictFound(b, *conflict)
			default:
				if e.setStatus(b, BuildValidating) {
					e.event(b, "validation.started", "", "Running the repository's checks on the integrated branch")
				}
			}
		}
	})
}

func (e *buildEngine) conflictFound(b *Build, c Conflict) {
	for _, t := range b.Tasks {
		if t.ID == c.TaskID || !t.Integrated || t.Completion == nil {
			continue
		}

		for _, f := range c.Files {
			if slices.Contains(t.Completion.FilesChanged, f) && !slices.Contains(c.Tasks, t.ID) {
				c.Tasks = append(c.Tasks, t.ID)
			}
		}
	}

	e.event(b, "integration.conflict", c.TaskID, fmt.Sprintf("Merging task %s conflicts with %s in %s", c.TaskID, orDash(strings.Join(c.Tasks, ", ")), strings.Join(c.Files, ", ")))

	if b.Paused {
		c.Status = "needs_attention"
		b.Integration.Conflicts = append(b.Integration.Conflicts, c)
		e.attention(b, "Integration conflict while paused; resume to let an agent propose a resolution or resolve it by hand", "continue_integration")

		return
	}

	s, err := e.state()
	owner := b.task(c.TaskID)
	first := ""

	if owner != nil {
		first = owner.Provider
	}

	agent := ""

	if err == nil {
		agent, err = e.pickAgent(s, b, first, nil)
	}

	if err == nil {
		var a *Attempt

		a, err = e.launch(b, RoleResolve, "", agent, b.Integration.Worktree, "resolve-"+c.TaskID, "conflict resolution for "+c.TaskID, resolveInstruction(b, c, b.Integration.Worktree))

		if err == nil {
			c.Status = "resolving"
			b.Integration.Pending = a.ID
			b.Integration.Conflicts = append(b.Integration.Conflicts, c)

			return
		}
	}

	c.Status = "needs_attention"
	b.Integration.Conflicts = append(b.Integration.Conflicts, c)
	e.attention(b, fmt.Sprintf("Merging task %s conflicts in %s and no agent could resolve it (%v). Resolve it in %s, then continue integration.", c.TaskID, strings.Join(c.Files, ", "), err, displayPath(b.Integration.Worktree)), "continue_integration")
}

func (e *buildEngine) finishResolve(b *Build, a *Attempt) {
	b.Integration.Pending = ""
	n := len(b.Integration.Conflicts)

	if n == 0 {
		return
	}

	c := &b.Integration.Conflicts[n-1]

	if a.Status != AttemptCompleted {
		c.Status = "needs_attention"
		e.attention(b, fmt.Sprintf("The conflict resolution for task %s did not finish (%s). Resolve %s in %s, then continue integration.", c.TaskID, a.Note, strings.Join(c.Files, ", "), displayPath(b.Integration.Worktree)), "continue_integration", "retry_resolve")

		return
	}

	if err := e.concludeMerge(b, c, "Resolved by "+agentLabel(a.Provider, a.Account)); err != nil {
		e.attention(b, err.Error(), "continue_integration", "retry_resolve")
	}
}

// concludeMerge commits a resolved merge only when no conflict markers remain; the resolution is then validated like any other change.
func (e *buildEngine) concludeMerge(b *Build, c *Conflict, by string) error {
	dir := b.Integration.Worktree

	if !mergeInProgress(dir) {
		return fmt.Errorf("no merge is in progress in %s", displayPath(dir))
	}

	if marked := conflictMarkers(dir, c.Files); len(marked) > 0 {
		c.Status = "needs_attention"

		return fmt.Errorf("conflict markers remain in %s; resolve them in %s, then continue integration", strings.Join(marked, ", "), displayPath(dir))
	}

	if _, _, err := commitAll(dir, fmt.Sprintf("Merge task %s (conflict resolved)", c.TaskID)); err != nil {
		return fmt.Errorf("could not commit the resolved merge: %w", err)
	}

	c.Status, c.Resolution = "resolved", by+"; CSM checked that no conflict markers remain, and the checks run next"

	if t := b.task(c.TaskID); t != nil {
		t.Integrated = true

		if !slices.Contains(b.Integration.Merged, t.ID) {
			b.Integration.Merged = append(b.Integration.Merged, t.ID)
		}
	}

	e.event(b, "integration.resolved", c.TaskID, "Conflict for task "+c.TaskID+" resolved: "+by)

	if b.Status == BuildNeedsAttention {
		e.setStatus(b, BuildIntegrating)
	}

	return nil
}

// Validation

func (e *buildEngine) baseline(b *Build) {
	if !b.Policy.RunBaseline || len(b.Policy.Checks) == 0 || e.busy(b, opBaseline) {
		return
	}

	for _, c := range b.Checks {
		if c.Stage == "baseline" {
			return
		}
	}

	repo, base, path, checks, check := b.Repo, b.BaseCommit, e.worktreePath(b, "baseline"), slices.Clone(b.Policy.Checks), atRevision(e.checker(b))
	e.event(b, "baseline.started", "", "Running the checks on the base commit to record existing failures")

	e.async(b, opBaseline, func() func(*Build) {
		var results []CheckResult

		if err := addDetachedWorktree(repo, path, base); err != nil {
			for _, c := range checks {
				results = append(results, CheckResult{Name: c.Name, Command: c.Command, Source: c.Source, Stage: "baseline", State: CheckNotRun, Reason: "baseline worktree: " + err.Error(), At: time.Now()})
			}
		} else {
			linkDependencies(repo, path)

			for _, c := range checks {
				results = append(results, check(path, c, "baseline"))
			}
		}

		return func(b *Build) {
			b.Checks = append(slices.DeleteFunc(b.Checks, func(c CheckResult) bool { return c.Stage == "baseline" }), results...)
			sum := summarizeChecks(results)
			e.event(b, "baseline.completed", "", fmt.Sprintf("Baseline on %s: %d passed, %d failed, %d blocked", shortSHA(base), sum.Passed, sum.Failed, sum.Blocked))
		}
	})
}

func (b *Build) baselineDone() bool {
	if !b.Policy.RunBaseline || len(b.Policy.Checks) == 0 {
		return true
	}

	return slices.ContainsFunc(b.Checks, func(c CheckResult) bool { return c.Stage == "baseline" })
}

func (e *buildEngine) validate(b *Build) {
	if e.busy(b, opValidate) {
		return
	}

	if !b.baselineDone() {
		e.baseline(b)

		return
	}

	path, checks := b.Integration.Worktree, slices.Clone(b.Policy.Checks)
	repo, check := b.Repo, atRevision(e.checker(b))

	var verifyFix func() func(*Verification)

	if b.autofix() {
		verifyFix = e.verifyFix(b, path)
		e.event(b, "verification.started", "", "Running the regression test and the repository's checks on the fix, in a sandbox")
	}

	e.async(b, opValidate, func() func(*Build) {
		linkDependencies(repo, path)

		var results []CheckResult

		for _, c := range checks {
			results = append(results, check(path, c, "integration"))
		}

		var applyFix func(*Verification)

		if verifyFix != nil {
			applyFix = verifyFix()
		}

		if len(checks) == 0 {
			results = append(results, CheckResult{Name: "checks", Stage: "integration", State: CheckNotRun, Reason: "No test, lint or build commands were detected in the repository and none were configured for this run", At: time.Now()})
		}

		return func(b *Build) {
			var baseline []CheckResult

			for _, c := range b.Checks {
				if c.Stage == "baseline" {
					baseline = append(baseline, c)
				}
			}

			results = compareBaseline(results, baseline)
			b.Checks = append(baseline, results...)
			sum := summarizeChecks(results)
			e.event(b, "validation.completed", "", fmt.Sprintf("Checks on the integrated branch: %d passed, %d failed, %d already failing on the base, %d blocked, %d not run", sum.Passed, sum.Failed, sum.Baseline, sum.Blocked, sum.NotRun))

			if b.CancelAsked {
				return
			}

			round := b.Cycle + 1

			for _, r := range results {
				if r.State == CheckFailed {
					b.Findings = append(b.Findings, Finding{ID: b.nextID("f"), Cycle: round, Severity: "high", Title: "Check failed: " + r.Name, Explanation: tail(r.Output, 1500), Recommendation: "Make " + r.Name + " pass", Blocking: true, Status: findingOpen, Source: "CSM ran " + r.Name})
				}
			}

			if applyFix != nil {
				e.recordVerification(b, results, applyFix, round)
			}

			if b.Policy.Review {
				e.setStatus(b, BuildReviewing)

				return
			}

			b.Cycle = round
			e.decide(b)
		}
	})
}

// Review

func (e *buildEngine) review(b *Build) {
	if b.Paused || b.runningAttempts() > 0 {
		return
	}

	round := b.Cycle + 1

	if slices.ContainsFunc(b.Reviews, func(r ReviewRound) bool { return r.Cycle == round }) {
		return
	}

	s, err := e.state()
	if err != nil {
		return
	}

	var implementers []string

	for _, t := range b.Tasks {
		if t.Provider != "" {
			implementers = append(implementers, t.Provider)
		}
	}

	reviewer, independent := "", true

	if id, err := e.pickAgent(s, b, b.Policy.Reviewer, implementers); err == nil {
		reviewer = id
	} else if id, err := e.pickAgent(s, b, b.Policy.Reviewer, nil); err == nil {
		reviewer, independent = id, false
	} else {
		e.attention(b, "No agent is available to review the change: "+err.Error(), "retry_review", "skip_review")

		return
	}

	dir := e.worktreePath(b, fmt.Sprintf("review-%d", round))

	if err := addDetachedWorktree(b.Repo, dir, b.Integration.Head); err != nil {
		e.attention(b, "Could not prepare the review copy: "+err.Error(), "retry_review", "skip_review")

		return
	}

	a, err := e.launch(b, RoleReview, "", reviewer, dir, fmt.Sprintf("review-%d", round), fmt.Sprintf("review round %d", round), reviewInstruction(b, dir, independent))
	if err != nil {
		e.attention(b, "Could not start the reviewer: "+err.Error(), "retry_review", "skip_review")

		return
	}

	rr := ReviewRound{Cycle: round, Provider: reviewer, Account: a.Account, Independent: independent, Status: "running", At: time.Now(), Worktree: dir, Commit: b.Integration.Head}

	if !independent {
		rr.Note = "No different agent was available, so this review is not independent: " + providerName(reviewer) + " also implemented part of the change."
	}

	b.Reviews = append(b.Reviews, rr)
	b.Cycle = round
	e.event(b, "review.started", "", fmt.Sprintf("Review round %d by %s%s", round, agentLabel(reviewer, a.Account), map[bool]string{true: "", false: " (not independent)"}[independent]))
}

func (e *buildEngine) finishReview(b *Build, a *Attempt) {
	idx := slices.IndexFunc(b.Reviews, func(r ReviewRound) bool { return r.Cycle == b.Cycle })
	if idx < 0 {
		return
	}

	rr := &b.Reviews[idx]

	if a.Status != AttemptCompleted {
		rr.Status = "failed"
		e.attention(b, "The review did not finish: "+a.Note, "retry_review", "skip_review")

		return
	}

	var rep reviewReport

	if err := readReport(a.Report, &rep); err != nil {
		rr.Status, rr.ReportProblems = "failed", err.Error()
		e.attention(b, "The reviewer's report could not be read: "+err.Error(), "retry_review", "skip_review")

		return
	}

	for _, line := range worktreeChanges(rr.Worktree) {
		rr.IgnoredEdits = append(rr.IgnoredEdits, strings.TrimSpace(line))
	}

	if len(rr.IgnoredEdits) > 0 {
		e.event(b, "review.ignored_edits", "", fmt.Sprintf("The reviewer edited %d file(s) in its review copy; those edits were not used", len(rr.IgnoredEdits)))
	}

	for i := range b.Findings {
		if b.Findings[i].Cycle < b.Cycle && b.Findings[i].Status == findingFixAsked {
			b.Findings[i].Status = findingRechecked
		}
	}

	source := "review by " + agentLabel(a.Provider, a.Account) + " (agent finding, not verified by CSM)"

	for _, f := range rep.Findings {
		fd := Finding{ID: b.nextID("f"), Cycle: b.Cycle, Severity: normalizeSeverity(f.Severity), File: cleanPath(f.File), Line: max(f.Line, 0), Title: clean(f.Title, 200), Explanation: clean(f.Explanation, 2000), Reasoning: clean(f.Reasoning, 2000), Recommendation: clean(f.Recommendation, 1000), Blocking: f.Blocking, Status: findingOpen, Source: source}

		if fd.Title == "" {
			continue
		}

		if fd.Severity == "low" || fd.Severity == "info" {
			fd.Blocking = false
		}

		fd.TaskID = b.ownerOf(fd.File)
		b.Findings = append(b.Findings, fd)
	}

	rr.Verdict, rr.Summary, rr.Status = clean(rep.Verdict, 40), clean(rep.Summary, 2000), "completed"
	rr.FindingsCount, rr.BlockingOpen = 0, 0

	for _, f := range b.Findings {
		if f.Cycle == b.Cycle {
			rr.FindingsCount++

			if f.Blocking && f.Status == findingOpen {
				rr.BlockingOpen++
			}
		}
	}

	e.event(b, "review.completed", "", fmt.Sprintf("Review round %d: %d finding(s), %d blocking", b.Cycle, rr.FindingsCount, rr.BlockingOpen))
	e.decide(b)
}

// decide ends an assessment round: no blocking findings goes to final approval, otherwise a bounded fix round, and past the limit the run stops for a person.
func (e *buildEngine) decide(b *Build) {
	var blocking []Finding

	for _, f := range b.Findings {
		if f.Cycle == b.Cycle && f.Blocking && f.Status == findingOpen {
			blocking = append(blocking, f)
		}
	}

	if len(blocking) == 0 {
		e.setStatus(b, BuildAwaitingFinal)
		e.event(b, "final.ready", "", "Ready for final approval")

		return
	}

	if b.Cycle >= b.Policy.MaxReviewCycles {
		e.attention(b, fmt.Sprintf("%d blocking finding(s) remain after %d round(s) of checks and review, the limit for this run", len(blocking), b.Cycle), "accept_findings", "retry_review")

		return
	}

	e.createFix(b, blocking)
}

func (e *buildEngine) createFix(b *Build, findings []Finding) {
	votes := map[string]int{}
	owner := ""

	for _, f := range findings {
		if t := b.task(f.TaskID); t != nil && t.Provider != "" {
			votes[t.Provider]++

			if owner == "" || votes[t.Provider] > votes[owner] {
				owner = t.Provider
			}
		}
	}

	if owner == "" {
		for _, t := range b.implementTasks() {
			if t.Provider != "" {
				owner = t.Provider

				break
			}
		}
	}

	t := &BuildTask{ID: fmt.Sprintf("fix-%d", b.Cycle), Kind: TaskKindFix, Title: fmt.Sprintf("Address review round %d", b.Cycle), Objective: fmt.Sprintf("Fix the %d blocking finding(s) from review round %d without changing unrelated code.", len(findings), b.Cycle), Provider: owner, Status: TaskReady, Cycle: b.Cycle, Order: len(b.Tasks)}

	for _, f := range findings {
		t.Findings = append(t.Findings, f.ID)
		t.Acceptance = append(t.Acceptance, "Resolved: "+f.Title)

		if f.File != "" && !slices.Contains(t.Files, f.File) {
			t.Files = append(t.Files, f.File)
		}

		for i := range b.Findings {
			if b.Findings[i].ID == f.ID {
				b.Findings[i].Status = findingFixAsked
			}
		}
	}

	b.Tasks = append(b.Tasks, t)

	if e.setStatus(b, BuildFixing) {
		e.event(b, "fix.created", t.ID, fmt.Sprintf("Sending %d blocking finding(s) back to %s", len(findings), providerName(owner)))
	}
}

// Pull request

func (e *buildEngine) preparePR(b *Build) {
	if e.busy(b, opPR) {
		return
	}

	b.PR.State = "preparing"
	body := prBody(b)

	if b.autofix() {
		body = autofixPRBody(b)
	}
	path := filepath.Join(e.store.dir(b.ID), "pr.md")
	repo, branch := b.Repo, b.Integration.Branch
	title := prTitle(b)

	e.async(b, opPR, func() func(*Build) {
		err := os.WriteFile(path, []byte(body), 0o600)
		remote, _ := gitOut(repo, "remote", "get-url", "origin")
		blocker := remoteBlocker(repo, remote)

		return func(b *Build) {
			b.PR.Title, b.PR.BodyFile, b.PR.Branch, b.PR.Base, b.PR.Remote, b.PR.Blocker = title, path, branch, b.BaseBranch, redactRemote(remote), blocker
			b.PR.State = "prepared"

			if err != nil {
				b.PR.Error = err.Error()
			}

			msg := "Pull request description prepared locally at " + displayPath(path)

			if blocker != "" {
				msg += "; remote push unavailable: " + blocker
			}

			e.event(b, "pr.prepared", "", msg)
		}
	})
}

func redactRemote(url string) string {
	return handoff.Redact(url)
}

// remoteBlocker says why CSM cannot push and open a PR from here, checked before any remote operation is offered.
func remoteBlocker(repo, remote string) string {
	if remote == "" {
		return "the repository has no origin remote"
	}

	if _, err := gitOut(repo, "ls-remote", "--exit-code", "--heads", "origin"); err != nil {
		return "the origin remote is not reachable or you are not authorized to read it"
	}

	if _, err := lookPath("gh"); err != nil {
		return "the GitHub CLI (gh) is not installed"
	}

	if out, err := runQuiet(repo, "gh", "auth", "status"); err != nil {
		return "the GitHub CLI is not signed in: " + firstLine(out)
	}

	return ""
}

func prTitle(b *Build) string {
	return clean(firstLine(b.Instruction), 70)
}

func prBody(b *Build) string {
	var s strings.Builder

	fmt.Fprintf(&s, "## Summary\n\n%s\n\n", b.Instruction)

	if b.Plan.Summary != "" {
		fmt.Fprintf(&s, "%s\n\n", b.Plan.Summary)
	}

	if len(b.Plan.Considerations) > 0 {
		s.WriteString("## Key decisions and considerations\n\n")

		for _, c := range b.Plan.Considerations {
			fmt.Fprintf(&s, "- %s\n", c)
		}

		s.WriteString("\n")
	}

	s.WriteString("## Tasks\n\n")

	for _, t := range b.Tasks {
		files := 0

		if t.Completion != nil {
			files = len(t.Completion.FilesChanged)
		}

		fmt.Fprintf(&s, "- **%s** (%s, %s): %s; %d file(s) changed", t.Title, t.ID, agentLabel(t.Provider, t.Account), t.Status, files)

		for _, sw := range t.Switches {
			fmt.Fprintf(&s, "; moved from %s to %s (%s)", sw.From, sw.To, strings.ReplaceAll(orDash(sw.Reason), "_", " "))

			if sw.Commit != "" {
				fmt.Fprintf(&s, ", %s's work is commit %s", sw.From, shortSHA(sw.Commit))
			}
		}

		s.WriteString("\n")
	}

	s.WriteString("\n## Checks run by CSM\n\n")

	for _, c := range b.Checks {
		if c.Stage != "integration" {
			continue
		}

		line := fmt.Sprintf("- `%s`: %s", c.Name, strings.ReplaceAll(string(c.State), "_", " "))

		if c.Reason != "" {
			line += " (" + c.Reason + ")"
		}

		s.WriteString(line + "\n")
	}

	s.WriteString("\n## Review\n\n")

	if len(b.Reviews) == 0 {
		s.WriteString("No agent review was run for this change.\n")
	}

	for _, r := range b.Reviews {
		fmt.Fprintf(&s, "- Round %d by %s: %s, %d finding(s)", r.Cycle, agentLabel(r.Provider, r.Account), orDash(r.Verdict), r.FindingsCount)

		if !r.Independent {
			s.WriteString(" (not independent)")
		}

		s.WriteString("\n")
	}

	var open []Finding

	for _, f := range b.Findings {
		if f.Status == findingOpen || f.Status == findingAccepted {
			open = append(open, f)
		}
	}

	if len(open) > 0 {
		s.WriteString("\nOpen findings:\n\n")

		for _, f := range open {
			kind := "not blocking"

			if f.Blocking {
				kind = "blocking, accepted by the user"
			}

			fmt.Fprintf(&s, "- [%s, %s] %s%s (%s)\n", f.Severity, kind, f.Title, location(f), f.Source)
		}
	}

	s.WriteString("\n## Known limitations\n\n")

	n := 0

	for _, t := range b.Tasks {
		if t.Completion == nil {
			continue
		}

		for _, l := range t.Completion.Limitations {
			fmt.Fprintf(&s, "- %s: %s\n", t.ID, l)
			n++
		}
	}

	if n == 0 {
		s.WriteString("- None reported.\n")
	}

	fmt.Fprintf(&s, "\n---\nCoordinated by CSM Build Room, run %s. Agent reports are claims; the checks above are what CSM executed. Review the diff before merging.\n", b.ID)

	return s.String()
}

func (e *buildEngine) pushAndOpenPR(b *Build) {
	if e.busy(b, opPush) {
		return
	}

	if !strings.HasPrefix(b.Integration.Branch, buildBranchRoot) || b.Integration.Branch == b.BaseBranch {
		b.PR.Error = "refusing to push a branch Build Room did not create"

		return
	}

	repo, dir, branch, base, title, body := b.Repo, b.Integration.Worktree, b.Integration.Branch, b.BaseBranch, b.PR.Title, b.PR.BodyFile
	pushed, url := b.PR.Pushed, b.PR.URL
	e.event(b, "pr.pushing", "", "Pushing "+branch+" to origin and opening a pull request")

	e.async(b, opPush, func() func(*Build) {
		if blocker := remoteBlocker(repo, "origin"); blocker != "" {
			return func(b *Build) { b.PR.Error, b.PR.Blocker = blocker, blocker }
		}

		var err error

		if !pushed {
			ref := "refs/heads/" + branch
			_, err = gitIn(dir, "push", "--set-upstream", "origin", ref+":"+ref)
			pushed = err == nil
		}

		if err == nil && url == "" {
			if out, viewErr := runQuiet(repo, "gh", "pr", "view", branch, "--json", "url", "--jq", ".url"); viewErr == nil && strings.HasPrefix(strings.TrimSpace(out), "https://") {
				url = strings.TrimSpace(out)
			} else {
				var out string

				out, err = runQuiet(repo, "gh", "pr", "create", "--base", base, "--head", branch, "--title", title, "--body-file", body)

				if err == nil {
					url = lastURL(out)
				}
			}
		}

		return func(b *Build) {
			b.PR.Pushed = pushed

			if err != nil {
				b.PR.Error = firstLine(handoff.Redact(err.Error()))
				e.event(b, "pr.failed", "", "Push or pull request failed: "+b.PR.Error)

				return
			}

			b.PR.URL, b.PR.State, b.PR.Error = url, "created", ""
			e.event(b, "pr.created", "", "Pull request opened: "+url)

			if e.setStatus(b, BuildCompleted) {
				e.event(b, "completed", "", "Run completed")
			}
		}
	})
}

func lastURL(out string) string {
	for _, f := range slices.Backward(strings.Fields(out)) {
		if strings.HasPrefix(f, "https://") {
			return f
		}
	}

	return ""
}

func lookPath(name string) (string, error) { return exec.LookPath(name) }

// runQuiet runs a tool with an argument list in dir and returns its combined output.
func runQuiet(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(childEnv(), "GH_PROMPT_DISABLED=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()

	if err != nil {
		return string(out), fmt.Errorf("%s %s: %s", name, strings.Join(args[:min(len(args), 2)], " "), firstLine(strings.TrimSpace(string(out))))
	}

	return string(out), nil
}
