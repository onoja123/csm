// Package skills keeps a library of Agent Skills (folders holding a SKILL.md), checks them, and makes them
// discoverable to each coding agent csm runs.
package skills

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	ManifestFile = "SKILL.md"
	// ownerFile marks a folder csm wrote, so it never removes or overwrites a skill it does not own.
	ownerFile      = ".csm-skill.json"
	maxFiles       = 500
	maxTotalBytes  = 20 << 20
	maxNameLen     = 64
	maxDescription = 1024
	maxCompat      = 500
)

var nameRule = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Skill is one skill folder: its validated frontmatter, its files and a hash of their contents.
type Skill struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	License       string            `json:"license,omitempty"`
	Compatibility string            `json:"compatibility,omitempty"`
	AllowedTools  []string          `json:"allowed_tools,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	// Extra holds frontmatter fields outside the Agent Skills specification, such as an agent's own extensions, untouched.
	Extra map[string]any `json:"extra,omitempty"`
	// Features are the agent-specific placeholders and commands the body uses, kept so checks work without the body.
	Features []string `json:"features,omitempty"`
	Body     string   `json:"-"`
	Files    []File   `json:"files"`
	Hash     string   `json:"hash"`
}

type File struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Executable bool   `json:"executable,omitempty"`
	SHA256     string `json:"sha256"`
}

// Problem is something wrong with a skill; Fatal problems keep it from being installed.
type Problem struct {
	Fatal   bool   `json:"fatal"`
	Message string `json:"message"`
}

type ValidationError struct {
	Problems []Problem
}

func (e *ValidationError) Error() string {
	var lines []string

	for _, p := range e.Problems {
		if p.Fatal {
			lines = append(lines, p.Message)
		}
	}

	return "the skill is not valid: " + strings.Join(lines, "; ")
}

// Load reads a skill folder from disk. Symlinks are never followed: a skill that contains one is refused.
func Load(dir string) (*Skill, []Problem, error) {
	files := map[string][]byte{}
	modes := map[string]fs.FileMode{}
	total := int64(0)

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if rel != "." && (d.Name() == ".git" || d.Name() == ".csm-skill") {
				return fs.SkipDir
			}

			return nil
		}

		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; csm does not install skills containing symlinks", rel)
		}

		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", rel)
		}

		if rel == ownerFile {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		if total += info.Size(); total > maxTotalBytes {
			return fmt.Errorf("the skill is larger than %d MB", maxTotalBytes>>20)
		}

		if len(files) >= maxFiles {
			return fmt.Errorf("the skill has more than %d files", maxFiles)
		}

		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}

		files[rel], modes[rel] = data, info.Mode()

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return FromFiles(filepath.Base(dir), files, modes)
}

// FromFiles builds a skill from file contents keyed by slash-separated relative path; dirName is the folder it lives in.
func FromFiles(dirName string, files map[string][]byte, modes map[string]fs.FileMode) (*Skill, []Problem, error) {
	for p := range files {
		if err := SafeRelPath(p); err != nil {
			return nil, nil, err
		}
	}

	manifest, ok := files[ManifestFile]
	if !ok {
		return nil, nil, fmt.Errorf("no %s at the top of the skill folder", ManifestFile)
	}

	s, problems := parseManifest(manifest)

	problems = append(problems, brokenLinks(s.Body, files)...)

	if s.Name != "" && dirName != "" && s.Name != dirName {
		problems = append(problems, Problem{Fatal: false, Message: fmt.Sprintf("name %q does not match its folder %q; agents that require a match may skip it", s.Name, dirName)})
	}

	paths := make([]string, 0, len(files))

	for p := range files {
		paths = append(paths, p)
	}

	sort.Strings(paths)

	for _, p := range paths {
		sum := sha256.Sum256(files[p])
		s.Files = append(s.Files, File{Path: p, Size: int64(len(files[p])), Executable: modes[p]&0o111 != 0 || hasShebang(files[p]), SHA256: hex.EncodeToString(sum[:])})
	}

	s.Hash = hashFiles(s.Files)

	for _, p := range problems {
		if p.Fatal {
			return s, problems, &ValidationError{Problems: problems}
		}
	}

	return s, problems, nil
}

// SafeRelPath rejects paths that could escape the skill folder once written to disk.
func SafeRelPath(p string) error {
	switch {
	case p == "" || strings.ContainsRune(p, 0) || strings.Contains(p, `\`):
		return fmt.Errorf("unsafe file name %q in the skill", p)
	case path.IsAbs(p) || filepath.IsAbs(p) || filepath.VolumeName(p) != "":
		return fmt.Errorf("absolute path %q in the skill", p)
	case path.Clean(p) != p || p == "." || strings.HasPrefix(p, "../") || p == "..":
		return fmt.Errorf("path %q would leave the skill folder", p)
	}

	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." || part == "" {
			return fmt.Errorf("path %q would leave the skill folder", p)
		}
	}

	return nil
}

func hasShebang(data []byte) bool { return bytes.HasPrefix(data, []byte("#!")) }

func hashFiles(files []File) string {
	h := sha256.New()

	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%t\x00%s\n", f.Path, f.Executable, f.SHA256)
	}

	return hex.EncodeToString(h.Sum(nil))
}

func parseManifest(data []byte) (*Skill, []Problem) {
	s := &Skill{}
	fatal := func(format string, args ...any) Problem {
		return Problem{Fatal: true, Message: fmt.Sprintf(format, args...)}
	}

	if !utf8.Valid(data) {
		return s, []Problem{fatal("%s is not UTF-8 text", ManifestFile)}
	}

	text := strings.TrimPrefix(strings.ReplaceAll(string(data), "\r\n", "\n"), "\ufeff")
	front, body, ok := splitFrontmatter(text)

	if !ok {
		return s, []Problem{fatal("%s has no YAML frontmatter between --- lines", ManifestFile)}
	}

	s.Body = body
	s.Features = bodyFeatures(body)

	var raw map[string]any

	if err := yaml.Unmarshal([]byte(front), &raw); err != nil {
		return s, []Problem{fatal("the frontmatter is not valid YAML: %v", err)}
	}

	var problems []Problem

	for key, value := range raw {
		switch key {
		case "name":
			s.Name, _ = value.(string)
		case "description":
			s.Description, _ = value.(string)
		case "license":
			s.License, _ = value.(string)
		case "compatibility":
			s.Compatibility, _ = value.(string)
		case "allowed-tools":
			s.AllowedTools = toolList(value)
		case "metadata":
			m, ok := value.(map[string]any)
			if !ok {
				problems = append(problems, Problem{Message: "metadata is not a map; it was ignored"})

				continue
			}

			s.Metadata = map[string]string{}

			for k, v := range m {
				s.Metadata[k] = fmt.Sprint(v)
			}
		default:
			if s.Extra == nil {
				s.Extra = map[string]any{}
			}

			s.Extra[key] = value
		}
	}

	s.Name, s.Description = strings.TrimSpace(s.Name), strings.TrimSpace(s.Description)

	switch {
	case s.Name == "":
		problems = append(problems, fatal("the frontmatter has no name"))
	case len(s.Name) > maxNameLen:
		problems = append(problems, fatal("name %q is longer than %d characters", s.Name, maxNameLen))
	case !nameRule.MatchString(s.Name):
		problems = append(problems, fatal("name %q may only contain lowercase letters, digits and single hyphens, and cannot start or end with a hyphen", s.Name))
	}

	switch {
	case s.Description == "":
		problems = append(problems, fatal("the frontmatter has no description, so no agent could tell when to use the skill"))
	case utf8.RuneCountInString(s.Description) > maxDescription:
		problems = append(problems, fatal("the description is longer than %d characters", maxDescription))
	}

	if utf8.RuneCountInString(s.Compatibility) > maxCompat {
		problems = append(problems, Problem{Message: fmt.Sprintf("compatibility is longer than %d characters", maxCompat)})
	}

	return s, problems
}

func splitFrontmatter(text string) (string, string, bool) {
	if !strings.HasPrefix(text, "---\n") {
		return "", "", false
	}

	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")

	if end < 0 {
		return "", "", false
	}

	front := rest[:end]
	body := strings.TrimPrefix(rest[end+len("\n---"):], "\n")

	return front, body, true
}

func toolList(v any) []string {
	switch t := v.(type) {
	case string:
		return splitTools(t)
	case []any:
		var out []string

		for _, item := range t {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}

		return out
	}

	return nil
}

// splitTools splits "Read Bash(git add:*), Grep" on spaces and commas outside parentheses.
func splitTools(s string) []string {
	var out []string
	var cur strings.Builder
	depth := 0

	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			out = append(out, t)
		}

		cur.Reset()
	}

	for _, r := range s {
		switch {
		case r == '(':
			depth++
		case r == ')' && depth > 0:
			depth--
		case (r == ',' || r == ' ' || r == '\t' || r == '\n') && depth == 0:
			flush()

			continue
		}

		cur.WriteRune(r)
	}

	flush()

	return out
}

var markdownLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// brokenLinks reports links in SKILL.md to files the skill does not contain; an agent following them would find nothing.
func brokenLinks(body string, files map[string][]byte) []Problem {
	var out []Problem
	seen := map[string]bool{}

	for _, m := range markdownLink.FindAllStringSubmatch(body, -1) {
		target := m[1]

		if i := strings.IndexAny(target, "#?"); i >= 0 {
			target = target[:i]
		}

		target = strings.TrimPrefix(target, "./")

		if target == "" || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "/") || strings.HasPrefix(target, "$") || seen[target] {
			continue
		}

		seen[target] = true
		clean := path.Clean(target)

		if _, ok := files[clean]; ok {
			continue
		}

		dir := false

		for f := range files {
			dir = dir || strings.HasPrefix(f, strings.TrimSuffix(clean, "/")+"/")
		}

		if !dir {
			out = append(out, Problem{Message: fmt.Sprintf("SKILL.md links to %s, which is not in the skill", target)})
		}
	}

	return out
}

var ErrNotFound = errors.New("not found")
