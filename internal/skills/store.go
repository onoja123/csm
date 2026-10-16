package skills

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const libraryVersion = 1

const (
	ScopeGlobal  = "global"
	ScopeProject = "project"
	ScopeTask    = "task"
)

// Library is csm's managed skill store under <csm home>/skills: library.json, one canonical copy per revision in
// store/<key>/<hash>, and a staging area that makes installs all-or-nothing.
type Library struct {
	Dir string
}

func Open(csmHome string) *Library { return &Library{Dir: filepath.Join(csmHome, "skills")} }

type Source struct {
	// Type is "github", "git", "local" or "agent" (a folder an agent already loads skills from).
	Type     string `json:"type"`
	URL      string `json:"url,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Path     string `json:"path,omitempty"`
	Ref      string `json:"ref,omitempty"`
	Provider string `json:"provider,omitempty"`
}

func (s Source) String() string {
	switch {
	case s.Repo != "":
		out := "github.com/" + s.Repo

		if s.Path != "" {
			out += "/" + s.Path
		}

		if s.Ref != "" {
			out += "@" + s.Ref
		}

		return out
	case s.Type == "git":
		out := s.URL

		if s.Path != "" {
			out += "//" + s.Path
		}

		if s.Ref != "" {
			out += "@" + s.Ref
		}

		return out
	case s.Path != "":
		return s.Path
	}

	return s.URL
}

// Revision is one version of a skill csm kept, newest last, so an update can be rolled back.
type Revision struct {
	Hash     string `json:"hash"`
	Revision string `json:"revision,omitempty"`
	// Upstream is the source's content hash this revision came from; it differs from Hash after a local change.
	Upstream string    `json:"upstream,omitempty"`
	At       time.Time `json:"at"`
	Note     string    `json:"note,omitempty"`
}

// Projection is a copy csm wrote where an agent discovers skills; csm owns it only while its marker and content match.
type Projection struct {
	Provider    string    `json:"provider"`
	Account     string    `json:"account,omitempty"`
	Scope       string    `json:"scope"`
	Dir         string    `json:"dir"`
	Hash        string    `json:"hash"`
	Canonical   string    `json:"canonical"`
	Adapter     string    `json:"adapter,omitempty"`
	Adaptations []string  `json:"adaptations,omitempty"`
	At          time.Time `json:"at"`
}

// Upstream is what csm last saw at the skill's source.
type Upstream struct {
	Revision  string    `json:"revision,omitempty"`
	Hash      string    `json:"hash,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	Error     string    `json:"error,omitempty"`
}

type Record struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      Source `json:"source"`
	// Revision is the source's immutable revision (a commit) when it has one.
	Revision string `json:"revision,omitempty"`
	// Hash identifies the installed contents; UpstreamHash the contents fetched from the source, which differ once a local change is adopted.
	Hash         string   `json:"hash"`
	UpstreamHash string   `json:"upstream_hash,omitempty"`
	Pinned       bool     `json:"pinned,omitempty"`
	License      string   `json:"license,omitempty"`
	RepoLicense  string   `json:"repo_license,omitempty"`
	Category     string   `json:"category,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Enabled      bool     `json:"enabled"`
	// Providers limits the agents the skill is made available to; empty means every agent that can use it.
	Providers []string `json:"providers,omitempty"`
	// Global makes the skill available in every project; Projects lists the projects it was installed into.
	Global   bool     `json:"global"`
	Projects []string `json:"projects,omitempty"`
	// PermissionsApproved lets allowed-tools and hooks through to agents that act on them.
	PermissionsApproved bool         `json:"permissions_approved,omitempty"`
	RiskAccepted        bool         `json:"risk_accepted,omitempty"`
	Skill               Skill        `json:"skill"`
	Inspection          Inspection   `json:"inspection"`
	Problems            []Problem    `json:"problems,omitempty"`
	Projections         []Projection `json:"projections,omitempty"`
	History             []Revision   `json:"history,omitempty"`
	Packs               []string     `json:"packs,omitempty"`
	// Facets are what the skill insists on (a package manager, a formatter), used to spot conflicts.
	Facets      map[string]string `json:"facets,omitempty"`
	Upstream    *Upstream         `json:"upstream,omitempty"`
	InstalledAt time.Time         `json:"installed_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// For reports whether the skill should be offered to an agent.
func (r Record) For(provider string) bool {
	return len(r.Providers) == 0 || contains(r.Providers, provider)
}

// InProject reports whether the skill applies to a project, either globally or because it was installed there.
func (r Record) InProject(dir string) bool {
	return r.Global || dir != "" && contains(r.Projects, dir)
}

// ScopeLabel describes where the skill applies, for lists.
func (r Record) ScopeLabel() string {
	switch {
	case r.Global && len(r.Projects) > 0:
		return fmt.Sprintf("global + %d project(s)", len(r.Projects))
	case r.Global:
		return ScopeGlobal
	case len(r.Projects) == 1:
		return ScopeProject
	case len(r.Projects) > 1:
		return fmt.Sprintf("%d projects", len(r.Projects))
	}

	return "not placed"
}

// PackRecord remembers which skills a pack installed, so members can be changed one by one afterwards.
type PackRecord struct {
	ID          string    `json:"id"`
	Version     string    `json:"version"`
	Members     []string  `json:"members"`
	InstalledAt time.Time `json:"installed_at"`
}

// LibraryFile is library.json: every installed skill and pack.
type LibraryFile struct {
	Version int          `json:"version"`
	Skills  []Record     `json:"skills"`
	Packs   []PackRecord `json:"packs,omitempty"`
}

var keyUnsafe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// storeKey is a readable folder name for a skill ID with a hash suffix, so two IDs never share a folder.
func storeKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	key := strings.Trim(keyUnsafe.ReplaceAllString(id, "_"), "_.")

	if len(key) > 80 {
		key = key[len(key)-80:]
	}

	return key + "-" + hex.EncodeToString(sum[:4])
}

func (l *Library) indexPath() string { return filepath.Join(l.Dir, "library.json") }

// RevisionDir is where the canonical copy of one revision of a skill lives.
func (l *Library) RevisionDir(id, hash string) string {
	return filepath.Join(l.Dir, "store", storeKey(id), hash)
}

func (l *Library) load() (LibraryFile, error) {
	var f LibraryFile

	data, err := os.ReadFile(l.indexPath())

	if errors.Is(err, fs.ErrNotExist) {
		return LibraryFile{Version: libraryVersion}, nil
	}

	if err != nil {
		return f, err
	}

	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("read %s: %w", l.indexPath(), err)
	}

	return f, nil
}

func (l *Library) Records() ([]Record, error) {
	f, err := l.load()

	return f.Skills, err
}

func (l *Library) Packs() ([]PackRecord, error) {
	f, err := l.load()

	return f.Packs, err
}

// Find looks a skill up by ID, then by name; a name shared by two sources is ambiguous.
func (l *Library) Find(ref string) (*Record, error) {
	records, err := l.Records()
	if err != nil {
		return nil, err
	}

	return FindIn(records, ref)
}

func FindIn(records []Record, ref string) (*Record, error) {
	var byName []Record

	for _, r := range records {
		if r.ID == ref {
			r := r

			return &r, nil
		}

		if r.Name == ref {
			byName = append(byName, r)
		}
	}

	switch len(byName) {
	case 0:
		return nil, fmt.Errorf("no installed skill %q: %w", ref, ErrNotFound)
	case 1:
		return &byName[0], nil
	}

	var ids []string

	for _, r := range byName {
		ids = append(ids, r.ID)
	}

	return nil, fmt.Errorf("%d installed skills are named %q; use the ID: %s", len(byName), ref, strings.Join(ids, ", "))
}

// Update changes library.json under an exclusive lock, so two csm processes cannot interleave installs.
func (l *Library) Update(fn func(f *LibraryFile) error) error {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}

	lock, err := os.OpenFile(filepath.Join(l.Dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}

	defer lock.Close()

	if err := lockFile(lock); err != nil {
		return err
	}

	defer unlockFile(lock)

	f, err := l.load()
	if err != nil {
		return err
	}

	if err := fn(&f); err != nil {
		return err
	}

	sort.Slice(f.Skills, func(i, j int) bool {
		return f.Skills[i].Name < f.Skills[j].Name || f.Skills[i].Name == f.Skills[j].Name && f.Skills[i].ID < f.Skills[j].ID
	})

	f.Version = libraryVersion

	return writeJSONFile(l.indexPath(), f)
}

// UpdateRecord changes one installed skill under the library lock.
func (l *Library) UpdateRecord(id string, fn func(r *Record) error) error {
	return l.Update(func(f *LibraryFile) error {
		for i := range f.Skills {
			if f.Skills[i].ID == id {
				return fn(&f.Skills[i])
			}
		}

		return fmt.Errorf("no installed skill %q: %w", id, ErrNotFound)
	})
}

// Store writes one revision to the canonical store. It stages the files, re-reads them, and only renames them into
// place when they hash to what was inspected, so a failed write never leaves a partial skill behind.
func (l *Library) Store(id string, s *Skill, contents map[string][]byte, modes map[string]fs.FileMode) (string, error) {
	final := l.RevisionDir(id, s.Hash)

	if got, err := hashDir(final, nil); err == nil && got == s.Hash {
		return final, nil
	}

	staged, err := l.stage(contents, modes)
	if err != nil {
		return "", err
	}

	defer os.RemoveAll(staged)

	if got, err := hashDir(staged, nil); err != nil || got != s.Hash {
		return "", fmt.Errorf("the staged copy does not match what was inspected (%v)", err)
	}

	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return "", err
	}

	os.RemoveAll(final)

	if err := os.Rename(staged, final); err != nil {
		return "", err
	}

	return final, nil
}

// Load reads a stored revision back and checks it still hashes to what was installed.
func (l *Library) LoadRevision(id, hash string) (*Skill, map[string][]byte, map[string]fs.FileMode, error) {
	dir := l.RevisionDir(id, hash)
	contents, modes, err := ReadTree(dir, nil)

	if err != nil {
		return nil, nil, nil, fmt.Errorf("the stored copy of %s is missing or unreadable: %w", id, err)
	}

	s, _, err := FromFiles("", contents, modes)
	if s == nil {
		return nil, nil, nil, err
	}

	if s.Hash != hash {
		return nil, nil, nil, fmt.Errorf("the stored copy of %s was changed on disk (expected %s, found %s)", id, short(hash), short(s.Hash))
	}

	return s, contents, modes, nil
}

// RemoveRevisions deletes every stored revision of a skill.
func (l *Library) RemoveRevisions(id string) error {
	return os.RemoveAll(filepath.Join(l.Dir, "store", storeKey(id)))
}

func (l *Library) stage(contents map[string][]byte, modes map[string]fs.FileMode) (string, error) {
	root := filepath.Join(l.Dir, "staging")

	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}

	dir := filepath.Join(root, randomName())

	if err := writeTree(dir, contents, modes); err != nil {
		os.RemoveAll(dir)

		return "", err
	}

	return dir, nil
}

// CleanStaging removes staging folders left by an interrupted install; nothing in them was ever in use.
func (l *Library) CleanStaging() (int, error) {
	entries, err := os.ReadDir(filepath.Join(l.Dir, "staging"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}

	if err != nil {
		return 0, err
	}

	for _, e := range entries {
		os.RemoveAll(filepath.Join(l.Dir, "staging", e.Name()))
	}

	return len(entries), nil
}

func writeTree(dir string, contents map[string][]byte, modes map[string]fs.FileMode) error {
	for rel, data := range contents {
		if err := SafeRelPath(rel); err != nil {
			return err
		}

		p := filepath.Join(dir, filepath.FromSlash(rel))

		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}

		mode := fs.FileMode(0o644)

		if modes[rel]&0o111 != 0 {
			mode = 0o755
		}

		if err := os.WriteFile(p, data, mode); err != nil {
			return err
		}
	}

	return nil
}

// ReadTree reads a skill folder's files, skipping csm's ownership marker and the files named in skip.
func ReadTree(dir string, skip []string) (map[string][]byte, map[string]fs.FileMode, error) {
	contents, modes := map[string][]byte{}, map[string]fs.FileMode{}

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", p)
		}

		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)

		if rel == ownerFile || contains(skip, rel) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}

		contents[rel], modes[rel] = data, info.Mode()

		return nil
	})

	return contents, modes, err
}

func hashDir(dir string, skip []string) (string, error) {
	contents, modes, err := ReadTree(dir, skip)
	if err != nil {
		return "", err
	}

	return hashContents(contents, modes), nil
}

// hashContents hashes files the same way FromFiles does, without requiring a valid manifest.
func hashContents(contents map[string][]byte, modes map[string]fs.FileMode) string {
	paths := make([]string, 0, len(contents))

	for p := range contents {
		paths = append(paths, p)
	}

	sort.Strings(paths)

	files := make([]File, 0, len(paths))

	for _, p := range paths {
		sum := sha256.Sum256(contents[p])
		files = append(files, File{Path: p, Executable: modes[p]&0o111 != 0 || hasShebang(contents[p]), SHA256: hex.EncodeToString(sum[:])})
	}

	return hashFiles(files)
}

func randomName() string {
	var b [8]byte

	rand.Read(b[:])

	return hex.EncodeToString(b[:])
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}

	return hash
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}

	return false
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}

	_, err = tmp.Write(append(data, '\n'))

	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}

	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}

	if err != nil {
		os.Remove(tmp.Name())
	}

	return err
}
