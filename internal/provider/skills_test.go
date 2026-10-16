package provider

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/onoja123/csm/internal/skills"
)

func TestSkillsAgentDescriptors(t *testing.T) {
	for _, id := range IDs {
		a := SkillsAgent(id)

		if !a.Supported() || a.Verified == "" || a.Name == "" {
			t.Errorf("%s: %+v", id, a)
		}

		if len(a.ProjectDirs) == 0 {
			t.Errorf("%s has no project skill folder for task copies", id)
		}
	}

	if SkillsAgent(CursorID).Evidence != "" || SkillsAgent(CursorID).Request != "" {
		t.Fatal("csm cannot read Cursor sessions, so Cursor must not claim evidence or a load request")
	}

	if SkillsAgent(CodexID).RequestText("pdf") != "$pdf" {
		t.Fatal("Codex loads a skill when the prompt mentions $name")
	}

	claude := SkillsAgent(ClaudeID)

	if !slices.Contains(claude.Fields, "hooks") || !slices.Contains(claude.Fields, "allowed-tools") || !slices.Contains(claude.Variables, "${CLAUDE_SKILL_DIR}") {
		t.Fatalf("claude: %+v", claude)
	}

	if a := SkillsAgent("nope"); a.Supported() {
		t.Fatal("an unknown agent has no skill support")
	}
}

func userDirs(t *testing.T, home string, dirs ...string) {
	t.Helper()

	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(home, d, "mine"), 0o755); err != nil {
			t.Fatal(err)
		}

		os.WriteFile(filepath.Join(home, d, "mine", "SKILL.md"), []byte("---\nname: mine\ndescription: d\n---\n"), 0o644)
	}
}

func linkTarget(t *testing.T, path string) string {
	t.Helper()

	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("%s is not a link: %v", path, err)
	}

	return target
}

func TestLinkUserConfigSharesSkillFolders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	userDirs(t, home, ".claude/skills", ".codex/skills", ".gemini/skills", ".agents/skills", ".copilot/skills")

	cases := []struct {
		p    Provider
		dst  string
		want string
	}{
		{Claude{}, "skills", ".claude/skills"},
		{Codex{}, "skills", ".codex/skills"},
		{Gemini{}, ".gemini/skills", ".gemini/skills"},
		{Gemini{}, ".agents/skills", ".agents/skills"},
		{Copilot{}, "skills", ".copilot/skills"},
	}

	profiles := map[string]string{}

	for _, c := range cases {
		profile, ok := profiles[c.p.ID()]

		if !ok {
			profile = t.TempDir()
			profiles[c.p.ID()] = profile

			if _, _, err := c.p.LinkUserConfig(profile); err != nil {
				t.Fatal(err)
			}
		}

		if got := linkTarget(t, filepath.Join(profile, c.dst)); got != filepath.Join(home, c.want) {
			t.Errorf("%s %s -> %s", c.p.ID(), c.dst, got)
		}
	}

	cursor := t.TempDir()

	if _, linked, _ := (Cursor{}).LinkUserConfig(cursor); slices.Contains(linked, "skills") {
		t.Fatal("Cursor reads skills from the real home; its profile needs no skills link")
	}
}

func TestRelinkNeverReplacesProfileFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	userDirs(t, home, ".claude/skills")
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte("{}"), 0o644)

	profile := t.TempDir()
	own := filepath.Join(profile, "skills", "kept")
	os.MkdirAll(own, 0o755)
	os.WriteFile(filepath.Join(own, "SKILL.md"), []byte("mine"), 0o644)

	_, linked, kept, err := RelinkUserConfig(Claude{}, profile)
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(linked, "settings.json") || slices.Contains(linked, "skills") || len(kept) != 1 || !strings.Contains(kept[0], "already exists") {
		t.Fatalf("linked %v kept %v", linked, kept)
	}

	if data, _ := os.ReadFile(filepath.Join(own, "SKILL.md")); string(data) != "mine" {
		t.Fatal("the profile's own skills folder was touched")
	}

	if _, linked, _, _ := RelinkUserConfig(Claude{}, profile); len(linked) != 0 {
		t.Fatalf("relinking twice linked again: %v", linked)
	}

	gaps := SkillLinkGaps(Claude{}, profile)

	if len(gaps) != 1 || !strings.Contains(gaps[0], "~/.claude/skills") {
		t.Fatalf("gaps = %v", gaps)
	}

	if gaps := SkillLinkGaps(Claude{}, t.TempDir()); len(gaps) != 0 {
		t.Fatalf("a profile that shares nothing reports no gaps: %v", gaps)
	}
}

func TestCopilotSharesAgentsSkillsWhenLinked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	userDirs(t, home, ".copilot/skills", ".agents/skills")
	t.Setenv("COPILOT_SKILLS_DIRS", "")

	linked := t.TempDir()

	if _, _, err := (Copilot{}).LinkUserConfig(linked); err != nil {
		t.Fatal(err)
	}

	want := "COPILOT_SKILLS_DIRS=" + filepath.Join(home, ".agents", "skills")

	if env := (Copilot{}).Env(linked); !slices.Contains(env, want) {
		t.Fatalf("a linked profile must see ~/.agents/skills: %v", filter(env, "COPILOT_SKILLS_DIRS"))
	}

	t.Setenv("COPILOT_SKILLS_DIRS", "/team/skills")

	if env := (Copilot{}).Env(linked); !slices.Contains(env, "COPILOT_SKILLS_DIRS=/team/skills,"+filepath.Join(home, ".agents", "skills")) {
		t.Fatalf("the user's own value must be kept: %v", filter(env, "COPILOT_SKILLS_DIRS"))
	}

	t.Setenv("COPILOT_SKILLS_DIRS", "")

	if env := (Copilot{}).Env(t.TempDir()); len(filter(env, "COPILOT_SKILLS_DIRS=/")) != 0 {
		t.Fatal("an isolated profile must not see the user's skills")
	}
}

func filter(env []string, prefix string) []string {
	var out []string

	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}

	return out
}

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func names(ev []skills.Evidence) []string {
	var out []string

	for _, e := range ev {
		out = append(out, e.Skill+":"+e.Kind)
	}

	return out
}

func TestSkillEvidenceFromSessionRecords(t *testing.T) {
	const sid = "11111111-2222-4333-8444-555555555555"

	claude := t.TempDir()
	write(t, filepath.Join(claude, "projects", "-repo", sid+".jsonl"), strings.Join([]string{
		`{"type":"user","timestamp":"2026-10-14T10:00:00Z","message":{"role":"user","content":"<command-message>pdf</command-message>\n<command-name>/pdf</command-name>"}}`,
		`{"type":"assistant","timestamp":"2026-10-14T10:00:05Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Skill","input":{"skill":"docs:xlsx"}},{"type":"text","text":"I will not mention SKILL.md here"}]}}`,
		`{"type":"assistant","timestamp":"2026-10-14T10:00:06Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":"/x/notes/SKILL.md"}}]}}`,
	}, "\n"))

	ev, err := (Claude{}).SkillEvidence(claude, "/repo", sid)
	if err != nil || !slices.Equal(names(ev), []string{"pdf:/pdf command", "xlsx:Skill tool call"}) {
		t.Fatalf("claude: %v %v", names(ev), err)
	}

	codex := t.TempDir()
	write(t, filepath.Join(codex, "sessions", "2026", "10", "14", "rollout-2026-10-14T10-00-00-"+sid+".jsonl"), strings.Join([]string{
		`{"timestamp":"2026-10-14T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<skill>\n<name>pdf</name>\n<path>/w/.agents/skills/pdf/SKILL.md</path>\n</skill>"}]}}`,
		`{"timestamp":"2026-10-14T10:00:02Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"sed -n 1,80p /h/.agents/skills/lint/SKILL.md\"}"}}`,
		`{"timestamp":"2026-10-14T10:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"<skill><name>fake</name></skill>"}]}}`,
	}, "\n"))

	ev, err = (Codex{}).SkillEvidence(codex, "/w", sid)
	if err != nil || !slices.Equal(names(ev), []string{"pdf:$skill instructions injected", "lint:read SKILL.md with a shell command"}) {
		t.Fatalf("codex: %v %v", names(ev), err)
	}

	gemini := t.TempDir()
	write(t, filepath.Join(gemini, ".gemini", "tmp", "p1", "chats", "session-2026-10-14-abc.jsonl"), strings.Join([]string{
		`{"sessionId":"` + sid + `","projectHash":"p1","kind":"main"}`,
		`{"type":"gemini","toolCalls":[{"name":"activate_skill","args":{"name":"pdf"},"status":"success","timestamp":"2026-10-14T10:00:00Z"},{"name":"activate_skill","args":{"name":"denied"},"status":"cancelled"}]}`,
	}, "\n"))

	ev, err = (Gemini{}).SkillEvidence(gemini, "/w", sid)
	if err != nil || !slices.Equal(names(ev), []string{"pdf:activate_skill call"}) {
		t.Fatalf("gemini: %v %v", names(ev), err)
	}

	copilot := t.TempDir()
	write(t, filepath.Join(copilot, "session-state", sid, "events.jsonl"), strings.Join([]string{
		`{"type":"skill.invoked","timestamp":"2026-10-14T10:00:00Z","data":{"name":"pdf","trigger":"agent-invoked"}}`,
		`{"type":"skill.context_delivered","data":{"name":"pdf"}}`,
	}, "\n"))

	ev, err = (Copilot{}).SkillEvidence(copilot, "/w", sid)
	if err != nil || !slices.Equal(names(ev), []string{"pdf:skill invoked (agent-invoked)"}) {
		t.Fatalf("copilot: %v %v", names(ev), err)
	}

	if _, err := (Claude{}).SkillEvidence(claude, "/repo", "../escape"); err == nil {
		t.Fatal("a session ID with a path was accepted")
	}

	if _, ok := SkillEvidence(Cursor{}); ok {
		t.Fatal("csm cannot read Cursor sessions")
	}

	if !ListingHas(`["pdf","xlsx"]`, "pdf") || ListingHas(`["pdf-tools"]`, "pdf") {
		t.Fatal("ListingHas must match whole skill names")
	}
}

func TestGeminiFolderTrustRules(t *testing.T) {
	t.Setenv("GEMINI_CLI_TRUST_WORKSPACE", "")
	t.Setenv("GEMINI_RESTRICTED_MODE", "")
	t.Setenv("GEMINI_CLI_TRUSTED_FOLDERS_PATH", "")

	profile := t.TempDir()
	root := t.TempDir()
	repo := filepath.Join(root, "work", "repo")
	other := filepath.Join(root, "work", "other")
	secret := filepath.Join(repo, "secret")

	for _, d := range []string{repo, other, secret, filepath.Join(repo, "sub")} {
		os.MkdirAll(d, 0o755)
	}

	g := Gemini{}

	if g.FolderTrusted(profile, repo) {
		t.Fatal("with no rules a folder is not trusted")
	}

	write(t, filepath.Join(profile, ".gemini", "trustedFolders.json"), `{"`+filepath.Join(repo, "x")+`": "TRUST_PARENT", "`+secret+`": "DO_NOT_TRUST"}`)

	if !g.FolderTrusted(profile, repo) || !g.FolderTrusted(profile, filepath.Join(repo, "sub")) {
		t.Fatal("TRUST_PARENT trusts the rule's parent and below")
	}

	if g.FolderTrusted(profile, secret) {
		t.Fatal("the longest matching rule wins")
	}

	if g.FolderTrusted(profile, other) {
		t.Fatal("a sibling is not trusted")
	}

	t.Setenv("GEMINI_CLI_TRUST_WORKSPACE", "true")

	if !g.FolderTrusted(profile, other) {
		t.Fatal("Gemini's own override trusts the workspace")
	}

	t.Setenv("GEMINI_CLI_TRUST_WORKSPACE", "")
	write(t, filepath.Join(profile, ".gemini", "settings.json"), `{"security":{"folderTrust":{"enabled":false}}}`)

	if !g.FolderTrusted(profile, other) {
		t.Fatal("with folder trust turned off every folder is trusted")
	}

	if !TrustsFolder(Claude{}, profile, other) {
		t.Fatal("agents without folder trust load project skills anywhere")
	}
}
