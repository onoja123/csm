package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/handoff"
	"github.com/onoja123/csm/internal/provider"
	"github.com/onoja123/csm/internal/skills"
)

func writeSkillFixture(t *testing.T, root, name, front, body string, extra map[string]string) string {
	t.Helper()

	dir := filepath.Join(root, name)
	os.MkdirAll(dir, 0o755)

	manifest := "---\nname: " + name + "\ndescription: " + name + " helps with PDF forms and exports.\n" + front + "---\n\n" + body + "\n"

	if err := os.WriteFile(filepath.Join(dir, skills.ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	for file, content := range extra {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0o755)
		os.WriteFile(filepath.Join(dir, file), []byte(content), 0o755)
	}

	return dir
}

// skillsState is a csm home with real profile folders for the given accounts, and a throwaway HOME.
func skillsState(t *testing.T, accounts map[string][]string) *State {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("CSM_GITHUB_TOKEN", "")

	s, err := initState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range provider.IDs {
		for _, name := range accounts[id] {
			a, err := s.addAccount(id, name, testNow)
			if err != nil {
				t.Fatal(err)
			}

			os.MkdirAll(a.ConfigDir, 0o700)
		}
	}

	if err := s.save(); err != nil {
		t.Fatal(err)
	}

	return s
}

func skillsCLI(t *testing.T, s *State, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	err := cmdSkills(&Logger{out: &out, err: &out}, s.Home, args)

	return out.String(), err
}

func decode[T any](t *testing.T, out string) T {
	t.Helper()

	var v T

	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}

	return v
}

func TestSkillsCommandsPlaceAndRemove(t *testing.T) {
	s := skillsState(t, map[string][]string{provider.ClaudeID: {"work"}, provider.CodexID: {"c1"}})
	src := writeSkillFixture(t, t.TempDir(), "pdf-fill", "", "Run ${CLAUDE_SKILL_DIR}/scripts/fill.sh.", map[string]string{"scripts/fill.sh": "#!/bin/sh\necho fill\n"})

	out, err := skillsCLI(t, s, "install", src, "--json")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	reports := decode[[]installReport](t, out)
	work, _ := s.resolveAccount("work")
	c1, _ := s.resolveAccount("c1")

	if len(reports) != 1 || reports[0].Action != "installed" || len(reports[0].Outcomes) != 2 {
		t.Fatalf("install: %s", out)
	}

	claudeCopy := filepath.Join(work.ConfigDir, "skills", "pdf-fill", skills.ManifestFile)
	codexCopy := filepath.Join(c1.ConfigDir, "skills", "pdf-fill", skills.ManifestFile)

	if data, _ := os.ReadFile(claudeCopy); !strings.Contains(string(data), "${CLAUDE_SKILL_DIR}") {
		t.Fatalf("Claude Code substitutes its own variable; its copy must keep it:\n%s", data)
	}

	if data, _ := os.ReadFile(codexCopy); strings.Contains(string(data), "${CLAUDE_SKILL_DIR}") || !strings.Contains(string(data), filepath.Dir(codexCopy)) {
		t.Fatalf("Codex has no such variable; its copy must point at the folder:\n%s", data)
	}

	out, err = skillsCLI(t, s, "list", "--provider", "codex", "--json")
	if err != nil {
		t.Fatal(err)
	}

	views := decode[[]skillView](t, out)

	if len(views) != 1 || views[0].Account != "c1" || !slices.ContainsFunc(views[0].Entries, func(e skillEntry) bool { return e.Name == "pdf-fill" && e.Status == entryAvailable && e.Managed != "" }) {
		t.Fatalf("availability: %s", out)
	}

	if out, err = skillsCLI(t, s, "disable", "pdf-fill", "--provider", "codex"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	if _, err := os.Stat(codexCopy); !os.IsNotExist(err) {
		t.Fatal("limiting the skill to Claude Code must remove the Codex copy")
	}

	if _, err := os.Stat(claudeCopy); err != nil {
		t.Fatal("the Claude Code copy must stay")
	}

	findings := decode[[]doctorFinding](t, mustCLI(t, s, "doctor", "--json"))

	if slices.ContainsFunc(findings, func(f doctorFinding) bool { return f.Level == findFail }) {
		t.Fatalf("doctor: %+v", findings)
	}

	if out, err = skillsCLI(t, s, "remove", "pdf-fill"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	if _, err := os.Stat(filepath.Dir(claudeCopy)); !os.IsNotExist(err) {
		t.Fatal("remove must delete csm's copies")
	}

	if _, err := os.Stat(src); err != nil {
		t.Fatal("remove must never touch the source folder")
	}
}

func mustCLI(t *testing.T, s *State, args ...string) string {
	t.Helper()

	out, err := skillsCLI(t, s, args...)
	if err != nil {
		t.Fatalf("csm skills %v: %v\n%s", args, err, out)
	}

	return out
}

func TestAccountSwitchKeepsSkillsCurrent(t *testing.T) {
	s := skillsState(t, map[string][]string{provider.ClaudeID: {"linked", "isolated"}})
	home := os.Getenv("HOME")
	userSkills := filepath.Join(home, ".claude", "skills")
	os.MkdirAll(userSkills, 0o755)

	linked, _ := s.resolveAccount("linked")
	isolated, _ := s.resolveAccount("isolated")
	os.Symlink(userSkills, filepath.Join(linked.ConfigDir, "skills"))

	srcRoot := t.TempDir()
	src := writeSkillFixture(t, srcRoot, "notes", "", "Version one.", nil)
	mustCLI(t, s, "install", src)

	if data, _ := os.ReadFile(filepath.Join(userSkills, "notes", skills.ManifestFile)); !strings.Contains(string(data), "Version one") {
		t.Fatal("a profile that shares the user's skills folder must get the copy there")
	}

	writeSkillFixture(t, srcRoot, "notes", "", "Version two.", nil)
	mustCLI(t, s, "update", "notes", "--yes")

	for _, dir := range []string{filepath.Join(userSkills, "notes"), filepath.Join(isolated.ConfigDir, "skills", "notes")} {
		if data, _ := os.ReadFile(filepath.Join(dir, skills.ManifestFile)); !strings.Contains(string(data), "Version two") {
			t.Fatalf("%s kept a stale version:\n%s", dir, data)
		}
	}

	edited := filepath.Join(isolated.ConfigDir, "skills", "notes", skills.ManifestFile)
	os.WriteFile(edited, []byte("---\nname: notes\ndescription: my own notes\n---\nMine.\n"), 0o644)

	writeSkillFixture(t, srcRoot, "notes", "", "Version three.", nil)
	out := mustCLI(t, s, "update", "notes", "--yes")

	if !strings.Contains(out, "your edits in") {
		t.Fatalf("an update must say it left the edited copy alone:\n%s", out)
	}

	if data, _ := os.ReadFile(edited); !strings.Contains(string(data), "Mine.") {
		t.Fatal("switching back to that account must not overwrite the user's edit")
	}

	if notes := placeForLaunch(s.Home, s, provider.ClaudeID, "isolated", ""); len(notes) != 0 {
		t.Fatalf("an edited copy is reported by doctor, not a launch error: %v", notes)
	}

	if data, _ := os.ReadFile(edited); !strings.Contains(string(data), "Mine.") {
		t.Fatal("a launch overwrote the edited copy")
	}

	late, err := s.addAccount(provider.ClaudeID, "late", testNow)
	if err != nil {
		t.Fatal(err)
	}

	os.MkdirAll(late.ConfigDir, 0o700)
	s.save()

	if notes := placeForLaunch(s.Home, s, provider.ClaudeID, "late", ""); len(notes) != 0 {
		t.Fatal(notes)
	}

	if data, _ := os.ReadFile(filepath.Join(late.ConfigDir, "skills", "notes", skills.ManifestFile)); !strings.Contains(string(data), "Version three") {
		t.Fatal("an account added later must get the current version when it starts")
	}
}

func claudeSkillSeed(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "seed.jsonl")
	os.WriteFile(path, []byte(`{"type":"assistant","timestamp":"2026-10-14T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"`+name+`"}}]}}`+"\n"), 0o600)

	return path
}

func latestHandoff(t *testing.T, s *State, project string) (handoff.State, string) {
	t.Helper()

	st, dir, err := handoff.Latest(s.handoffRoot(project))
	if err != nil {
		t.Fatal(err)
	}

	return st, dir
}

func TestHandoffCarriesPinnedSkills(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	res, err := runFailoverScenario(testBinary(t), scenario{
		limited: "alpha,beta",
		crossTo: provider.CodexID,
		seed:    claudeSkillSeed(t, "pdf-fill"),
		skills:  []string{"pdf-fill"},
		before: func(s *State, dir string) {
			s.Config.HandoffTo = []string{provider.CodexID}
			s.save()

			lib := skills.Open(s.Home)
			cands, _ := (&skills.Fetcher{}).Fetch(t.Context(), writeSkillFixture(t, t.TempDir(), "pdf-fill", "", "Fill with ${CLAUDE_SKILL_DIR}/fill.sh.", nil), nil)

			if _, err := lib.Install(cands[0], skills.InstallOptions{}, time.Now()); err != nil {
				t.Fatal(err)
			}
		},
		after: func(s *State, project, gitStatus string) {
			checkCarriedSkills(t, s, project, gitStatus)
		},
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, res.output)
	}
}

func checkCarriedSkills(t *testing.T, s *State, project, gitStatus string) {
	t.Helper()

	st, dir := latestHandoff(t, s, project)

	if st.Skills == nil || len(st.Skills.Carried) != 1 || st.Skills.Carried[0].Used != string(skills.StateInvoked) {
		t.Fatalf("handoff skills: %+v", st.Skills)
	}

	carried := st.Skills.Carried[0]

	if !strings.HasPrefix(carried.To, project) || !strings.HasSuffix(carried.To, filepath.Join(".agents", "skills", "pdf-fill")) || carried.Use != "$pdf-fill" {
		t.Fatalf("Codex must get a task copy where it reads project skills: %+v", carried)
	}

	if !strings.Contains(st.Skills.Brief, "$pdf-fill") || !strings.Contains(handoff.Instruction(st, "x"), "$pdf-fill") {
		t.Fatalf("the brief must ask Codex for the skill in its own syntax:\n%s", st.Skills.Brief)
	}

	m, err := skills.LoadManifest(filepath.Join(dir, skillsManifestFile))
	if err != nil {
		t.Fatal(err)
	}

	cp := m.Skills[0].Placement(provider.CodexID)

	if cp == nil || cp.State != skills.StateInvoked || len(cp.Evidence) == 0 || !cp.Handoff {
		t.Fatalf("Codex's rollout shows the $pdf-fill injection, so its use is verified: %+v", cp)
	}

	if !slices.Contains(cp.Adaptations, "${CLAUDE_SKILL_DIR} replaced with the skill's installed folder") {
		t.Fatalf("adaptations: %v", cp.Adaptations)
	}

	if leftovers, _ := filepath.Glob(filepath.Join(project, ".agents", "skills", "*")); len(leftovers) != 0 {
		t.Fatalf("the session's task copies must be removed when csm stops: %v", leftovers)
	}

	if strings.Contains(gitStatus, ".agents") || strings.Contains(gitStatus, ".claude") {
		t.Fatalf("task copies must never show in git status:\n%s", gitStatus)
	}

	md, _ := os.ReadFile(filepath.Join(dir, handoff.FileMD))

	if !strings.Contains(string(md), "## Skills") || strings.Contains(string(md), "Fill with") {
		t.Fatalf("handoff.md must name the skill without copying its contents:\n%s", md)
	}
}

func TestHandoffNeverCarriesHooksOrAutoRunCommands(t *testing.T) {
	s := skillsState(t, map[string][]string{provider.CodexID: {"c1"}, provider.ClaudeID: {"work"}})
	lib := skills.Open(s.Home)
	root := t.TempDir()

	for name, front := range map[string]string{
		"hooked":  "hooks:\n  PreToolUse:\n    - matcher: Bash\n      hooks:\n        - type: command\n          command: ./scripts/check.sh\n",
		"autorun": "",
		"tools":   "allowed-tools: Bash(*)\n",
	} {
		body := "Steps."

		if name == "autorun" {
			body = "Context: !`git log -5`"
		}

		cands, _ := (&skills.Fetcher{}).Fetch(t.Context(), writeSkillFixture(t, root, name, front, body, nil), nil)

		if _, err := lib.Install(cands[0], skills.InstallOptions{ApprovePermissions: true}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	records, _ := lib.Records()
	res := skills.Resolve(records, provider.SkillsAgent(provider.CodexID), skills.Request{Explicit: []string{"hooked", "autorun", "tools"}, Provider: provider.CodexID}, skills.Policy{Mode: skills.ModeManual})
	m := skills.NewManifest(res, records, "task", time.Now())
	project := t.TempDir()
	c1, _ := s.resolveAccount("c1")
	work, _ := s.resolveAccount("work")

	lib.Prepare(&m, skills.Target{Agent: provider.SkillsAgent(provider.CodexID), Account: "c1", Dir: project, ProfileDir: c1.ConfigDir}, records, time.Now())

	for _, ts := range m.Skills {
		if p := ts.Placement(provider.CodexID); p == nil || p.Dir == "" {
			t.Fatalf("Codex ignores hooks and !`command`, so %s is usable there: %+v", ts.Name, p)
		}
	}

	lib.Prepare(&m, skills.Target{Agent: provider.SkillsAgent(provider.ClaudeID), Account: "work", Dir: project, ProfileDir: work.ConfigDir, Handoff: true}, records, time.Now())

	for _, ts := range m.Skills {
		p := ts.Placement(provider.ClaudeID)

		switch ts.Name {
		case "hooked", "autorun":
			if p.State != skills.StateUnavailable || !strings.Contains(p.Problem, "handoff") {
				t.Fatalf("%s must not be activated for Claude Code by a handoff: %+v", ts.Name, p)
			}

			if _, err := os.Stat(filepath.Join(project, ".claude", "skills", ts.Name)); !os.IsNotExist(err) {
				t.Fatalf("%s was written where Claude Code would load it", ts.Name)
			}
		case "tools":
			data, _ := os.ReadFile(filepath.Join(p.Dir, skills.ManifestFile))

			if p.Dir == "" || strings.Contains(string(data), "allowed-tools") {
				t.Fatalf("approved tool permissions are never carried by a handoff:\n%s", data)
			}
		}
	}

	if src, _ := os.ReadFile(filepath.Join(root, "tools", skills.ManifestFile)); !strings.Contains(string(src), "allowed-tools") {
		t.Fatal("csm must never change the original skill")
	}
}

func TestSkillSecurityControls(t *testing.T) {
	s := skillsState(t, map[string][]string{provider.ClaudeID: {"work"}})
	key := "AKIA" + "ABCDEFGHIJKLMNOP"
	src := writeSkillFixture(t, t.TempDir(), "deploy", "", "Use key "+key+" to deploy.\n\nNormal text‮evil", nil)

	out := mustCLI(t, s, "inspect", src, "--json")

	if strings.Contains(out, key) || !strings.Contains(out, "redacted AWS access key") {
		t.Fatalf("inspect must not show the credential:\n%s", out)
	}

	if !strings.Contains(out, "AWS access key; value hidden") || !strings.Contains(out, "U+202E") {
		t.Fatalf("inspect must report the key and the hidden character:\n%s", out)
	}

	if out, err := skillsCLI(t, s, "install", src); err == nil || !strings.Contains(out, "--accept-risk") {
		t.Fatalf("a risky skill needs explicit approval: %v\n%s", err, out)
	}

	if recs, _ := skills.Open(s.Home).Records(); len(recs) != 0 {
		t.Fatal("a refused install left a record")
	}

	mustCLI(t, s, "install", src, "--accept-risk")

	out = mustCLI(t, s, "show", "deploy")

	if strings.Contains(out, key) {
		t.Fatal("show must not print the credential")
	}

	for _, bad := range []string{"../escape", "/etc/passwd"} {
		if _, err := skillsCLI(t, s, "keep", "deploy", bad); err == nil {
			t.Fatalf("keep accepted %s", bad)
		}
	}
}

func TestInterruptedHandoffKeepsSkillManifest(t *testing.T) {
	s := skillsState(t, map[string][]string{provider.ClaudeID: {"work"}, provider.CodexID: {"c1"}})
	project := t.TempDir()
	lib := skills.Open(s.Home)
	cands, _ := (&skills.Fetcher{}).Fetch(t.Context(), writeSkillFixture(t, t.TempDir(), "notes", "", "Steps.", nil), nil)

	if _, err := lib.Install(cands[0], skills.InstallOptions{}, time.Now()); err != nil {
		t.Fatal(err)
	}

	records, _ := lib.Records()
	res := skills.Resolve(records, provider.SkillsAgent(provider.ClaudeID), skills.Request{Explicit: []string{"notes"}, Provider: provider.ClaudeID}, skills.Policy{Mode: skills.ModeManual})
	m := skills.NewManifest(res, records, "task", time.Now())
	source := filepath.Join(s.projectStateDir(provider.ClaudeID, project), skillsManifestFile)

	if err := skills.UpdateManifest(source, func(cur *skills.Manifest) error { *cur = m; return nil }); err != nil {
		t.Fatal(err)
	}

	work, _ := s.resolveAccount("work")
	c1, _ := s.resolveAccount("c1")
	codex, _ := provider.New(provider.CodexID, "")
	claude, _ := provider.New(provider.ClaudeID, "")

	for attempt := 1; attempt <= 2; attempt++ {
		r := &runner{state: s, provider: codex, cwd: project, project: Project{Dir: project}, log: &Logger{out: &bytes.Buffer{}, err: &bytes.Buffer{}}}
		r.stateDir = s.projectStateDir(provider.CodexID, project)
		os.MkdirAll(r.stateDir, 0o700)

		carried := r.carrySkills(claude, work, "", handoffTarget{provider: codex, account: c1})

		if carried == nil || len(carried.Carried) != 1 || carried.Carried[0].Name != "notes" {
			t.Fatalf("attempt %d: the restarted handoff must recover the source manifest: %+v", attempt, carried)
		}
	}

	copies, _ := filepath.Glob(filepath.Join(project, ".agents", "skills", "*"))

	if len(copies) != 1 {
		t.Fatalf("a retried handoff must not place a second copy: %v", copies)
	}

	carriedManifest, _ := skills.LoadManifest(filepath.Join(s.projectStateDir(provider.CodexID, project), skillsManifestFile))

	if len(carriedManifest.Skills) != 1 || carriedManifest.Skills[0].Hash != m.Skills[0].Hash {
		t.Fatalf("the pinned revision must survive: %+v", carriedManifest.Skills)
	}
}

func TestSkillsDoctorReportsProblems(t *testing.T) {
	s := skillsState(t, map[string][]string{provider.ClaudeID: {"work"}})
	lib := skills.Open(s.Home)
	src := writeSkillFixture(t, t.TempDir(), "notes", "", "Steps.", nil)
	mustCLI(t, s, "install", src)
	rec, _ := lib.Find("notes")
	work, _ := s.resolveAccount("work")

	os.WriteFile(filepath.Join(lib.RevisionDir(rec.ID, rec.Hash), skills.ManifestFile), []byte("tampered"), 0o644)
	os.RemoveAll(filepath.Join(work.ConfigDir, "skills", "notes"))
	os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(work.ConfigDir, "skills", "dangling"))
	bad := filepath.Join(work.ConfigDir, "skills", "broken")
	os.MkdirAll(bad, 0o755)
	os.WriteFile(filepath.Join(bad, skills.ManifestFile), []byte("no frontmatter"), 0o644)

	findings := decode[[]doctorFinding](t, func() string { out, _ := skillsCLI(t, s, "doctor", "--json"); return out }())
	has := func(level, text string) bool {
		return slices.ContainsFunc(findings, func(f doctorFinding) bool {
			return f.Level == level && strings.Contains(f.Label+" "+f.Detail, text)
		})
	}

	for _, want := range []struct{ level, text string }{
		{findFail, "changed on disk"},
		{findWarn, "is missing"},
		{findWarn, "may not load"},
	} {
		if !has(want.level, want.text) {
			t.Errorf("doctor did not report %q (%s): %+v", want.text, want.level, findings)
		}
	}

	var out bytes.Buffer
	var problems []string
	doctorSkills(&Logger{out: &out, err: &out}, s, t.TempDir(), func(label, reason string) { problems = append(problems, label+": "+reason) })

	if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "changed on disk") }) {
		t.Fatalf("csm doctor must fail on a tampered skill: %v", problems)
	}
}

func TestSkillsAPIUsesTheSameLibrary(t *testing.T) {
	s := skillsState(t, map[string][]string{provider.ClaudeID: {"work"}})
	d := &daemon{home: s.Home, bus: newEventBus()}
	d.log = newActivityLog(s.Home, d.bus)
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	src := writeSkillFixture(t, t.TempDir(), "notes", "", "Steps.", nil)
	body, _ := json.Marshal(map[string]any{"source": src, "global": true})
	resp, err := http.Post(srv.URL+"/v1/skills/install", "application/json", bytes.NewReader(body))

	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("install: %v %v", err, resp.Status)
	}

	resp.Body.Close()

	out := mustCLI(t, s, "list", "--json")

	if !strings.Contains(out, `"name": "notes"`) {
		t.Fatalf("the CLI must see what the app installed:\n%s", out)
	}

	resp, _ = http.Get(srv.URL + "/v1/skills")

	var view struct {
		Skills []skillRecordView `json:"skills"`
	}

	json.NewDecoder(resp.Body).Decode(&view)
	resp.Body.Close()

	if len(view.Skills) != 1 || len(view.Skills[0].Compatibility) != len(provider.IDs) || view.Skills[0].Placements[view.Skills[0].Projections[0].Dir] != "ok" {
		t.Fatalf("api view: %+v", view.Skills)
	}

	body, _ = json.Marshal(map[string]any{"id": "notes"})
	resp, _ = http.Post(srv.URL+"/v1/skills/disable", "application/json", bytes.NewReader(body))
	resp.Body.Close()

	if rec, _ := skills.Open(s.Home).Find("notes"); rec.Enabled || len(rec.Projections) != 0 {
		t.Fatalf("disable through the app must remove csm's copies: %+v", rec)
	}

	resp, _ = http.Post(srv.URL+"/v1/skills/nonsense", "application/json", bytes.NewReader(body))

	if resp.StatusCode != 404 {
		t.Fatalf("unknown action: %s", resp.Status)
	}

	project := t.TempDir()
	session := filepath.Join(s.projectStateDir(provider.ClaudeID, project), skillsManifestFile)
	skills.UpdateManifest(session, func(m *skills.Manifest) error {
		m.Skills = []skills.TaskSkill{{ID: "x", Name: "notes", Hash: "h", Origin: skills.OriginExplicit, State: skills.StateSelected}}

		return nil
	})

	resp, _ = http.Get(srv.URL + "/v1/skills/session?provider=claude&project_dir=" + project)

	var m skills.Manifest

	json.NewDecoder(resp.Body).Decode(&m)
	resp.Body.Close()

	if len(m.Skills) != 1 || m.Skills[0].Name != "notes" {
		t.Fatalf("session skills: %+v", m)
	}
}

func TestBuildRoomTaskSkills(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := newBuildHarness(t, map[string][]string{provider.ClaudeID: {"alpha"}}, nil)
	t.Setenv("CSM_FAKE_SEED", claudeSkillSeed(t, "greetings"))

	lib := skills.Open(h.home)
	cands, _ := (&skills.Fetcher{}).Fetch(t.Context(), writeSkillFixture(t, t.TempDir(), "greetings", "", "Greet politely.", nil), nil)

	if _, err := lib.Install(cands[0], skills.InstallOptions{}, time.Now()); err != nil {
		t.Fatal(err)
	}

	h.fixture("plan", nil, planFixture(map[string]any{"id": "greet", "title": "Greeting function", "objective": "Add Greet(name)", "files": []string{"greet.go"}, "agent": "claude"}))
	h.fixture("greet", map[string]string{"greet.go": "package demo\n\nfunc Greet(name string) string { return \"Hello, \" + name }\n"}, workReportFixture("greet.go"))

	b := h.create("Add a greeting", BuildPolicy{RequirePlanApproval: true, MaxParallel: 1})
	h.waitFor(b.ID, 30*time.Second, statusIs(BuildAwaitingPlan))

	edit := planEdit{}
	edit.Tasks = append(edit.Tasks, struct {
		ID         string   `json:"id"`
		Title      string   `json:"title"`
		Objective  string   `json:"objective"`
		Scope      string   `json:"scope"`
		Acceptance []string `json:"acceptance"`
		Context    string   `json:"context"`
		Files      []string `json:"files"`
		DependsOn  []string `json:"depends_on"`
		Provider   string   `json:"provider"`
		Skills     []string `json:"skills"`
	}{ID: "greet", Title: "Greeting function", Objective: "Add Greet(name)", Files: []string{"greet.go"}, Provider: provider.ClaudeID, Skills: []string{"greetings"}})

	if err := h.engine.editPlan(b.ID, edit); err != nil {
		t.Fatal(err)
	}

	h.act(b.ID, "approve-plan", actionRequest{})
	final := h.waitFor(b.ID, 90*time.Second, statusIs(BuildAwaitingFinal, BuildNeedsAttention, BuildFailed))
	greet := final.task("greet")

	if greet.SkillManifest == "" || slices.ContainsFunc(greet.Completion.FilesChanged, func(f string) bool { return strings.Contains(f, "skills") }) {
		t.Fatalf("status %s (%s); manifest %q; files %v", final.Status, final.Attention, greet.SkillManifest, greet.Completion)
	}

	m, err := skills.LoadManifest(greet.SkillManifest)
	if err != nil {
		t.Fatal(err)
	}

	p := m.Skills[0].Placement(provider.ClaudeID)

	if p == nil || !strings.HasPrefix(p.Dir, greet.Worktree) || p.State != skills.StateInvoked {
		t.Fatalf("placement: %+v", p)
	}

	transcripts, _ := filepath.Glob(filepath.Join(h.home, "accounts", "alpha", "projects", "*", "*.jsonl"))
	found := false

	for _, tr := range transcripts {
		data, _ := os.ReadFile(tr)
		found = found || strings.Contains(string(data), `load it with your Skill tool (skill: \"greetings\")`)
	}

	if !found {
		t.Fatal("the task instruction must ask Claude Code for the skill")
	}

	r := newReceipt(&final, time.Now())

	if len(r.Skills) != 1 || r.Skills[0].Usage != SkillUsageVerified || r.Skills[0].TaskID != "greet" || r.Skills[0].Hash == "" {
		t.Fatalf("receipt skills: %+v", r.Skills)
	}

	if out := h.git("status", "--porcelain"); strings.Contains(out, "skills") {
		t.Fatalf("task skill copies must not touch the user's checkout:\n%s", out)
	}
}
