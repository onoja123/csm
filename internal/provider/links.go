package provider

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// userLink is one entry of the user's own agent settings that a profile can share through a symlink.
type userLink struct {
	name   string
	src    string
	dst    string
	skills bool
}

type linker interface {
	userLinks(profileDir string) (userDir string, links []userLink, err error)
}

func pairs(userDir, profileBase string, names ...string) []userLink {
	var out []userLink

	for _, n := range names {
		out = append(out, userLink{name: n, src: filepath.Join(userDir, n), dst: filepath.Join(profileBase, n), skills: n == "skills"})
	}

	return out
}

func homeDir() (string, error) { return os.UserHomeDir() }

func (c Claude) userLinks(profileDir string) (string, []userLink, error) {
	home, err := homeDir()
	if err != nil {
		return "", nil, err
	}

	userDir := filepath.Join(home, ".claude")

	return userDir, pairs(userDir, profileDir, "settings.json", "CLAUDE.md", "agents", "commands", "skills"), nil
}

// Codex keeps reading $CODEX_HOME/skills, so a profile shares ~/.codex/skills like the other settings; ~/.agents/skills
// needs no link because Codex reads it from the real home whatever CODEX_HOME is.
func (c Codex) userLinks(profileDir string) (string, []userLink, error) {
	home, err := homeDir()
	if err != nil {
		return "", nil, err
	}

	userDir := filepath.Join(home, ".codex")

	return userDir, pairs(userDir, profileDir, "config.toml", "AGENTS.md", "hooks.json", "prompts", "rules", "skills"), nil
}

// Gemini CLI resolves ~/.agents/skills against GEMINI_CLI_HOME, so a profile needs its own link to the user's copy.
func (g Gemini) userLinks(profileDir string) (string, []userLink, error) {
	home, err := homeDir()
	if err != nil {
		return "", nil, err
	}

	userDir := filepath.Join(home, ".gemini")
	links := pairs(userDir, geminiDir(profileDir), "settings.json", "GEMINI.md", "commands", "extensions", "skills")
	links = append(links, userLink{name: ".agents/skills", src: filepath.Join(home, ".agents", "skills"), dst: filepath.Join(profileDir, ".agents", "skills"), skills: true})

	return userDir, links, nil
}

// config.json is left out because it names the signed-in user.
func (c Copilot) userLinks(profileDir string) (string, []userLink, error) {
	home, err := homeDir()
	if err != nil {
		return "", nil, err
	}

	userDir := filepath.Join(home, ".copilot")

	return userDir, pairs(userDir, profileDir, "settings.json", "mcp-config.json", "copilot-instructions.md", "agents", "skills"), nil
}

// Only cli-config.json lives in the config dir; Cursor reads skills and MCP servers from the real home anyway.
func (c Cursor) userLinks(profileDir string) (string, []userLink, error) {
	userDir, err := cursorUserDir()
	if err != nil {
		return "", nil, err
	}

	return userDir, pairs(userDir, profileDir, "cli-config.json"), nil
}

// applyLinks symlinks each existing user entry into the profile. An entry already in the profile is never replaced:
// it is reported as kept, with why.
func applyLinks(links []userLink) (linked, kept []string, err error) {
	for _, l := range links {
		if _, err := os.Stat(l.src); err != nil {
			continue
		}

		if fi, err := os.Lstat(l.dst); err == nil {
			target, _ := os.Readlink(l.dst)

			switch {
			case fi.Mode()&fs.ModeSymlink != 0 && filepath.Clean(target) == filepath.Clean(l.src):
			case fi.Mode()&fs.ModeSymlink != 0:
				kept = append(kept, fmt.Sprintf("%s links to %s, not to your %s; csm left it", l.name, target, l.src))
			default:
				kept = append(kept, fmt.Sprintf("%s already exists in the profile; csm left it instead of linking your %s", l.name, l.src))
			}

			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return linked, kept, err
		}

		if err := os.MkdirAll(filepath.Dir(l.dst), 0o700); err != nil {
			return linked, kept, err
		}

		if err := os.Symlink(l.src, l.dst); err != nil {
			return linked, kept, err
		}

		linked = append(linked, l.name)
	}

	return linked, kept, nil
}

// RelinkUserConfig shares the user's own settings with an existing profile, linking only what is missing.
func RelinkUserConfig(p Provider, profileDir string) (userDir string, linked, kept []string, err error) {
	l, ok := p.(linker)
	if !ok {
		return "", nil, nil, fmt.Errorf("%s has no settings csm can share", p.Name())
	}

	userDir, links, err := l.userLinks(profileDir)
	if err != nil {
		return "", nil, nil, err
	}

	linked, kept, err = applyLinks(links)

	return userDir, linked, kept, err
}

// profileLinked reports whether a profile was set up to share the user's settings: one of its entries links there.
func profileLinked(links []userLink) bool {
	for _, l := range links {
		if target, err := os.Readlink(l.dst); err == nil && filepath.Clean(target) == filepath.Clean(l.src) {
			return true
		}
	}

	return false
}

// SkillLinkGaps names the user's own skill folders a profile that shares settings does not see.
func SkillLinkGaps(p Provider, profileDir string) []string {
	l, ok := p.(linker)
	if !ok {
		return nil
	}

	_, links, err := l.userLinks(profileDir)

	if err != nil || !profileLinked(links) {
		return nil
	}

	var gaps []string

	for _, link := range links {
		if !link.skills || countSkills(link.src) == 0 {
			continue
		}

		if target, err := os.Readlink(link.dst); err == nil && filepath.Clean(target) == filepath.Clean(link.src) {
			continue
		}

		gaps = append(gaps, fmt.Sprintf("does not see the %d skill(s) in your %s", countSkills(link.src), displayHome(link.src)))
	}

	return gaps
}

func countSkills(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}

	n := 0

	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			n++
		}
	}

	return n
}

func displayHome(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(p, home)
	}

	return p
}

// copilotSharedSkills is ~/.agents/skills for a Copilot profile that shares the user's settings: Copilot only reads
// it with its default home, so csm names it in COPILOT_SKILLS_DIRS to keep what the user sees outside csm.
func copilotSharedSkills(profileDir string) string {
	home, err := homeDir()
	if err != nil {
		return ""
	}

	shared := filepath.Join(home, ".agents", "skills")

	if target, err := os.Readlink(filepath.Join(profileDir, "skills")); err != nil || filepath.Clean(target) != filepath.Join(home, ".copilot", "skills") {
		return ""
	}

	if fi, err := os.Stat(shared); err != nil || !fi.IsDir() {
		return ""
	}

	return shared
}

// appendEnvList adds a value to a comma-separated variable, keeping what the user already set.
func appendEnvList(env []string, key, value string) []string {
	for i, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			for _, part := range strings.Split(v, ",") {
				if part == value {
					return env
				}
			}

			if v != "" {
				value = v + "," + value
			}

			env[i] = key + "=" + value

			return env
		}
	}

	return append(env, key+"="+value)
}
