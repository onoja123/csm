package provider

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// A signed-out profile signs in on its first launch, as a person would inside the real Gemini CLI; CSM_FAKE_HOLD profiles run until stopped.
func RunFakeGemini(args []string) int {
	home := os.Getenv("GEMINI_CLI_HOME")
	dir := geminiDir(home)
	profile := filepath.Base(home)

	switch {
	case len(args) > 0 && args[0] == "--version":
		fmt.Println("0.0.0-fake")

		return 0

	case len(args) > 0 && args[0] == "--help":
		fmt.Println("Usage: gemini [options] [command]\n\nOptions:\n  -r, --resume              Resume a previous session.\n  -i, --prompt-interactive  Execute the provided prompt and continue in interactive mode\n      --session-file        Load a session from a JSON file\n      --session-id          Start a new session with a manually provided UUID.")

		return 0
	}

	if _, err := os.Stat(filepath.Join(dir, "oauth_creds.json")); err != nil {
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "oauth_creds.json"), []byte("{}"), 0o600)
		os.WriteFile(filepath.Join(dir, "google_accounts.json"), fmt.Appendf(nil, "{\"active\":%q,\"old\":[]}", profile+"@example.test"), 0o600)

		return 0
	}

	sessionID, imported, prompt := NewSessionID(), false, ""

	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--session-id":
			sessionID = args[i+1]

		case "-i", "--prompt-interactive":
			prompt = args[i+1]

		case "--session-file":
			if _, err := os.Stat(args[i+1]); err != nil {
				fmt.Fprintf(os.Stderr, "Error importing session from file: %v\n", err)

				return 42
			}

			imported = true
		}
	}

	cwd, _ := os.Getwd()
	chats := filepath.Join(dir, "tmp", filepath.Base(cwd), "chats")
	os.MkdirAll(chats, 0o700)
	name := fmt.Sprintf("session-%d-%s.jsonl", time.Now().UnixMilli(), sessionID[:8])
	content := fmt.Sprintf("{\"sessionId\":%q,\"projectHash\":%q}\n{\"profile\":%q,\"resumed\":%t,\"prompt\":%q}\n", sessionID, geminiProjectHash(cwd), profile, imported, prompt)

	if err := os.WriteFile(filepath.Join(chats, name), []byte(content), 0o600); err != nil {
		return 1
	}

	if !slices.Contains(strings.Split(os.Getenv("CSM_FAKE_HOLD"), ","), profile) {
		return 0
	}

	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)

	select {
	case <-term:
		return 143
	case <-time.After(30 * time.Second):
		return 0
	}
}
