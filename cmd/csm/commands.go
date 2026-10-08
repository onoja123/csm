package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

func displayPath(path string) string {
	home, err := os.UserHomeDir()
	if err == nil && (path == home || strings.HasPrefix(path, home+"/")) {
		return "~" + strings.TrimPrefix(path, home)
	}

	return path
}

func providerName(id string) string {
	p, err := provider.New(id, "")
	if err != nil {
		return id
	}

	return p.Name()
}

func cmdSetup(log *Logger, home string) error {
	log.Info("Code Session Manager setup")
	log.Blank()
	s, err := initState(home)
	if err != nil {
		return err
	}

	log.Row("OS", runtime.GOOS)

	ready := 0
	var missing, broken []error

	for _, id := range provider.IDs {
		installed, err := setupProvider(log, s, id)

		switch {
		case err == nil:
			ready++
		case installed:
			broken = append(broken, err)
		default:
			missing = append(missing, err)
		}
	}

	log.Blank()

	if ready == 0 {
		if len(broken) > 0 {
			return broken[0]
		}

		return missing[0]
	}

	for _, err := range broken {
		log.Info("%v\n", err)
	}

	log.Info("State directory: %s", displayPath(home))

	if len(s.Accounts) == 0 {
		log.Info("\nReady. Add your first account with:\n\n    csm account add personal")
	} else {
		log.Info("\nReady. %d account(s) configured.", len(s.Accounts))
	}

	return nil
}

func setupProvider(log *Logger, s *State, id string) (installed bool, err error) {
	p, err := provider.Find(id)
	if err != nil {
		log.Row("Provider: "+providerName(id), "not installed")

		return false, err
	}

	log.Check("Provider: "+p.Name(), true)
	v, err := p.Version()
	if err != nil {
		return true, err
	}

	log.Row("  Version", v)
	features, err := p.Features()
	if err != nil {
		return true, err
	}

	isolationErr := errUnsupported(p)

	if features.CanIsolate() {
		isolationErr = probeIsolation(s, p)
	}

	log.Check("  Profile isolation", isolationErr == nil)
	log.Check("  Session resume", features.CanContinue())

	if isolationErr != nil {
		return true, fmt.Errorf("%s detected, but this installed version does not provide\nthe required profile isolation: %v\n\nUpgrade %s or run: csm doctor", p.Name(), isolationErr, p.Name())
	}

	return true, nil
}

func cmdDoctor(log *Logger, home string) error {
	log.Info("csm doctor")
	log.Blank()
	var problems []string

	fail := func(label, reason string) {
		log.Check(label, false)
		problems = append(problems, label+": "+reason)
	}

	s, err := loadState(home)

	if err != nil {
		fail("csm state directory", err.Error())
	} else {
		fi, err := os.Stat(home)

		if err == nil && fi.Mode().Perm()&0o077 != 0 {

			fail("csm state directory", fmt.Sprintf("%s is readable by other users; run: chmod 700 %s", home, home))

		} else {
			log.Check("csm state directory", true)
		}
	}

	installed := 0
	var firstMissing error

	usable := map[string]provider.Provider{}

	for _, id := range provider.IDs {
		p, err := provider.Find(id)
		if err != nil {
			if firstMissing == nil {
				firstMissing = err
			}

			label := providerName(id) + " executable"

			if s != nil && len(s.accountNames(id)) > 0 {
				fail(label, err.Error())
			} else {
				log.Row(label, "not installed")
			}

			continue
		}

		installed++

		if doctorProvider(log, s, p, fail) {
			usable[id] = p
		}
	}

	if installed == 0 {
		fail("Coding agent", firstMissing.Error())

		return reportDoctor(log, problems)
	}

	if s != nil {
		seen := map[string]string{}

		for i := range s.Accounts {
			a := &s.Accounts[i]
			label := "Account " + a.Name

			if !a.Enabled {
				log.Info("%-44s disabled", label)
				continue
			}

			p, ok := usable[a.providerID()]
			if !ok {
				fail(label, providerName(a.providerID())+" is not usable; see above")
				continue
			}

			st, err := verifyProfile(p, a)
			if err != nil {
				fail(label, err.Error())
				continue
			}

			identity := p.ID() + " " + st.Email

			if other, dup := seen[identity]; dup {
				fail(label, fmt.Sprintf("identifies as the same user as %s; profiles are not isolated", other))
				continue
			}

			seen[identity] = a.Name
			log.Check(label+" ("+st.Email+")", true)
		}

		if cwd, err := os.Getwd(); err == nil {
			doctorMCP(log, s, usable, cwd, fail)
			doctorHandoff(log, s, usable, cwd, fail)
		}
	}

	if isTerminal(os.Stdin.Fd()) {
		log.Check("Interactive terminal", true)
	} else {
		fail("Interactive terminal", "stdin is not a terminal; the agent needs one")
	}

	if _, err := exec.LookPath("git"); err != nil {
		fail("Git", "git not found; project branch detection is disabled")
	} else {
		log.Check("Git", true)
	}

	return reportDoctor(log, problems)
}

func doctorProvider(log *Logger, s *State, p provider.Provider, fail func(label, reason string)) bool {
	log.Check(p.Name()+" executable", true)
	v, err := p.Version()
	if err != nil {
		fail(p.Name()+" version", err.Error())

		return false
	}

	log.Check(p.Name()+" version ("+v+")", true)
	features, err := p.Features()
	if err != nil {
		fail(p.Name()+" CLI flags", err.Error())

		return false
	}

	if err := p.CheckEnv(); err != nil {
		fail(p.Name()+" auth environment", err.Error())
	} else {
		log.Check(p.Name()+" auth environment", true)
	}

	if !features.CanIsolate() {
		fail(p.Name()+" profile isolation", errUnsupported(p).Error())

		return false
	}

	if s != nil {
		if err := probeIsolation(s, p); err != nil {
			fail(p.Name()+" credential isolation", err.Error())
		} else {
			log.Check(p.Name()+" profile isolation", true)
			log.Check(p.Name()+" credential isolation", true)
		}
	}

	if features.CanContinue() {
		log.Check(p.Name()+" session continuation", true)
	} else {
		fail(p.Name()+" session continuation", p.Name()+" cannot resume sessions by ID; switches will start new sessions")
	}

	return true
}

func reportDoctor(log *Logger, problems []string) error {
	log.Blank()

	if len(problems) == 0 {
		log.Info("Result: ready")

		return nil
	}

	for _, p := range problems {
		log.Info("%s\n", p)
	}

	log.Info("No credentials were modified.")

	return errors.New("Result: not ready")
}

func cmdStatus(log *Logger, home string) error {
	s, err := loadState(home)
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	proj := detectProject(cwd)
	now := time.Now()

	log.Info("Code Session Manager")
	log.Blank()
	log.Info("Project")
	log.Info("  Directory: %s", displayPath(proj.Dir))

	if proj.Branch != "" {
		log.Info("  Branch:    %s", proj.Branch)
	}

	log.Blank()

	if len(s.Accounts) == 0 {
		log.Info("Accounts")
		log.Blank()
		log.Info("  none")
		log.Blank()
	}

	for _, id := range s.providersInUse() {
		log.Info("Accounts (%s)\n", providerName(id))
		printAccounts(log, s, id, now)
		log.Blank()
	}

	log.Info("Failover")
	log.Info("  %s\n", describePolicy(s.Config.policy()))

	log.Info("Current session")
	running := false

	for _, id := range provider.IDs {
		if sess, live := s.loadLiveSession(id, proj.Dir); live {
			running = true
			log.Info("  %s: running as %s for %s", providerName(id), sess.Account, now.Sub(sess.StartedAt).Round(time.Second))
		}
	}

	if !running {
		log.Info("  not running")
	}

	log.Blank()

	log.Info("Checkpoint")
	saved := false

	for _, id := range provider.IDs {
		if cp, err := s.loadCheckpoint(id, proj.Dir); err == nil {
			saved = true
			log.Info("  %s: saved %s ago (%s, %s)", providerName(id), now.Sub(cp.CheckpointedAt).Round(time.Second), cp.Account, cp.Reason)
		}
	}

	if !saved {
		log.Info("  none")
	}

	printLatestHandoff(log, s, proj.Dir, now)

	for _, id := range provider.IDs {
		t, ok, _ := s.loadTransition(id, proj.Dir)
		if !ok || processAlive(t.CSMPID) {
			continue
		}

		log.Blank()
		log.Info("Previous %s switch did not complete.", providerName(id))
		log.Info("\n  Saved state: %s → %s", t.From, t.To)

		if a, err := s.resolveAccount(t.To); err == nil {
			log.Info("  %s profile: %s", a.Name, a.status(now))
		}

		log.Info("  Project:     %s\n\nResume the handoff with:\n\n    csm %s", displayPath(t.ProjectDir), id)
	}

	return nil
}

func printAccounts(log *Logger, s *State, providerID string, now time.Time) {
	for i, name := range s.accountNames(providerID) {
		a, err := s.resolveAccount(name)
		if err != nil {
			continue
		}

		marker := "○"
		if name == s.active(providerID) {
			marker = "●"
		}

		health := string(s.accountHealth(a, now))

		if a.status(now) == statusCooldown {
			health += " until " + a.CooldownUntil.Local().Format("15:04")
		}

		log.Info("  %s %-12s %-24s %-18s priority %d", marker, name, health, s.usageLabel(name, now), i+1)
	}
}

func providerArg(s *State, args []string, usage string) (string, error) {
	switch len(args) {
	case 0:
		if inUse := s.providersInUse(); len(inUse) == 1 {
			return inUse[0], nil
		}

		return provider.ClaudeID, nil

	case 1:
		if _, err := provider.New(args[0], ""); err != nil {
			return "", err
		}

		return args[0], nil
	}

	return "", errors.New(usage)
}

func cmdCurrent(log *Logger, home string, args []string) error {
	s, err := loadState(home)
	if err != nil {
		return err
	}

	providerID, err := providerArg(s, args, "usage: csm current ["+strings.Join(provider.IDs, "|")+"]")
	if err != nil {
		return err
	}

	if s.active(providerID) == "" {
		return fmt.Errorf("no active %s account; run: csm account add <name> --provider %s", providerName(providerID), providerID)
	}

	log.Info("%s", s.active(providerID))

	return nil
}

func cmdAccounts(log *Logger, home string) error {
	s, err := loadState(home)
	if err != nil {
		return err
	}

	if len(s.Accounts) == 0 {
		log.Info("No accounts yet. Add one with:\n\n    csm account add personal")

		return nil
	}

	for i, id := range s.providersInUse() {
		if i > 0 {
			log.Blank()
		}

		log.Info("Accounts (%s)", providerName(id))
		log.Info("────────────────────────────────")
		log.Blank()
		printAccounts(log, s, id, time.Now())
		log.Info("\nOrder:\n%s", strings.Join(s.accountNames(id), " → "))
	}

	return nil
}

func cmdAccount(log *Logger, home string, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: csm account add|login|remove|enable|disable|health|priority <name>")
	}

	s, err := loadState(home)
	if err != nil {
		return err
	}

	sub, name := args[0], args[1]

	switch sub {
	case "add":
		providerID, linkSettings := provider.ClaudeID, false

		for i := 2; i < len(args); i++ {
			switch {
			case args[i] == "--link-settings":
				linkSettings = true

			case args[i] == "--provider" && i+1 < len(args):
				i++
				providerID = args[i]

			default:
				return fmt.Errorf("unknown option %q\n\nusage: csm account add <name> [--provider %s] [--link-settings]", args[i], strings.Join(provider.IDs, "|"))
			}
		}

		return accountAdd(log, s, providerID, name, linkSettings)

	case "login":
		a, err := s.resolveAccount(name)
		if err != nil {
			return err
		}

		p, err := provider.Find(a.providerID())
		if err != nil {
			return err
		}

		return accountLogin(log, s, p, a)

	case "remove":
		for _, sess := range s.liveSessions() {
			if sess.Account == name {
				return fmt.Errorf("%s is in use by a running csm session in %s; switch away from it first", name, displayPath(sess.ProjectDir))
			}
		}

		a, err := s.resolveAccount(name)
		if err != nil {
			return err
		}

		p, err := provider.New(a.providerID(), "")
		if err != nil {
			return err
		}

		dir := a.ConfigDir

		if err := s.update(func(state *State) error { return state.removeAccount(name) }); err != nil {
			return err
		}

		log.Success("Removed %s\n\nIts profile directory was kept:\n    %s\n\nTo sign it out and delete it:\n\n    %s\n    rm -rf %s", name, displayPath(dir), p.LogoutCommand(dir), dir)

		return nil

	case "health":
		return cmdAccountHealth(log, s, name)

	case "priority":
		if len(args) != 3 {
			return errors.New("usage: csm account priority <name> <position>")
		}

		position, err := strconv.Atoi(args[2])
		if err != nil {
			return fmt.Errorf("priority must be a number, got %q", args[2])
		}

		err = s.update(func(state *State) error { return state.setPriority(name, position) })
		if err != nil {
			return err
		}

		a, _ := s.resolveAccount(name)
		log.Success("%s is now priority %d for %s", name, position, providerName(a.providerID()))
		log.Info("Order: %s", strings.Join(s.accountNames(a.providerID()), " → "))

		return nil

	case "enable", "disable":
		err := s.update(func(state *State) error {
			a, err := state.resolveAccount(name)
			if err != nil {
				return err
			}

			a.Enabled = sub == "enable"

			return nil
		})
		if err != nil {
			return err
		}

		log.Success("%s %sd", name, sub)

		return nil

	default:
		return fmt.Errorf("unknown account command %q", sub)
	}
}

func accountAdd(log *Logger, s *State, providerID, name string, linkSettings bool) error {
	p, err := provider.Find(providerID)
	if err != nil {
		return err
	}

	if err := p.CheckEnv(); err != nil {
		return err
	}

	features, err := p.Features()
	if err != nil {
		return err
	}

	if !features.CanIsolate() {
		return errUnsupported(p)
	}

	a, err := s.addAccount(p.ID(), name, time.Now())
	if err != nil {
		return err
	}

	if err := os.Mkdir(a.ConfigDir, 0o700); err != nil {
		return fmt.Errorf("create profile directory: %w", err)
	}

	if linkSettings {
		userDir, linked, err := p.LinkUserConfig(a.ConfigDir)
		if err != nil {
			os.RemoveAll(a.ConfigDir)

			return fmt.Errorf("link settings: %w", err)
		}

		if len(linked) > 0 {
			log.Info("Sharing from %s: %s", displayPath(userDir), strings.Join(linked, ", "))
		}
	}

	if err := checkFreshProfileIsolated(p, a.ConfigDir); err != nil {
		os.RemoveAll(a.ConfigDir)

		return err
	}

	err = s.update(func(state *State) error {
		_, err := state.addAccount(p.ID(), name, time.Now())

		return err
	})
	if err != nil {
		os.RemoveAll(a.ConfigDir)

		return err
	}

	log.Info("Created profile %s at %s", name, displayPath(a.ConfigDir))

	return accountLogin(log, s, p, a)
}

func accountLogin(log *Logger, s *State, p provider.Provider, a *Account) error {
	log.Info("\nAuthenticate %s normally in the browser window %s opens.\n", a.Name, p.Name())

	if err := p.Login(a.ConfigDir); err != nil {
		return fmt.Errorf("%s login for %s did not complete: %w\n\nTry again with:\n\n    csm account login %s", p.Name(), a.Name, err, a.Name)
	}

	st, err := verifyProfile(p, a)
	if err != nil {
		return err
	}

	err = s.update(func(state *State) error {
		for _, other := range state.Accounts {
			if other.Name != a.Name && other.providerID() == p.ID() && other.Email != "" && other.Email == st.Email {
				return fmt.Errorf("the %s profile identifies as %s, which is already the %s account.\n\nEither the same account was used twice, or credentials are shared across\nprofiles. csm will not mark %s ready. Log in with a different account:\n\n    csm account login %s", a.Name, st.Email, other.Name, a.Name, a.Name)
			}
		}

		acct, err := state.resolveAccount(a.Name)
		if err != nil {
			return err
		}

		acct.Email = st.Email
		acct.VerifiedAt = time.Now()
		acct.CooldownUntil = time.Time{}

		return nil
	})
	if err != nil {
		return err
	}

	log.Info("\n✓ %s ready (%s)", a.Name, st.Email)

	return nil
}

func findManagedSession(s *State, providerID string) (Session, bool, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return Session{}, false, err
	}

	dir := detectProject(cwd).Dir
	var live []Session

	for _, id := range provider.IDs {
		if providerID != "" && id != providerID {
			continue
		}

		if sess, ok := s.loadLiveSession(id, dir); ok {
			live = append(live, sess)
		}
	}

	if len(live) == 0 {
		for _, sess := range s.liveSessions() {
			if providerID == "" || providerOrDefault(sess.Provider) == providerID {
				live = append(live, sess)
			}
		}
	}

	switch len(live) {
	case 0:
		return Session{}, false, nil
	case 1:
		return live[0], true, nil
	default:
		return Session{}, false, errors.New("several csm-managed sessions are running; run this command from the project directory you want to switch, or name the provider (for example: csm next codex)")
	}
}

func requestSwitch(log *Logger, s *State, sess Session, to string) error {
	path := filepath.Join(s.projectStateDir(sess.Provider, sess.ProjectDir), requestFile)

	if err := writeJSON(path, SwitchRequest{To: to, RequestedAt: time.Now()}); err != nil {
		return err
	}

	if err := syscall.Kill(sess.CSMPID, syscall.SIGUSR2); err != nil {
		os.Remove(path)

		return fmt.Errorf("signal the running csm session: %w", err)
	}

	log.Info("Switching:\n\n%s → %s\n\n✓ switch requested\n\nThe csm session in %s is checkpointing, stopping\nthe agent and restarting it as %s.", sess.Account, to, displayPath(sess.ProjectDir), to)

	return nil
}

func selectAccount(log *Logger, s *State, target *Account) error {
	p, err := provider.Find(target.providerID())
	if err != nil {
		return err
	}

	if _, err := verifyProfile(p, target); err != nil {
		return fmt.Errorf("cannot switch to %q.\n\n%w", target.Name, err)
	}

	sess, live, err := findManagedSession(s, p.ID())
	if err != nil {
		return err
	}

	if live {
		if sess.Account == target.Name {
			log.Info("The running session is already using %s.", target.Name)

			return nil
		}

		return requestSwitch(log, s, sess, target.Name)
	}

	err = s.update(func(state *State) error {
		state.setActive(p.ID(), target.Name)

		return nil
	})
	if err != nil {
		return err
	}

	log.Success("Active %s account changed to %s", p.Name(), target.Name)

	return nil
}

func cmdUse(log *Logger, home, nameOrIndex string) error {
	s, err := loadState(home)
	if err != nil {
		return err
	}

	target, err := s.resolveAccount(nameOrIndex)
	if err != nil {
		return err
	}

	switch target.status(time.Now()) {
	case statusDisabled:
		return fmt.Errorf("cannot switch to %q: it is disabled.\n\nRun:\n\n    csm account enable %s", target.Name, target.Name)
	case statusNotAuthenticated:
		return fmt.Errorf("cannot switch to %q.\n\nThe %s profile is not authenticated.\n\nRun:\n\n    csm account login %s", target.Name, target.Name, target.Name)
	}

	return selectAccount(log, s, target)
}

func cmdNext(log *Logger, home string, args []string) error {
	s, err := loadState(home)
	if err != nil {
		return err
	}

	providerID, err := providerArg(s, args, "usage: csm next ["+strings.Join(provider.IDs, "|")+"]")
	if err != nil {
		return err
	}

	wanted := ""

	if len(args) > 0 {
		wanted = providerID
	}

	sess, live, err := findManagedSession(s, wanted)
	if err != nil {
		return err
	}

	current := s.active(providerID)

	if live {
		providerID, current = providerOrDefault(sess.Provider), sess.Account
	}

	target, err := s.nextAccount(providerID, current, nil, time.Now())
	if err != nil {
		return fmt.Errorf("no other %s account is ready (%w).\n\nRun: csm accounts", providerName(providerID), err)
	}

	return selectAccount(log, s, target)
}

func cmdAuto(log *Logger, home string, args []string) error {
	s, err := loadState(home)
	if err != nil {
		return err
	}

	if len(args) != 1 {
		return errBadPolicy
	}

	if args[0] != "status" {
		err := s.update(func(state *State) error { return state.Config.setPolicy(args[0]) })
		if err != nil {
			return err
		}
	}

	log.Info("Failover: %s", describePolicy(s.Config.policy()))
	now := time.Now()

	for _, id := range s.providersInUse() {
		log.Info("Priority (%s): %s", providerName(id), strings.Join(s.accountNames(id), " → "))

		if ranked := s.rankAccounts(id, nil, now); len(ranked) > 0 {
			log.Info("Would switch to (%s): %s", providerName(id), joinNames(ranked))
		}
	}

	return nil
}

func cmdRun(log *Logger, home, providerID string, args []string) (int, error) {
	s, err := loadState(home)
	if err != nil {
		return 1, err
	}

	for {
		r, err := newRunner(log, s, providerID)
		if err != nil {
			return 1, err
		}

		code, next, err := r.run(args)
		if err != nil || next == nil {
			return code, err
		}

		providerID, args = next.providerID, next.args
	}
}

// newRunner checks that an agent is installed and usable and prepares to run it in the current directory.
func newRunner(log *Logger, s *State, providerID string) (*runner, error) {
	csmPath, err := os.Executable()
	if err != nil {
		return nil, err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	return newRunnerAt(log, s, providerID, csmPath, cwd)
}

func newRunnerAt(log *Logger, s *State, providerID, csmPath, cwd string) (*runner, error) {
	p, err := provider.Find(providerID)
	if err != nil {
		return nil, err
	}

	if len(s.accountNames(providerID)) == 0 {
		return nil, fmt.Errorf("no %s accounts configured.\n\nRun:\n\n    csm account add <name> --provider %s", p.Name(), providerID)
	}

	if err := p.CheckEnv(); err != nil {
		return nil, err
	}

	features, err := p.Features()
	if err != nil {
		return nil, err
	}

	if !features.CanIsolate() {
		return nil, errUnsupported(p)
	}

	r := &runner{
		state:        s,
		provider:     p,
		csmPath:      csmPath,
		features:     features,
		pollInterval: p.LimitPollInterval(),
		cwd:          cwd,
		project:      detectProject(cwd),
		log:          log,
		in:           bufio.NewReader(os.Stdin),
		interactive:  isTerminal(os.Stdin.Fd()),
		unavailable:  map[string]bool{},
	}
	r.stateDir = s.projectStateDir(providerID, r.project.Dir)

	if err := os.MkdirAll(r.stateDir, 0o700); err != nil {
		return nil, err
	}

	return r, nil
}
