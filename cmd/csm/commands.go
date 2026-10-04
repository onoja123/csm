package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

func stdinIsTerminal() bool {
	return isTerminal(os.Stdin.Fd())
}

func check(label string, ok bool) {
	mark := "✓"
	if !ok {
		mark = "✗"
	}
	fmt.Printf("%-44s %s\n", label, mark)
}

func providerName(id string) string {
	p, err := provider.New(id, "")
	if err != nil {
		return id
	}
	return p.Name()
}

func cmdSetup(home string) error {
	fmt.Println("Code Session Manager setup")
	fmt.Println()
	s, err := initState(home)
	if err != nil {
		return err
	}
	fmt.Printf("%-44s %s\n", "OS", runtime.GOOS)

	ready := 0
	var missing, broken []error
	for _, id := range provider.IDs {
		installed, err := setupProvider(s, id)
		switch {
		case err == nil:
			ready++
		case installed:
			broken = append(broken, err)
		default:
			missing = append(missing, err)
		}
	}
	fmt.Println()
	if ready == 0 {
		return slices.Concat(broken, missing)[0]
	}
	for _, err := range broken {
		fmt.Printf("%v\n\n", err)
	}
	fmt.Printf("State directory: %s\n", displayPath(home))
	if len(s.Accounts) == 0 {
		fmt.Println("\nReady. Add your first account with:\n\n    csm account add personal")
	} else {
		fmt.Printf("\nReady. %d account(s) configured.\n", len(s.Accounts))
	}
	return nil
}

func setupProvider(s *State, id string) (installed bool, err error) {
	p, err := provider.Find(id)
	if err != nil {
		fmt.Printf("%-44s %s\n", "Provider: "+providerName(id), "not installed")
		return false, err
	}
	check("Provider: "+p.Name(), true)
	v, err := p.Version()
	if err != nil {
		return true, err
	}
	fmt.Printf("%-44s %s\n", "  Version", v)
	features, err := p.Features()
	if err != nil {
		return true, err
	}
	isolationErr := errUnsupported(p)
	if features.CanIsolate() {
		isolationErr = probeIsolation(s, p)
	}
	check("  Profile isolation", isolationErr == nil)
	check("  Session resume", features.CanContinue())
	if isolationErr != nil {
		return true, fmt.Errorf("%s detected, but this installed version does not provide\nthe required profile isolation: %v\n\nUpgrade %s or run: csm doctor", p.Name(), isolationErr, p.Name())
	}
	return true, nil
}

func cmdDoctor(home string) error {
	fmt.Println("csm doctor")
	fmt.Println()
	var problems []string
	fail := func(label, reason string) {
		check(label, false)
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
			check("csm state directory", true)
		}
	}

	installed := 0
	usable := map[string]provider.Provider{}
	for _, id := range provider.IDs {
		p, err := provider.Find(id)
		if err != nil {
			label := providerName(id) + " executable"
			if s != nil && len(s.accountNames(id)) > 0 {
				fail(label, err.Error())
			} else {
				fmt.Printf("%-44s %s\n", label, "not installed")
			}
			continue
		}
		installed++
		if doctorProvider(s, p, fail) {
			usable[id] = p
		}
	}
	if installed == 0 {
		_, err := provider.Find(provider.ClaudeID)
		fail("Coding agent", err.Error())
		return reportDoctor(problems)
	}

	if s != nil {
		seen := map[string]string{}
		for i := range s.Accounts {
			a := &s.Accounts[i]
			label := "Account " + a.Name
			if !a.Enabled {
				fmt.Printf("%-44s disabled\n", label)
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
			check(label+" ("+st.Email+")", true)
		}
	}

	if stdinIsTerminal() {
		check("Interactive terminal", true)
	} else {
		fail("Interactive terminal", "stdin is not a terminal; the agent needs one")
	}
	if _, err := exec.LookPath("git"); err != nil {
		fail("Git", "git not found; project branch detection is disabled")
	} else {
		check("Git", true)
	}
	return reportDoctor(problems)
}

// doctorProvider reports whether accounts of this provider can be checked.
func doctorProvider(s *State, p provider.Provider, fail func(label, reason string)) bool {
	check(p.Name()+" executable", true)
	v, err := p.Version()
	if err != nil {
		fail(p.Name()+" version", err.Error())
		return false
	}
	check(p.Name()+" version ("+v+")", true)
	features, err := p.Features()
	if err != nil {
		fail(p.Name()+" CLI flags", err.Error())
		return false
	}

	if err := p.CheckEnv(); err != nil {
		fail(p.Name()+" auth environment", err.Error())
	} else {
		check(p.Name()+" auth environment", true)
	}

	if !features.CanIsolate() {
		fail(p.Name()+" profile isolation", errUnsupported(p).Error())
		return false
	}
	if s != nil {
		if err := probeIsolation(s, p); err != nil {
			fail(p.Name()+" credential isolation", err.Error())
		} else {
			check(p.Name()+" profile isolation", true)
			check(p.Name()+" credential isolation", true)
		}
	}
	if features.CanContinue() {
		check(p.Name()+" session continuation", true)
	} else {
		fail(p.Name()+" session continuation", p.Name()+" cannot resume sessions by ID; switches will start new sessions")
	}
	return true
}

func reportDoctor(problems []string) error {
	fmt.Println()
	if len(problems) == 0 {
		fmt.Println("Result: ready")
		return nil
	}
	for _, p := range problems {
		fmt.Printf("%s\n\n", p)
	}
	fmt.Println("No credentials were modified.")
	return errors.New("Result: not ready")
}

func cmdStatus(home string) error {
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

	fmt.Println("Code Session Manager")
	fmt.Println()
	fmt.Println("Project")
	fmt.Printf("  Directory: %s\n", displayPath(proj.Dir))
	if proj.Branch != "" {
		fmt.Printf("  Branch:    %s\n", proj.Branch)
	}
	fmt.Println()

	if len(s.Accounts) == 0 {
		fmt.Println("Accounts")
		fmt.Println()
		fmt.Println("  none")
		fmt.Println()
	}
	for _, id := range s.providersInUse() {
		fmt.Printf("Accounts (%s)\n\n", providerName(id))
		printAccounts(s, id, now)
		fmt.Println()
	}
	fmt.Println("Auto failover")
	fmt.Printf("  %s\n\n", onOff(s.Config.AutoFailover, "enabled", "disabled"))

	fmt.Println("Current session")
	running := false
	for _, id := range provider.IDs {
		if sess, live := s.loadLiveSession(id, proj.Dir); live {
			running = true
			fmt.Printf("  %s: running as %s for %s\n", providerName(id), sess.Account, now.Sub(sess.StartedAt).Round(time.Second))
		}
	}
	if !running {
		fmt.Println("  not running")
	}
	fmt.Println()

	fmt.Println("Checkpoint")
	saved := false
	for _, id := range provider.IDs {
		if cp, err := s.loadCheckpoint(id, proj.Dir); err == nil {
			saved = true
			fmt.Printf("  %s: saved %s ago (%s, %s)\n", providerName(id), now.Sub(cp.CheckpointedAt).Round(time.Second), cp.Account, cp.Reason)
		}
	}
	if !saved {
		fmt.Println("  none")
	}

	for _, id := range provider.IDs {
		t, ok, _ := s.loadTransition(id, proj.Dir)
		if !ok || processAlive(t.CSMPID) {
			continue
		}
		fmt.Println()
		fmt.Printf("Previous %s switch did not complete.\n", providerName(id))
		fmt.Printf("\n  Saved state: %s → %s\n", t.From, t.To)
		if a, err := s.resolveAccount(t.To); err == nil {
			fmt.Printf("  %s profile: %s\n", a.Name, a.status(now))
		}
		fmt.Printf("  Project:     %s\n\nResume the handoff with:\n\n    csm %s\n", displayPath(t.ProjectDir), id)
	}
	return nil
}

func onOff(b bool, on, off string) string {
	if b {
		return on
	}
	return off
}

func printAccounts(s *State, providerID string, now time.Time) {
	for _, name := range s.accountNames(providerID) {
		a, err := s.resolveAccount(name)
		if err != nil {
			continue
		}
		marker, active := "○", ""
		if name == s.active(providerID) {
			marker, active = "●", "active, "
		}
		detail := string(a.status(now))
		if a.status(now) == statusCooldown {
			detail += " until " + a.CooldownUntil.Local().Format("15:04")
		}
		if rec, ok := s.loadUsage(name); ok {
			if summary := shortUsage(rec.Usage, now); summary != "" {
				detail += "  (" + summary + ")"
			}
		}
		fmt.Printf("  %s %-12s %s%s\n", marker, name, active, detail)
	}
}

// providerArg reads an optional provider name; with none it is the only provider in use, or Claude Code.
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

func cmdCurrent(home string, args []string) error {
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
	fmt.Println(s.active(providerID))
	return nil
}

func cmdAccounts(home string) error {
	s, err := loadState(home)
	if err != nil {
		return err
	}
	if len(s.Accounts) == 0 {
		fmt.Println("No accounts yet. Add one with:\n\n    csm account add personal")
		return nil
	}
	for i, id := range s.providersInUse() {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("Accounts (%s)\n", providerName(id))
		fmt.Println("────────────────────────────────")
		fmt.Println()
		printAccounts(s, id, time.Now())
		fmt.Printf("\nOrder:\n%s\n", strings.Join(s.accountNames(id), " → "))
	}
	return nil
}

func cmdAccount(home string, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: csm account add|login|remove|enable|disable <name>")
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
		return accountAdd(s, providerID, name, linkSettings)
	case "login":
		a, err := s.resolveAccount(name)
		if err != nil {
			return err
		}
		p, err := provider.Find(a.providerID())
		if err != nil {
			return err
		}
		return accountLogin(s, p, a)
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
		if err := s.removeAccount(name); err != nil {
			return err
		}
		if err := s.save(); err != nil {
			return err
		}
		fmt.Printf("✓ Removed %s\n\nIts profile directory was kept:\n    %s\n\nTo sign it out and delete it:\n\n    %s\n    rm -rf %s\n", name, displayPath(dir), p.LogoutCommand(dir), dir)
		return nil
	case "enable", "disable":
		a, err := s.resolveAccount(name)
		if err != nil {
			return err
		}
		a.Enabled = sub == "enable"
		if err := s.save(); err != nil {
			return err
		}
		fmt.Printf("✓ %s %sd\n", name, sub)
		return nil
	default:
		return fmt.Errorf("unknown account command %q", sub)
	}
}

func accountAdd(s *State, providerID, name string, linkSettings bool) error {
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
	// Linked first: the isolation check must see the settings the profile will really run with.
	if linkSettings {
		userDir, linked, err := p.LinkUserConfig(a.ConfigDir)
		if err != nil {
			os.RemoveAll(a.ConfigDir)
			return fmt.Errorf("link settings: %w", err)
		}
		if len(linked) > 0 {
			fmt.Printf("Sharing from %s: %s\n", displayPath(userDir), strings.Join(linked, ", "))
		}
	}
	if err := checkFreshProfileIsolated(p, a.ConfigDir); err != nil {
		os.RemoveAll(a.ConfigDir)
		return err
	}
	if err := s.save(); err != nil {
		return err
	}
	fmt.Printf("Created profile %s at %s\n", name, displayPath(a.ConfigDir))
	return accountLogin(s, p, a)
}

func accountLogin(s *State, p provider.Provider, a *Account) error {
	fmt.Printf("\nAuthenticate %s normally in the browser window %s opens.\n\n", a.Name, p.Name())
	if err := p.Login(a.ConfigDir); err != nil {
		return fmt.Errorf("%s login for %s did not complete: %w\n\nTry again with:\n\n    csm account login %s", p.Name(), a.Name, err, a.Name)
	}

	st, err := verifyProfile(p, a)
	if err != nil {
		return err
	}
	for _, other := range s.Accounts {
		if other.Name != a.Name && other.providerID() == p.ID() && other.Email != "" && other.Email == st.Email {
			return fmt.Errorf("the %s profile identifies as %s, which is already the %s account.\n\nEither the same account was used twice, or credentials are shared across\nprofiles. csm will not mark %s ready. Log in with a different account:\n\n    csm account login %s", a.Name, st.Email, other.Name, a.Name, a.Name)
		}
	}
	a.Email = st.Email
	a.VerifiedAt = time.Now()
	a.CooldownUntil = time.Time{}
	if err := s.save(); err != nil {
		return err
	}
	fmt.Printf("\n✓ %s ready (%s)\n", a.Name, st.Email)
	return nil
}

// Falls back to the only running session anywhere so `csm next` works outside the project; an empty providerID means any.
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

func requestSwitch(s *State, sess Session, to string) error {
	path := filepath.Join(s.projectStateDir(sess.Provider, sess.ProjectDir), requestFile)
	if err := writeJSON(path, SwitchRequest{To: to, RequestedAt: time.Now()}); err != nil {
		return err
	}
	if err := syscall.Kill(sess.CSMPID, syscall.SIGUSR2); err != nil {
		os.Remove(path)
		return fmt.Errorf("signal the running csm session: %w", err)
	}
	fmt.Printf("Switching:\n\n%s → %s\n\n✓ switch requested\n\nThe csm session in %s is checkpointing, stopping\nthe agent and restarting it as %s.\n", sess.Account, to, displayPath(sess.ProjectDir), to)
	return nil
}

func selectAccount(s *State, target *Account) error {
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
			fmt.Printf("The running session is already using %s.\n", target.Name)
			return nil
		}
		return requestSwitch(s, sess, target.Name)
	}
	s.setActive(p.ID(), target.Name)
	if err := s.save(); err != nil {
		return err
	}
	fmt.Printf("✓ Active %s account changed to %s\n", p.Name(), target.Name)
	return nil
}

func cmdUse(home, nameOrIndex string) error {
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
	return selectAccount(s, target)
}

func cmdNext(home string, args []string) error {
	s, err := loadState(home)
	if err != nil {
		return err
	}
	providerID, err := providerArg(s, args, "usage: csm next ["+strings.Join(provider.IDs, "|")+"]")
	if err != nil {
		return err
	}
	// With no provider named, a running session decides which provider is meant.
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
	return selectAccount(s, target)
}

func cmdAuto(home string, args []string) error {
	s, err := loadState(home)
	if err != nil {
		return err
	}
	if len(args) != 1 {
		return errors.New("usage: csm auto on|off|status")
	}
	switch args[0] {
	case "on", "off":
		s.Config.AutoFailover = args[0] == "on"
		if err := s.save(); err != nil {
			return err
		}
	case "status":
	default:
		return errors.New("usage: csm auto on|off|status")
	}
	fmt.Printf("Automatic failover: %s\n", onOff(s.Config.AutoFailover, "ON", "OFF"))
	for _, id := range s.providersInUse() {
		fmt.Printf("Order (%s): %s\n", providerName(id), strings.Join(s.accountNames(id), " → "))
	}
	return nil
}

func cmdRun(home, providerID string, args []string) (int, error) {
	s, err := loadState(home)
	if err != nil {
		return 1, err
	}
	p, err := provider.Find(providerID)
	if err != nil {
		return 1, err
	}
	if len(s.accountNames(providerID)) == 0 {
		return 1, fmt.Errorf("no %s accounts configured.\n\nRun:\n\n    csm account add <name> --provider %s", p.Name(), providerID)
	}
	if err := p.CheckEnv(); err != nil {
		return 1, err
	}
	features, err := p.Features()
	if err != nil {
		return 1, err
	}
	if !features.CanIsolate() {
		return 1, errUnsupported(p)
	}
	csmPath, err := os.Executable()
	if err != nil {
		return 1, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 1, err
	}
	r := &runner{
		state:        s,
		provider:     p,
		csmPath:      csmPath,
		features:     features,
		pollInterval: p.LimitPollInterval(),
		cwd:          cwd,
		project:      detectProject(cwd),
		out:          os.Stdout,
		in:           bufio.NewReader(os.Stdin),
		interactive:  stdinIsTerminal(),
		unavailable:  map[string]bool{},
	}
	r.stateDir = s.projectStateDir(providerID, r.project.Dir)
	if err := os.MkdirAll(r.stateDir, 0o700); err != nil {
		return 1, err
	}
	return r.run(args)
}
