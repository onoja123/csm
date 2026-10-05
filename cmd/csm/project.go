package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

const (
	sessionFile    = "session.json"
	checkpointFile = "checkpoint.json"
	transitionFile = "transition.json"
	eventFile      = "event.json"
	requestFile    = "switch-request.json"
	historyDir     = "history"
)

type Project struct {
	Dir    string
	Branch string
	IsGit  bool
}

func detectProject(cwd string) Project {
	p := Project{Dir: cwd}
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return p
	}

	p.IsGit = true
	p.Dir = strings.TrimSpace(string(out))
	out, err = exec.Command("git", "-C", cwd, "branch", "--show-current").Output()
	if err == nil {
		p.Branch = strings.TrimSpace(string(out))
	}

	return p
}

func gitStatusShort(dir string) string {
	out, err := exec.Command("git", "-C", dir, "status", "--short", "--branch").Output()
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(out))
}

func projectID(dir string) string {
	sum := sha256.Sum256([]byte(dir))

	return filepath.Base(dir) + "-" + hex.EncodeToString(sum[:])[:12]
}

func (s *State) projectStateDir(providerID, dir string) string {
	id := projectID(dir)

	if providerID = providerOrDefault(providerID); providerID != provider.ClaudeID {
		id += "-" + providerID
	}

	return filepath.Join(s.projectsDir(), id)
}

type Session struct {
	Version    int       `json:"version"`
	Provider   string    `json:"provider,omitempty"`
	ProjectDir string    `json:"project_dir"`
	Account    string    `json:"account"`
	SessionID  string    `json:"session_id"`
	CSMPID     int       `json:"csm_pid"`
	ProcessPID int       `json:"process_pid"`
	StartedAt  time.Time `json:"started_at"`
}

type Checkpoint struct {
	Version        int                 `json:"version"`
	Provider       string              `json:"provider,omitempty"`
	ProjectDir     string              `json:"project_dir"`
	Account        string              `json:"account"`
	SessionID      string              `json:"session_id"`
	StartedAt      time.Time           `json:"started_at"`
	CheckpointedAt time.Time           `json:"checkpointed_at"`
	Branch         string              `json:"branch"`
	Reason         provider.StopReason `json:"reason"`
	Detail         string              `json:"detail,omitempty"`
}

type Transition struct {
	Version    int                 `json:"version"`
	Provider   string              `json:"provider,omitempty"`
	ProjectDir string              `json:"project_dir"`
	From       string              `json:"from"`
	To         string              `json:"to"`
	SessionID  string              `json:"session_id"`
	Reason     provider.StopReason `json:"reason"`
	CSMPID     int                 `json:"csm_pid"`
	StartedAt  time.Time           `json:"started_at"`
}

type SwitchRequest struct {
	To          string    `json:"to"`
	RequestedAt time.Time `json:"requested_at"`
}

func (s *State) saveCheckpoint(c Checkpoint) error {
	dir := s.projectStateDir(c.Provider, c.ProjectDir)

	if err := os.MkdirAll(filepath.Join(dir, historyDir), 0o700); err != nil {
		return err
	}

	name := c.CheckpointedAt.UTC().Format("20060102T150405Z") + "-" + c.Account + ".json"

	if err := writeJSON(filepath.Join(dir, historyDir, name), c); err != nil {
		return err
	}

	return writeJSON(filepath.Join(dir, checkpointFile), c)
}

func (s *State) loadCheckpoint(providerID, projectDir string) (Checkpoint, error) {
	var c Checkpoint

	err := readJSON(filepath.Join(s.projectStateDir(providerID, projectDir), checkpointFile), &c)

	return c, err
}

func (s *State) loadTransition(providerID, projectDir string) (Transition, bool, error) {
	var t Transition

	err := readJSON(filepath.Join(s.projectStateDir(providerID, projectDir), transitionFile), &t)
	if errors.Is(err, os.ErrNotExist) {
		return t, false, nil
	}

	return t, err == nil, err
}

func (s *State) loadLiveSession(providerID, projectDir string) (Session, bool) {
	var sess Session

	if err := readJSON(filepath.Join(s.projectStateDir(providerID, projectDir), sessionFile), &sess); err != nil {
		return sess, false
	}

	return sess, processAlive(sess.CSMPID)
}

func (s *State) liveSessions() []Session {
	matches, _ := filepath.Glob(filepath.Join(s.projectsDir(), "*", sessionFile))
	var live []Session

	for _, path := range matches {
		var sess Session

		if readJSON(path, &sess) == nil && processAlive(sess.CSMPID) {
			live = append(live, sess)
		}
	}

	return live
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	err := syscall.Kill(pid, 0)

	return err == nil || errors.Is(err, syscall.EPERM)
}
