package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
	"github.com/onoja123/csm/internal/provider"
)

const (
	outputKeep     = 1 << 20
	runKindAgent   = "agent"
	runKindHandoff = "handoff"
	runStatusLive  = "running"
	runStatusDone  = "exited"
)

// run is one agent the service started in a pseudo-terminal. The child is `csm <agent>` itself, so every switch, failover and handoff rule stays in the runner.
type runInfo struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Provider   string    `json:"provider"`
	Account    string    `json:"account"`
	ProjectDir string    `json:"project_dir"`
	Branch     string    `json:"branch,omitempty"`
	Args       []string  `json:"args,omitempty"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitzero"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	CSMPID     int       `json:"csm_pid"`
}

// agentRun is one agent the service started in a pseudo-terminal. The child is `csm <agent>` itself, so every switch, failover and handoff rule stays in the runner.
type agentRun struct {
	runInfo

	mu           sync.Mutex
	cmd          *exec.Cmd
	tty          *os.File
	buf          []byte
	lastOutputAt time.Time
	task         taskInfo
	taskCheck    time.Time

	// The virtual terminal keeps the screen a phone can show as text; the raw bytes still go to desktop clients untouched.
	vtMu       sync.Mutex
	vt         vt10x.Terminal
	screenPend *time.Timer
}

type screenView struct {
	RunID   string   `json:"run_id"`
	Cols    int      `json:"cols"`
	Rows    int      `json:"rows"`
	Lines   []string `json:"lines"`
	CursorX int      `json:"cursor_x"`
	CursorY int      `json:"cursor_y"`
	Cursor  bool     `json:"cursor_visible"`
	Alt     bool     `json:"alt_screen"`
}

type taskInfo struct {
	Summary       string    `json:"summary,omitempty"`
	SessionID     string    `json:"session_id,omitempty"`
	Messages      int       `json:"messages,omitempty"`
	FilesModified []string  `json:"files_modified,omitempty"`
	ReadAt        time.Time `json:"read_at,omitzero"`
}

type runView struct {
	runInfo
	LastOutputAt time.Time `json:"last_output_at,omitzero"`
	SessionID    string    `json:"session_id,omitempty"`
	AgentPID     int       `json:"agent_pid,omitempty"`
	Task         taskInfo  `json:"task"`
	Duration     int64     `json:"duration_seconds"`
}

type runManager struct {
	mu      sync.Mutex
	runs    map[string]*agentRun
	seq     int
	home    string
	csmPath string
	socket  string
	bus     *eventBus
	log     *activityLog
}

func newRunManager(home, csmPath string, bus *eventBus, log *activityLog) *runManager {
	return &runManager{runs: map[string]*agentRun{}, home: home, csmPath: csmPath, socket: socketPath(home), bus: bus, log: log}
}

type startRequest struct {
	Provider   string   `json:"provider"`
	ProjectDir string   `json:"project_dir"`
	Args       []string `json:"args,omitempty"`
	Resume     string   `json:"resume_session_id,omitempty"`
	Cols       int      `json:"cols,omitempty"`
	Rows       int      `json:"rows,omitempty"`
}

func (m *runManager) start(s *State, req startRequest) (*agentRun, error) {
	if _, err := provider.New(req.Provider, ""); err != nil {
		return nil, err
	}

	if len(s.accountNames(req.Provider)) == 0 {
		return nil, fmt.Errorf("no %s accounts configured; add one with: csm account add <name> --provider %s", providerName(req.Provider), req.Provider)
	}

	dir, err := cleanProjectDir(req.ProjectDir)
	if err != nil {
		return nil, err
	}

	args := append([]string{}, req.Args...)

	if req.Resume != "" {
		args = append([]string{"--resume", req.Resume}, args...)
	}

	return m.spawn(s, runKindAgent, req.Provider, dir, append([]string{req.Provider}, args...), args, req.Cols, req.Rows)
}

func (m *runManager) startHandoff(s *State, providerID, projectDir, targetAccount string, cols, rows int) (*agentRun, error) {
	dir, err := cleanProjectDir(projectDir)
	if err != nil {
		return nil, err
	}

	return m.spawn(s, runKindHandoff, providerID, dir, []string{"handoff", "--yes", targetAccount}, nil, cols, rows)
}

// childEnv drops markers an enclosing agent session leaves behind, so an agent started here is a fresh top-level session.
func childEnv() []string {
	var env []string

	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CLAUDE_CODE_") || strings.HasPrefix(kv, "CLAUDECODE=") || strings.HasPrefix(kv, "CSM_SOCKET=") || strings.HasPrefix(kv, "CSM_HOME=") {
			continue
		}

		env = append(env, kv)
	}

	return env
}

func cleanProjectDir(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("project_dir is required")
	}

	if strings.HasPrefix(dir, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, dir[2:])
		}
	}

	dir = filepath.Clean(dir)
	fi, err := os.Stat(dir)

	if err != nil {
		return "", fmt.Errorf("project directory: %w", err)
	}

	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}

	return dir, nil
}

func (m *runManager) spawn(s *State, kind, providerID, dir string, csmArgs, userArgs []string, cols, rows int) (*agentRun, error) {
	proj := detectProject(dir)

	if kind == runKindAgent {
		if sess, live := s.loadLiveSession(providerID, proj.Dir); live {
			return nil, fmt.Errorf("csm is already managing %s for %s (pid %d, account %s)", providerName(providerID), displayPath(proj.Dir), sess.CSMPID, sess.Account)
		}
	}

	cmd := exec.Command(m.csmPath, csmArgs...)
	cmd.Dir = dir
	cmd.Env = append(childEnv(), "CSM_HOME="+m.home, "CSM_SOCKET="+m.socket, "TERM=xterm-256color", "COLORTERM=truecolor")

	if cols <= 0 || rows <= 0 {
		cols, rows = 120, 36
	}

	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", providerName(providerID), err)
	}

	m.mu.Lock()
	m.seq++
	r := &agentRun{
		runInfo: runInfo{
			ID:         time.Now().UTC().Format("20060102T150405Z") + "-" + itoa(m.seq),
			Kind:       kind,
			Provider:   providerID,
			Account:    s.active(providerID),
			ProjectDir: proj.Dir,
			Branch:     proj.Branch,
			Args:       userArgs,
			Status:     runStatusLive,
			StartedAt:  time.Now(),
			CSMPID:     cmd.Process.Pid,
		},
		cmd: cmd,
		tty: tty,
		vt:  vt10x.New(vt10x.WithSize(cols, rows)),
	}
	m.runs[r.ID] = r
	m.mu.Unlock()

	title := providerName(providerID) + " started"

	if kind == runKindHandoff {
		title = "Handoff to " + csmArgs[len(csmArgs)-1] + " started"
	}

	m.log.add(Activity{Kind: "run.started", Provider: providerID, Account: r.Account, Title: title, Detail: displayPath(proj.Dir), RunID: r.ID})
	m.bus.publish("runs", nil)

	go m.pump(r)

	return r, nil
}

func (m *runManager) pump(r *agentRun) {
	buf := make([]byte, 32*1024)

	for {
		n, err := r.tty.Read(buf)

		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			r.mu.Lock()
			r.buf = append(r.buf, chunk...)

			if len(r.buf) > outputKeep {
				r.buf = r.buf[len(r.buf)-outputKeep:]
			}

			r.lastOutputAt = time.Now()
			r.mu.Unlock()
			m.bus.publish("run.output", map[string]string{"run_id": r.ID, "data": base64.StdEncoding.EncodeToString(chunk)})
			r.feedScreen(chunk, func() { m.bus.publish("run.screen", r.screen()) })
		}

		if err != nil {
			break
		}
	}

	waitErr := r.cmd.Wait()
	code := exitCode(waitErr)
	r.mu.Lock()
	r.Status = runStatusDone
	r.EndedAt = time.Now()
	r.ExitCode = &code
	r.mu.Unlock()
	r.tty.Close()
	m.log.add(Activity{Kind: "run.exited", Provider: r.Provider, Account: r.Account, Title: providerName(r.Provider) + " exited", Detail: fmt.Sprintf("exit status %d", code), RunID: r.ID})
	m.bus.publish("run.exit", map[string]any{"run_id": r.ID, "exit_code": code})
	m.bus.publish("runs", nil)
}

func (m *runManager) get(id string) (*agentRun, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.runs[id]

	return r, ok
}

func (m *runManager) byPID(pid int) (*agentRun, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, r := range m.runs {
		if r.CSMPID == pid && r.Status == runStatusLive {
			return r, true
		}
	}

	return nil, false
}

func (m *runManager) liveFor(providerID, projectDir string) (*agentRun, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, r := range m.runs {
		if r.Status == runStatusLive && r.Provider == providerID && (projectDir == "" || r.ProjectDir == projectDir) {
			return r, true
		}
	}

	return nil, false
}

func (m *runManager) list() []*agentRun {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]*agentRun, 0, len(m.runs))

	for _, r := range m.runs {
		out = append(out, r)
	}

	sortRuns(out)

	return out
}

func sortRuns(rs []*agentRun) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].StartedAt.After(rs[j-1].StartedAt); j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

func (m *runManager) forget(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.runs[id]
	if !ok {
		return errors.New("no such run")
	}

	if r.Status == runStatusLive {
		return errors.New("the run is still live; stop it first")
	}

	delete(m.runs, id)
	m.bus.publish("runs", nil)

	return nil
}

func (r *agentRun) write(data []byte) error {
	if r.Status != runStatusLive {
		return errors.New("the run has exited")
	}

	_, err := r.tty.Write(data)

	return err
}

func (r *agentRun) resize(cols, rows int) error {
	if r.Status != runStatusLive || cols <= 0 || rows <= 0 {
		return nil
	}

	r.vtMu.Lock()
	r.vt.Resize(cols, rows)
	r.vtMu.Unlock()

	return pty.Setsize(r.tty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (r *agentRun) feedScreen(chunk []byte, publish func()) {
	r.vtMu.Lock()
	r.vt.Write(chunk)

	if r.screenPend == nil {
		r.screenPend = time.AfterFunc(150*time.Millisecond, func() {
			r.vtMu.Lock()
			r.screenPend = nil
			r.vtMu.Unlock()
			publish()
		})
	}

	r.vtMu.Unlock()
}

func (r *agentRun) screen() screenView {
	r.vtMu.Lock()
	defer r.vtMu.Unlock()

	return screenOf(r.ID, r.vt)
}

func screenOf(id string, t vt10x.Terminal) screenView {
	t.Lock()
	defer t.Unlock()

	cols, rows := t.Size()
	v := screenView{RunID: id, Cols: cols, Rows: rows, Lines: make([]string, rows), Cursor: t.CursorVisible(), Alt: t.Mode()&vt10x.ModeAltScreen != 0}
	cur := t.Cursor()
	v.CursorX, v.CursorY = cur.X, cur.Y

	for y := 0; y < rows; y++ {
		line := make([]rune, 0, cols)

		for x := 0; x < cols; x++ {
			ch := t.Cell(x, y).Char

			if ch == 0 {
				ch = ' '
			}

			line = append(line, ch)
		}

		v.Lines[y] = strings.TrimRight(string(line), " ")
	}

	return v
}

func (r *agentRun) output() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]byte(nil), r.buf...)
}

// stop asks the child csm to end its agent and escalates to the whole process group if nothing happened after the grace period.
func (r *agentRun) stop(grace time.Duration) {
	if r.Status != runStatusLive {
		return
	}

	r.cmd.Process.Signal(syscall.SIGTERM)

	go func() {
		time.Sleep(grace)

		if r.Status == runStatusLive {
			syscall.Kill(-r.CSMPID, syscall.SIGKILL)
		}
	}()
}

func (m *runManager) stopAll(grace time.Duration) {
	for _, r := range m.list() {
		r.stop(grace)
	}

	deadline := time.Now().Add(grace + 2*time.Second)

	for time.Now().Before(deadline) {
		live := false

		for _, r := range m.list() {
			if r.Status == runStatusLive {
				live = true
			}
		}

		if !live {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// view joins what the runner wrote to disk (the account after a switch, the session id, the agent pid) with what the service knows.
func (m *runManager) view(s *State, r *agentRun) runView {
	r.mu.Lock()
	v := runView{runInfo: r.runInfo, LastOutputAt: r.lastOutputAt, Task: r.task}
	r.mu.Unlock()

	end := time.Now()

	if !v.EndedAt.IsZero() {
		end = v.EndedAt
	}

	v.Duration = int64(end.Sub(v.StartedAt).Seconds())

	if v.Status != runStatusLive {
		return v
	}

	for _, id := range provider.IDs {
		sess, live := s.loadLiveSession(id, r.ProjectDir)

		if live && sess.CSMPID == r.CSMPID {
			v.Provider, v.Account, v.SessionID, v.AgentPID = providerOrDefault(sess.Provider), sess.Account, sess.SessionID, sess.ProcessPID
			r.mu.Lock()
			r.Account = v.Account
			r.mu.Unlock()
		}
	}

	if v.SessionID == "" {
		var ps providerSession

		if readJSON(filepath.Join(s.projectStateDir(v.Provider, r.ProjectDir), providerSessionFile), &ps) == nil {
			v.SessionID = ps.SessionID
		}
	}

	v.Task = m.taskFor(s, r, v)

	return v
}

// taskFor reads the agent's own session record for the last thing the user asked; it is a label, never a progress figure.
func (m *runManager) taskFor(s *State, r *agentRun, v runView) taskInfo {
	r.mu.Lock()
	cached, checked := r.task, r.taskCheck
	r.mu.Unlock()

	if v.SessionID == "" || time.Since(checked) < 10*time.Second {
		return cached
	}

	r.mu.Lock()
	r.taskCheck = time.Now()
	r.mu.Unlock()

	acct, err := s.resolveAccount(v.Account)
	if err != nil {
		return cached
	}

	p, err := provider.New(v.Provider, "")
	if err != nil {
		return cached
	}

	extractor, ok := provider.Extractor(p)
	if !ok {
		return cached
	}

	ctx, err := extractor.SessionContext(acct.ConfigDir, r.ProjectDir, v.SessionID)
	if err != nil {
		return cached
	}

	info := taskInfo{SessionID: v.SessionID, Messages: ctx.Messages, FilesModified: ctx.FilesModified, ReadAt: time.Now()}

	if n := len(ctx.UserRequests); n > 0 {
		info.Summary = firstLine(ctx.UserRequests[n-1])
	}

	r.mu.Lock()
	r.task = info
	r.mu.Unlock()

	return info
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)

	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}

	if len(s) > 140 {
		s = s[:140] + "…"
	}

	return s
}

var _ = io.EOF
