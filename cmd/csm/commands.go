package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func cmdSetup(home string) error {
	fmt.Println("Code Session Manager setup")
	fmt.Println()
	s, err := initState(home)
	if err != nil {
		return err
	}
	p, err := provider.FindClaude()
	if err != nil {
		check("Provider: Claude Code", false)
		return err
	}
	check("Provider: "+p.Name(), true)
	v, err := p.Version()
	if err != nil {
		return err
	}
	fmt.Printf("%-44s %s\n", "Version", v)
	fmt.Printf("%-44s %s\n", "OS", runtime.GOOS)
	features, err := p.Features()
	if err != nil {
		return err
	}

	isolationErr := errUnsupported(p)
	if features.CanIsolate() {
		isolationErr = probeIsolation(s, p)
	}
	check("Profile isolation", isolationErr == nil)
	check("Session resume", features.CanContinue())
	fmt.Println()
	if isolationErr != nil {
		return fmt.Errorf("%s detected, but this installed version does not provide\nthe required profile isolation: %v\n\nUpgrade %s or run: csm doctor", p.Name(), isolationErr, p.Name())
	}
	fmt.Printf("State directory: %s\n", displayPath(home))
	if len(s.Accounts) == 0 {
		fmt.Println("\nReady. Add your first account with:\n\n    csm account add personal")
	} else {
		fmt.Printf("\nReady. %d account(s) configured.\n", len(s.Accounts))
	}
	return nil
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

	p, err := provider.FindClaude()
	if err != nil {
		fail("Claude Code executable", err.Error())
		return reportDoctor(problems)
	}
	check(p.Name()+" executable", true)
	v, err := p.Version()
	if err != nil {
		fail(p.Name()+" version", err.Error())
		return reportDoctor(problems)
	}
	check(p.Name()+" version ("+v+")", true)
	features, err := p.Features()
	if err != nil {
		fail(p.Name()+" CLI flags", err.Error())
		return reportDoctor(problems)
	}

	if err := p.CheckEnv(); err != nil {
		fail("Auth environment", err.Error())
	} else {
		check("Auth environment", true)
	}

	if !features.CanIsolate() {
		fail("Profile isolation", errUnsupported(p).Error())
	} else if s != nil {
		if err := probeIsolation(s, p); err != nil {
			fail("Secure credential isolation", err.Error())
		} else {
			check("Profile isolation", true)
			check("Secure credential isolation", true)
		}
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
			st, err := verifyProfile(p, a)
			if err != nil {
				fail(label, err.Error())
				continue
			}
			if other, dup := seen[st.Email]; dup {
				fail(label, fmt.Sprintf("identifies as the same user as %s; profiles are not isolated", other))
				continue
			}
			seen[st.Email] = a.Name
			check(label+" ("+st.Email+")", true)
		}
	}

	if stdinIsTerminal() {
		check("Interactive terminal", true)
	} else {
		fail("Interactive terminal", "stdin is not a terminal; the agent needs one")
	}
	if features.CanContinue() {
		check("Session continuation", true)
	} else {
		fail("Session continuation", p.Name()+" cannot resume sessions by ID; switches will start new sessions")
	}
	if _, err := exec.LookPath("git"); err != nil {
		fail("Git", "git not found; project branch detection is disabled")
	} else {
		check("Git", true)
	}
	return reportDoctor(problems)
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

	sess, live := s.loadLiveSession(proj.Dir)
	fmt.Println("Accounts")
	fmt.Println()
	printAccounts(s, now)
	fmt.Println()
	fmt.Println("Auto failover")
	fmt.Printf("  %s\n\n", onOff(s.Config.AutoFailover, "enabled", "disabled"))
	fmt.Println("Current session")
	if live {
		fmt.Printf("  running as %s for %s\n\n", sess.Account, now.Sub(sess.StartedAt).Round(time.Second))
	} else {
		fmt.Printf("  not running\n\n")
	}
	fmt.Println("Checkpoint")
	if cp, err := s.loadCheckpoint(proj.Dir); err == nil {
		fmt.Printf("  saved %s ago (%s, %s)\n", now.Sub(cp.CheckpointedAt).Round(time.Second), cp.Account, cp.Reason)
	} else {
		fmt.Println("  none")
	}

	if t, ok, _ := s.loadTransition(proj.Dir); ok && !processAlive(t.CSMPID) {
		fmt.Println()
		fmt.Println("Previous switch did not complete.")
		fmt.Printf("\n  Saved state: %s → %s\n", t.From, t.To)
		if a, err := s.resolveAccount(t.To); err == nil {
			fmt.Printf("  %s profile: %s\n", a.Name, a.status(now))
		}
		fmt.Printf("  Project:     %s\n\nResume the handoff with:\n\n    csm claude\n", displayPath(t.ProjectDir))
	}
	return nil
}

func onOff(b bool, on, off string) string {
	if b {
		return on
	}
	return off
}

func printAccounts(s *State, now time.Time) {
	for _, name := range s.Config.AccountOrder {
		a, err := s.resolveAccount(name)
		if err != nil {
			continue
		}
		marker, active := "○", ""
		if name == s.Config.ActiveAccount {
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

func cmdCurrent(home string) error {
	s, err := loadState(home)
	if err != nil {
		return err
	}
	if s.Config.ActiveAccount == "" {
		return errors.New("no active account; run: csm account add <name>")
	}
	fmt.Println(s.Config.ActiveAccount)
	return nil
}

func cmdAccounts(home string) error {
	s, err := loadState(home)
	if err != nil {
		return err
	}
	fmt.Println("Accounts (Claude Code)")
	fmt.Println("────────────────────────────────")
	fmt.Println()
	if len(s.Accounts) == 0 {
		fmt.Println("No accounts yet. Add one with:\n\n    csm account add personal")
		return nil
	}
	printAccounts(s, time.Now())
	fmt.Printf("\nOrder:\n%s\n", strings.Join(s.Config.AccountOrder, " → "))
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
		linkSettings := len(args) > 2 && args[2] == "--link-settings"
		return accountAdd(s, name, linkSettings)
	case "login":
		a, err := s.resolveAccount(name)
		if err != nil {
			return err
		}
		p, err := provider.FindClaude()
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
		dir := a.ConfigDir
		if err := s.removeAccount(name); err != nil {
			return err
		}
		if err := s.save(); err != nil {
			return err
		}
		fmt.Printf("✓ Removed %s\n\nIts profile directory was kept:\n    %s\n\nTo sign it out and delete it:\n\n    %s\n    rm -rf %s\n", name, displayPath(dir), provider.Claude{}.LogoutCommand(dir), dir)
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

func accountAdd(s *State, name string, linkSettings bool) error {
	p, err := provider.FindClaude()
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

	a, err := s.addAccount(name, time.Now())
	if err != nil {
		return err
	}
	if err := os.Mkdir(a.ConfigDir, 0o700); err != nil {
		return fmt.Errorf("create profile directory: %w", err)
	}
	if err := checkFreshProfileIsolated(p, a.ConfigDir); err != nil {
		os.RemoveAll(a.ConfigDir)
		return err
	}
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
	if err := s.save(); err != nil {
		return err
	}
	fmt.Printf("Created profile %s at %s\n", name, displayPath(a.ConfigDir))
	return accountLogin(s, p, a)
}

func accountLogin(s *State, p provider.Claude, a *Account) error {
	fmt.Printf("\nAuthenticate %s normally in the browser window %s opens.\n\n", a.Name, p.Name())
	if err := p.Login(a.ConfigDir); err != nil {
		return fmt.Errorf("%s login for %s did not complete: %w\n\nTry again with:\n\n    csm account login %s", p.Name(), a.Name, err, a.Name)
	}

	st, err := verifyProfile(p, a)
	if err != nil {
		return err
	}
	for _, other := range s.Accounts {
		if other.Name != a.Name && other.Email != "" && other.Email == st.Email {
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

// Falls back to the only running session anywhere so `csm next` works outside the project.
func findManagedSession(s *State) (Session, bool, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return Session{}, false, err
	}
	if sess, live := s.loadLiveSession(detectProject(cwd).Dir); live {
		return sess, true, nil
	}
	live := s.liveSessions()
	switch len(live) {
	case 0:
		return Session{}, false, nil
	case 1:
		return live[0], true, nil
	default:
		return Session{}, false, errors.New("several csm-managed sessions are running; run this command from the project directory you want to switch")
	}
}

func requestSwitch(s *State, sess Session, to string) error {
	path := filepath.Join(s.projectStateDir(sess.ProjectDir), requestFile)
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
	p, err := provider.FindClaude()
	if err != nil {
		return err
	}
	if _, err := verifyProfile(p, target); err != nil {
		return fmt.Errorf("cannot switch to %q.\n\n%w", target.Name, err)
	}
	sess, live, err := findManagedSession(s)
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
	s.Config.ActiveAccount = target.Name
	if err := s.save(); err != nil {
		return err
	}
	fmt.Printf("✓ Active account changed to %s\n", target.Name)
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

func cmdNext(home string) error {
	s, err := loadState(home)
	if err != nil {
		return err
	}
	current := s.Config.ActiveAccount
	if sess, live, err := findManagedSession(s); err == nil && live {
		current = sess.Account
	}
	target, err := s.nextAccount(current, nil, time.Now())
	if err != nil {
		return fmt.Errorf("no other account is ready (%w).\n\nRun: csm accounts", err)
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
	fmt.Printf("Order: %s\n", strings.Join(s.Config.AccountOrder, " → "))
	return nil
}

func cmdClaude(home string, args []string) (int, error) {
	s, err := loadState(home)
	if err != nil {
		return 1, err
	}
	if len(s.Accounts) == 0 {
		return 1, errors.New("no accounts configured.\n\nRun:\n\n    csm account add personal")
	}
	p, err := provider.FindClaude()
	if err != nil {
		return 1, err
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
		state:       s,
		provider:    p,
		csmPath:     csmPath,
		features:    features,
		cwd:         cwd,
		project:     detectProject(cwd),
		out:         os.Stdout,
		in:          bufio.NewReader(os.Stdin),
		interactive: stdinIsTerminal(),
		unavailable: map[string]bool{},
	}
	r.stateDir = s.projectStateDir(r.project.Dir)
	if err := os.MkdirAll(r.stateDir, 0o700); err != nil {
		return 1, err
	}
	return r.run(args)
}
