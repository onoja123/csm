package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)

func newTestState(t *testing.T, names ...string) *State {
	t.Helper()
	s, err := initState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		a, err := s.addAccount(name, testNow)
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
	if _, err := s.addAccount("personal", testNow); err == nil {
		t.Fatal("expected duplicate error")
	}
	for _, bad := range []string{"", "Work", "../x", "a b", strings.Repeat("a", 33)} {
		if _, err := s.addAccount(bad, testNow); err == nil {
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
			got, err := s.nextAccount(tt.current, tt.skip, testNow)
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
