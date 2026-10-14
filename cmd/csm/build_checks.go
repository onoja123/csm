package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/onoja123/csm/internal/handoff"
)

const checkOutputMax = 6 << 10

var checkTimeout = 10 * time.Minute

// detectChecks reads the repository's own build files for the commands its maintainers run; it never takes commands from an agent.
func detectChecks(dir string) []CheckDef {
	var out []CheckDef

	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))

		return err == nil
	}

	if exists("go.mod") {
		out = append(out,
			CheckDef{Name: "go build", Command: []string{"go", "build", "./..."}, Source: "go.mod"},
			CheckDef{Name: "go vet", Command: []string{"go", "vet", "./..."}, Source: "go.mod"},
			CheckDef{Name: "go test", Command: []string{"go", "test", "./..."}, Source: "go.mod"},
		)
	}

	if exists("package.json") {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}

		if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil && json.Unmarshal(data, &pkg) == nil {
			runner := "npm"

			switch {
			case exists("pnpm-lock.yaml"):
				runner = "pnpm"
			case exists("yarn.lock"):
				runner = "yarn"
			case exists("bun.lockb") || exists("bun.lock"):
				runner = "bun"
			}

			for _, name := range []string{"lint", "typecheck", "test", "build"} {
				if _, ok := pkg.Scripts[name]; ok {
					out = append(out, CheckDef{Name: runner + " run " + name, Command: []string{runner, "run", name}, Source: "package.json"})
				}
			}
		}
	}

	if exists("Cargo.toml") {
		out = append(out,
			CheckDef{Name: "cargo build", Command: []string{"cargo", "build"}, Source: "Cargo.toml"},
			CheckDef{Name: "cargo test", Command: []string{"cargo", "test"}, Source: "Cargo.toml"},
		)
	}

	if exists("pytest.ini") || exists("pyproject.toml") && fileContains(filepath.Join(dir, "pyproject.toml"), "pytest") {
		out = append(out, CheckDef{Name: "pytest", Command: []string{"python3", "-m", "pytest", "-q"}, Source: "pytest config"})
	}

	if len(out) == 0 && exists("Makefile") && makeHasTarget(filepath.Join(dir, "Makefile"), "test") {
		out = append(out, CheckDef{Name: "make test", Command: []string{"make", "test"}, Source: "Makefile"})
	}

	return out
}

func fileContains(path, needle string) bool {
	data, err := os.ReadFile(path)

	return err == nil && bytes.Contains(data, []byte(needle))
}

func makeHasTarget(path, target string) bool {
	data, err := os.ReadFile(path)

	return err == nil && regexp.MustCompile(`(?m)^`+regexp.QuoteMeta(target)+`\s*:`).Match(data)
}

// customCheck turns a command the user typed when creating the run into a check; it runs through the shell because the user wrote it as a shell command.
func customCheck(line string) (CheckDef, bool) {
	line = strings.TrimSpace(line)

	if line == "" || len(line) > 500 {
		return CheckDef{}, false
	}

	return CheckDef{Name: line, Command: []string{"/bin/sh", "-c", line}, Source: "custom"}, true
}

// linkDependencies points a fresh worktree at the repository's installed node_modules when git ignores that directory, so JavaScript checks can run without installing anything.
func linkDependencies(repo, worktree string) string {
	src := filepath.Join(repo, "node_modules")

	if _, err := os.Stat(filepath.Join(worktree, "package.json")); err != nil {
		return ""
	}

	if _, err := os.Stat(src); err != nil {
		return "node_modules is not installed in the repository, so JavaScript checks may fail in the worktree."
	}

	dest := filepath.Join(worktree, "node_modules")

	if _, err := os.Lstat(dest); err == nil {
		return ""
	}

	if _, err := gitOut(worktree, "check-ignore", "-q", "node_modules"); err != nil {
		return "node_modules is not ignored by git here, so CSM did not link it into the worktree."
	}

	if err := os.Symlink(src, dest); err != nil {
		return "Could not link node_modules into the worktree: " + err.Error()
	}

	return ""
}

func runCheck(dir string, def CheckDef, stage string) CheckResult {
	res := CheckResult{Name: def.Name, Command: def.Command, Source: def.Source, Stage: stage, At: time.Now()}

	if len(def.Command) == 0 {
		res.State, res.Reason = CheckSkipped, "no command"

		return res
	}

	if _, err := exec.LookPath(def.Command[0]); err != nil {
		res.State, res.Reason = CheckBlocked, def.Command[0]+" is not installed or not on PATH"

		return res
	}

	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, def.Command[0], def.Command[1:]...)
	cmd.Dir = dir
	cmd.Env = append(childEnv(), "CI=1", "NO_COLOR=1")

	var buf bytes.Buffer

	cmd.Stdout, cmd.Stderr = &buf, &buf
	start := time.Now()
	err := cmd.Run()
	res.DurationMS = time.Since(start).Milliseconds()
	res.Output = tail(handoff.Redact(buf.String()), checkOutputMax)
	code := exitCode(err)
	res.ExitCode = &code

	var exitErr *exec.ExitError

	switch {
	case ctx.Err() != nil:
		res.State, res.Reason, res.TimedOut = CheckFailed, "timed out after "+checkTimeout.String(), true
	case err == nil:
		res.State = CheckPassed
	case errors.As(err, &exitErr):
		res.State = CheckFailed
	default:
		res.State, res.Reason = CheckBlocked, err.Error()
	}

	return res
}

// atRevision runs a check and records the revision it ran on, read before the command so a command that edits the tree cannot change the answer.
func atRevision(check func(string, CheckDef, string) CheckResult) func(string, CheckDef, string) CheckResult {
	return func(dir string, def CheckDef, stage string) CheckResult {
		head, _ := gitOut(dir, "rev-parse", "HEAD")
		dirty := len(worktreeChanges(dir)) > 0
		res := check(dir, def, stage)
		res.Commit, res.Dirty = head, dirty

		return res
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return "…" + s[len(s)-n:]
}

// compareBaseline marks a failure that already happened on the base commit, and only then; without a baseline result a failure stays a failure.
func compareBaseline(final, baseline []CheckResult) []CheckResult {
	out := make([]CheckResult, len(final))

	for i, r := range final {
		for _, b := range baseline {
			if b.Name == r.Name {
				r.Baseline = b.State

				if r.State == CheckFailed && b.State == CheckFailed {
					r.State = CheckBaselineFailure
				}
			}
		}

		out[i] = r
	}

	return out
}

type checkSummary struct {
	Passed, Failed, Baseline, Blocked, Skipped, NotRun int
}

func summarizeChecks(results []CheckResult) checkSummary {
	var s checkSummary

	for _, r := range results {
		switch r.State {
		case CheckPassed:
			s.Passed++
		case CheckFailed:
			s.Failed++
		case CheckBaselineFailure:
			s.Baseline++
		case CheckBlocked:
			s.Blocked++
		case CheckSkipped:
			s.Skipped++
		default:
			s.NotRun++
		}
	}

	return s
}

// Verified means every configured check ran and passed; a skipped, blocked or pre-existing failure keeps the run from being called verified.
func (s checkSummary) Verified() bool {
	return s.Passed > 0 && s.Failed+s.Baseline+s.Blocked+s.Skipped+s.NotRun == 0
}
