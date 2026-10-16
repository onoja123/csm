package skills

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Consumer is one agent account (or one project) that should be able to load a skill.
type Consumer struct {
	Provider string
	Account  string
	Scope    string
	// Read are the skill folders the agent loads from; Write is the one csm places copies in.
	Read  []string
	Write string
	Label string
}

const (
	StatusPlaced      = "placed"
	StatusCurrent     = "up to date"
	StatusShared      = "shared"
	StatusNative      = "native"
	StatusConflict    = "conflict"
	StatusChanged     = "changed"
	StatusUnsupported = "unsupported"
	StatusRemoved     = "removed"
	StatusFailed      = "failed"
)

// Outcome is what sync did for one consumer.
type Outcome struct {
	Provider    string   `json:"provider"`
	Account     string   `json:"account,omitempty"`
	Scope       string   `json:"scope"`
	Dir         string   `json:"dir,omitempty"`
	Status      string   `json:"status"`
	Detail      string   `json:"detail,omitempty"`
	Level       Level    `json:"level,omitempty"`
	Adaptations []string `json:"adaptations,omitempty"`
}

// Usable reports whether the agent can load the skill after this outcome.
func (o Outcome) Usable() bool {
	switch o.Status {
	case StatusPlaced, StatusCurrent, StatusShared, StatusNative, StatusChanged:
		return true
	}

	return false
}

type variant struct {
	dir      string
	contents map[string][]byte
	scope    string
	owners   []int
}

// SyncOptions: a partial sync places what its consumers need and removes nothing, for one agent launch.
type SyncOptions struct {
	Partial bool
}

// Sync makes a record's projections match what its consumers need, writing one copy per folder and sharing it with
// every agent that reads that folder and would get identical files. It never writes over a folder csm does not own
// or one someone changed, and it removes only its own unchanged copies that are no longer needed.
func (l *Library) Sync(rec *Record, consumers []Consumer, agents map[string]Agent, now time.Time, opts ...SyncOptions) []Outcome {
	out := make([]Outcome, len(consumers))
	var chosen []*variant

	s, contents, modes, err := l.LoadRevision(rec.ID, rec.Hash)

	if err == nil && len(rec.Skill.Features) == 0 {
		rec.Skill.Features = s.Features
	}

	for i, c := range consumers {
		o := &out[i]
		*o = Outcome{Provider: c.Provider, Account: c.Account, Scope: c.Scope}
		a := agents[c.Provider]

		if err != nil {
			o.Status, o.Detail = StatusFailed, err.Error()

			continue
		}

		if !rec.Enabled || !rec.For(c.Provider) {
			o.Status, o.Detail = StatusUnsupported, "not enabled for "+a.Name

			continue
		}

		compat := Assess(s, a, rec.PermissionsApproved)
		o.Level = compat.Level

		if !compat.Level.Usable() {
			o.Status, o.Detail = StatusUnsupported, joinReasons(compat)

			continue
		}

		if c.Write == "" {
			o.Status, o.Detail = StatusUnsupported, a.Name+" has no skill folder csm can write to here"

			continue
		}

		var placed *variant

		for _, v := range chosen {
			if !inList(c.Read, filepath.Dir(v.dir)) {
				continue
			}

			files, adaptations, err := Adapt(s, contents, a, AdaptOptions{Dir: v.dir, PermissionsApproved: rec.PermissionsApproved})

			if err == nil && sameFiles(files, v.contents) {
				placed, o.Adaptations = v, adaptations
				o.Status, o.Dir = StatusShared, v.dir

				break
			}
		}

		if placed != nil {
			placed.owners = append(placed.owners, i)

			continue
		}

		dir := filepath.Join(c.Write, rec.Name)
		files, adaptations, err := Adapt(s, contents, a, AdaptOptions{Dir: dir, PermissionsApproved: rec.PermissionsApproved})

		if err != nil {
			o.Status, o.Detail = StatusFailed, err.Error()

			continue
		}

		o.Adaptations, o.Dir = adaptations, dir

		for _, v := range chosen {
			if v.dir == dir {
				o.Status, o.Detail = StatusConflict, "another agent needs a different copy in this same folder"
				placed = v
			}
		}

		if placed != nil {
			continue
		}

		v := &variant{dir: dir, contents: files, scope: c.Scope, owners: []int{i}}
		chosen = append(chosen, v)
		o.Status, o.Detail = l.write(rec, v, modes, now)
	}

	for _, v := range chosen {
		status := out[v.owners[0]].Status

		for _, i := range v.owners[1:] {
			if status != StatusPlaced && status != StatusCurrent && status != StatusChanged {
				out[i].Status, out[i].Detail = status, out[v.owners[0]].Detail
			}
		}
	}

	for i, c := range consumers {
		if !out[i].Usable() {
			continue
		}

		for _, root := range c.Read {
			other := filepath.Join(root, rec.Name)

			if samePath(other, out[i].Dir) {
				continue
			}

			if _, err := os.Stat(filepath.Join(other, ManifestFile)); err == nil {
				what := "a different skill with this name"

				if o, ok := readOwnership(other); ok && o.ID == rec.ID {
					what = "csm's copy of the same skill for another agent"
				}

				out[i].Detail = joinDetail(out[i].Detail, fmt.Sprintf("%s also reads %s at %s", agents[c.Provider].Name, what, other))
			}
		}
	}

	if len(opts) > 0 && opts[0].Partial {
		return out
	}

	return append(out, l.prune(rec, chosen, consumers)...)
}

func (l *Library) write(rec *Record, v *variant, modes map[string]fs.FileMode, now time.Time) (string, string) {
	dir := v.dir

	if _, err := os.Lstat(dir); err == nil {
		owned, err := CheckOwned(dir, rec.ID)

		switch {
		case errors.Is(err, ErrNotOwned):
			if o, ok := readOwnership(dir); ok {
				return StatusConflict, fmt.Sprintf("csm placed a different skill (%s) here; remove it first", o.ID)
			}

			if got, err := hashDir(dir, nil); err == nil && got == hashContents(v.contents, modes) {
				return StatusNative, "your own identical copy is already here"
			}

			return StatusConflict, "a skill with this name that csm does not manage is already here; csm left it alone"
		case errors.Is(err, ErrModified):
			l.remember(rec, v, modes, now)

			return StatusChanged, "csm's copy here was edited; it was left as it is (see csm skills diff " + rec.Name + ")"
		case err != nil:
			return StatusFailed, err.Error()
		case owned.Hash == hashContents(v.contents, modes):
			l.remember(rec, v, modes, now)

			return StatusCurrent, ""
		}
	}

	if err := Project(v.contents, modes, dir, ProjectOptions{ID: rec.ID, Canonical: rec.Hash, Revision: rec.Revision, Scope: v.scope}); err != nil {
		return StatusFailed, err.Error()
	}

	l.remember(rec, v, modes, now)

	return StatusPlaced, ""
}

func (l *Library) remember(rec *Record, v *variant, modes map[string]fs.FileMode, now time.Time) {
	p := Projection{Scope: v.scope, Dir: v.dir, Hash: hashContents(v.contents, modes), Canonical: rec.Hash, Adapter: AdapterVersion, At: now}

	for i := range rec.Projections {
		if rec.Projections[i].Dir == v.dir {
			p.Provider, p.Account = rec.Projections[i].Provider, rec.Projections[i].Account
			rec.Projections[i] = p

			return
		}
	}

	rec.Projections = append(rec.Projections, p)
}

// prune removes csm's unchanged copies that no consumer needs any more and labels the kept ones with their agents.
func (l *Library) prune(rec *Record, chosen []*variant, consumers []Consumer) []Outcome {
	var out []Outcome
	keep := map[string][]int{}

	for _, v := range chosen {
		keep[v.dir] = v.owners
	}

	var kept []Projection

	for _, p := range rec.Projections {
		owners, ok := keep[p.Dir]

		if !ok {
			if err := Unproject(p.Dir, rec.ID); err != nil {
				kept = append(kept, p)
			} else {
				out = append(out, Outcome{Provider: p.Provider, Account: p.Account, Scope: p.Scope, Dir: p.Dir, Status: StatusRemoved})
			}

			continue
		}

		if len(owners) > 0 {
			c := consumers[owners[0]]
			p.Provider, p.Account = c.Provider, c.Account
		}

		kept = append(kept, p)
	}

	sort.Slice(kept, func(i, j int) bool { return kept[i].Dir < kept[j].Dir })
	rec.Projections = kept

	return out
}

func sameFiles(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}

	for k, v := range a {
		if w, ok := b[k]; !ok || !bytes.Equal(v, w) {
			return false
		}
	}

	return true
}

func inList(list []string, dir string) bool {
	for _, d := range list {
		if samePath(d, dir) {
			return true
		}
	}

	return false
}

func joinReasons(c Compat) string {
	if len(c.Reasons) == 0 {
		return string(c.Level)
	}

	out := c.Reasons[0]

	for _, r := range c.Reasons[1:] {
		out += "; " + r
	}

	return out
}

func joinDetail(a, b string) string {
	if a == "" {
		return b
	}

	return a + "; " + b
}
