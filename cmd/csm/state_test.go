package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

var testNow = time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)

func newTestState(t *testing.T, names ...string) *State {
	t.Helper()
	s, err := initState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range names {
		a, err := s.addAccount(provider.ClaudeID, name, testNow)
		if err != nil {
			t.Fatal(err)
		}

		a.VerifiedAt = testNow
	}

	return s
}

func TestLoadMissingConfig(t *testing.T) {
	_, err := loadState(t.TempDir())
	if !errors.Is(err, errNotSetUp) {
		t.Fatalf("got %v, want errNotSetUp", err)
	}
}

func TestSaveAndLoad(t *testing.T) {
	s := newTestState(t, "personal", "work")
	s.Config.AutoFailover = true

	if err := s.save(); err != nil {
		t.Fatal(err)
	}

	got, err := loadState(s.Home)
	if err != nil {
		t.Fatal(err)
	}

	if !got.Config.AutoFailover || got.Config.ActiveAccount != "personal" {
		t.Fatalf("config not round-tripped: %+v", got.Config)
	}

	if !slices.Equal(got.Config.AccountOrder, []string{"personal", "work"}) {
		t.Fatalf("order = %v", got.Config.AccountOrder)
	}

	if len(got.Accounts) != 2 || got.Accounts[1].ConfigDir != filepath.Join(s.Home, "accounts", "work") {
		t.Fatalf("accounts = %+v", got.Accounts)
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	s := newTestState(t)
	os.WriteFile(s.configPath(), []byte("{not json"), 0o600)

	if _, err := loadState(s.Home); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadRejectsUnknownVersion(t *testing.T) {
	s := newTestState(t)
	os.WriteFile(s.configPath(), []byte(`{"version": 99}`), 0o600)
	_, err := loadState(s.Home)
	if err == nil || !strings.Contains(err.Error(), "version 99") {
		t.Fatalf("got %v, want version error", err)
	}
}

func TestInitStateKeepsExistingConfig(t *testing.T) {
	s := newTestState(t, "personal")
	s.save()
	again, err := initState(s.Home)
	if err != nil {
		t.Fatal(err)
	}

	if len(again.Accounts) != 1 {
		t.Fatalf("setup overwrote accounts: %+v", again.Accounts)
	}
}

func TestAddAccountRejectsDuplicatesAndBadNames(t *testing.T) {
	s := newTestState(t, "personal")

	if _, err := s.addAccount(provider.ClaudeID, "personal", testNow); err == nil {
		t.Fatal("expected duplicate error")
	}

	for _, bad := range []string{"", "Work", "../x", "a b", strings.Repeat("a", 33)} {
		if _, err := s.addAccount(provider.ClaudeID, bad, testNow); err == nil {
			t.Fatalf("accepted bad name %q", bad)
		}
	}
}

func TestRemoveAccount(t *testing.T) {
	s := newTestState(t, "personal", "work", "backup")

	if err := s.removeAccount("personal"); err != nil {
		t.Fatal(err)
	}

	if s.Config.ActiveAccount != "work" {
		t.Fatalf("active = %q, want work", s.Config.ActiveAccount)
	}

	if !slices.Equal(s.Config.AccountOrder, []string{"work", "backup"}) {
		t.Fatalf("order = %v", s.Config.AccountOrder)
	}

	if err := s.removeAccount("personal"); err == nil {
		t.Fatal("expected error removing missing account")
	}
}

func TestResolveAccountByIndex(t *testing.T) {
	s := newTestState(t, "personal", "work", "backup")
	a, err := s.resolveAccount("2")
	if err != nil || a.Name != "work" {
		t.Fatalf("got %v, %v", a, err)
	}

	if _, err := s.resolveAccount("4"); err == nil {
		t.Fatal("expected error for out-of-range index")
	}
}

func TestAccountStatus(t *testing.T) {
	a := Account{Enabled: true}
	if a.status(testNow) != statusNotAuthenticated {
		t.Fatal("unverified account should not be ready")
	}

	a.VerifiedAt = testNow

	if a.status(testNow) != statusReady {
		t.Fatal("verified account should be ready")
	}

	a.CooldownUntil = testNow.Add(time.Minute)

	if a.status(testNow) != statusCooldown {
		t.Fatal("expected cooldown")
	}

	if a.status(testNow.Add(2*time.Minute)) != statusReady {
		t.Fatal("cooldown should expire")
	}

	a.Enabled = false

	if a.status(testNow) != statusDisabled {
		t.Fatal("expected disabled")
	}
}

func TestNextAccount(t *testing.T) {
	tests := []struct {
		name    string
		current string
		setup   func(s *State)
		skip    map[string]bool
		want    string
		wantErr bool
	}{
		{name: "next in order", current: "personal", want: "work"},
		{name: "wraps around", current: "backup", want: "personal"},
		{name: "skips disabled", current: "personal", setup: func(s *State) { s.Accounts[1].Enabled = false }, want: "backup"},
		{name: "skips unauthenticated", current: "personal", setup: func(s *State) { s.Accounts[1].VerifiedAt = time.Time{} }, want: "backup"},
		{name: "skips cooldown", current: "personal", setup: func(s *State) { s.Accounts[1].CooldownUntil = testNow.Add(time.Hour) }, want: "backup"},
		{name: "skips unavailable this run", current: "personal", skip: map[string]bool{"work": true}, want: "backup"},
		{name: "all unavailable", current: "personal", skip: map[string]bool{"work": true, "backup": true}, wantErr: true},
		{name: "unknown current starts at first", current: "", want: "personal"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestState(t, "personal", "work", "backup")

			if tt.setup != nil {
				tt.setup(s)
			}

			got, err := s.nextAccount(provider.ClaudeID, tt.current, tt.skip, testNow)

			if tt.wantErr {
				if !errors.Is(err, errNoAccountAvailable) {
					t.Fatalf("got %v, want errNoAccountAvailable", err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if got.Name != tt.want {
				t.Fatalf("got %s, want %s", got.Name, tt.want)
			}
		})
	}
}

func TestProvidersKeepSeparateAccounts(t *testing.T) {
	s := newTestState(t, "personal", "work")

	for _, name := range []string{"codex-a", "codex-b"} {
		a, err := s.addAccount(provider.CodexID, name, testNow)
		if err != nil {
			t.Fatal(err)
		}

		a.VerifiedAt = testNow
	}

	if s.active(provider.ClaudeID) != "personal" || s.active(provider.CodexID) != "codex-a" {
		t.Fatalf("active: claude=%q codex=%q", s.active(provider.ClaudeID), s.active(provider.CodexID))
	}

	if got := s.providersInUse(); !slices.Equal(got, []string{provider.ClaudeID, provider.CodexID}) {
		t.Fatalf("providers in use = %v", got)
	}

	next, err := s.nextAccount(provider.CodexID, "codex-b", nil, testNow)
	if err != nil || next.Name != "codex-a" {
		t.Fatalf("next Codex account = %v, %v; it must never be a Claude Code account", next, err)
	}

	if _, err := s.nextAccount(provider.CodexID, "codex-a", map[string]bool{"codex-b": true}, testNow); !errors.Is(err, errNoAccountAvailable) {
		t.Fatalf("got %v, want errNoAccountAvailable", err)
	}

	if err := s.save(); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadState(s.Home)
	if err != nil || loaded.active(provider.CodexID) != "codex-a" || loaded.Accounts[2].providerID() != provider.CodexID {
		t.Fatalf("not round-tripped: %+v %v", loaded, err)
	}

	if err := s.removeAccount("codex-a"); err != nil {
		t.Fatal(err)
	}

	if s.active(provider.CodexID) != "codex-b" || s.active(provider.ClaudeID) != "personal" {
		t.Fatalf("active after remove: claude=%q codex=%q", s.active(provider.ClaudeID), s.active(provider.CodexID))
	}

	if s.projectStateDir(provider.ClaudeID, "/work/p") == s.projectStateDir(provider.CodexID, "/work/p") {
		t.Fatal("providers share one project state directory")
	}

	if s.projectStateDir("", "/work/p") != s.projectStateDir(provider.ClaudeID, "/work/p") {
		t.Fatal("records without a provider must read as Claude Code")
	}
}

func TestWriteJSONFromManyWritersAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	const writers, rounds = 8, 50
	errs := make(chan error, writers)

	for range writers {
		go func() {
			for range rounds {
				if err := writeJSON(path, Config{Version: stateVersion, AccountOrder: []string{"personal", "work", "backup"}}); err != nil {
					errs <- err

					return
				}

				var got Config
				if err := readJSON(path, &got); err != nil {
					errs <- err

					return
				}
			}

			errs <- nil
		}()
	}

	for range writers {
		if err := <-errs; err != nil {
			t.Fatalf("a concurrent write or read failed: %v", err)
		}
	}

	leftovers, _ := filepath.Glob(path + "*")
	if len(leftovers) != 1 {
		t.Fatalf("temporary files were left behind: %v", leftovers)
	}
}

func TestUpdateKeepsEveryWritersChange(t *testing.T) {
	home := newTestState(t).Home
	const writers = 8
	errs := make(chan error, writers)

	for i := range writers {
		go func() {
			s, err := loadState(home)
			if err != nil {
				errs <- err

				return
			}

			errs <- s.update(func(state *State) error {
				_, err := state.addAccount(provider.ClaudeID, fmt.Sprintf("n%d", i), testNow)

				return err
			})
		}()
	}

	for range writers {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	s, err := loadState(home)
	if err != nil {
		t.Fatal(err)
	}

	if len(s.Accounts) != writers || len(s.Config.AccountOrder) != writers {
		t.Fatalf("%d accounts and %d in the order survived %d simultaneous additions", len(s.Accounts), len(s.Config.AccountOrder), writers)
	}
}
