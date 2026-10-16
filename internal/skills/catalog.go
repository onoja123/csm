package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

// CatalogSource is a repository csm indexes for the library. The entries come from the repository itself at the
// commit csm read; csm adds no ratings, counts or claims of its own.
type CatalogSource struct {
	Repo string `json:"repo"`
	Path string `json:"path,omitempty"`
	Note string `json:"note,omitempty"`
}

// CuratedSources are public repositories of Agent Skills csm indexes; each skill keeps its own license.
var CuratedSources = []CatalogSource{
	{Repo: "anthropics/skills", Path: "skills", Note: "Anthropic's example skills"},
	{Repo: "obra/superpowers", Path: "skills", Note: "Development workflow skills by Jesse Vincent"},
	{Repo: "vercel-labs/agent-skills", Path: "skills", Note: "Vercel's React and web skills"},
	{Repo: "callstackincubator/agent-skills", Path: "skills", Note: "Callstack's React Native skills"},
	{Repo: "expo/skills", Path: "plugins/expo/skills", Note: "Expo's skills"},
	{Repo: "trailofbits/skills", Path: "plugins", Note: "Trail of Bits security skills"},
}

type CatalogEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      Source `json:"source"`
	Commit      string `json:"commit"`
	License     string `json:"license,omitempty"`
	RepoLicense string `json:"repo_license,omitempty"`
	Files       int    `json:"files"`
	Scripts     int    `json:"scripts"`
	Problem     string `json:"problem,omitempty"`
}

type catalogCache struct {
	Repo    string         `json:"repo"`
	Path    string         `json:"path,omitempty"`
	Commit  string         `json:"commit"`
	Fetched time.Time      `json:"fetched"`
	Entries []CatalogEntry `json:"entries"`
}

// SkillID is the stable identity of a skill in a repository folder; it does not change with the version.
func SkillID(repo, dir string) string {
	if dir == "" {
		return "github.com/" + repo
	}

	return "github.com/" + repo + "/" + dir
}

func (l *Library) cachePath(src CatalogSource) string {
	return filepath.Join(l.Dir, "cache", storeKey(src.Repo+"_"+src.Path)+".json")
}

// Refresh reads a source repository now: one tree listing, then only the SKILL.md of each skill.
func (l *Library) Refresh(ctx context.Context, g *GitHub, src CatalogSource) error {
	owner, repo, _ := strings.Cut(src.Repo, "/")
	t, err := g.Resolve(ctx, Ref{Owner: owner, Repo: repo, Path: src.Path})

	if err != nil {
		return err
	}

	cache := catalogCache{Repo: src.Repo, Path: src.Path, Commit: t.Commit, Fetched: time.Now()}
	repoLicense := g.RepoLicense(ctx, Ref{Owner: owner, Repo: repo})

	for _, dir := range t.SkillDirs() {
		e := CatalogEntry{ID: SkillID(src.Repo, dir), Name: path.Base(dir), Source: Source{Type: "github", URL: "https://github.com/" + src.Repo, Repo: src.Repo, Path: dir}, Commit: t.Commit}

		for _, te := range t.Entries {
			if te.Type == "blob" && strings.HasPrefix(te.Path, dir+"/") {
				e.Files++

				if te.Mode == "100755" || scriptExt[strings.ToLower(path.Ext(te.Path))] {
					e.Scripts++
				}

				if strings.HasPrefix(strings.ToUpper(path.Base(te.Path)), "LICENSE") && e.License == "" {
					e.License = "see " + path.Base(te.Path)
				}
			}
		}

		data, err := g.get(ctx, fmt.Sprintf("%s/%s/%s/%s/%s/%s", g.Raw, owner, repo, t.Commit, escapePath(dir), ManifestFile), "")

		if err != nil {
			e.Problem = err.Error()
		} else if s, _ := parseManifest(data); s != nil {
			if s.Name != "" {
				e.Name = s.Name
			}

			e.Description = s.Description

			if s.License != "" {
				e.License = s.License
			}
		}

		if e.License == "" {
			e.RepoLicense = repoLicense
		}

		cache.Entries = append(cache.Entries, e)
	}

	return writeJSONFile(l.cachePath(src), cache)
}

// Catalog returns the cached entries of every source and when each was read; it never touches the network.
func (l *Library) Catalog(sources []CatalogSource) ([]CatalogEntry, map[string]time.Time, error) {
	var entries []CatalogEntry
	fetched := map[string]time.Time{}

	for _, src := range sources {
		var c catalogCache

		data, err := os.ReadFile(l.cachePath(src))

		if errors.Is(err, fs.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, nil, err
		}

		if err := json.Unmarshal(data, &c); err != nil {
			continue
		}

		entries = append(entries, c.Entries...)
		fetched[src.Repo] = c.Fetched
	}

	return entries, fetched, nil
}

// Item is anything search can rank: an installed skill or a catalog entry.
type Item struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
	Source      string   `json:"source"`
	Installed   bool     `json:"installed"`
	Score       float64  `json:"score,omitempty"`
}

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields("a an and are as at be by for from has have how i in into is it its of on or that the this to use used using when with you your we our can should will do does make add new code task please need want") {
		stopwords[w] = true
	}
}

// Terms splits text into lowercase words worth matching, without stopwords.
func Terms(text string) []string {
	var out []string
	seen := map[string]bool{}

	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(w) < 2 || stopwords[w] || seen[w] {
			continue
		}

		seen[w] = true
		out = append(out, w)
	}

	return out
}

// Search ranks items by how many query terms they contain and where: the name counts most, then tags, then the
// description. Items without any match are dropped.
func Search(items []Item, query string) []Item {
	terms := Terms(query)

	if len(terms) == 0 {
		out := append([]Item(nil), items...)
		sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })

		return out
	}

	var out []Item

	for _, it := range items {
		if score := relevance(it, terms); score > 0 {
			it.Score = score
			out = append(out, it)
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}

		return out[i].Name < out[j].Name
	})

	return out
}

func relevance(it Item, terms []string) float64 {
	name := strings.ToLower(it.Name)
	nameTerms := set(Terms(strings.ReplaceAll(it.Name, "-", " ")))
	desc := set(Terms(it.Description))
	tags := set(Terms(strings.Join(it.Tags, " ")))
	score := 0.0

	for _, t := range terms {
		switch {
		case name == t:
			score += 10
		case nameTerms[t]:
			score += 5
		case tags[t]:
			score += 3
		case desc[t]:
			score += 2
		case strings.Contains(name, t) || prefixMatch(desc, t):
			score += 1
		}
	}

	return score
}

func prefixMatch(words map[string]bool, t string) bool {
	if len(t) < 4 {
		return false
	}

	for w := range words {
		if strings.HasPrefix(w, t) || strings.HasPrefix(t, w) && len(w) >= 4 {
			return true
		}
	}

	return false
}

func set(words []string) map[string]bool {
	m := map[string]bool{}

	for _, w := range words {
		m[w] = true
	}

	return m
}
