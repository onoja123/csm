package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

const (
	buildsDir       = "builds"
	buildFile       = "build.json"
	buildEventsFile = "events.jsonl"
	buildVersion    = 1

	maxPlanTasks        = 12
	maxBuildParallel    = 4
	defaultParallel     = 2
	defaultReviewCycles = 2
	maxReviewCycles     = 3
	maxTaskAttempts     = 3
)

type BuildStatus string

const (
	BuildDraft          BuildStatus = "draft"
	BuildPlanning       BuildStatus = "planning"
	BuildAwaitingPlan   BuildStatus = "awaiting_plan_approval"
	BuildReady          BuildStatus = "ready"
	BuildRunning        BuildStatus = "running"
	BuildIntegrating    BuildStatus = "integrating"
	BuildReviewing      BuildStatus = "reviewing"
	BuildFixing         BuildStatus = "fixing"
	BuildValidating     BuildStatus = "validating"
	BuildAwaitingFinal  BuildStatus = "awaiting_final_approval"
	BuildCompleted      BuildStatus = "completed"
	BuildFailed         BuildStatus = "failed"
	BuildCancelled      BuildStatus = "cancelled"
	BuildNeedsAttention BuildStatus = "needs_attention"
)

var buildTransitions = map[BuildStatus][]BuildStatus{
	BuildDraft:          {BuildPlanning, BuildAwaitingPlan, BuildCancelled, BuildFailed},
	BuildPlanning:       {BuildAwaitingPlan, BuildReady, BuildNeedsAttention, BuildCancelled, BuildFailed},
	BuildAwaitingPlan:   {BuildReady, BuildPlanning, BuildCancelled},
	BuildReady:          {BuildRunning, BuildCancelled, BuildNeedsAttention},
	BuildRunning:        {BuildIntegrating, BuildNeedsAttention, BuildCancelled, BuildFailed},
	BuildIntegrating:    {BuildValidating, BuildNeedsAttention, BuildCancelled, BuildFailed},
	BuildValidating:     {BuildReviewing, BuildFixing, BuildAwaitingFinal, BuildNeedsAttention, BuildCancelled, BuildFailed},
	BuildReviewing:      {BuildFixing, BuildAwaitingFinal, BuildNeedsAttention, BuildCancelled, BuildFailed},
	BuildFixing:         {BuildIntegrating, BuildNeedsAttention, BuildCancelled, BuildFailed},
	BuildAwaitingFinal:  {BuildCompleted, BuildNeedsAttention, BuildCancelled},
	BuildNeedsAttention: {BuildPlanning, BuildAwaitingPlan, BuildRunning, BuildIntegrating, BuildValidating, BuildReviewing, BuildFixing, BuildAwaitingFinal, BuildCancelled, BuildFailed},
}

func (s BuildStatus) Terminal() bool {
	return s == BuildCompleted || s == BuildFailed || s == BuildCancelled
}

type TaskStatus string

const (
	TaskBlocked        TaskStatus = "blocked"
	TaskReady          TaskStatus = "ready"
	TaskAssigned       TaskStatus = "assigned"
	TaskRunning        TaskStatus = "running"
	TaskNeedsReview    TaskStatus = "needs_review"
	TaskCompleted      TaskStatus = "completed"
	TaskFailed         TaskStatus = "failed"
	TaskNeedsAttention TaskStatus = "needs_attention"
	TaskCancelled      TaskStatus = "cancelled"
)

const (
	TaskKindImplement = "implement"
	TaskKindFix       = "fix"
)

const (
	RolePlan      = "plan"
	RoleImplement = "implement"
	RoleFix       = "fix"
	RoleResolve   = "resolve"
	RoleReview    = "review"
)

const (
	AttemptRunning     = "running"
	AttemptReported    = "reported"
	AttemptCompleted   = "completed"
	AttemptFailed      = "failed"
	AttemptStopped     = "stopped"
	AttemptInterrupted = "interrupted"
)

type CheckState string

const (
	CheckPassed          CheckState = "passed"
	CheckFailed          CheckState = "failed"
	CheckNotRun          CheckState = "not_run"
	CheckSkipped         CheckState = "skipped"
	CheckBlocked         CheckState = "blocked"
	CheckBaselineFailure CheckState = "baseline_failure"
)

type BuildPolicy struct {
	MaxParallel         int        `json:"max_parallel"`
	RequirePlanApproval bool       `json:"require_plan_approval"`
	Review              bool       `json:"review"`
	MaxReviewCycles     int        `json:"max_review_cycles"`
	Agents              []string   `json:"agents,omitempty"`
	Planner             string     `json:"planner,omitempty"`
	Reviewer            string     `json:"reviewer,omitempty"`
	Checks              []CheckDef `json:"checks,omitempty"`
	RunBaseline         bool       `json:"run_baseline"`
	AllowRemote         bool       `json:"allow_remote"`
}

type CheckDef struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
	Source  string   `json:"source"`
}

type Plan struct {
	Summary         string   `json:"summary,omitempty"`
	Layers          []string `json:"layers,omitempty"`
	Considerations  []string `json:"considerations,omitempty"`
	SuggestedChecks []string `json:"suggested_checks,omitempty"`
	Source          string   `json:"source,omitempty"`
	EditedByUser    bool     `json:"edited_by_user,omitempty"`
}

type BuildTask struct {
	ID         string        `json:"id"`
	Kind       string        `json:"kind"`
	Title      string        `json:"title"`
	Objective  string        `json:"objective"`
	Scope      string        `json:"scope,omitempty"`
	Acceptance []string      `json:"acceptance,omitempty"`
	Context    string        `json:"context,omitempty"`
	Files      []string      `json:"files,omitempty"`
	DependsOn  []string      `json:"depends_on,omitempty"`
	Provider   string        `json:"provider,omitempty"`
	Account    string        `json:"account,omitempty"`
	Status     TaskStatus    `json:"status"`
	Branch     string        `json:"branch,omitempty"`
	Worktree   string        `json:"worktree,omitempty"`
	BaseCommit string        `json:"base_commit,omitempty"`
	Head       string        `json:"head,omitempty"`
	Attempts   []Attempt     `json:"attempts,omitempty"`
	Completion *Completion   `json:"completion,omitempty"`
	Findings   []string      `json:"findings,omitempty"`
	Cycle      int           `json:"cycle,omitempty"`
	Problem    string        `json:"problem,omitempty"`
	Recovery   string        `json:"recovery,omitempty"`
	FinishedAt time.Time     `json:"finished_at,omitzero"`
	StartedAt  time.Time     `json:"started_at,omitzero"`
	Order      int           `json:"order"`
	Integrated bool          `json:"integrated,omitempty"`
	Switches   []AgentSwitch `json:"switches,omitempty"`
}

type Attempt struct {
	ID         string        `json:"id"`
	Role       string        `json:"role"`
	TaskID     string        `json:"task_id,omitempty"`
	Provider   string        `json:"provider"`
	Account    string        `json:"account,omitempty"`
	RunID      string        `json:"run_id,omitempty"`
	CSMPID     int           `json:"csm_pid,omitempty"`
	Dir        string        `json:"dir"`
	Report     string        `json:"report"`
	Status     string        `json:"status"`
	StartedAt  time.Time     `json:"started_at"`
	EndedAt    time.Time     `json:"ended_at,omitzero"`
	ExitCode   *int          `json:"exit_code,omitempty"`
	Note       string        `json:"note,omitempty"`
	Switches   []AgentSwitch `json:"switches,omitempty"`
	ReportedAt time.Time     `json:"reported_at,omitzero"`
	// StartedAs is the agent the attempt began with when the runner later moved it to another account or agent.
	StartedAs string `json:"started_as,omitempty"`
}

// AgentSwitch is a change of account or agent the runner made inside one attempt, read from its handoff records.
type AgentSwitch struct {
	At      time.Time `json:"at"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	Reason  string    `json:"reason,omitempty"`
	Started bool      `json:"started"`
	Handoff string    `json:"handoff,omitempty"`
	// Commit holds the outgoing agent's work at the switch; the task's own commit then holds only the receiving agent's.
	Commit      string `json:"commit,omitempty"`
	FromSession string `json:"from_session,omitempty"`
	ToSession   string `json:"to_session,omitempty"`
}

// Completion separates what CSM measured from what the agent claimed; Claimed* fields are never treated as facts.
type Completion struct {
	FilesChanged      []string          `json:"files_changed"`
	DiffStat          string            `json:"diff_stat,omitempty"`
	Commit            string            `json:"commit,omitempty"`
	Summary           string            `json:"summary,omitempty"`
	ClaimedFiles      []string          `json:"claimed_files,omitempty"`
	ClaimedCommands   []string          `json:"claimed_commands,omitempty"`
	ClaimedTests      []string          `json:"claimed_tests,omitempty"`
	Limitations       []string          `json:"limitations,omitempty"`
	Acceptance        []AcceptanceClaim `json:"acceptance,omitempty"`
	Discrepancies     []string          `json:"discrepancies,omitempty"`
	ReportMissing     bool              `json:"report_missing,omitempty"`
	ReportUnparseable bool              `json:"report_unparseable,omitempty"`
}

type AcceptanceClaim struct {
	Criterion string `json:"criterion"`
	Addressed bool   `json:"addressed"`
	Note      string `json:"note,omitempty"`
}

type CheckResult struct {
	Name       string     `json:"name"`
	Command    []string   `json:"command"`
	Source     string     `json:"source"`
	Stage      string     `json:"stage"`
	State      CheckState `json:"state"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	DurationMS int64      `json:"duration_ms"`
	Output     string     `json:"output,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	At         time.Time  `json:"at"`
	Baseline   CheckState `json:"baseline,omitempty"`
}

type Finding struct {
	ID             string `json:"id"`
	Cycle          int    `json:"cycle"`
	Severity       string `json:"severity"`
	File           string `json:"file,omitempty"`
	Line           int    `json:"line,omitempty"`
	Title          string `json:"title"`
	Explanation    string `json:"explanation,omitempty"`
	Reasoning      string `json:"reasoning,omitempty"`
	Recommendation string `json:"recommendation,omitempty"`
	Blocking       bool   `json:"blocking"`
	TaskID         string `json:"task_id,omitempty"`
	Status         string `json:"status"`
	Source         string `json:"source"`
}

type ReviewRound struct {
	Cycle          int       `json:"cycle"`
	Provider       string    `json:"provider"`
	Account        string    `json:"account,omitempty"`
	Independent    bool      `json:"independent"`
	Note           string    `json:"note,omitempty"`
	Verdict        string    `json:"verdict,omitempty"`
	Summary        string    `json:"summary,omitempty"`
	Commit         string    `json:"commit,omitempty"`
	IgnoredEdits   []string  `json:"ignored_edits,omitempty"`
	Status         string    `json:"status"`
	At             time.Time `json:"at"`
	Worktree       string    `json:"worktree,omitempty"`
	BlockingOpen   int       `json:"blocking_open"`
	FindingsCount  int       `json:"findings"`
	ReportProblems string    `json:"report_problems,omitempty"`
}

type Overlap struct {
	File  string   `json:"file"`
	Tasks []string `json:"tasks"`
}

type Conflict struct {
	TaskID     string    `json:"task_id"`
	Files      []string  `json:"files"`
	Tasks      []string  `json:"tasks"`
	Status     string    `json:"status"`
	Resolution string    `json:"resolution,omitempty"`
	At         time.Time `json:"at"`
}

type Integration struct {
	Branch    string     `json:"branch,omitempty"`
	Worktree  string     `json:"worktree,omitempty"`
	Head      string     `json:"head,omitempty"`
	Merged    []string   `json:"merged,omitempty"`
	Overlaps  []Overlap  `json:"overlaps,omitempty"`
	Conflicts []Conflict `json:"conflicts,omitempty"`
	DiffStat  string     `json:"diff_stat,omitempty"`
	Files     []string   `json:"files,omitempty"`
	Pending   string     `json:"pending,omitempty"`
}

type PullRequest struct {
	State      string    `json:"state"`
	Title      string    `json:"title,omitempty"`
	BodyFile   string    `json:"body_file,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	Base       string    `json:"base,omitempty"`
	Remote     string    `json:"remote,omitempty"`
	URL        string    `json:"url,omitempty"`
	Pushed     bool      `json:"pushed,omitempty"`
	Approved   bool      `json:"approved,omitempty"`
	ApprovedAt time.Time `json:"approved_at,omitzero"`
	Error      string    `json:"error,omitempty"`
	Blocker    string    `json:"blocker,omitempty"`
}

type Build struct {
	Version     int           `json:"version"`
	ID          string        `json:"id"`
	Instruction string        `json:"instruction"`
	Title       string        `json:"title"`
	Repo        string        `json:"repo"`
	RepoName    string        `json:"repo_name"`
	BaseBranch  string        `json:"base_branch"`
	BaseCommit  string        `json:"base_commit"`
	Status      BuildStatus   `json:"status"`
	Paused      bool          `json:"paused,omitempty"`
	CancelAsked bool          `json:"cancel_requested,omitempty"`
	Attention   string        `json:"attention,omitempty"`
	Recovery    []string      `json:"recovery,omitempty"`
	Policy      BuildPolicy   `json:"policy"`
	Plan        Plan          `json:"plan"`
	Tasks       []*BuildTask  `json:"tasks"`
	Attempts    []Attempt     `json:"attempts,omitempty"`
	Checks      []CheckResult `json:"checks,omitempty"`
	Reviews     []ReviewRound `json:"reviews,omitempty"`
	Findings    []Finding     `json:"findings,omitempty"`
	Integration Integration   `json:"integration"`
	PR          PullRequest   `json:"pr"`
	Cycle       int           `json:"cycle"`
	CreatedAt   time.Time     `json:"created_at"`
	StartedAt   time.Time     `json:"started_at,omitzero"`
	CompletedAt time.Time     `json:"completed_at,omitzero"`
	UpdatedAt   time.Time     `json:"updated_at"`
	Error       string        `json:"error,omitempty"`
	Seq         int           `json:"seq"`
	Dirty       bool          `json:"source_dirty,omitempty"`
}

type BuildEvent struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	TaskID  string    `json:"task_id,omitempty"`
	Message string    `json:"message"`
}

var errBadTransition = errors.New("invalid build transition")

func (b *Build) transition(to BuildStatus) error {
	if b.Status == to {
		return nil
	}

	if !slices.Contains(buildTransitions[b.Status], to) {
		return fmt.Errorf("%w: %s → %s", errBadTransition, b.Status, to)
	}

	b.Status = to

	if to.Terminal() {
		b.CompletedAt = time.Now()
	}

	if to != BuildNeedsAttention {
		b.Attention, b.Recovery = "", nil
	}

	return nil
}

func (b *Build) task(id string) *BuildTask {
	for _, t := range b.Tasks {
		if t.ID == id {
			return t
		}
	}

	return nil
}

func (b *Build) nextID(prefix string) string {
	b.Seq++

	return fmt.Sprintf("%s%d", prefix, b.Seq)
}

func newBuildID(now time.Time) string {
	var r [3]byte

	rand.Read(r[:])

	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(r[:])
}

var buildIDPattern = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

func validBuildID(id string) bool { return buildIDPattern.MatchString(id) }

var taskIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

func normalizePolicy(p BuildPolicy) BuildPolicy {
	if p.MaxParallel <= 0 {
		p.MaxParallel = defaultParallel
	}

	p.MaxParallel = min(p.MaxParallel, maxBuildParallel)

	if p.MaxReviewCycles <= 0 {
		p.MaxReviewCycles = defaultReviewCycles
	}

	p.MaxReviewCycles = min(p.MaxReviewCycles, maxReviewCycles)

	var agents []string

	for _, id := range p.Agents {
		if slices.Contains(provider.IDs, id) && !slices.Contains(agents, id) {
			agents = append(agents, id)
		}
	}

	p.Agents = agents

	if !slices.Contains(provider.IDs, p.Planner) {
		p.Planner = ""
	}

	if !slices.Contains(provider.IDs, p.Reviewer) {
		p.Reviewer = ""
	}

	return p
}

// validatePlan checks a plan the planner wrote or the user edited before anything runs: bounded size, unique ids, known agents, existing dependencies and no cycles.
func validatePlan(tasks []*BuildTask) error {
	if len(tasks) == 0 {
		return errors.New("the plan has no tasks")
	}

	if len(tasks) > maxPlanTasks {
		return fmt.Errorf("the plan has %d tasks; at most %d are allowed", len(tasks), maxPlanTasks)
	}

	ids := map[string]bool{}

	for _, t := range tasks {
		if !taskIDPattern.MatchString(t.ID) {
			return fmt.Errorf("task id %q must be lowercase letters, digits and dashes", t.ID)
		}

		if ids[t.ID] {
			return fmt.Errorf("task id %q is used twice", t.ID)
		}

		ids[t.ID] = true

		if strings.TrimSpace(t.Title) == "" || strings.TrimSpace(t.Objective) == "" {
			return fmt.Errorf("task %s needs a title and an objective", t.ID)
		}

		if t.Provider != "" && !slices.Contains(provider.IDs, t.Provider) {
			return fmt.Errorf("task %s is assigned to unknown agent %q", t.ID, t.Provider)
		}
	}

	for _, t := range tasks {
		for _, d := range t.DependsOn {
			if d == t.ID {
				return fmt.Errorf("task %s depends on itself", t.ID)
			}

			if !ids[d] {
				return fmt.Errorf("task %s depends on %q, which is not in the plan", t.ID, d)
			}
		}
	}

	if cycle := findCycle(tasks); len(cycle) > 0 {
		return fmt.Errorf("the dependencies form a cycle: %s", strings.Join(cycle, " → "))
	}

	return nil
}

func findCycle(tasks []*BuildTask) []string {
	deps := map[string][]string{}

	for _, t := range tasks {
		deps[t.ID] = t.DependsOn
	}

	const (
		unseen = iota
		visiting
		done
	)

	mark := map[string]int{}

	var stack []string
	var found []string
	var visit func(id string) bool

	visit = func(id string) bool {
		mark[id] = visiting
		stack = append(stack, id)

		for _, d := range deps[id] {
			switch mark[d] {
			case visiting:
				i := slices.Index(stack, d)
				found = append(append([]string{}, stack[i:]...), d)

				return true
			case unseen:
				if visit(d) {
					return true
				}
			}
		}

		stack = stack[:len(stack)-1]
		mark[id] = done

		return false
	}

	for _, t := range tasks {
		if mark[t.ID] == unseen && visit(t.ID) {
			return found
		}
	}

	return nil
}

// topoOrder lists tasks so every task comes after its dependencies, keeping plan order among equals.
func topoOrder(tasks []*BuildTask) []*BuildTask {
	byID := map[string]*BuildTask{}

	for _, t := range tasks {
		byID[t.ID] = t
	}

	placed := map[string]bool{}

	var out []*BuildTask

	for len(out) < len(tasks) {
		progressed := false

		for _, t := range tasks {
			if placed[t.ID] {
				continue
			}

			ready := true

			for _, d := range t.DependsOn {
				if _, known := byID[d]; known && !placed[d] {
					ready = false
				}
			}

			if ready {
				placed[t.ID] = true
				out = append(out, t)
				progressed = true
			}
		}

		if !progressed {
			break
		}
	}

	return out
}

// refreshTaskStates moves tasks between blocked and ready from their dependencies; it never touches a task that is running or finished.
func (b *Build) refreshTaskStates() {
	for _, t := range b.Tasks {
		if t.Status != TaskBlocked && t.Status != TaskReady {
			continue
		}

		ready := true

		for _, d := range t.DependsOn {
			if dep := b.task(d); dep == nil || dep.Status != TaskCompleted {
				ready = false
			}
		}

		if ready {
			t.Status = TaskReady
		} else {
			t.Status = TaskBlocked
		}
	}
}

// schedulable picks the ready tasks that may start now: within the parallel limit, and never alongside a running task that expects to touch the same files.
func (b *Build) schedulable() []*BuildTask {
	if b.Paused || b.CancelAsked {
		return nil
	}

	b.refreshTaskStates()

	var active []*BuildTask

	for _, t := range b.Tasks {
		if t.Status == TaskRunning || t.Status == TaskAssigned {
			active = append(active, t)
		}
	}

	capacity := b.Policy.MaxParallel - len(active)

	var out []*BuildTask

	for _, t := range b.Tasks {
		if capacity <= 0 {
			break
		}

		if t.Status != TaskReady {
			continue
		}

		if slices.ContainsFunc(active, func(o *BuildTask) bool { return sharesFiles(o, t) }) {
			continue
		}

		out = append(out, t)
		active = append(active, t)
		capacity--
	}

	return out
}

func sharesFiles(a, b *BuildTask) bool {
	for _, f := range a.Files {
		if slices.Contains(b.Files, f) {
			return true
		}
	}

	return false
}

func (b *Build) implementTasks() []*BuildTask {
	var out []*BuildTask

	for _, t := range b.Tasks {
		if t.Kind != TaskKindFix {
			out = append(out, t)
		}
	}

	return out
}

func (b *Build) allDone(kind string) bool {
	for _, t := range b.Tasks {
		if (kind == "" || t.Kind == kind || (kind == TaskKindImplement && t.Kind == "")) && t.Status != TaskCompleted {
			return false
		}
	}

	return true
}

func (b *Build) runningAttempts() int {
	n := 0

	for _, a := range b.Attempts {
		if a.Status == AttemptRunning || a.Status == AttemptReported {
			n++
		}
	}

	return n
}

func (b *Build) attempt(id string) *Attempt {
	for i := range b.Attempts {
		if b.Attempts[i].ID == id {
			return &b.Attempts[i]
		}
	}

	return nil
}

func (b *Build) openBlocking() []Finding {
	var out []Finding

	for _, f := range b.Findings {
		if f.Blocking && f.Status == "open" {
			out = append(out, f)
		}
	}

	return out
}

// ownerOf maps a file to the task whose branch changed it, so a finding goes back to the agent that wrote the code.
func (b *Build) ownerOf(file string) string {
	if file == "" {
		return ""
	}

	for i := len(b.Tasks) - 1; i >= 0; i-- {
		t := b.Tasks[i]

		if t.Completion != nil && slices.Contains(t.Completion.FilesChanged, file) {
			return t.ID
		}
	}

	return ""
}

// summary is the run as the overview needs it, grounded in task states.
type buildSummary struct {
	ID          string      `json:"id"`
	Title       string      `json:"title"`
	Instruction string      `json:"instruction"`
	Repo        string      `json:"repo"`
	RepoName    string      `json:"repo_name"`
	BaseBranch  string      `json:"base_branch"`
	Status      BuildStatus `json:"status"`
	Paused      bool        `json:"paused"`
	Attention   string      `json:"attention,omitempty"`
	Tasks       int         `json:"tasks"`
	Completed   int         `json:"tasks_completed"`
	Running     int         `json:"tasks_running"`
	Failed      int         `json:"tasks_failed"`
	Agents      []string    `json:"agents"`
	PRURL       string      `json:"pr_url,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
	CompletedAt time.Time   `json:"completed_at,omitzero"`
}

func (b *Build) summary() buildSummary {
	s := buildSummary{ID: b.ID, Title: b.Title, Instruction: b.Instruction, Repo: b.Repo, RepoName: b.RepoName, BaseBranch: b.BaseBranch, Status: b.Status, Paused: b.Paused, Attention: b.Attention, PRURL: b.PR.URL, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt, CompletedAt: b.CompletedAt, Agents: []string{}}

	for _, t := range b.Tasks {
		s.Tasks++

		switch t.Status {
		case TaskCompleted:
			s.Completed++
		case TaskRunning, TaskAssigned:
			s.Running++
		case TaskFailed, TaskNeedsAttention:
			s.Failed++
		}

		if t.Provider != "" && !slices.Contains(s.Agents, t.Provider) {
			s.Agents = append(s.Agents, t.Provider)
		}
	}

	return s
}

type buildStore struct {
	root string
}

func newBuildStore(home string) buildStore {
	return buildStore{root: filepath.Join(home, buildsDir)}
}

func (s buildStore) dir(id string) string { return filepath.Join(s.root, id) }

func (s buildStore) save(b *Build) error {
	if !validBuildID(b.ID) {
		return fmt.Errorf("invalid build id %q", b.ID)
	}

	if err := os.MkdirAll(s.dir(b.ID), 0o700); err != nil {
		return err
	}

	b.UpdatedAt = time.Now()

	return writeJSON(filepath.Join(s.dir(b.ID), buildFile), b)
}

func (s buildStore) load(id string) (*Build, error) {
	if !validBuildID(id) {
		return nil, fmt.Errorf("invalid build id %q", id)
	}

	var b Build

	if err := readJSON(filepath.Join(s.dir(id), buildFile), &b); err != nil {
		return nil, err
	}

	return &b, nil
}

func (s buildStore) all() []*Build {
	entries, _ := os.ReadDir(s.root)

	var out []*Build

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		if b, err := s.load(e.Name()); err == nil {
			out = append(out, b)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })

	return out
}

func (s buildStore) appendEvent(id string, ev BuildEvent) {
	f, err := os.OpenFile(filepath.Join(s.dir(id), buildEventsFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}

	defer f.Close()

	if line, err := json.Marshal(ev); err == nil {
		f.Write(append(line, '\n'))
	}
}

func (s buildStore) events(id string) []BuildEvent {
	f, err := os.Open(filepath.Join(s.dir(id), buildEventsFile))
	if err != nil {
		return []BuildEvent{}
	}

	defer f.Close()

	out := []BuildEvent{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)

	for sc.Scan() {
		var ev BuildEvent

		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			out = append(out, ev)
		}
	}

	return out
}
