package handoff

import (
	"bytes"
	"os/exec"
	"strings"
)

type Git struct {
	Root            string   `json:"root,omitempty"`
	WorkingDir      string   `json:"working_dir"`
	Branch          string   `json:"branch,omitempty"`
	Worktree        bool     `json:"worktree,omitempty"`
	Staged          int      `json:"staged"`
	Unstaged        int      `json:"unstaged"`
	Untracked       int      `json:"untracked"`
	Status          []string `json:"status,omitempty"`
	StatusTruncated bool     `json:"status_truncated,omitempty"`
	StatusFull      []string `json:"-"`
	DiffStat        string   `json:"diff_stat,omitempty"`
	DiffFile        string   `json:"diff_file,omitempty"`
	DiffTruncated   bool     `json:"diff_truncated,omitempty"`
	RecentCommits   []string `json:"recent_commits,omitempty"`
}

func git(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()

	return strings.TrimSpace(string(out)), err
}

// gitLines keeps leading spaces, which `git status --short` uses to mark the index column.
func gitLines(dir string, args ...string) ([]string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	text := strings.TrimRight(string(out), "\n")

	if err != nil || text == "" {
		return nil, err
	}

	return strings.Split(text, "\n"), nil
}

// CaptureGit records the working tree as it is; the diff is returned separately because it is stored, not rendered.
func CaptureGit(cwd string) (Git, []byte) {
	g := Git{WorkingDir: cwd}
	root, err := git(cwd, "rev-parse", "--show-toplevel")

	if err != nil {
		return g, nil
	}

	g.Root = root
	g.Branch, _ = git(cwd, "branch", "--show-current")

	gitDir, _ := git(cwd, "rev-parse", "--git-dir")
	commonDir, _ := git(cwd, "rev-parse", "--git-common-dir")
	g.Worktree = gitDir != "" && commonDir != "" && gitDir != commonDir

	if lines, err := gitLines(cwd, "status", "--short", "--untracked-files=all"); err == nil && len(lines) > 0 {
		g.StatusFull = lines

		for _, line := range lines {
			if len(line) < 2 {
				continue
			}

			switch {
			case strings.HasPrefix(line, "??"):
				g.Untracked++
			default:
				if line[0] != ' ' {
					g.Staged++
				}

				if line[1] != ' ' {
					g.Unstaged++
				}
			}
		}

		if len(lines) > maxStatusLines {
			lines, g.StatusTruncated = lines[:maxStatusLines], true
		}

		g.Status = redactAll(lines)
	}

	if stat, err := git(cwd, "diff", "--stat", "HEAD"); err == nil && stat != "" {
		lines := strings.Split(stat, "\n")

		if len(lines) > maxDiffStatLines {
			lines = append(lines[:maxDiffStatLines], "…")
		}

		g.DiffStat = Redact(strings.Join(lines, "\n"))
	}

	if log, err := git(cwd, "log", "--oneline", "-5"); err == nil && log != "" {
		g.RecentCommits = redactAll(strings.Split(log, "\n"))
	}

	diff, err := exec.Command("git", "-C", cwd, "diff", "HEAD").Output()

	if err != nil || len(bytes.TrimSpace(diff)) == 0 {
		return g, nil
	}

	if len(diff) > MaxDiffBytes {
		diff = append(diff[:MaxDiffBytes], []byte("\n[truncated by csm]\n")...)
		g.DiffTruncated = true
	}

	return g, diff
}

func redactAll(lines []string) []string {
	out := make([]string, len(lines))

	for i, l := range lines {
		out[i] = Redact(l)
	}

	return out
}
