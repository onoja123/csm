package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

const maxInstruction = 8000

type createBuildRequest struct {
	Instruction  string      `json:"instruction"`
	ProjectDir   string      `json:"project_dir"`
	BaseBranch   string      `json:"base_branch"`
	Policy       BuildPolicy `json:"policy"`
	CustomChecks []string    `json:"custom_checks"`
	SkipDetected bool        `json:"skip_detected_checks"`
}

type agentChoice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Eligible bool   `json:"eligible"`
	Account  string `json:"account,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type buildPreflight struct {
	Repo     string        `json:"repo"`
	RepoName string        `json:"repo_name"`
	Branch   string        `json:"branch"`
	Branches []string      `json:"branches"`
	Head     string        `json:"head"`
	Dirty    []string      `json:"dirty"`
	Checks   []CheckDef    `json:"checks"`
	Agents   []agentChoice `json:"agents"`
	Defaults BuildPolicy   `json:"defaults"`
}

func defaultPolicy() BuildPolicy {
	return BuildPolicy{MaxParallel: defaultParallel, RequirePlanApproval: true, Review: true, MaxReviewCycles: defaultReviewCycles, RunBaseline: true, AllowRemote: true}
}

func agentChoices(s *State) []agentChoice {
	var out []agentChoice

	for _, id := range provider.IDs {
		c := agentChoice{ID: id, Name: providerName(id)}

		if acct, _, _, err := eligible(s, id, time.Now()); err != nil {
			c.Reason = err.Error()
		} else {
			c.Eligible, c.Account = true, acct.Name
		}

		out = append(out, c)
	}

	return out
}

func preflight(s *State, dir string) (buildPreflight, error) {
	var p buildPreflight

	clean, err := cleanProjectDir(dir)
	if err != nil {
		return p, err
	}

	rs, err := inspectRepo(clean)
	if err != nil {
		return p, err
	}

	p.Repo, p.RepoName, p.Branch, p.Head = rs.Root, displayName(rs.Root), rs.Branch, rs.Head
	p.Dirty = rs.Dirty[:min(len(rs.Dirty), 20)]
	p.Branches = gitList(rs.Root, "for-each-ref", "--count=50", "--sort=-committerdate", "--format=%(refname:short)", "refs/heads")
	p.Branches = slices.DeleteFunc(p.Branches, func(b string) bool { return strings.HasPrefix(b, buildBranchRoot) })
	p.Checks = detectChecks(rs.Root)
	p.Agents = agentChoices(s)
	p.Defaults = defaultPolicy()

	for _, a := range p.Agents {
		if a.Eligible {
			p.Defaults.Agents = append(p.Defaults.Agents, a.ID)
		}
	}

	if p.Checks == nil {
		p.Checks = []CheckDef{}
	}

	return p, nil
}

func displayName(dir string) string {
	parts := strings.Split(strings.TrimRight(dir, "/"), "/")

	return parts[len(parts)-1]
}

// create validates the request against the real repository and the agents that can work now, then records the run; nothing starts before it is saved.
func (e *buildEngine) create(s *State, req createBuildRequest) (*Build, error) {
	instruction := strings.TrimSpace(req.Instruction)

	if instruction == "" {
		return nil, errors.New("describe the change you want")
	}

	if len(instruction) > maxInstruction {
		return nil, fmt.Errorf("the task description is longer than %d characters", maxInstruction)
	}

	dir, err := cleanProjectDir(req.ProjectDir)
	if err != nil {
		return nil, err
	}

	rs, err := inspectRepo(dir)
	if err != nil {
		return nil, err
	}

	base := strings.TrimSpace(req.BaseBranch)

	if base == "" {
		base = rs.Branch
	}

	if base == "" {
		return nil, errors.New("the repository is on a detached HEAD; choose a base branch")
	}

	if !validBranchName(rs.Root, base) || !branchExists(rs.Root, base) {
		return nil, fmt.Errorf("%q is not a local branch of this repository", base)
	}

	commit, err := resolveCommit(rs.Root, base)
	if err != nil {
		return nil, err
	}

	policy := normalizePolicy(req.Policy)

	if !req.SkipDetected {
		policy.Checks = detectChecks(rs.Root)
	} else {
		policy.Checks = nil
	}

	for _, line := range req.CustomChecks {
		if c, ok := customCheck(line); ok && len(policy.Checks) < 12 {
			policy.Checks = append(policy.Checks, c)
		}
	}

	var ready []string

	for _, c := range agentChoices(s) {
		if c.Eligible {
			ready = append(ready, c.ID)
		}
	}

	if len(ready) == 0 {
		return nil, errors.New("no coding agent is ready: install one and sign in to an account first")
	}

	if len(policy.Agents) == 0 {
		policy.Agents = ready
	}

	if !slices.ContainsFunc(policy.Agents, func(id string) bool { return slices.Contains(ready, id) }) {
		return nil, errors.New("none of the chosen agents has a ready account")
	}

	now := time.Now()
	b := &Build{
		Version:     buildVersion,
		ID:          newBuildID(now),
		Instruction: instruction,
		Title:       buildTitle(instruction),
		Repo:        rs.Root,
		RepoName:    displayName(rs.Root),
		BaseBranch:  base,
		BaseCommit:  commit,
		Status:      BuildDraft,
		Policy:      policy,
		Tasks:       []*BuildTask{},
		CreatedAt:   now,
		Dirty:       len(rs.Dirty) > 0 && base == rs.Branch,
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.builds[b.ID] = b
	e.save(b)
	msg := fmt.Sprintf("Run created on %s (%s at %s)", b.RepoName, base, shortSHA(commit))

	if b.Dirty {
		msg += "; uncommitted changes in your working tree are not part of the run and were left untouched"
	}

	e.event(b, "created", "", msg)
	e.poke()

	return b, nil
}

type liveAttempt struct {
	RunStatus    string    `json:"run_status"`
	Provider     string    `json:"provider"`
	Account      string    `json:"account"`
	LastOutputAt time.Time `json:"last_output_at,omitzero"`
}

type buildView struct {
	*Build
	Events   []BuildEvent           `json:"events"`
	Live     map[string]liveAttempt `json:"live"`
	Ops      []string               `json:"ops"`
	Summary  buildSummary           `json:"summary"`
	Verified bool                   `json:"verified"`
	ChecksBy checkSummary           `json:"check_summary"`
}

// view copies the run under the lock, so the response never races the engine.
func (e *buildEngine) view(s *State, id string) (buildView, error) {
	e.mu.Lock()
	b, ok := e.builds[id]

	if !ok {
		e.mu.Unlock()

		return buildView{}, errors.New("no such build run")
	}

	data, _ := json.Marshal(b)
	ops := []string{}

	for op := range e.ops[id] {
		ops = append(ops, op)
	}

	e.mu.Unlock()

	var cp Build

	json.Unmarshal(data, &cp)

	v := buildView{Build: &cp, Events: e.store.events(id), Live: map[string]liveAttempt{}, Ops: ops, Summary: cp.summary()}

	if len(v.Events) > 400 {
		v.Events = v.Events[len(v.Events)-400:]
	}

	var final []CheckResult

	for _, c := range cp.Checks {
		if c.Stage == "integration" {
			final = append(final, c)
		}
	}

	v.ChecksBy = summarizeChecks(final)
	v.Verified = v.ChecksBy.Verified()

	for _, a := range cp.Attempts {
		if a.Status != AttemptRunning && a.Status != AttemptReported {
			continue
		}

		r, ok := e.runs.get(a.RunID)
		if !ok {
			continue
		}

		rv := e.runs.view(s, r)
		v.Live[a.ID] = liveAttempt{RunStatus: rv.Status, Provider: rv.Provider, Account: rv.Account, LastOutputAt: rv.LastOutputAt}
	}

	return v, nil
}

func (e *buildEngine) list() []buildSummary {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := []buildSummary{}

	for _, b := range e.builds {
		out = append(out, b.summary())
	}

	slices.SortFunc(out, func(a, b buildSummary) int { return b.CreatedAt.Compare(a.CreatedAt) })

	return out
}

type planEdit struct {
	Summary string `json:"summary"`
	Tasks   []struct {
		ID         string   `json:"id"`
		Title      string   `json:"title"`
		Objective  string   `json:"objective"`
		Scope      string   `json:"scope"`
		Acceptance []string `json:"acceptance"`
		Context    string   `json:"context"`
		Files      []string `json:"files"`
		DependsOn  []string `json:"depends_on"`
		Provider   string   `json:"provider"`
	} `json:"tasks"`
}

func (e *buildEngine) editPlan(id string, in planEdit) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	b, ok := e.builds[id]
	if !ok {
		return errors.New("no such build run")
	}

	if b.Status != BuildAwaitingPlan && !(b.Status == BuildNeedsAttention && slices.Contains(b.Recovery, "write_plan")) {
		return fmt.Errorf("the plan can only be edited before it is approved (the run is %s)", b.Status)
	}

	var tasks []*BuildTask

	for i, t := range in.Tasks {
		bt := &BuildTask{ID: strings.TrimSpace(t.ID), Kind: TaskKindImplement, Title: clean(t.Title, 120), Objective: clean(t.Objective, 2000), Scope: clean(t.Scope, 1000), Acceptance: cleanAll(t.Acceptance, 12, 300), Context: clean(t.Context, 2000), DependsOn: cleanAll(t.DependsOn, maxPlanTasks, 40), Provider: t.Provider, Status: TaskBlocked, Order: i}

		for _, f := range t.Files {
			if p := cleanPath(f); p != "" {
				bt.Files = append(bt.Files, p)
			}
		}

		tasks = append(tasks, bt)
	}

	if err := validatePlan(tasks); err != nil {
		return err
	}

	b.Tasks = tasks
	b.Plan.EditedByUser = true

	if s := clean(in.Summary, 2000); s != "" {
		b.Plan.Summary = s
	}

	if b.Plan.Source == "" {
		b.Plan.Source = "you"
	}

	b.refreshTaskStates()

	if b.Status == BuildNeedsAttention {
		e.setStatus(b, BuildAwaitingPlan)
	}

	e.event(b, "plan.edited", "", fmt.Sprintf("You edited the plan: %d task(s)", len(tasks)))
	e.save(b)

	return nil
}

type actionRequest struct {
	Task     string `json:"task"`
	Provider string `json:"provider"`
}

var errActionState = errors.New("that action is not available in the run's current state")

// act applies one user decision. Every action checks the current state first, so a repeated click or a stale screen cannot start work twice.
func (e *buildEngine) act(id, action string, req actionRequest) error {
	e.mu.Lock()
	defer func() {
		e.mu.Unlock()
		e.poke()
	}()

	b, ok := e.builds[id]
	if !ok {
		return errors.New("no such build run")
	}

	if b.Status.Terminal() {
		return fmt.Errorf("the run is %s", b.Status)
	}

	if req.Provider != "" && !slices.Contains(provider.IDs, req.Provider) {
		return fmt.Errorf("unknown agent %q", req.Provider)
	}

	err := e.apply(b, action, req)

	if err == nil {
		e.save(b)
	}

	return err
}

func (e *buildEngine) apply(b *Build, action string, req actionRequest) error {
	switch action {
	case "approve-plan":
		if b.Status != BuildAwaitingPlan {
			return errActionState
		}

		if err := validatePlan(b.Tasks); err != nil {
			return err
		}

		e.setStatus(b, BuildReady)
		e.event(b, "plan.approved", "", fmt.Sprintf("You approved the plan with %d task(s)", len(b.Tasks)))

	case "retry-plan":
		if b.Status != BuildNeedsAttention && b.Status != BuildAwaitingPlan {
			return errActionState
		}

		e.startPlanning(b)

	case "pause":
		if b.Paused {
			return nil
		}

		b.Paused = true
		e.event(b, "paused", "", "Paused: no new agents will start; running agents continue")

	case "resume":
		if !b.Paused {
			return nil
		}

		b.Paused = false
		e.event(b, "resumed", "", "Resumed scheduling")

	case "cancel":
		if b.CancelAsked {
			return nil
		}

		b.CancelAsked = true
		e.event(b, "cancel.requested", "", "Cancelling: scheduling stopped and running agents were asked to stop; worktrees are kept")

	case "retry-task":
		t := b.task(req.Task)

		if t == nil || (t.Status != TaskNeedsAttention && t.Status != TaskFailed) {
			return errActionState
		}

		if req.Provider != "" {
			t.Provider = req.Provider
		}

		t.Status, t.Problem, t.Recovery = TaskReady, "", ""
		e.event(b, "task.retry", t.ID, fmt.Sprintf("You retried task %q with %s", t.Title, providerName(t.Provider)))
		e.resumePhase(b, t)

	case "assign":
		t := b.task(req.Task)

		if t == nil || req.Provider == "" || t.Status == TaskRunning || t.Status == TaskAssigned || t.Status == TaskCompleted {
			return errActionState
		}

		t.Provider = req.Provider
		e.event(b, "task.assigned", t.ID, fmt.Sprintf("You assigned task %q to %s", t.Title, providerName(req.Provider)))

	case "accept-task":
		t := b.task(req.Task)

		if t == nil || t.Status != TaskNeedsAttention || t.Worktree == "" {
			return errActionState
		}

		head, _, err := commitAll(t.Worktree, fmt.Sprintf("%s\n\nCSM Build Room task %s, accepted by the user", t.Title, t.ID))
		if err != nil {
			return err
		}

		if t.Completion == nil {
			t.Completion = &Completion{ReportMissing: true}
		}

		t.Completion.FilesChanged, t.Completion.DiffStat, t.Completion.Commit = changedSince(t.Worktree, t.BaseCommit), diffStatSince(t.Worktree, t.BaseCommit), head
		t.Head, t.Status, t.FinishedAt, t.Problem, t.Recovery = head, TaskCompleted, time.Now(), "", ""
		e.event(b, "task.accepted", t.ID, fmt.Sprintf("You accepted task %q as it is (%d changed file(s))", t.Title, len(t.Completion.FilesChanged)))
		e.resumePhase(b, t)

	case "continue-integration":
		if b.Status != BuildNeedsAttention || !slices.Contains(b.Recovery, "continue_integration") {
			return errActionState
		}

		if n := len(b.Integration.Conflicts); n > 0 && mergeInProgress(b.Integration.Worktree) {
			if err := e.concludeMerge(b, &b.Integration.Conflicts[n-1], "Resolved by you"); err != nil {
				return err
			}

			return nil
		}

		e.setStatus(b, BuildIntegrating)

	case "retry-resolve":
		n := len(b.Integration.Conflicts)

		if b.Status != BuildNeedsAttention || n == 0 || !mergeInProgress(b.Integration.Worktree) {
			return errActionState
		}

		c := b.Integration.Conflicts[n-1]
		b.Integration.Conflicts = b.Integration.Conflicts[:n-1]
		e.setStatus(b, BuildIntegrating)
		e.conflictFound(b, c)

	case "accept-findings":
		if b.Status != BuildNeedsAttention || !slices.Contains(b.Recovery, "accept_findings") {
			return errActionState
		}

		n := 0

		for i := range b.Findings {
			if b.Findings[i].Cycle == b.Cycle && b.Findings[i].Blocking && b.Findings[i].Status == findingOpen {
				b.Findings[i].Status = findingAccepted
				n++
			}
		}

		e.setStatus(b, BuildAwaitingFinal)
		e.event(b, "findings.accepted", "", fmt.Sprintf("You accepted %d unresolved blocking finding(s); they are listed in the pull request", n))

	case "retry-review":
		if b.Status != BuildNeedsAttention || !slices.Contains(b.Recovery, "retry_review") {
			return errActionState
		}

		if i := slices.IndexFunc(b.Reviews, func(r ReviewRound) bool { return r.Cycle == b.Cycle }); i >= 0 && b.Reviews[i].Status == "failed" {
			b.Reviews = slices.Delete(b.Reviews, i, i+1)
			b.Cycle--
		} else if b.Cycle >= b.Policy.MaxReviewCycles {
			return errors.New("the review limit for this run has been reached; accept the findings or cancel")
		}

		e.setStatus(b, BuildReviewing)

	case "skip-review":
		if b.Status != BuildNeedsAttention || !slices.Contains(b.Recovery, "skip_review") {
			return errActionState
		}

		e.setStatus(b, BuildAwaitingFinal)
		e.event(b, "review.skipped", "", "You skipped the agent review; the change was not reviewed by an agent")

	case "retry":
		if b.Status != BuildNeedsAttention {
			return errActionState
		}

		switch {
		case len(b.Tasks) == 0:
			e.startPlanning(b)
		case !b.allDone(""):
			e.setStatus(b, BuildRunning)
		case len(b.Integration.Merged) < len(b.Tasks) || b.Integration.Head == "":
			e.setStatus(b, BuildIntegrating)
		default:
			e.setStatus(b, BuildValidating)
		}

	case "approve-pr":
		if b.Status != BuildAwaitingFinal || b.PR.State != "prepared" {
			return errActionState
		}

		if !b.Policy.AllowRemote {
			return errors.New("this run was created without permission for remote operations; the pull request stays local")
		}

		if b.PR.Blocker != "" {
			return errors.New("cannot push: " + b.PR.Blocker)
		}

		b.PR.Approved, b.PR.ApprovedAt = true, time.Now()
		e.event(b, "pr.approved", "", "You approved pushing "+b.Integration.Branch+" and opening a pull request")
		e.pushAndOpenPR(b)

	case "finish":
		if b.Status != BuildAwaitingFinal || e.busy(b, opPush) {
			return errActionState
		}

		e.setStatus(b, BuildCompleted)
		e.event(b, "completed", "", "Run completed; the branch "+b.Integration.Branch+" and the pull request description are ready locally")

	default:
		return fmt.Errorf("unknown action %q", action)
	}

	return nil
}

func (e *buildEngine) resumePhase(b *Build, t *BuildTask) {
	if b.Status != BuildNeedsAttention {
		return
	}

	if t.Kind == TaskKindFix {
		e.setStatus(b, BuildFixing)
	} else {
		e.setStatus(b, BuildRunning)
	}
}

type buildDiff struct {
	Target    string   `json:"target"`
	Base      string   `json:"base"`
	Head      string   `json:"head"`
	Files     []string `json:"files"`
	Stat      string   `json:"stat"`
	Diff      string   `json:"diff"`
	Truncated bool     `json:"truncated"`
}

func (e *buildEngine) diff(id, target string) (buildDiff, error) {
	e.mu.Lock()
	b, ok := e.builds[id]

	var dir, base string

	if ok {
		if target == "integration" {
			dir, base = b.Integration.Worktree, b.BaseCommit
		} else if t := b.task(target); t != nil {
			dir, base = t.Worktree, t.BaseCommit
		}
	}

	e.mu.Unlock()

	if !ok {
		return buildDiff{}, errors.New("no such build run")
	}

	if dir == "" {
		return buildDiff{}, errors.New("there is nothing to compare yet")
	}

	d := buildDiff{Target: target, Base: base}
	d.Head, _ = gitOut(dir, "rev-parse", "HEAD")
	d.Files = changedSince(dir, base)
	d.Stat = diffStatSince(dir, base)
	d.Diff, d.Truncated = diffSince(dir, base)

	if d.Files == nil {
		d.Files = []string{}
	}

	return d, nil
}

// HTTP

func (d *daemon) getBuilds(w http.ResponseWriter, r *http.Request) {
	writeOK(w, d.builds.list())
}

func (d *daemon) getBuildPreflight(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	p, err := preflight(s, r.URL.Query().Get("project_dir"))
	if err != nil {
		writeErr(w, 400, err)

		return
	}

	writeOK(w, p)
}

func (d *daemon) postBuild(w http.ResponseWriter, r *http.Request) {
	var req createBuildRequest

	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, err)

		return
	}

	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	b, err := d.builds.create(s, req)
	if err != nil {
		writeErr(w, 400, err)

		return
	}

	d.rememberProject(b.Repo)

	v, err := d.builds.view(s, b.ID)
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	writeOK(w, v)
}

func (d *daemon) getBuild(w http.ResponseWriter, r *http.Request) {
	s, err := d.state()
	if err != nil {
		writeErr(w, 500, err)

		return
	}

	v, err := d.builds.view(s, r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)

		return
	}

	writeOK(w, v)
}

func (d *daemon) getBuildDiff(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")

	if target == "" {
		target = "integration"
	}

	diff, err := d.builds.diff(r.PathValue("id"), target)
	if err != nil {
		writeErr(w, 404, err)

		return
	}

	writeOK(w, diff)
}

func (d *daemon) putBuildPlan(w http.ResponseWriter, r *http.Request) {
	var in planEdit

	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, err)

		return
	}

	if err := d.builds.editPlan(r.PathValue("id"), in); err != nil {
		writeErr(w, 409, err)

		return
	}

	d.getBuild(w, r)
}

func (d *daemon) postBuildAction(w http.ResponseWriter, r *http.Request) {
	var req actionRequest

	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, err)

		return
	}

	if err := d.builds.act(r.PathValue("id"), r.PathValue("action"), req); err != nil {
		writeErr(w, 409, err)

		return
	}

	d.getBuild(w, r)
}

// buildTitle keeps the lead of an instruction such as "Add team invitations: …" as the run's name.
func buildTitle(instruction string) string {
	line := firstLine(instruction)

	if i := strings.Index(line, ": "); i >= 8 && i <= 80 {
		line = line[:i]
	}

	return clean(line, 90)
}
