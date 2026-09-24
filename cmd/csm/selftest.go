package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

type failoverResult struct {
	exitCode   int
	active     string
	checkpoint Checkpoint
	resumed    bool
	gitBefore  string
	gitAfter   string
	output     string
	usage      map[string]UsageRecord
}

type scenario struct {
	limited   string
	fakeError string
	hold      string
	before    func(s *State, projectDir string)
	during    func(home, projectDir string)
}

func runFailoverScenario(csmPath string, sc scenario) (failoverResult, error) {
	var res failoverResult
	tmp, err := os.MkdirTemp("", "csm-failover-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)
	tmp, _ = filepath.EvalSymlinks(tmp)

	project := filepath.Join(tmp, "project")
	git := func(args ...string) (string, error) {
		out, err := exec.Command("git", append([]string{"-C", project, "-c", "user.name=csm", "-c", "user.email=csm@example.test"}, args...)...).CombinedOutput()
		return string(out), err
	}
	os.MkdirAll(project, 0o700)
	if _, err := git("init", "-q", "-b", "cards"); err != nil {
		return res, fmt.Errorf("git init: %w", err)
	}
	os.WriteFile(filepath.Join(project, "main.go"), []byte("package main\n"), 0o600)
	git("add", ".")
	if out, err := git("commit", "-q", "-m", "init"); err != nil {
		return res, fmt.Errorf("git commit: %s", out)
	}
	os.WriteFile(filepath.Join(project, "main.go"), []byte("package main // uncommitted\n"), 0o600)
	res.gitBefore, _ = git("status", "--short", "--branch")

	shim := filepath.Join(tmp, "claude")
	script := fmt.Sprintf("#!/bin/sh\nexec '%s' __fake-claude \"$@\"\n", strings.ReplaceAll(csmPath, "'", `'\''`))
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		return res, err
	}
	for key, value := range map[string]string{"CSM_CLAUDE": shim, "CSM_FAKE_LIMIT": sc.limited, "CSM_FAKE_ERROR": sc.fakeError, "CSM_FAKE_HOLD": sc.hold} {
		defer os.Setenv(key, os.Getenv(key))
		os.Setenv(key, value)
	}

	p := provider.Claude{Path: shim}
	s, err := initState(filepath.Join(tmp, "csm"))
	if err != nil {
		return res, err
	}
	for _, name := range []string{"alpha", "beta"} {
		a, err := s.addAccount(name, time.Now())
		if err != nil {
			return res, err
		}
		os.MkdirAll(a.ConfigDir, 0o700)
		if err := p.Login(a.ConfigDir); err != nil {
			return res, err
		}
		st, err := verifyProfile(p, a)
		if err != nil {
			return res, err
		}
		a.Email, a.VerifiedAt = st.Email, time.Now()
	}
	s.Config.AutoFailover = true
	if err := s.save(); err != nil {
		return res, err
	}

	features, err := p.Features()
	if err != nil {
		return res, err
	}
	var out bytes.Buffer
	r := &runner{
		state:       s,
		provider:    p,
		csmPath:     csmPath,
		features:    features,
		cwd:         project,
		project:     detectProject(project),
		out:         &out,
		in:          bufio.NewReader(strings.NewReader("")),
		unavailable: map[string]bool{},
	}
	r.stateDir = s.projectStateDir(r.project.Dir)
	os.MkdirAll(r.stateDir, 0o700)

	if sc.before != nil {
		sc.before(s, r.project.Dir)
	}
	if sc.during != nil {
		go sc.during(s.Home, r.project.Dir)
	}
	res.exitCode, err = r.run(nil)
	res.output = out.String()
	if err != nil {
		return res, err
	}
	res.active = s.Config.ActiveAccount
	res.checkpoint, _ = s.loadCheckpoint(r.project.Dir)
	res.gitAfter, _ = git("status", "--short", "--branch")
	res.usage = map[string]UsageRecord{}
	for _, name := range s.Config.AccountOrder {
		if rec, ok := s.loadUsage(name); ok {
			res.usage[name] = rec
		}
	}
	if res.checkpoint.SessionID != "" {
		matches, _ := filepath.Glob(filepath.Join(s.accountsDir(), "beta", "projects", "*", res.checkpoint.SessionID+".jsonl"))
		if len(matches) == 1 {
			data, _ := os.ReadFile(matches[0])
			res.resumed = strings.Contains(string(data), `"profile":"beta","resumed":true`)
		}
	}
	if _, ok, _ := s.loadTransition(r.project.Dir); ok {
		return res, errors.New("transition record left behind after a completed switch")
	}
	return res, nil
}

func cmdTestFailover() error {
	csmPath, err := os.Executable()
	if err != nil {
		return err
	}
	fmt.Println("Simulating failover with a fake Claude Code (no real usage)...")
	fmt.Println()

	res, err := runFailoverScenario(csmPath, scenario{limited: "alpha"})
	if err != nil {
		return fmt.Errorf("FAIL: %w\n\n%s", err, res.output)
	}
	var failures []string
	if res.active != "beta" {
		failures = append(failures, fmt.Sprintf("active account is %q, want beta", res.active))
	}
	if res.checkpoint.Account != "alpha" || res.checkpoint.Reason != provider.StopReasonUsageLimit {
		failures = append(failures, fmt.Sprintf("checkpoint = %+v", res.checkpoint))
	}
	if !res.resumed {
		failures = append(failures, "beta did not resume the alpha session")
	}
	if res.gitBefore != res.gitAfter {
		failures = append(failures, fmt.Sprintf("git state changed:\n%s\n→\n%s", res.gitBefore, res.gitAfter))
	}

	exhausted, err := runFailoverScenario(csmPath, scenario{limited: "alpha,beta"})
	if err != nil {
		return fmt.Errorf("FAIL: %w\n\n%s", err, exhausted.output)
	}
	if exhausted.active != "beta" || !strings.Contains(exhausted.output, "All configured accounts are currently unavailable") {
		failures = append(failures, "with every account limited, failover did not pause after one pass")
	}

	network, err := runFailoverScenario(csmPath, scenario{limited: "alpha", fakeError: "overloaded"})
	if err != nil {
		return fmt.Errorf("FAIL: %w\n\n%s", err, network.output)
	}
	if network.active != "alpha" {
		failures = append(failures, "a network/overloaded error caused an account switch")
	}

	if len(failures) > 0 {
		return fmt.Errorf("FAIL\n\n%s\n\nOutput:\n%s", strings.Join(failures, "\n"), res.output)
	}
	fmt.Println("PASS")
	fmt.Println()
	fmt.Println("alpha → beta")
	fmt.Println("checkpoint saved")
	fmt.Println("session restarted (resumed " + res.checkpoint.SessionID[:8] + " under beta)")
	fmt.Println("project preserved (branch and uncommitted changes untouched)")
	fmt.Println("all accounts limited → failover paused, no cycling")
	fmt.Println("overloaded error → no switch")
	return nil
}
