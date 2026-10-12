package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const maxDiffBytes = 400 << 10

type gitError struct {
	args []string
	out  string
	err  error
}

func (e *gitError) Error() string {
	msg := strings.TrimSpace(e.out)

	if msg == "" {
		msg = e.err.Error()
	}

	return fmt.Sprintf("git %s: %s", strings.Join(e.args, " "), firstLine(msg))
}

// gitIn runs git with an argument list, never through a shell, so branch names and paths cannot inject commands.
func gitIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true")
	out, err := cmd.CombinedOutput()

	if err != nil {
		return string(out), &gitError{args: args, out: string(out), err: err}
	}

	return strings.TrimRight(string(out), "\n"), nil
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()

	if err != nil {
		return "", &gitError{args: args, err: err}
	}

	return strings.TrimRight(string(out), "\n"), nil
}

func gitList(dir string, args ...string) []string {
	out, err := gitOut(dir, args...)
	if err != nil || out == "" {
		return nil
	}

	return strings.Split(out, "\n")
}

type repoState struct {
	Root   string
	Branch string
	Head   string
	Dirty  []string
}

func inspectRepo(dir string) (repoState, error) {
	var rs repoState

	root, err := gitOut(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return rs, fmt.Errorf("%s is not inside a git repository", displayPath(dir))
	}

	rs.Root = root
	rs.Branch, _ = gitOut(root, "branch", "--show-current")

	if rs.Head, err = gitOut(root, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		return rs, errors.New("the repository has no commits yet; Build Room needs a base commit")
	}

	rs.Dirty = gitList(root, "status", "--porcelain")

	return rs, nil
}

func validBranchName(dir, name string) bool {
	if name == "" || strings.HasPrefix(name, "-") {
		return false
	}

	_, err := gitOut(dir, "check-ref-format", "--branch", name)

	return err == nil
}

func resolveCommit(root, ref string) (string, error) {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("invalid base %q", ref)
	}

	sha, err := gitOut(root, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("base branch %q does not resolve to a commit", ref)
	}

	return sha, nil
}

func branchExists(root, branch string) bool {
	_, err := gitOut(root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)

	return err == nil
}

func registeredWorktrees(root string) map[string]string {
	out := map[string]string{}
	path := ""

	for _, line := range gitList(root, "worktree", "list", "--porcelain") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
			out[path] = ""
		case strings.HasPrefix(line, "branch refs/heads/") && path != "":
			out[path] = strings.TrimPrefix(line, "branch refs/heads/")
		}
	}

	return out
}

func outside(root, path string) bool {
	rel, err := filepath.Rel(root, path)

	return err == nil && (rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// addWorktree creates a CSM-owned worktree on a new branch at base. It reuses one CSM created earlier for the same branch, and refuses anything that could overwrite user work.
func addWorktree(root, path, branch, base string) error {
	if !filepath.IsAbs(path) || !outside(root, path) {
		return fmt.Errorf("refusing to create a worktree at %s: it must be outside the repository", displayPath(path))
	}

	if !validBranchName(root, branch) {
		return fmt.Errorf("invalid branch name %q", branch)
	}

	resolved := path

	if p, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		resolved = filepath.Join(p, filepath.Base(path))
	}

	trees := registeredWorktrees(root)

	if b, ok := trees[resolved]; ok {
		if b == branch {
			return nil
		}

		return fmt.Errorf("%s is already a worktree for %q", displayPath(path), b)
	}

	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists and is not a worktree of this repository", displayPath(path))
	}

	for p, b := range trees {
		if b == branch {
			return fmt.Errorf("branch %s is already checked out in %s", branch, displayPath(p))
		}
	}

	if branchExists(root, branch) {
		return fmt.Errorf("branch %s already exists; CSM will not reuse a branch it did not create for this run", branch)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	_, err := gitIn(root, "worktree", "add", "-b", branch, path, base)

	return err
}

func addDetachedWorktree(root, path, commit string) error {
	if !filepath.IsAbs(path) || !outside(root, path) {
		return fmt.Errorf("refusing to create a worktree at %s: it must be outside the repository", displayPath(path))
	}

	if _, err := os.Stat(path); err == nil {
		if head, err := gitOut(path, "rev-parse", "HEAD"); err == nil && head == commit {
			return nil
		}

		return fmt.Errorf("%s already exists", displayPath(path))
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	_, err := gitIn(root, "worktree", "add", "--detach", path, commit)

	return err
}

func identityArgs(dir string) []string {
	if name, _ := gitOut(dir, "config", "user.name"); name != "" {
		if email, _ := gitOut(dir, "config", "user.email"); email != "" {
			return nil
		}
	}

	return []string{"-c", "user.name=CSM Build Room", "-c", "user.email=build-room@csm.invalid"}
}

// commitAll records everything an agent left in its worktree as one commit on the task branch, so the work is never lost and can be merged.
func commitAll(dir, message string) (string, bool, error) {
	if _, err := gitIn(dir, "add", "-A"); err != nil {
		return "", false, err
	}

	if _, err := gitOut(dir, "diff", "--cached", "--quiet"); err == nil {
		head, _ := gitOut(dir, "rev-parse", "HEAD")

		return head, false, nil
	}

	args := append(identityArgs(dir), "commit", "--no-verify", "-q", "-m", message)

	if _, err := gitIn(dir, args...); err != nil {
		return "", false, err
	}

	head, err := gitOut(dir, "rev-parse", "HEAD")

	return head, true, err
}

func changedSince(dir, base string) []string {
	return gitList(dir, "diff", "--name-only", base+"..HEAD")
}

func diffStatSince(dir, base string) string {
	out, _ := gitOut(dir, "diff", "--stat", base+"..HEAD")

	return out
}

func diffSince(dir, base string) (string, bool) {
	out, _ := gitOut(dir, "diff", "--no-color", "--no-ext-diff", base+"..HEAD")

	if len(out) > maxDiffBytes {
		return out[:maxDiffBytes], true
	}

	return out, false
}

// mergeInto merges a branch into the worktree's branch; on a conflict it leaves the merge in progress and returns the conflicted files.
func mergeInto(dir, branch, message string) ([]string, error) {
	args := append(identityArgs(dir), "merge", "--no-ff", "--no-verify", "-m", message, "--end-of-options", branch)

	if _, err := gitIn(dir, args...); err != nil {
		if files := unmergedFiles(dir); len(files) > 0 {
			return files, nil
		}

		return nil, err
	}

	return nil, nil
}

func unmergedFiles(dir string) []string {
	return gitList(dir, "diff", "--name-only", "--diff-filter=U")
}

func mergeInProgress(dir string) bool {
	_, err := gitOut(dir, "rev-parse", "-q", "--verify", "MERGE_HEAD")

	return err == nil
}

func conflictMarkers(dir string, files []string) []string {
	var marked []string

	for _, f := range files {
		fh, err := os.Open(filepath.Join(dir, f))
		if err != nil {
			continue
		}

		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<16), 1<<22)

		for sc.Scan() {
			line := sc.Text()

			if strings.HasPrefix(line, "<<<<<<< ") || strings.HasPrefix(line, ">>>>>>> ") {
				marked = append(marked, f)

				break
			}
		}

		fh.Close()
	}

	return marked
}

func isAncestor(dir, a, b string) bool {
	_, err := gitOut(dir, "merge-base", "--is-ancestor", a, b)

	return err == nil
}

func worktreeChanges(dir string) []string {
	return gitList(dir, "status", "--porcelain")
}
