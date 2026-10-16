package skills

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Candidate is a skill read from a source, validated and inspected, not yet installed.
type Candidate struct {
	ID         string     `json:"id"`
	Dir        string     `json:"dir,omitempty"`
	Source     Source     `json:"source"`
	Revision   string     `json:"revision,omitempty"`
	Skill      *Skill     `json:"skill,omitempty"`
	Problems   []Problem  `json:"problems,omitempty"`
	Inspection Inspection `json:"inspection"`
	// RepoLicense is the license of the repository the skill came from, when the skill states none of its own.
	RepoLicense string                 `json:"repo_license,omitempty"`
	Err         string                 `json:"error,omitempty"`
	Contents    map[string][]byte      `json:"-"`
	Modes       map[string]fs.FileMode `json:"-"`
}

// Listing names the skill folders a source holds, before any of them is downloaded.
type Listing struct {
	Source   Source   `json:"source"`
	Revision string   `json:"revision,omitempty"`
	Dirs     []string `json:"dirs"`
	// Names holds each folder's skill name when it could be read cheaply.
	Names map[string]string `json:"names,omitempty"`
}

// Fetcher reads candidates from every kind of source csm supports.
type Fetcher struct {
	GitHub  *GitHub
	Catalog []CatalogEntry
	// AgentDirs are folders agents load user skills from, so a skill found there is labelled with its agent.
	AgentDirs map[string][]string
	Git       string
}

var (
	scpLike    = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^/].*$`)
	shortRepo  = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+(/[^@\s]*)?(@[^\s]+)?$`)
	commitHash = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Kind tells which adapter handles a source string: "local", "github", "git" or "catalog".
func Kind(src string) string {
	src = strings.TrimSpace(src)

	switch {
	case strings.HasPrefix(src, "/") || strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") || strings.HasPrefix(src, "~/") || src == "." || src == "~":
		return "local"
	case strings.HasPrefix(src, "https://github.com/") || strings.HasPrefix(src, "http://github.com/") || strings.HasPrefix(src, "github.com/"):
		if strings.Contains(src, ".git//") {
			return "git"
		}

		return "github"
	case scpLike.MatchString(src) || strings.HasPrefix(src, "ssh://") || strings.HasPrefix(src, "git://") || strings.HasPrefix(src, "file://") ||
		(strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "http://")) && strings.Contains(src, ".git"):
		return "git"
	case strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "http://"):
		return "unsupported"
	}

	if _, err := os.Stat(src); err == nil {
		return "local"
	}

	if shortRepo.MatchString(src) {
		return "github"
	}

	return "catalog"
}

// List finds the skills a source holds. For GitHub it downloads only each SKILL.md.
func (f *Fetcher) List(ctx context.Context, src string) (Listing, error) {
	switch Kind(src) {
	case "local":
		dir, err := expand(src)
		if err != nil {
			return Listing{}, err
		}

		dirs, err := localSkillDirs(dir)
		if err != nil {
			return Listing{}, err
		}

		l := Listing{Source: f.localSource(dir), Dirs: dirs, Names: map[string]string{}}

		for _, d := range dirs {
			if data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(d), ManifestFile)); err == nil {
				if s, _ := parseManifest(data); s.Name != "" {
					l.Names[d] = s.Name
				}
			}
		}

		return l, nil
	case "github":
		ref, err := ParseRef(src)
		if err != nil {
			return Listing{}, err
		}

		t, err := f.GitHub.Resolve(ctx, ref)
		if err != nil {
			return Listing{}, err
		}

		l := Listing{Source: githubSource(ref, ""), Revision: t.Commit, Dirs: t.SkillDirs(), Names: map[string]string{}}

		if len(l.Dirs) == 0 {
			return l, fmt.Errorf("%s has no SKILL.md at or below %q", ref.Repository(), ref.Path)
		}

		if len(l.Dirs) <= 50 {
			for _, d := range l.Dirs {
				if data, err := f.GitHub.get(ctx, fmt.Sprintf("%s/%s/%s/%s/%s", f.GitHub.Raw, url.PathEscape(ref.Owner), url.PathEscape(ref.Repo), t.Commit, escapePath(path.Join(d, ManifestFile))), ""); err == nil {
					if s, _ := parseManifest(data); s.Name != "" {
						l.Names[d] = s.Name
					}
				}
			}
		}

		return l, nil
	case "git":
		checkout, cleanup, err := f.gitCheckout(ctx, src)
		if err != nil {
			return Listing{}, err
		}

		defer cleanup()

		dirs, err := localSkillDirs(checkout.dir)
		if err != nil {
			return Listing{}, err
		}

		l := Listing{Source: checkout.source, Revision: checkout.commit, Names: map[string]string{}}

		for _, d := range dirs {
			full := path.Join(checkout.source.Path, d)

			if d == "" {
				full = checkout.source.Path
			}

			l.Dirs = append(l.Dirs, full)

			if data, err := os.ReadFile(filepath.Join(checkout.dir, filepath.FromSlash(d), ManifestFile)); err == nil {
				if s, _ := parseManifest(data); s.Name != "" {
					l.Names[full] = s.Name
				}
			}
		}

		return l, nil
	case "catalog":
		e, err := f.catalogEntry(src)
		if err != nil {
			return Listing{}, err
		}

		return f.List(ctx, "github.com/"+e.Source.Repo+"/"+e.Source.Path)
	}

	return Listing{}, fmt.Errorf("%q is not a source csm can read: use a local folder, a GitHub repository (owner/repo/path), a git URL ending in .git, or a catalog name", src)
}

// Fetch downloads and checks the skill folders a source holds; dirs limits it to some of them.
func (f *Fetcher) Fetch(ctx context.Context, src string, dirs []string) ([]Candidate, error) {
	switch Kind(src) {
	case "local":
		dir, err := expand(src)
		if err != nil {
			return nil, err
		}

		found, err := localSkillDirs(dir)
		if err != nil {
			return nil, err
		}

		var out []Candidate

		for _, d := range pick(found, dirs) {
			full := filepath.Join(dir, filepath.FromSlash(d))
			c := Candidate{Source: f.localSource(full), Dir: d}
			c.ID = localID(c.Source, full)
			f.fill(&c, func() (map[string][]byte, map[string]fs.FileMode, error) { return readLocal(full) }, filepath.Base(full))
			out = append(out, c)
		}

		return out, nil
	case "github":
		ref, err := ParseRef(src)
		if err != nil {
			return nil, err
		}

		t, err := f.GitHub.Resolve(ctx, ref)
		if err != nil {
			return nil, err
		}

		found := t.SkillDirs()

		if len(found) == 0 {
			return nil, fmt.Errorf("%s has no SKILL.md at or below %q", ref.Repository(), ref.Path)
		}

		var out []Candidate
		repoLicense := ""

		for _, d := range pick(found, dirs) {
			c := Candidate{ID: SkillID(ref.Repository(), d), Dir: d, Source: githubSource(ref, d), Revision: t.Commit}
			f.fill(&c, func() (map[string][]byte, map[string]fs.FileMode, error) { return f.GitHub.Fetch(ctx, t, d) }, path.Base(d))

			if c.Skill != nil && c.Skill.License == "" {
				if repoLicense == "" {
					repoLicense = f.GitHub.RepoLicense(ctx, ref)
				}

				c.RepoLicense = repoLicense
			}

			out = append(out, c)
		}

		return out, nil
	case "git":
		checkout, cleanup, err := f.gitCheckout(ctx, src)
		if err != nil {
			return nil, err
		}

		defer cleanup()

		found, err := localSkillDirs(checkout.dir)
		if err != nil {
			return nil, err
		}

		var out []Candidate

		for _, d := range found {
			full := path.Join(checkout.source.Path, d)

			if d == "" {
				full = checkout.source.Path
			}

			if len(dirs) > 0 && !contains(dirs, full) {
				continue
			}

			src := checkout.source
			src.Path = full
			local := filepath.Join(checkout.dir, filepath.FromSlash(d))
			c := Candidate{ID: gitID(src.URL, full), Dir: full, Source: src, Revision: checkout.commit}
			f.fill(&c, func() (map[string][]byte, map[string]fs.FileMode, error) { return readLocal(local) }, path.Base(full))
			out = append(out, c)
		}

		return out, nil
	case "catalog":
		e, err := f.catalogEntry(src)
		if err != nil {
			return nil, err
		}

		return f.Fetch(ctx, "github.com/"+e.Source.Repo+"/"+e.Source.Path, nil)
	}

	return nil, fmt.Errorf("%q is not a source csm can read: use a local folder, a GitHub repository (owner/repo/path), a git URL ending in .git, or a catalog name", src)
}

func pick(found, want []string) []string {
	if len(want) == 0 {
		return found
	}

	var out []string

	for _, d := range found {
		if contains(want, d) {
			out = append(out, d)
		}
	}

	return out
}

func (f *Fetcher) fill(c *Candidate, read func() (map[string][]byte, map[string]fs.FileMode, error), dirName string) {
	contents, modes, err := read()
	if err != nil {
		c.Err = err.Error()

		return
	}

	s, problems, err := FromFiles(dirName, contents, modes)
	c.Skill, c.Problems, c.Contents, c.Modes = s, problems, contents, modes

	if err != nil {
		c.Err = err.Error()
	}

	if s != nil {
		c.Inspection = Inspect(s, contents)
	}
}

func (f *Fetcher) catalogEntry(name string) (CatalogEntry, error) {
	var matches []CatalogEntry

	for _, e := range f.Catalog {
		if e.Name == name || e.ID == name {
			matches = append(matches, e)
		}
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if len(f.Catalog) == 0 {
			return CatalogEntry{}, fmt.Errorf("%q is not a folder, a repository or a skill in the catalog (the catalog has not been fetched; run: csm skills search --refresh)", name)
		}

		return CatalogEntry{}, fmt.Errorf("%q is not a folder, a repository or a skill in the catalog: %w", name, ErrNotFound)
	}

	var ids []string

	for _, e := range matches {
		ids = append(ids, e.ID)
	}

	return CatalogEntry{}, fmt.Errorf("several catalog skills are named %q; use one of: %s", name, strings.Join(ids, ", "))
}

func githubSource(ref Ref, dir string) Source {
	return Source{Type: "github", URL: "https://github.com/" + ref.Repository(), Repo: ref.Repository(), Path: dir, Ref: ref.Ref}
}

// localSource labels a folder an agent already loads skills from with that agent.
func (f *Fetcher) localSource(dir string) Source {
	for provider, roots := range f.AgentDirs {
		for _, root := range roots {
			if rel, err := filepath.Rel(root, dir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				return Source{Type: "agent", Path: dir, Provider: provider}
			}
		}
	}

	return Source{Type: "local", Path: dir}
}

func localID(src Source, dir string) string {
	if src.Type == "agent" {
		return "agent:" + src.Provider + ":" + dir
	}

	return "local:" + dir
}

func gitID(rawURL, dir string) string {
	id := "git:" + rawURL

	if dir != "" {
		id += "//" + dir
	}

	return id
}

func expand(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}

		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}

	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a folder; point at the folder holding SKILL.md", p)
	}

	return abs, nil
}

func readLocal(dir string) (map[string][]byte, map[string]fs.FileMode, error) {
	s, _, err := Load(dir)

	if s == nil && err != nil {
		var ve *ValidationError

		if !errors.As(err, &ve) {
			return nil, nil, err
		}
	}

	return ReadTree(dir, nil)
}

// localSkillDirs finds folders holding a SKILL.md at or up to three levels below dir, without following symlinks.
func localSkillDirs(dir string) ([]string, error) {
	var dirs []string

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") && d.Name() != ".claude" && d.Name() != ".agents" && d.Name() != ".github" && d.Name() != ".gemini" && d.Name() != ".cursor" && d.Name() != ".codex" || d.Name() == "node_modules") {
				return fs.SkipDir
			}

			if rel != "." && strings.Count(rel, "/") >= 4 {
				return fs.SkipDir
			}

			return nil
		}

		if d.Name() == ManifestFile && d.Type().IsRegular() {
			skillDir := path.Dir(rel)

			if skillDir == "." {
				skillDir = ""
			}

			dirs = append(dirs, skillDir)

			if skillDir == "" {
				return fs.SkipAll
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(dirs) == 0 {
		return nil, fmt.Errorf("no SKILL.md in %s or the folders below it", dir)
	}

	if contains(dirs, "") {
		return []string{""}, nil
	}

	sort.Strings(dirs)

	return dirs, nil
}

type checkout struct {
	dir    string
	commit string
	source Source
}

var gitSourcePattern = regexp.MustCompile(`^(.+?\.git)(?://([^@]*))?(?:@([^@/]+))?$`)

// ParseGitSource splits url.git//path/in/repo@ref, removing any user name or password from the URL kept on record.
func ParseGitSource(src string) (Source, error) {
	m := gitSourcePattern.FindStringSubmatch(strings.TrimSpace(src))
	if m == nil {
		return Source{}, fmt.Errorf("%q is not a git URL; use https://host/owner/repo.git, optionally followed by //path/in/repo and @ref", src)
	}

	s := Source{Type: "git", URL: RedactURL(m[1]), Path: strings.Trim(m[2], "/"), Ref: m[3]}

	if s.Path != "" {
		if err := SafeRelPath(s.Path); err != nil {
			return s, err
		}
	}

	return s, nil
}

// RedactURL removes credentials from a URL so it can be stored or shown.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}

	u.User = nil

	return u.String()
}

func (f *Fetcher) gitCheckout(ctx context.Context, src string) (checkout, func(), error) {
	s, err := ParseGitSource(src)
	if err != nil {
		return checkout{}, func() {}, err
	}

	fetchURL := strings.TrimSpace(gitSourcePattern.FindStringSubmatch(strings.TrimSpace(src))[1])
	gitPath := f.Git

	if gitPath == "" {
		if gitPath, err = exec.LookPath("git"); err != nil {
			return checkout{}, func() {}, errors.New("git is not installed, so csm cannot read git URLs")
		}
	}

	dir, err := os.MkdirTemp("", "csm-skill-git-")
	if err != nil {
		return checkout{}, func() {}, err
	}

	cleanup := func() { os.RemoveAll(dir) }
	ref := s.Ref

	if ref == "" {
		ref = "HEAD"
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, gitPath, append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "protocol.ext.allow=never", "-c", "core.symlinks=true"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
		out, err := cmd.CombinedOutput()

		if err != nil {
			msg := strings.TrimSpace(string(out))

			if fetchURL != s.URL {
				msg = strings.ReplaceAll(msg, fetchURL, s.URL)
			}

			return "", fmt.Errorf("git %s: %s", args[0], lastLine(msg))
		}

		return strings.TrimSpace(string(out)), nil
	}

	steps := [][]string{{"init", "-q"}, {"remote", "add", "origin", fetchURL}}

	if s.Path != "" {
		steps = append(steps, []string{"sparse-checkout", "set", "--no-cone", "/" + s.Path + "/"})
	}

	steps = append(steps, []string{"fetch", "-q", "--depth", "1", "--filter=blob:none", "origin", ref}, []string{"checkout", "-q", "FETCH_HEAD"})

	for _, args := range steps {
		if _, err := run(args...); err != nil {
			cleanup()

			return checkout{}, func() {}, fmt.Errorf("%s: %w", s.URL, err)
		}
	}

	commit, err := run("rev-parse", "HEAD")
	if err != nil || !commitHash.MatchString(commit) {
		cleanup()

		return checkout{}, func() {}, fmt.Errorf("%s: could not read the commit: %v", s.URL, err)
	}

	root := filepath.Join(dir, filepath.FromSlash(s.Path))

	if info, err := os.Lstat(root); err != nil || !info.IsDir() {
		cleanup()

		return checkout{}, func() {}, fmt.Errorf("%s has no folder %q at %s", s.URL, s.Path, short(commit))
	}

	return checkout{dir: root, commit: commit, source: s}, cleanup, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")

	return lines[len(lines)-1]
}
