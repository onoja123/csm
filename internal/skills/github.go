package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// GitHub reads skills from public (or, with a token, private) GitHub repositories: it resolves a ref to a commit and
// downloads only the files of the skill folder asked for.
type GitHub struct {
	API    string
	Raw    string
	Client *http.Client
	// Token comes from CSM_GITHUB_TOKEN, GITHUB_TOKEN or GH_TOKEN; it is sent to GitHub only and never printed.
	Token string
}

func NewGitHub() *GitHub {
	token := ""

	for _, name := range []string{"CSM_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"} {
		if token = os.Getenv(name); token != "" {
			break
		}
	}

	return &GitHub{API: "https://api.github.com", Raw: "https://raw.githubusercontent.com", Client: &http.Client{Timeout: 30 * time.Second}, Token: token}
}

// Ref names a folder in a repository: owner/repo, an optional path and an optional ref.
type Ref struct {
	Owner, Repo, Path, Ref string
}

func (r Ref) Repository() string { return r.Owner + "/" + r.Repo }

func (r Ref) String() string {
	s := "github.com/" + r.Repository()

	if r.Path != "" {
		s += "/" + r.Path
	}

	if r.Ref != "" {
		s += "@" + r.Ref
	}

	return s
}

// ParseRef accepts github.com/owner/repo/path@ref, owner/repo/path@ref and https://github.com/owner/repo/tree/ref/path.
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://"), "/")
	s = strings.TrimPrefix(s, "github.com/")

	var r Ref

	if at := strings.LastIndex(s, "@"); at > 0 {
		s, r.Ref = s[:at], s[at+1:]
	}

	parts := strings.Split(s, "/")

	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return r, fmt.Errorf("%q is not a GitHub source; use owner/repo or owner/repo/path/to/skill", s)
	}

	r.Owner, r.Repo = parts[0], strings.TrimSuffix(parts[1], ".git")
	rest := parts[2:]

	if len(rest) >= 2 && (rest[0] == "tree" || rest[0] == "blob") {
		if r.Ref == "" {
			r.Ref = rest[1]
		}

		rest = rest[2:]
	}

	r.Path = strings.TrimSuffix(strings.Join(rest, "/"), "/"+ManifestFile)

	if r.Path == ManifestFile {
		r.Path = ""
	}

	for _, p := range []string{r.Owner, r.Repo} {
		if strings.ContainsAny(p, " \\?#") {
			return r, fmt.Errorf("%q is not a GitHub source", s)
		}
	}

	if r.Path != "" {
		if err := SafeRelPath(r.Path); err != nil {
			return r, err
		}
	}

	return r, nil
}

type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// Tree is a repository at one commit.
type Tree struct {
	Ref     Ref
	Commit  string
	Entries []treeEntry
}

var ErrRateLimited = errors.New("GitHub rate limit reached")

func (g *GitHub) get(ctx context.Context, rawURL, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}

	if accept != "" {
		req.Header.Set("Accept", accept)
	}

	req.Header.Set("User-Agent", "csm-skills")

	if g.Token != "" && (strings.HasPrefix(rawURL, g.API) || strings.HasPrefix(rawURL, g.Raw)) {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}

	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach GitHub: %w", redactURLError(err))
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTotalBytes+1))
	if err != nil {
		return nil, err
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		return body, nil
	case (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) && resp.Header.Get("X-RateLimit-Remaining") == "0":
		reset := ""

		if sec, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			reset = " until " + time.Unix(sec, 0).Format("15:04")
		}

		hint := "; set CSM_GITHUB_TOKEN to raise the limit"

		if g.Token != "" {
			hint = ""
		}

		return nil, fmt.Errorf("%w%s%s", ErrRateLimited, reset, hint)
	case resp.StatusCode == http.StatusNotFound:
		hint := " (or it is private: set CSM_GITHUB_TOKEN)"

		if g.Token != "" {
			hint = ""
		}

		return nil, fmt.Errorf("not found on GitHub%s: %w", hint, ErrNotFound)
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errors.New("GitHub refused the token csm sent (CSM_GITHUB_TOKEN, GITHUB_TOKEN or GH_TOKEN)")
	}

	return nil, fmt.Errorf("GitHub answered %s", resp.Status)
}

// redactURLError drops the URL from a transport error so query strings never reach a log.
func redactURLError(err error) error {
	var ue *url.Error

	if errors.As(err, &ue) {
		return ue.Err
	}

	return err
}

// Resolve pins a ref (a branch, tag or commit; the default branch when empty) to a commit and lists that tree.
func (g *GitHub) Resolve(ctx context.Context, r Ref) (*Tree, error) {
	ref := r.Ref

	if ref == "" {
		ref = "HEAD"
	}

	sha, err := g.get(ctx, fmt.Sprintf("%s/repos/%s/%s/commits/%s", g.API, url.PathEscape(r.Owner), url.PathEscape(r.Repo), url.PathEscape(ref)), "application/vnd.github.sha")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r.String(), err)
	}

	commit := strings.TrimSpace(string(sha))

	if len(commit) < 40 {
		return nil, fmt.Errorf("%s: GitHub did not return a commit for %q", r.String(), ref)
	}

	data, err := g.get(ctx, fmt.Sprintf("%s/repos/%s/%s/git/trees/%s?recursive=1", g.API, url.PathEscape(r.Owner), url.PathEscape(r.Repo), commit), "application/vnd.github+json")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r.String(), err)
	}

	var t struct {
		Tree      []treeEntry `json:"tree"`
		Truncated bool        `json:"truncated"`
	}

	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("%s: unexpected answer from GitHub: %w", r.String(), err)
	}

	if t.Truncated {
		return nil, fmt.Errorf("%s: the repository is too large to list in one request; point at the skill folder's own repository", r.String())
	}

	return &Tree{Ref: r, Commit: commit, Entries: t.Tree}, nil
}

// RepoLicense is the license GitHub detected for the whole repository, or "" when it found none.
func (g *GitHub) RepoLicense(ctx context.Context, r Ref) string {
	data, err := g.get(ctx, fmt.Sprintf("%s/repos/%s/%s", g.API, url.PathEscape(r.Owner), url.PathEscape(r.Repo)), "application/vnd.github+json")
	if err != nil {
		return ""
	}

	var v struct {
		License *struct {
			SPDX string `json:"spdx_id"`
		} `json:"license"`
	}

	if json.Unmarshal(data, &v) != nil || v.License == nil || v.License.SPDX == "" || v.License.SPDX == "NOASSERTION" {
		return ""
	}

	return v.License.SPDX
}

// SkillDirs lists the folders holding a SKILL.md at or under the tree's path, at most three levels down.
func (t *Tree) SkillDirs() []string {
	var dirs []string

	for _, e := range t.Entries {
		if e.Type != "blob" || path.Base(e.Path) != ManifestFile {
			continue
		}

		dir := path.Dir(e.Path)

		if dir == "." {
			dir = ""
		}

		if !within(dir, t.Ref.Path) || depth(dir)-depth(t.Ref.Path) > 3 {
			continue
		}

		dirs = append(dirs, dir)
	}

	return dirs
}

func within(dir, root string) bool {
	return root == "" || dir == root || strings.HasPrefix(dir, root+"/")
}

func depth(p string) int {
	if p == "" {
		return 0
	}

	return strings.Count(p, "/") + 1
}

// Fetch downloads every file of the skill folder dir at the tree's commit.
func (g *GitHub) Fetch(ctx context.Context, t *Tree, dir string) (map[string][]byte, map[string]fs.FileMode, error) {
	contents, modes := map[string][]byte{}, map[string]fs.FileMode{}
	total := int64(0)

	for _, e := range t.Entries {
		if e.Type != "blob" || !within(e.Path, dir) || e.Path == dir {
			continue
		}

		rel := strings.TrimPrefix(strings.TrimPrefix(e.Path, dir), "/")

		if err := SafeRelPath(rel); err != nil {
			return nil, nil, err
		}

		if e.Mode == "120000" {
			return nil, nil, fmt.Errorf("%s is a symlink; csm does not install skills containing symlinks", rel)
		}

		if e.Mode == "160000" {
			continue
		}

		if total += e.Size; total > maxTotalBytes {
			return nil, nil, fmt.Errorf("the skill is larger than %d MB", maxTotalBytes>>20)
		}

		if len(contents) >= maxFiles {
			return nil, nil, fmt.Errorf("the skill has more than %d files", maxFiles)
		}

		data, err := g.get(ctx, fmt.Sprintf("%s/%s/%s/%s/%s", g.Raw, url.PathEscape(t.Ref.Owner), url.PathEscape(t.Ref.Repo), t.Commit, escapePath(e.Path)), "")
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", e.Path, err)
		}

		contents[rel] = data

		if e.Mode == "100755" {
			modes[rel] = 0o755
		} else {
			modes[rel] = 0o644
		}
	}

	if len(contents) == 0 {
		return nil, nil, fmt.Errorf("%s has no files at %s", t.Ref.Repository(), dir)
	}

	return contents, modes, nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")

	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}

	return strings.Join(parts, "/")
}
