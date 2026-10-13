// Package sandbox runs repository code CSM does not trust (tests that reproduce a failure, a repository's checks) in a
// resource-limited container with no network, no credentials and only the run's own worktree mounted.
package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimeout = 10 * time.Minute
	outputKeep     = 64 << 10
	label          = "csm.sandbox"
)

// Spec is one command to run against a worktree. Command is an argument list, never a shell string.
type Spec struct {
	Dir     string
	Command []string
	Image   string
	Timeout time.Duration
	CPUs    string
	Memory  string
	// Network is off unless a run's policy names it; most reproductions need none.
	Network bool
	// ReadOnly are host paths mounted read-only, such as a Go module cache, keyed by their path in the container.
	ReadOnly map[string]string
	// Cache is a host directory owned by the run, mounted read-write at /cache so builds are not repeated from scratch.
	// It holds build output only and is never shared between runs.
	Cache string
	Env   []string
}

type Result struct {
	ExitCode    int           `json:"exit_code"`
	Output      string        `json:"output,omitempty"`
	Duration    time.Duration `json:"duration"`
	TimedOut    bool          `json:"timed_out,omitempty"`
	Sandboxed   bool          `json:"sandboxed"`
	Runner      string        `json:"runner"`
	Image       string        `json:"image,omitempty"`
	Error       string        `json:"error,omitempty"`
	Limits      string        `json:"limits,omitempty"`
	StartedAt   time.Time     `json:"started_at"`
	Command     []string      `json:"command"`
	unavailable bool
}

// Ran reports whether the command executed at all; a false value means the sandbox itself was unavailable or broke.
func (r Result) Ran() bool { return r.Error == "" }

type Runner interface {
	Name() string
	Available(ctx context.Context) error
	Run(ctx context.Context, spec Spec) Result
}

var ErrUnavailable = errors.New("the sandbox is not available")

// Docker runs each command in a fresh container that is removed afterwards. Docker is not a complete isolation boundary
// (a kernel shared with the host, image supply chain); the flags below remove the easy escapes: no network, no
// capabilities, no privilege escalation, read-only root, bounded CPU, memory and processes, the host user's uid, and
// nothing mounted but the worktree and explicit read-only caches. The Docker socket and credentials are never mounted.
type Docker struct {
	Binary string
}

func (d Docker) Name() string { return "docker" }

func (d Docker) bin() string {
	if d.Binary != "" {
		return d.Binary
	}

	return "docker"
}

func (d Docker) Available(ctx context.Context) error {
	if _, err := exec.LookPath(d.bin()); err != nil {
		return fmt.Errorf("%w: docker is not installed", ErrUnavailable)
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if out, err := exec.CommandContext(ctx, d.bin(), "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		return fmt.Errorf("%w: the Docker daemon is not reachable (%s)", ErrUnavailable, firstLine(string(out)))
	}

	return nil
}

func (d Docker) Run(ctx context.Context, spec Spec) Result {
	res := Result{Runner: "docker", Sandboxed: true, Image: spec.Image, StartedAt: time.Now(), Command: spec.Command}

	if err := validate(spec); err != nil {
		res.Error = err.Error()

		return res
	}

	if err := d.Available(ctx); err != nil {
		res.Error, res.unavailable = err.Error(), true

		return res
	}

	timeout, cpus, memory := orDuration(spec.Timeout, defaultTimeout), orString(spec.CPUs, "2"), orString(spec.Memory, "2g")
	res.Limits = fmt.Sprintf("%s CPU, %s memory, 512 processes, %s, network %s", cpus, memory, timeout, map[bool]string{true: "on", false: "off"}[spec.Network])
	name := "csm-sandbox-" + randomID()
	args := d.args(name, spec, cpus, memory)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(d.bin(), args...)

	var out bytes.Buffer

	cmd.Stdout, cmd.Stderr = &out, &out

	if err := cmd.Start(); err != nil {
		res.Error = "start docker: " + err.Error()

		return res
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var err error

	select {
	case err = <-done:
	case <-ctx.Done():
		res.TimedOut = true
		d.remove(name)
		err = <-done
	}

	d.remove(name)
	res.Duration = time.Since(res.StartedAt)
	res.Output = tail(out.String())

	var exitErr *exec.ExitError

	switch {
	case res.TimedOut:
		res.ExitCode = -1
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()

		// 125 is docker's own failure (bad image, daemon error), not the command's.
		if res.ExitCode == 125 {
			res.Error = "docker could not start the container: " + firstLine(res.Output)

			if strings.Contains(res.Output, "operation not permitted") || strings.Contains(res.Output, "mounts denied") {
				res.Error += " (Docker cannot read " + spec.Dir + ". On macOS, keep CSM's home outside Documents, Desktop and Downloads, or share the folder in Docker Desktop under Settings → Resources → File sharing.)"
			}
		}
	default:
		res.Error = err.Error()
	}

	return res
}

func (d Docker) args(name string, spec Spec, cpus, memory string) []string {
	args := []string{"run", "--rm", "--name", name, "--label", label + "=1",
		"--cpus", cpus, "--memory", memory, "--memory-swap", memory, "--pids-limit", "512",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only",
		"--tmpfs", "/tmp:rw,exec,size=1g",
		"--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		"-v", spec.Dir + ":/work:rw", "-w", "/work",
		"-e", "HOME=/tmp", "-e", "GOCACHE=/tmp/go-cache", "-e", "GOTOOLCHAIN=local", "-e", "CGO_ENABLED=0", "-e", "GOFLAGS=-mod=readonly",
		"-e", "PIP_NO_INDEX=1", "-e", "CI=1",
	}

	if !spec.Network {
		args = append(args, "--network", "none")
	}

	for inside, host := range spec.ReadOnly {
		args = append(args, "-v", host+":"+inside+":ro")
	}

	if spec.Cache != "" {
		args = append(args, "-v", spec.Cache+":/cache:rw", "-e", "GOCACHE=/cache/go-build", "-e", "npm_config_cache=/cache/npm")
	}

	for _, kv := range spec.Env {
		args = append(args, "-e", kv)
	}

	return append(append(args, "--entrypoint", spec.Command[0], spec.Image), spec.Command[1:]...)
}

func (d Docker) remove(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	exec.CommandContext(ctx, d.bin(), "rm", "-f", name).Run()
}

func validate(spec Spec) error {
	if len(spec.Command) == 0 || spec.Command[0] == "" {
		return errors.New("no command")
	}

	if spec.Image == "" || strings.HasPrefix(spec.Image, "-") || strings.ContainsAny(spec.Image, " \t\n") {
		return fmt.Errorf("invalid image %q", spec.Image)
	}

	abs, err := filepath.Abs(spec.Dir)
	if err != nil || abs != spec.Dir {
		return fmt.Errorf("the worktree path must be absolute")
	}

	if fi, err := os.Stat(spec.Dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("the worktree %s does not exist", spec.Dir)
	}

	home, _ := os.UserHomeDir()

	for _, forbidden := range []string{"/", home, "/var/run/docker.sock", filepath.Join(home, ".ssh"), filepath.Join(home, ".csm")} {
		if spec.Dir == forbidden {
			return fmt.Errorf("refusing to mount %s into a sandbox", spec.Dir)
		}
	}

	if spec.Cache != "" {
		if !filepath.IsAbs(spec.Cache) || spec.Cache == home || strings.Contains(spec.Cache, ".ssh") || strings.Contains(spec.Cache, "docker.sock") {
			return fmt.Errorf("refusing to mount %s as a cache", spec.Cache)
		}

		if err := os.MkdirAll(spec.Cache, 0o700); err != nil {
			return err
		}
	}

	for inside, host := range spec.ReadOnly {
		if !strings.HasPrefix(inside, "/") || strings.Contains(host, "docker.sock") || strings.Contains(host, ".ssh") || strings.Contains(host, ".csm") {
			return fmt.Errorf("refusing to mount %s", host)
		}
	}

	return nil
}

// Unavailable says whether a result failed because the sandbox could not run at all, as opposed to the command failing.
func Unavailable(r Result) bool { return r.unavailable }

// Host runs a command directly in the worktree. It is not a sandbox and is used only when a run's policy explicitly
// allows unsandboxed execution; its results are marked as such.
type Host struct{}

func (Host) Name() string { return "host" }

func (Host) Available(context.Context) error { return nil }

func (Host) Run(ctx context.Context, spec Spec) Result {
	res := Result{Runner: "host", StartedAt: time.Now(), Command: spec.Command}

	if len(spec.Command) == 0 {
		res.Error = "no command"

		return res
	}

	ctx, cancel := context.WithTimeout(ctx, orDuration(spec.Timeout, defaultTimeout))
	defer cancel()

	cmd := exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = append(os.Environ(), append([]string{"CI=1"}, spec.Env...)...)

	var out bytes.Buffer

	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	res.Duration, res.Output = time.Since(res.StartedAt), tail(out.String())

	var exitErr *exec.ExitError

	switch {
	case ctx.Err() != nil:
		res.TimedOut, res.ExitCode = true, -1
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		res.Error = err.Error()
	}

	return res
}

// Image picks a toolchain image for a repository from its build files.
func Image(dir string) string {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))

		return err == nil
	}

	switch {
	case exists("go.mod"):
		return "golang:1.25-alpine"
	case exists("package.json"):
		return "node:22-alpine"
	case exists("pyproject.toml") || exists("requirements.txt") || exists("setup.py"):
		return "python:3.12-slim"
	}

	return ""
}

// Caches returns read-only dependency caches worth mounting for a repository: only package sources, never credentials.
func Caches(dir string) map[string]string {
	out := map[string]string{}

	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		if gomod, err := exec.Command("go", "env", "GOMODCACHE").Output(); err == nil {
			if p := strings.TrimSpace(string(gomod)); p != "" {
				if _, err := os.Stat(p); err == nil {
					out["/go/pkg/mod"] = p
				}
			}
		}
	}

	return out
}

func randomID() string {
	var b [6]byte

	rand.Read(b[:])

	return hex.EncodeToString(b[:])
}

func tail(s string) string {
	if len(s) > outputKeep {
		return "…" + s[len(s)-outputKeep:]
	}

	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)

	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}

	return s
}

func orDuration(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}

	return def
}

func orString(s, def string) string {
	if s != "" {
		return s
	}

	return def
}
