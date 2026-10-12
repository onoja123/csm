package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/onoja123/csm/internal/handoff"
	"github.com/onoja123/csm/internal/mcp"
	"github.com/onoja123/csm/internal/provider"
)

// contextSource is the agent session a checkpoint describes: the live one in the project, else the last stopped one.
type contextSource struct {
	provider  provider.Provider
	account   *Account
	sessionID string
	live      bool
	at        time.Time
}

func (s *State) contextSource(projectDir string) (contextSource, bool) {
	for _, id := range provider.IDs {
		sess, live := s.loadLiveSession(id, projectDir)
		if !live {
			continue
		}

		if src, ok := s.sourceFor(providerOrDefault(sess.Provider), sess.Account, sess.SessionID); ok {
			src.live, src.at = true, sess.StartedAt

			return src, true
		}
	}

	if cp, found := s.latestCheckpoint(projectDir); found {
		if src, ok := s.sourceFor(providerOrDefault(cp.Provider), cp.Account, cp.SessionID); ok {
			src.at = cp.CheckpointedAt

			return src, true
		}
	}

	return contextSource{}, false
}

func (s *State) sourceFor(providerID, account, sessionID string) (contextSource, bool) {
	acct, err := s.resolveAccount(account)
	if err != nil {
		return contextSource{}, false
	}

	p, err := provider.Find(providerID)
	if err != nil {
		if p, err = provider.New(providerID, ""); err != nil {
			return contextSource{}, false
		}
	}

	return contextSource{provider: p, account: acct, sessionID: sessionID}, true
}

// extractContext reads the agent's own session record. It never touches the running agent.
func extractContext(source provider.Provider, from *Account, cwd, sessionID string) (handoff.Context, bool) {
	if sessionID == "" {
		return handoff.Context{Note: "No session was recorded for the previous agent."}, false
	}

	extractor, ok := provider.Extractor(source)
	if !ok {
		return handoff.Context{Note: source.Name() + " session records cannot be read, so only the repository state is known."}, false
	}

	ctx, err := extractor.SessionContext(from.ConfigDir, cwd, sessionID)
	if err != nil {
		slog.Debug("extract session context", "account", from.Name, "err", err)

		return handoff.Context{Note: "The previous agent's session could not be read, so only the repository state is known."}, false
	}

	return handoff.Clean(ctx), !ctx.Empty()
}

// sourceMCP records which MCP servers the source account has here, with the same verdicts `csm mcp` shows; values are never read.
func sourceMCP(source provider.Provider, from *Account, cwd string) []mcp.Result {
	adapter, ok := provider.MCPAdapter(source)
	if !ok {
		return nil
	}

	results, err := mcp.Inspect(adapter, from.ConfigDir, cwd, envPresent)
	if err != nil {
		slog.Debug("inspect mcp", "account", from.Name, "err", err)

		return nil
	}

	return results
}

// saveCheckpoint captures the task here without switching anything: session context, git state and MCP setup.
func (s *State) captureCheckpoint(cwd string, edits handoff.Edits, now time.Time) (*handoff.State, string, error) {
	proj := detectProject(cwd)
	root := s.handoffRoot(proj.Dir)
	st := &handoff.State{Version: handoff.Version, Kind: handoff.KindCheckpoint, Stage: handoff.StageSaved, CreatedAt: now, UpdatedAt: now}

	if src, ok := s.contextSource(proj.Dir); ok {
		st.Source = handoff.Agent{Provider: src.provider.ID(), Account: src.account.Name, SessionID: src.sessionID}
		st.Context, _ = extractContext(src.provider, src.account, cwd, src.sessionID)
		st.MCP = sourceMCP(src.provider, src.account, cwd)
	} else {
		st.Context.Note = "No agent session is recorded for this project, so only the repository state is known."
	}

	st.ID = handoff.NewCheckpointID(now, st.Source)

	if prev, _, err := handoff.LatestCheckpoint(root, ""); err == nil {
		st.Previous = prev.ID
	}

	var diff []byte

	st.Git, diff = handoff.CaptureGit(cwd)

	if !edits.Empty() {
		edits = handoff.CleanEdits(edits)
		edits.EditedAt = now
		st.Edits = &edits
	}

	st.SetCompleteness()
	dir := filepath.Join(root, st.ID)

	if err := handoff.Write(dir, st, diff); err != nil {
		return nil, "", fmt.Errorf("save checkpoint: %w", err)
	}

	if _, err := handoff.Load(dir); err != nil {
		return nil, "", fmt.Errorf("the checkpoint was written but cannot be read back: %w", err)
	}

	return st, dir, nil
}

// findHandoff resolves an ID to its directory, searching every project; IDs are validated so they cannot name a path.
func (s *State) findHandoff(id string) (handoff.State, string, error) {
	if !handoff.ValidID(id) {
		return handoff.State{}, "", fmt.Errorf("%q is not a checkpoint ID", id)
	}

	matches, _ := filepath.Glob(filepath.Join(s.Home, handoffsDir, "*", id))

	for _, dir := range matches {
		if st, err := handoff.Load(dir); err == nil {
			return st, dir, nil
		} else {
			return handoff.State{}, "", err
		}
	}

	return handoff.State{}, "", fmt.Errorf("no checkpoint %s; list them with: csm checkpoint list", id)
}

// editCheckpoint rewrites the user's part of a checkpoint and its summary; the extracted context is left as it was.
func editCheckpoint(dir string, st *handoff.State, edits handoff.Edits, now time.Time) error {
	if st.Kind != handoff.KindCheckpoint {
		return errors.New("only saved checkpoints can be edited; a handoff is a record of what was sent")
	}

	merged := handoff.Edits{}

	if st.Edits != nil {
		merged = *st.Edits
	}

	if edits.Task != "" {
		merged.Task = edits.Task
	}

	if edits.NextSteps != nil {
		merged.NextSteps = edits.NextSteps
	}

	if edits.Notes != "" {
		merged.Notes = edits.Notes
	}

	merged = handoff.CleanEdits(merged)
	merged.EditedAt = now
	st.Edits = &merged
	st.UpdatedAt = now
	st.SetCompleteness()

	diff, _ := os.ReadFile(filepath.Join(dir, handoff.FileDiff))

	return handoff.Write(dir, st, diff)
}

type checkpointFlags struct {
	edits    handoff.Edits
	args     []string
	nextSeen bool
}

func parseCheckpointFlags(args []string) (checkpointFlags, error) {
	var f checkpointFlags

	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}

			i++

			return args[i], nil
		}

		switch a {
		case "--task":
			v, err := value()
			if err != nil {
				return f, err
			}

			f.edits.Task = v
		case "--next":
			v, err := value()
			if err != nil {
				return f, err
			}

			f.edits.NextSteps = append(f.edits.NextSteps, v)
			f.nextSeen = true
		case "--notes":
			v, err := value()
			if err != nil {
				return f, err
			}

			f.edits.Notes = v
		default:
			if strings.HasPrefix(a, "-") {
				return f, fmt.Errorf("unknown flag %s", a)
			}

			f.args = append(f.args, a)
		}
	}

	return f, nil
}

func checkpointUsage() error {
	return errors.New("usage: csm checkpoint [--task <text>] [--next <step>]... [--notes <text>]\n       csm checkpoint list\n       csm checkpoint show <id>\n       csm checkpoint edit <id> [--task <text>] [--next <step>]... [--notes <text>]")
}

func cmdCheckpoint(log *Logger, home string, args []string) error {
	s, err := loadState(home)
	if err != nil {
		return err
	}

	sub := ""

	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}

	f, err := parseCheckpointFlags(args)
	if err != nil {
		return err
	}

	switch sub {
	case "":
		if len(f.args) > 0 {
			return checkpointUsage()
		}

		cwd, err := os.Getwd()
		if err != nil {
			return err
		}

		st, dir, err := s.captureCheckpoint(cwd, f.edits, time.Now())
		if err != nil {
			return err
		}

		log.Info("CSM checkpoint")
		log.Blank()
		printCheckpoint(log, *st)
		log.Blank()
		log.Info("Saved: %s", displayPath(filepath.Join(dir, handoff.FileMD)))
		log.Info("Continue it with another agent: csm handoff --checkpoint %s <account>", st.ID)

		return nil

	case "list", "ls":
		cwd, _ := os.Getwd()

		return listCheckpoints(log, s, detectProject(cwd).Dir)

	case "show":
		if len(f.args) != 1 {
			return checkpointUsage()
		}

		st, dir, err := s.findHandoff(f.args[0])
		if err != nil {
			return err
		}

		md, err := os.ReadFile(filepath.Join(dir, handoff.FileMD))
		if err != nil {
			return err
		}

		log.Info("%s", strings.TrimRight(string(md), "\n"))

		if st.Kind == handoff.KindCheckpoint && st.Git.Root != "" {
			cwd, _ := os.Getwd()
			v := handoff.Verify(st, cwd, time.Now())
			log.Blank()

			if problems := v.Problems(); len(problems) > 0 {
				log.Info("Since this checkpoint:")

				for _, p := range problems {
					log.Info("  - %s", p)
				}
			} else {
				log.Info("The repository matches this checkpoint.")
			}
		}

		return nil

	case "edit":
		if len(f.args) != 1 || f.edits.Empty() && !f.nextSeen {
			return checkpointUsage()
		}

		st, dir, err := s.findHandoff(f.args[0])
		if err != nil {
			return err
		}

		if err := editCheckpoint(dir, &st, f.edits, time.Now()); err != nil {
			return err
		}

		log.Info("Updated %s", st.ID)
		printCheckpoint(log, st)

		return nil
	}

	return checkpointUsage()
}

func printCheckpoint(log *Logger, st handoff.State) {
	task, origin := st.Task()

	if task == "" {
		task = "not recorded"
	} else if origin == "user" {
		task += " (yours)"
	}

	log.Row("ID", st.ID)
	log.Row("Agent", map[bool]string{true: st.Source.String(), false: "none recorded"}[st.Source.Provider != ""])

	if st.Source.SessionID != "" {
		log.Row("Session", st.Source.SessionID)
	}

	log.Row("Task", oneLineText(task, 80))

	if st.Git.Root != "" {
		log.Row("Repository", displayPath(st.Git.Root))
		log.Row("Branch", map[bool]string{true: st.Git.Branch, false: "(detached HEAD)"}[st.Git.Branch != ""])
		log.Row("Working tree", fmt.Sprintf("%d staged, %d unstaged, %d untracked", st.Git.Staged, st.Git.Unstaged, st.Git.Untracked))
	} else {
		log.Row("Repository", "not a git repository")
	}

	touched := map[string]bool{}

	for _, f := range append(append([]string{}, st.Context.FilesModified...), st.Context.FilesCreated...) {
		touched[f] = true
	}

	log.Row("Files touched", fmt.Sprintf("%d (from the session record)", len(touched)))
	log.Row("Next steps", fmt.Sprintf("%d", len(st.Steps())))

	if len(st.MCP) > 0 {
		log.Row("MCP servers", fmt.Sprintf("%d", len(st.MCP)))
	}

	log.Row("Complete", map[bool]string{true: "yes", false: "partly: " + missingParts(st)}[st.Completeness == handoff.CompletenessComplete])
}

func missingParts(st handoff.State) string {
	var out []string

	if task, _ := st.Task(); task == "" {
		out = append(out, "no task recorded (add one with --task)")
	}

	if st.Git.Root == "" {
		out = append(out, "not a git repository")
	}

	return strings.Join(out, ", ")
}

func oneLineText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")

	if len(s) > n {
		return s[:n-1] + "…"
	}

	return s
}

func listCheckpoints(log *Logger, s *State, projectDir string) error {
	root := s.handoffRoot(projectDir)
	entries, _ := os.ReadDir(root)
	var all []handoff.State

	for _, e := range entries {
		if !e.IsDir() || !handoff.ValidID(e.Name()) {
			continue
		}

		if st, err := handoff.Load(filepath.Join(root, e.Name())); err == nil {
			all = append(all, st)
		} else {
			log.Info("  skipped %s: %v", e.Name(), err)
		}
	}

	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })

	if len(all) == 0 {
		log.Info("No checkpoints or handoffs for %s yet. Save one with: csm checkpoint", displayPath(projectDir))

		return nil
	}

	log.Info("Checkpoints and handoffs for %s", displayPath(projectDir))
	log.Blank()

	for _, st := range all {
		task, _ := st.Task()

		if task == "" {
			task = "repository state only"
		}

		what := "checkpoint"

		if st.Kind != handoff.KindCheckpoint {
			what = "handoff → " + st.Target.String()

			if st.Outcome != nil && st.Outcome.Status != "" {
				what += " (" + st.Outcome.Status + ")"
			}
		}

		log.Info("  %s  %s  %s", st.ID, what, oneLineText(task, 60))
	}

	return nil
}

// finishHandoff records what happened when the receiving agent was launched; it runs once the process exists or failed to.
func finishHandoff(dir, projectDir string, started bool, startErr error, now time.Time) {
	if dir == "" {
		return
	}

	st, err := handoff.Load(dir)
	if err != nil {
		slog.Debug("finish handoff", "err", err)

		return
	}

	if st.Outcome == nil {
		st.Outcome = &handoff.Outcome{Delivery: handoff.DeliveryNone}
	}

	st.Outcome.Started = started
	st.Outcome.ProjectDir = projectDir

	if started {
		st.Stage = handoff.StageTargetStarted
		st.Outcome.StartedAt = now

		if st.Git.Root != "" && detectProject(projectDir).Dir != detectProject(st.Git.Root).Dir {
			st.Outcome.Problems = append(st.Outcome.Problems, "The receiving agent started in "+displayPath(projectDir)+", not in the checkpoint's repository.")
		}
	} else {
		st.Stage = handoff.StageFailed

		if startErr != nil {
			st.Error = startErr.Error()
		}
	}

	st.Outcome.Settle()

	if err := handoff.Save(dir, &st); err != nil {
		slog.Debug("save handoff outcome", "err", err)
	}
}

// recordTargetSession links a handoff to the session the receiving agent ran, so the history can be followed from either side.
func recordTargetSession(dir, sessionID string) {
	if dir == "" || sessionID == "" {
		return
	}

	st, err := handoff.Load(dir)
	if err != nil || st.Target.SessionID != "" {
		return
	}

	st.Target.SessionID = sessionID

	if err := handoff.Save(dir, &st); err != nil {
		slog.Debug("save handoff target session", "err", err)
	}
}
