package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/onoja123/csm/engrun"
)

var stdin = bufio.NewReader(os.Stdin)

func ask(log *Logger, prompt, def string) string {
	if def != "" {
		log.Prompt(fmt.Sprintf("%s [%s]: ", prompt, def))
	} else {
		log.Prompt(prompt + ": ")
	}

	line, _ := stdin.ReadString('\n')

	if line = strings.TrimSpace(line); line == "" {
		return def
	}

	return line
}

// askSecret reads a line without echoing it when stdin is a terminal.
func askSecret(log *Logger, prompt string) string {
	log.Prompt(prompt + ": ")

	if isTerminal(os.Stdin.Fd()) && setEcho(os.Stdin.Fd(), false) == nil {
		defer func() {
			setEcho(os.Stdin.Fd(), true)
			log.Blank()
		}()
	}

	line, _ := stdin.ReadString('\n')

	return strings.TrimSpace(line)
}

func choose(log *Logger, what string, names []string) (int, error) {
	return chooseDefault(log, what, names, 0)
}

func chooseDefault(log *Logger, what string, names []string, def int) (int, error) {
	if len(names) == 1 {
		return 0, nil
	}

	log.Blank()

	for i, n := range names {
		log.Info("  %d. %s", i+1, n)
	}

	n, err := strconv.Atoi(ask(log, "Choose the "+what, strconv.Itoa(def+1)))
	if err != nil || n < 1 || n > len(names) {
		return 0, fmt.Errorf("no %s %q", what, strconv.Itoa(n))
	}

	return n - 1, nil
}

// service talks to the local CSM service over its socket, the same API the desktop app uses.
type service struct {
	http *http.Client
}

func newService(home string) service {
	path := socketPath(home)

	return service{http: &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}}
}

func (s service) do(method, path string, in, out any) error {
	var body io.Reader

	if in != nil {
		data, _ := json.Marshal(in)
		body = bytes.NewReader(data)
	}

	req, _ := http.NewRequest(method, "http://csm"+path, body)

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("the CSM service is not reachable: %v", err)
	}

	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		var e apiError

		json.Unmarshal(data, &e)

		return errors.New(orValue(e.Error, resp.Status))
	}

	if out != nil {
		return json.Unmarshal(data, out)
	}

	return nil
}

// ensureService starts csm serve in the background when it is not running: runs live in the service, not in this
// command, so they keep going after it returns.
func ensureService(log *Logger, home string) (service, error) {
	if !dialable(socketPath(home)) {
		if err := serveDetach(log, home, ""); err != nil {
			return service{}, err
		}
	}

	return newService(home), nil
}

func loadAutofixConfig(home string) autofixConfig {
	var cfg autofixConfig

	readJSON(autofixConfigPath(home), &cfg)

	return cfg
}

func saveAutofixConfigAt(home string, cfg autofixConfig) error {
	if err := writeJSON(autofixConfigPath(home), cfg); err != nil {
		return err
	}

	return os.Chmod(autofixConfigPath(home), 0o600)
}

func cmdSentry(log *Logger, home string, args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}

	switch args[0] {
	case "connect":
		return sentryConnect(log, home, args[1:])
	case "status":
		return sentryStatus(log, home)
	case "disconnect":
		cfg := loadAutofixConfig(home)
		cfg.Sentry = nil

		if err := saveAutofixConfigAt(home, cfg); err != nil {
			return err
		}

		log.Success("Disconnected from Sentry. The auth token was removed from this machine.")

		return nil
	}

	return errors.New("usage: csm sentry connect [--url U] [--org O] [--project P] | csm sentry status | csm sentry disconnect")
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}

		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}

	return ""
}

func sentryConnect(log *Logger, home string, args []string) error {
	cwd, _ := os.Getwd()

	rs, err := inspectRepo(orValue(flagValue(args, "--repo"), cwd))
	if err != nil {
		return fmt.Errorf("%v; run csm sentry connect inside the repository Sentry's errors come from", err)
	}

	cfg := loadAutofixConfig(home)
	prev := sentryConfig{}

	if cfg.Sentry != nil {
		prev = *cfg.Sentry
	}

	u, err := validSentryURL(orValue(flagValue(args, "--url"), orValue(prev.URL, "https://sentry.io")))
	if err != nil {
		return err
	}

	token := os.Getenv("SENTRY_AUTH_TOKEN")

	if token == "" && prev.Token != "" && prev.URL == u {
		token = prev.Token
	}

	if token == "" {
		log.Info("Create a personal auth token in Sentry (User settings → Personal Tokens) with org:read, project:read and event:read.")
		log.Info("CSM keeps it in %s, readable only by you, and sends it only to %s.", displayPath(autofixConfigPath(home)), u)
		log.Blank()
		token = askSecret(log, "Sentry auth token")
	}

	if token == "" {
		return errors.New("no auth token given")
	}

	c := newSentryClient(sentryConfig{URL: u, Token: token})
	ctx := context.Background()

	orgs, err := c.orgs(ctx)
	if err != nil {
		return err
	}

	if len(orgs) == 0 {
		return errors.New("this token cannot see any Sentry organization")
	}

	org := flagValue(args, "--org")

	if org == "" {
		names := make([]string, len(orgs))

		for i, o := range orgs {
			names[i] = o.Slug
		}

		i, err := choose(log, "organization", names)
		if err != nil {
			return err
		}

		org = orgs[i].Slug
	}

	projects, err := c.projects(ctx, org)
	if err != nil {
		return err
	}

	slug := flagValue(args, "--project")

	if slug == "" {
		names := make([]string, len(projects))

		for i, p := range projects {
			names[i] = p.Slug
		}

		if len(names) == 0 {
			return fmt.Errorf("organization %s has no projects this token can read", org)
		}

		log.Blank()
		log.Info("Which Sentry project's errors come from %s?", displayPath(rs.Root))

		def := max(0, slices.Index(names, filepath.Base(rs.Root)))

		i, err := chooseDefault(log, "project", names, def)
		if err != nil {
			return err
		}

		slug = projects[i].Slug
	}

	i := slices.IndexFunc(projects, func(p sentryProject) bool { return p.Slug == slug })
	if i < 0 {
		return fmt.Errorf("organization %s has no project %q", org, slug)
	}

	sc := sentryConfig{URL: u, Token: token, Org: org, Projects: map[string]string{}}

	if prev.Org == org && prev.URL == u {
		for k, v := range prev.Projects {
			sc.Projects[k] = v
		}
	}

	sc.Projects[slug] = projects[i].ID
	cfg.Sentry = &sc
	cfg.Mappings = upsertMapping(cfg.Mappings, RepoMapping{Source: engrun.SourceSentry, Project: slug, Repo: rs.Root, BaseBranch: rs.Branch})

	if err := saveAutofixConfigAt(home, cfg); err != nil {
		return err
	}

	log.Blank()
	log.Success("Connected Sentry %s/%s to %s (%s).", org, slug, displayPath(rs.Root), rs.Branch)
	log.Blank()
	log.Info("Next:")
	log.Info("  csm issues          list the unresolved issues")
	log.Info("  csm fix <issue>     investigate, reproduce, fix and verify one, then wait for your approval")

	return nil
}

func sentryStatus(log *Logger, home string) error {
	cfg := loadAutofixConfig(home)

	if cfg.Sentry == nil || cfg.Sentry.Token == "" {
		log.Info("Sentry is not connected. Run csm sentry connect inside your repository.")

		return nil
	}

	log.Row("Sentry", cfg.Sentry.URL)
	log.Row("Organization", cfg.Sentry.Org)

	for slug := range cfg.Sentry.Projects {
		repo := "not mapped"

		if m, ok := cfg.mapping(engrun.SourceSentry, slug); ok {
			repo = displayPath(m.Repo)
		}

		log.Row("Project "+slug, repo)
	}

	_, err := newSentryClient(*cfg.Sentry).orgs(context.Background())
	log.Check("Auth token works", err == nil)

	return err
}

type issueRun struct {
	id      string
	status  BuildStatus
	verdict engrun.Verdict
}

// localRuns maps each Sentry issue fingerprint to the newest run CSM has for it.
func localRuns(home string) map[string]issueRun {
	out := map[string]issueRun{}
	builds := map[string]*Build{}

	for _, b := range newBuildStore(home).all() {
		builds[b.ID] = b
	}

	for _, inc := range newSignalStore(home).list() {
		for i := len(inc.Runs) - 1; i >= 0; i-- {
			if b, ok := builds[inc.Runs[i]]; ok {
				r := issueRun{id: b.ID, status: b.Status}

				if b.Verification != nil {
					r.verdict = b.Verification.Verdict
				}

				out[inc.Fingerprint] = r

				break
			}
		}
	}

	return out
}

func cmdIssues(log *Logger, home string, args []string) error {
	cfg := loadAutofixConfig(home)

	if cfg.Sentry == nil || cfg.Sentry.Token == "" {
		return errors.New("Sentry is not connected; run csm sentry connect inside your repository")
	}

	limit := 25

	if v := flagValue(args, "--limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}

	issues, err := newSentryClient(*cfg.Sentry).issues(context.Background(), limit)
	if err != nil {
		return err
	}

	if len(issues) == 0 {
		log.Info("No unresolved issues in the last 14 days.")

		return nil
	}

	runs := localRuns(home)
	now := time.Now()
	idWidth := 8

	for _, i := range issues {
		idWidth = max(idWidth, len(i.ShortID))
	}

	log.Info("%-*s  %7s  %5s  %-9s  %-22s  %s", idWidth, "ISSUE", "EVENTS", "USERS", "LAST", "CSM", "TITLE")

	for _, i := range issues {
		state := "—"

		if r, ok := runs["sentry:"+i.Project.Slug+":"+i.ID]; ok {
			state = strings.ReplaceAll(string(r.status), "_", " ")

			if r.verdict != "" && r.verdict != engrun.VerdictNotRun {
				state = verdictWord(r.verdict)
			}
		}

		log.Info("%-*s  %7s  %5d  %-9s  %-22s  %s", idWidth, i.ShortID, i.Count, i.UserCount, shortAgo(i.LastSeen, now), clip(state, 22), clip(i.Title, 70))
	}

	log.Blank()
	log.Info("Fix one with csm fix <issue>.")

	return nil
}

func shortAgo(t, now time.Time) string {
	d := now.Sub(t)

	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}

	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func verdictWord(v engrun.Verdict) string {
	switch v {
	case engrun.VerdictVerified:
		return "fixed and verified"
	case engrun.VerdictTestsPassedUnconfirmed:
		return "fixed, unconfirmed"
	case engrun.VerdictFailed:
		return "verification failed"
	case engrun.VerdictBlocked:
		return "verification blocked"
	}

	return string(v)
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}

	return s
}

func cmdFix(log *Logger, home string, args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: csm fix <issue> [--detach]   (the issue id from csm issues, like ORDERS-API-3F)")
	}

	svc, err := ensureService(log, home)
	if err != nil {
		return err
	}

	var out struct {
		Run      buildSummary `json:"run"`
		Existing bool         `json:"existing"`
	}

	if err := svc.do("POST", "/v1/sentry/fix", map[string]string{"issue": args[0]}, &out); err != nil {
		return err
	}

	if out.Existing {
		log.Info("%s already has a run in progress: %s", args[0], out.Run.ID)
	} else {
		log.Info("Started AutoFix run %s for %s in %s.", out.Run.ID, args[0], out.Run.RepoName)
	}

	if slices.Contains(args, "--detach") {
		log.Info("Follow it with csm build watch %s.", out.Run.ID)

		return nil
	}

	return watchBuild(log, svc, out.Run.ID)
}

type buildWatchView struct {
	Status       BuildStatus `json:"status"`
	Attention    string      `json:"attention"`
	CreatedAt    time.Time   `json:"created_at"`
	Verification *struct {
		Verdict engrun.Verdict `json:"verdict"`
	} `json:"verification"`
	Integration struct {
		Branch string `json:"branch"`
	} `json:"integration"`
	PR struct {
		State   string `json:"state"`
		URL     string `json:"url"`
		Blocker string `json:"blocker"`
	} `json:"pr"`
	Events []struct {
		At      time.Time `json:"at"`
		Kind    string    `json:"kind"`
		Message string    `json:"message"`
	} `json:"events"`
}

// quietEvents are bookkeeping the terminal view leaves out; csm build show and the app have them all.
var quietEvents = map[string]bool{"agent.started": true, "agent.reported": true, "agent.exited": true, "task.assigned": true, "task.scheduled": true, "hub.linked": true, "worktree.note": true, "integration.merged": true, "build.json": true}

func watchBuild(log *Logger, svc service, id string) error {
	log.Info("Following the run; Ctrl+C stops following, the run continues.")
	log.Blank()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	defer signal.Stop(stop)

	seen := 0

	for {
		var b buildWatchView

		if err := svc.do("GET", "/v1/builds/"+id, nil, &b); err != nil {
			return err
		}

		for ; seen < len(b.Events); seen++ {
			e := b.Events[seen]

			if quietEvents[e.Kind] {
				continue
			}

			off := e.At.Sub(b.CreatedAt).Round(time.Second)
			log.Info("  %02d:%02d  %s", int(off.Minutes()), int(off.Seconds())%60, e.Message)
		}

		switch b.Status {
		case BuildAwaitingFinal, BuildNeedsAttention, BuildCompleted, BuildFailed, BuildCancelled:
			log.Blank()
			reportBuild(log, id, b)

			return nil
		}

		select {
		case <-stop:
			log.Blank()
			log.Info("Stopped following. The run continues: csm build watch %s", id)

			return nil
		case <-time.After(time.Second):
		}
	}
}

func reportBuild(log *Logger, id string, b buildWatchView) {
	switch b.Status {
	case BuildAwaitingFinal:
		if b.Verification != nil && b.Verification.Verdict != engrun.VerdictNotRun {
			log.Success("%s. Ready for your approval.", strings.ToUpper(verdictWord(b.Verification.Verdict)[:1])+verdictWord(b.Verification.Verdict)[1:])
		} else {
			log.Success("Ready for your approval.")
		}

		log.Info("  Branch  %s", b.Integration.Branch)
		log.Blank()
		log.Info("  csm build show %s      the change, checks and review", id)

		if b.PR.State == "prepared" && b.PR.Blocker == "" {
			log.Info("  csm build approve %s   push the branch and open the pull request", id)
		}

		log.Info("  csm build finish %s    keep the branch local and finish", id)

	case BuildNeedsAttention:
		log.Info("Needs your attention: %s", b.Attention)
		log.Info("  csm build show %s, then csm build retry %s or open it in the CSM app", id, id)

	default:
		log.Info("Run %s: %s", id, strings.ReplaceAll(string(b.Status), "_", " "))
	}
}

// buildActions are the run actions the CLI exposes, by the word the user types.
var buildActions = map[string]string{"approve": "approve-pr", "finish": "finish", "cancel": "cancel", "pause": "pause", "resume": "resume", "retry": "retry"}

func cmdBuildAction(log *Logger, home, verb, id string) error {
	svc, err := ensureService(log, home)
	if err != nil {
		return err
	}

	if verb == "watch" {
		return watchBuild(log, svc, id)
	}

	var b buildWatchView

	if err := svc.do("POST", "/v1/builds/"+id+"/actions/"+buildActions[verb], map[string]any{}, &b); err != nil {
		return err
	}

	log.Success("%s: %s", id, strings.ReplaceAll(string(b.Status), "_", " "))

	if verb == "approve" || verb == "retry" || verb == "resume" {
		return watchBuild(log, svc, id)
	}

	return nil
}

func autofixConfigPath(home string) string { return filepath.Join(home, autofixConfigFile) }

// hubSetup is the terminal version of the app's Connect Sentry panel: everything goes through the service's endpoints.
func hubSetup(log *Logger, home string, args []string) error {
	svc, err := ensureService(log, home)
	if err != nil {
		return err
	}

	cwd, _ := os.Getwd()

	rs, err := inspectRepo(orValue(flagValue(args, "--repo"), cwd))
	if err != nil {
		return fmt.Errorf("%v; run csm hub setup inside the repository Sentry's errors come from", err)
	}

	hubURL := ask(log, "CSM Hub URL", flagValue(args, "--url"))
	email := ask(log, "Email", flagValue(args, "--email"))
	password := askSecret(log, "Password")

	var signin struct {
		Public     bool           `json:"public"`
		Workspaces []hubWorkspace `json:"workspaces"`
	}

	if err := svc.do("POST", "/v1/autofix/hub/signin", map[string]any{"url": hubURL, "email": email, "password": password}, &signin); err != nil {
		return err
	}

	if !signin.Public {
		log.Info("Note: Sentry cannot reach %s from the internet; real events need a Hub on a public HTTPS address.", hubURL)
	}

	setup := map[string]string{"project": "", "repo": rs.Root, "base_branch": rs.Branch}
	names := []string{}

	for _, w := range signin.Workspaces {
		names = append(names, w.Name)
	}

	names = append(names, "New workspace…")

	i, err := choose(log, "workspace", names)
	if err != nil {
		return err
	}

	if i == len(signin.Workspaces) {
		setup["workspace_name"] = ask(log, "Workspace name", "")
	} else {
		setup["workspace_id"] = signin.Workspaces[i].ID
	}

	setup["project"] = ask(log, "Sentry project slug", flagValue(args, "--project"))

	if repo := repoSlug(rs.Root); repo == "" {
		setup["repository"] = ask(log, "GitHub repository (owner/name)", "")
	}

	var st hubStatusView

	if err := svc.do("POST", "/v1/autofix/hub/setup", setup, &st); err != nil {
		return err
	}

	if len(st.Integrations) == 0 {
		return errors.New("the Hub did not create a Sentry integration")
	}

	integ := st.Integrations[0]

	log.Blank()
	log.Success("Workspace %s is set up and this machine is its runner.", st.Workspace.Name)
	log.Blank()
	log.Info("In Sentry: Settings → Developer Settings → Custom Integrations → Create New Integration → Internal Integration.")
	log.Info("Paste this as the Webhook URL, give Issue & Event read access, tick the issue and error webhooks, and save:")
	log.Blank()
	log.Info("  %s", integ.WebhookURL)
	log.Blank()

	secret := askSecret(log, "Then paste the integration's Client Secret")

	if secret == "" {
		log.Info("No secret given. Set it later with csm hub setup; until then the Hub refuses Sentry's webhooks.")

		return nil
	}

	if err := svc.do("PUT", "/v1/autofix/hub/secret", map[string]string{"secret": secret}, &st); err != nil {
		return err
	}

	var test struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}

	if err := svc.do("POST", "/v1/autofix/hub/test", nil, &test); err != nil {
		return err
	}

	if !test.OK {
		return errors.New(test.Message)
	}

	log.Success("%s", test.Message)
	log.Info("New Sentry errors now become AutoFix runs on this machine while csm serve is running.")

	return nil
}
