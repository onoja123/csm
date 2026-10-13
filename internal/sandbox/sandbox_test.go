package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDockerArgsLockTheContainerDown(t *testing.T) {
	args := Docker{}.args("n", Spec{Dir: "/w", Command: []string{"go", "test", "./..."}, Image: "golang:1.25-alpine", ReadOnly: map[string]string{"/go/pkg/mod": "/cache"}}, "2", "2g")
	joined := strings.Join(args, " ")

	for _, want := range []string{"--network none", "--cap-drop ALL", "--security-opt no-new-privileges", "--read-only", "--pids-limit 512", "--memory 2g", "-v /w:/work:rw", "-v /cache:/go/pkg/mod:ro", "--entrypoint go golang:1.25-alpine test ./..."} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}

	if c := strings.Join(Docker{}.args("n", Spec{Dir: "/w", Command: []string{"x"}, Image: "i", Cache: "/runs/r1/cache"}, "1", "1g"), " "); !strings.Contains(c, "-v /runs/r1/cache:/cache:rw") || !strings.Contains(c, "GOCACHE=/cache/go-build") {
		t.Fatalf("cache mount: %s", c)
	}

	if slices.Contains(args, "--privileged") || strings.Contains(joined, "docker.sock") {
		t.Fatal(joined)
	}

	if strings.Contains(strings.Join(Docker{}.args("n", Spec{Dir: "/w", Command: []string{"x"}, Image: "i", Network: true}, "1", "1g"), " "), "--network none") {
		t.Fatal("network is only off by default")
	}
}

func TestValidateRefusesDangerousSpecs(t *testing.T) {
	dir := t.TempDir()
	home, _ := os.UserHomeDir()

	for name, spec := range map[string]Spec{
		"no command":   {Dir: dir, Image: "alpine"},
		"option image": {Dir: dir, Image: "--privileged", Command: []string{"sh"}},
		"relative dir": {Dir: "work", Image: "alpine", Command: []string{"sh"}},
		"home":         {Dir: home, Image: "alpine", Command: []string{"sh"}},
		"root":         {Dir: "/", Image: "alpine", Command: []string{"sh"}},
		"socket":       {Dir: dir, Image: "alpine", Command: []string{"sh"}, ReadOnly: map[string]string{"/s": "/var/run/docker.sock"}},
		"ssh":          {Dir: dir, Image: "alpine", Command: []string{"sh"}, ReadOnly: map[string]string{"/k": filepath.Join(home, ".ssh")}},
		"home cache":   {Dir: dir, Image: "alpine", Command: []string{"sh"}, Cache: home},
	} {
		if err := validate(spec); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func dockerOrSkip(t *testing.T) Docker {
	t.Helper()

	d := Docker{}

	if err := d.Available(context.Background()); err != nil {
		t.Skip(err)
	}

	return d
}

func TestDockerRunsInIsolation(t *testing.T) {
	d := dockerOrSkip(t)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	os.WriteFile(filepath.Join(dir, "in.txt"), []byte("hello"), 0o644)
	ctx := context.Background()

	r := d.Run(ctx, Spec{Dir: dir, Image: "alpine:3.20", Command: []string{"sh", "-c", "cat in.txt; echo out > out.txt; exit 3"}})
	if !r.Ran() || r.ExitCode != 3 || !strings.Contains(r.Output, "hello") || !r.Sandboxed {
		t.Fatalf("%+v", r)
	}

	if data, _ := os.ReadFile(filepath.Join(dir, "out.txt")); string(data) != "out\n" {
		t.Fatal("the worktree is mounted read-write")
	}

	if r := d.Run(ctx, Spec{Dir: dir, Image: "alpine:3.20", Command: []string{"sh", "-c", "wget -q -T 3 -O- http://1.1.1.1 && echo REACHED"}}); strings.Contains(r.Output, "REACHED") || r.ExitCode == 0 {
		t.Fatalf("network must be off: %+v", r)
	}

	if r := d.Run(ctx, Spec{Dir: dir, Image: "alpine:3.20", Command: []string{"sh", "-c", "touch /etc/x"}}); r.ExitCode == 0 {
		t.Fatal("the root filesystem must be read-only")
	}

	if r := d.Run(ctx, Spec{Dir: dir, Image: "alpine:3.20", Command: []string{"id", "-u"}}); strings.TrimSpace(r.Output) == "0" {
		t.Fatal("the container must not run as root")
	}

	start := time.Now()

	if r := d.Run(ctx, Spec{Dir: dir, Image: "alpine:3.20", Command: []string{"sleep", "60"}, Timeout: 3 * time.Second}); !r.TimedOut || time.Since(start) > 30*time.Second {
		t.Fatalf("timeout: %+v after %s", r, time.Since(start))
	}

	if r := d.Run(ctx, Spec{Dir: dir, Image: "csm-no-such-image:never", Command: []string{"true"}}); r.Ran() {
		t.Fatalf("a missing image is a sandbox failure, not a command result: %+v", r)
	}
}

func TestHostRunnerIsMarkedUnsandboxed(t *testing.T) {
	r := Host{}.Run(context.Background(), Spec{Dir: t.TempDir(), Command: []string{"sh", "-c", "exit 2"}})

	if r.Sandboxed || r.ExitCode != 2 || r.Runner != "host" {
		t.Fatalf("%+v", r)
	}
}
