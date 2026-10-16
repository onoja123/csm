package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ownership is the marker csm writes into every projection; it is how csm proves a folder is its own.
type ownership struct {
	ID        string    `json:"id"`
	Hash      string    `json:"hash"`
	Canonical string    `json:"canonical"`
	Revision  string    `json:"revision,omitempty"`
	Adapter   string    `json:"adapter,omitempty"`
	Scope     string    `json:"scope,omitempty"`
	Extra     []string  `json:"extra,omitempty"`
	Written   time.Time `json:"written_at"`
}

var ErrNotOwned = errors.New("not a folder csm wrote")

// ErrModified means a folder csm wrote was changed afterwards; csm leaves it alone.
var ErrModified = errors.New("changed since csm wrote it")

const gitignoreFile = ".gitignore"

// ProjectOptions identify what a projection holds.
type ProjectOptions struct {
	ID        string
	Canonical string
	Revision  string
	Scope     string
	// HideFromGit adds a .gitignore of "*" so a copy inside a worktree is never committed.
	HideFromGit bool
}

// Owned describes a projection csm can prove it wrote.
type Owned struct {
	ID        string
	Hash      string
	Canonical string
	Revision  string
	Scope     string
}

func readOwnership(dir string) (ownership, bool) {
	var o ownership

	data, err := os.ReadFile(filepath.Join(dir, ownerFile))
	if err != nil || json.Unmarshal(data, &o) != nil || o.ID == "" || o.Hash == "" {
		return ownership{}, false
	}

	return o, true
}

// CheckOwned reports whether dir is a projection csm wrote for this skill and whether it is unchanged since.
func CheckOwned(dir, id string) (Owned, error) {
	o, ok := readOwnership(dir)

	if !ok || o.ID != id {
		return Owned{}, ErrNotOwned
	}

	owned := Owned{ID: o.ID, Hash: o.Hash, Canonical: o.Canonical, Revision: o.Revision, Scope: o.Scope}

	got, err := hashDir(dir, o.Extra)
	if err != nil {
		return owned, fmt.Errorf("%w: %v", ErrModified, err)
	}

	if got != o.Hash {
		return owned, ErrModified
	}

	return owned, nil
}

// Project writes files where an agent discovers skills. It only replaces a folder csm wrote for the same skill that
// nobody changed since; anything else is a conflict the caller must resolve. The new copy is written beside the
// target and renamed into place, so an agent never sees half a skill.
func Project(contents map[string][]byte, modes map[string]fs.FileMode, dir string, opts ProjectOptions) error {
	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; csm does not write through it", dir)
		}

		owned, err := CheckOwned(dir, opts.ID)
		if err != nil {
			return fmt.Errorf("%s already exists and is %w", dir, err)
		}

		if o, _ := readOwnership(dir); owned.Hash == hashContents(contents, modes) && o.Scope == opts.Scope && len(o.Extra) > 0 == opts.HideFromGit {
			return nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	parent := filepath.Dir(dir)

	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}

	tmp := filepath.Join(parent, ".csm-tmp-"+randomName())

	if err := writeTree(tmp, contents, modes); err != nil {
		os.RemoveAll(tmp)

		return err
	}

	o := ownership{ID: opts.ID, Hash: hashContents(contents, modes), Canonical: opts.Canonical, Revision: opts.Revision, Adapter: AdapterVersion, Scope: opts.Scope, Written: time.Now()}

	if _, own := contents[gitignoreFile]; opts.HideFromGit && !own {
		if err := os.WriteFile(filepath.Join(tmp, gitignoreFile), []byte("*\n"), 0o644); err != nil {
			os.RemoveAll(tmp)

			return err
		}

		o.Extra = []string{gitignoreFile}
	}

	if err := writeJSONFile(filepath.Join(tmp, ownerFile), o); err != nil {
		os.RemoveAll(tmp)

		return err
	}

	old := ""

	if _, err := os.Lstat(dir); err == nil {
		old = filepath.Join(parent, ".csm-old-"+randomName())

		if err := os.Rename(dir, old); err != nil {
			os.RemoveAll(tmp)

			return err
		}
	}

	if err := os.Rename(tmp, dir); err != nil {
		if old != "" {
			os.Rename(old, dir)
		}

		os.RemoveAll(tmp)

		return err
	}

	if old != "" {
		os.RemoveAll(old)
	}

	return nil
}

// Unproject removes a projection only when csm can still prove it wrote it and nobody changed it.
func Unproject(dir, id string) error {
	fi, err := os.Lstat(dir)

	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s was left in place: it is a symlink csm did not create", dir)
	}

	if _, err := CheckOwned(dir, id); err != nil {
		return fmt.Errorf("%s was left in place: it is %w", dir, err)
	}

	return os.RemoveAll(dir)
}

// Backup copies a folder csm is about to replace at the user's request, so the user's edits stay recoverable.
func (l *Library) Backup(dir string) (string, error) {
	contents, modes, err := ReadTree(dir, nil)
	if err != nil {
		return "", err
	}

	dest := filepath.Join(l.Dir, "backups", time.Now().Format("20060102-150405")+"-"+filepath.Base(dir))

	if err := writeTree(dest, contents, modes); err != nil {
		os.RemoveAll(dest)

		return "", err
	}

	return dest, nil
}

// Reclaim records that the files now in dir are csm's copy of a revision, once the user made their edit the
// installed version.
func Reclaim(dir, id, canonical, revision string) error {
	o, ok := readOwnership(dir)
	if !ok || o.ID != id {
		return ErrNotOwned
	}

	got, err := hashDir(dir, o.Extra)
	if err != nil {
		return err
	}

	o.Hash, o.Canonical, o.Revision, o.Written = got, canonical, revision, time.Now()

	return writeJSONFile(filepath.Join(dir, ownerFile), o)
}

// Found is a skill folder an agent would load, managed by csm or not.
type Found struct {
	Name     string    `json:"name"`
	Dir      string    `json:"dir"`
	Root     string    `json:"root"`
	Managed  string    `json:"managed,omitempty"`
	Edited   bool      `json:"edited,omitempty"`
	Hash     string    `json:"hash,omitempty"`
	Problem  string    `json:"problem,omitempty"`
	Skill    *Skill    `json:"-"`
	Problems []Problem `json:"problems,omitempty"`
}

// Scan lists the skill folders directly inside root, and one level below, without following symlinked skills
// outside it.
func Scan(root string) []Found {
	var out []Found

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	for _, e := range entries {
		dir := filepath.Join(root, e.Name())

		if strings.HasPrefix(e.Name(), ".") {
			continue
		}

		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			if e.Type()&fs.ModeSymlink != 0 {
				out = append(out, Found{Name: e.Name(), Dir: dir, Root: root, Problem: "broken link"})
			}

			continue
		}

		if _, err := os.Stat(filepath.Join(dir, ManifestFile)); err != nil {
			for _, nested := range Scan(dir) {
				if !strings.Contains(strings.TrimPrefix(nested.Dir, dir+string(filepath.Separator)), string(filepath.Separator)) {
					out = append(out, nested)
				}
			}

			continue
		}

		f := Found{Name: e.Name(), Dir: dir, Root: root}
		s, problems, err := Load(dir)

		switch {
		case s == nil:
			f.Problem = err.Error()
		case err != nil:
			f.Problem, f.Name = err.Error(), orName(s.Name, e.Name())
		default:
			f.Name = s.Name
		}

		f.Skill, f.Problems = s, problems

		if s != nil {
			f.Hash = s.Hash
		}

		if o, ok := readOwnership(dir); ok {
			f.Managed = o.ID

			if _, err := CheckOwned(dir, o.ID); err != nil {
				f.Edited = true
			}
		}

		out = append(out, f)
	}

	return out
}

func orName(a, b string) string {
	if a != "" {
		return a
	}

	return b
}
