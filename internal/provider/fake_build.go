package provider

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var fakeStepName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// fakeBuild plays a Build Room step from a fixture directory: it copies the scripted files into the working tree and writes the scripted report. It is a test stand-in and never writes code of its own.
func fakeBuild(term <-chan os.Signal) (int, bool) {
	step, report, fixtures := os.Getenv("CSM_BUILD_STEP"), os.Getenv("CSM_TASK_REPORT"), os.Getenv("CSM_FAKE_WORK")

	if step == "" || report == "" {
		return 0, false
	}

	dir := filepath.Join(fixtures, step)

	if fixtures == "" || !fakeStepName.MatchString(step) {
		dir = ""
	}

	if fi, err := os.Stat(dir); dir == "" || err != nil || !fi.IsDir() {
		fakeEchoLine("No scripted fixture for " + step + "; this test agent does nothing.")

		return fakeWait(term, fakeHold()), true
	}

	if ms, err := strconv.Atoi(os.Getenv("CSM_FAKE_WORK_MS")); err == nil && ms > 0 {
		select {
		case <-term:
			return 143, true
		case <-time.After(time.Duration(ms) * time.Millisecond):
		}
	}

	fakeEchoLine("Test agent (no model): applying the scripted fixture for " + step)

	fakeCopy(filepath.Join(dir, "files"))

	if data, err := os.ReadFile(filepath.Join(dir, "report.json")); err == nil {
		os.MkdirAll(filepath.Dir(report), 0o700)

		if os.WriteFile(report, data, 0o600) == nil {
			fakeEchoLine("Wrote the scripted completion report.")
		}
	}

	return fakeWait(term, 10*time.Minute), true
}

func fakeWait(term <-chan os.Signal, d time.Duration) int {
	select {
	case <-term:
		return 143
	case <-time.After(d):
		return 0
	}
}

// fakePartial writes a step's "partial" files, standing in for work an agent did before it hit a limit.
func fakePartial() {
	step, fixtures := os.Getenv("CSM_BUILD_STEP"), os.Getenv("CSM_FAKE_WORK")

	if step == "" || fixtures == "" || !fakeStepName.MatchString(step) {
		return
	}

	fakeCopy(filepath.Join(fixtures, step, "partial"))
}

func fakeCopy(files string) {
	cwd, _ := os.Getwd()

	filepath.WalkDir(files, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(files, path)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		dest := filepath.Join(cwd, rel)
		os.MkdirAll(filepath.Dir(dest), 0o755)

		if os.WriteFile(dest, data, 0o644) == nil && os.Getenv("CSM_FAKE_ECHO") != "" {
			fmt.Print("  wrote " + rel + "\r\n")
		}

		return nil
	})
}
