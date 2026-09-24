package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/onoja123/csm/internal/provider"
)

func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

var errNotLoggedIn = errors.New("not logged in")

// verifyProfile fails unless the provider reports this profile's directory, a login, and the recorded identity.
func verifyProfile(p provider.Claude, a *Account) (provider.Identity, error) {
	id, err := p.Identity(a.ConfigDir)
	if err != nil {
		return id, err
	}
	if !id.LoggedIn {
		return id, fmt.Errorf("the %s profile is %w.\n\nRun:\n\n    csm account login %s", a.Name, errNotLoggedIn, a.Name)
	}
	if !samePath(id.ProfileDir, a.ConfigDir) {
		return id, fmt.Errorf("%s reported profile directory %q for %s, expected %q.\n\nThis %s version does not isolate profiles as csm requires. Run: csm doctor", p.Name(), id.ProfileDir, a.Name, a.ConfigDir, p.Name())
	}
	if a.Email != "" && id.Email != a.Email {
		return id, fmt.Errorf("the %s profile now identifies as %s, but it was set up as %s.\n\nNo credentials were modified. Re-authenticate it with:\n\n    csm account login %s", a.Name, id.Email, a.Email, a.Name)
	}
	return id, nil
}

// A login visible in an empty profile means credentials leak in from shared storage.
func checkFreshProfileIsolated(p provider.Claude, profileDir string) error {
	id, err := p.Identity(profileDir)
	if err != nil {
		return err
	}
	if !samePath(id.ProfileDir, profileDir) {
		return fmt.Errorf("%s ignored the profile directory (reported %q)", p.Name(), id.ProfileDir)
	}
	if id.LoggedIn {
		return fmt.Errorf("a brand-new profile directory already reports a logged-in account.\n\nThis %s installation shares credentials across profiles,\nso csm cannot keep accounts isolated. No credentials were modified.\n\nUpgrade %s and rerun: csm doctor", p.Name(), p.Name())
	}
	return nil
}

func probeIsolation(s *State, p provider.Claude) error {
	dir, err := os.MkdirTemp(s.accountsDir(), ".probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	return checkFreshProfileIsolated(p, dir)
}

func errUnsupported(p provider.Claude) error {
	return fmt.Errorf("this %s version lacks the profile login and hook support csm needs.\n\nUpgrade %s and run: csm doctor", p.Name(), p.Name())
}
