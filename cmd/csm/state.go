package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"time"
)

const stateVersion = 1

type Config struct {
	Version       int      `json:"version"`
	ActiveAccount string   `json:"active_account"`
	AccountOrder  []string `json:"account_order"`
	AutoFailover  bool     `json:"auto_failover"`
}

type Account struct {
	Name          string    `json:"name"`
	Enabled       bool      `json:"enabled"`
	ConfigDir     string    `json:"config_dir"`
	CreatedAt     time.Time `json:"created_at"`
	LastUsedAt    time.Time `json:"last_used_at,omitzero"`
	VerifiedAt    time.Time `json:"verified_at,omitzero"`
	Email         string    `json:"email,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitzero"`
}

type accountsFile struct {
	Version  int       `json:"version"`
	Accounts []Account `json:"accounts"`
}

// State is everything csm persists globally. Paths are derived from Home.
type State struct {
	Home     string
	Config   Config
	Accounts []Account
}

var errNotSetUp = errors.New("csm is not set up; run: csm setup")

var accountNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func csmHome() (string, error) {
	if dir := os.Getenv("CSM_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".csm"), nil
}

func (s *State) configPath() string   { return filepath.Join(s.Home, "config.json") }
func (s *State) accountsPath() string { return filepath.Join(s.Home, "accounts.json") }
func (s *State) accountsDir() string  { return filepath.Join(s.Home, "accounts") }
func (s *State) projectsDir() string  { return filepath.Join(s.Home, "projects") }

func initState(home string) (*State, error) {
	for _, dir := range []string{home, filepath.Join(home, "accounts"), filepath.Join(home, "projects")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	s, err := loadState(home)
	if errors.Is(err, errNotSetUp) {
		s = &State{Home: home, Config: Config{Version: stateVersion}}
		return s, s.save()
	}
	return s, err
}

func loadState(home string) (*State, error) {
	s := &State{Home: home}

	data, err := os.ReadFile(s.configPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNotSetUp
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.Config); err != nil {
		return nil, fmt.Errorf("read %s: %w", s.configPath(), err)
	}
	if s.Config.Version != stateVersion {
		return nil, fmt.Errorf("%s has version %d; this csm understands version %d", s.configPath(), s.Config.Version, stateVersion)
	}

	data, err = os.ReadFile(s.accountsPath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var af accountsFile
	if err := json.Unmarshal(data, &af); err != nil {
		return nil, fmt.Errorf("read %s: %w", s.accountsPath(), err)
	}
	if af.Version != stateVersion {
		return nil, fmt.Errorf("%s has version %d; this csm understands version %d", s.accountsPath(), af.Version, stateVersion)
	}
	s.Accounts = af.Accounts
	return s, nil
}

func (s *State) save() error {
	if err := writeJSON(s.configPath(), s.Config); err != nil {
		return err
	}
	return writeJSON(s.accountsPath(), accountsFile{Version: stateVersion, Accounts: s.Accounts})
}

// writeJSON writes via rename so a crash never leaves a half-written file.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

// resolveAccount accepts an account name or its 1-based position in the order.
func (s *State) resolveAccount(nameOrIndex string) (*Account, error) {
	if n, err := strconv.Atoi(nameOrIndex); err == nil {
		if n < 1 || n > len(s.Config.AccountOrder) {
			return nil, fmt.Errorf("no account at position %d; run: csm accounts", n)
		}
		nameOrIndex = s.Config.AccountOrder[n-1]
	}
	for i := range s.Accounts {
		if s.Accounts[i].Name == nameOrIndex {
			return &s.Accounts[i], nil
		}
	}
	return nil, fmt.Errorf("no account named %q; run: csm accounts", nameOrIndex)
}

func (s *State) addAccount(name string, now time.Time) (*Account, error) {
	if !accountNamePattern.MatchString(name) {
		return nil, fmt.Errorf("invalid account name %q: use lowercase letters, digits, - or _ (max 32)", name)
	}
	if _, err := s.resolveAccount(name); err == nil {
		return nil, fmt.Errorf("account %q already exists", name)
	}
	s.Accounts = append(s.Accounts, Account{
		Name:      name,
		Enabled:   true,
		ConfigDir: filepath.Join(s.accountsDir(), name),
		CreatedAt: now,
	})
	s.Config.AccountOrder = append(s.Config.AccountOrder, name)
	if s.Config.ActiveAccount == "" {
		s.Config.ActiveAccount = name
	}
	return &s.Accounts[len(s.Accounts)-1], nil
}

func (s *State) removeAccount(name string) error {
	i := slices.IndexFunc(s.Accounts, func(a Account) bool { return a.Name == name })
	if i < 0 {
		return fmt.Errorf("no account named %q", name)
	}
	s.Accounts = slices.Delete(s.Accounts, i, i+1)
	s.Config.AccountOrder = slices.DeleteFunc(s.Config.AccountOrder, func(n string) bool { return n == name })
	if s.Config.ActiveAccount == name {
		s.Config.ActiveAccount = ""
		if len(s.Config.AccountOrder) > 0 {
			s.Config.ActiveAccount = s.Config.AccountOrder[0]
		}
	}
	return nil
}

type accountStatus string

const (
	statusReady            accountStatus = "ready"
	statusDisabled         accountStatus = "disabled"
	statusNotAuthenticated accountStatus = "not authenticated"
	statusCooldown         accountStatus = "cooldown"
)

func (a *Account) status(now time.Time) accountStatus {
	switch {
	case !a.Enabled:
		return statusDisabled
	case a.VerifiedAt.IsZero():
		return statusNotAuthenticated
	case now.Before(a.CooldownUntil):
		return statusCooldown
	default:
		return statusReady
	}
}

func (s *State) nextAccount(current string, skip map[string]bool, now time.Time) (*Account, error) {
	order := s.Config.AccountOrder
	start := slices.Index(order, current)
	for i := 1; i <= len(order); i++ {
		name := order[(start+i+len(order))%len(order)]
		if name == current || skip[name] {
			continue
		}
		a, err := s.resolveAccount(name)
		if err != nil {
			return nil, err
		}
		if a.status(now) == statusReady {
			return a, nil
		}
	}
	return nil, errNoAccountAvailable
}

var errNoAccountAvailable = errors.New("all configured accounts are currently unavailable")
