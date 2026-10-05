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

func RunFakeCopilot(args []string) int {
	home := os.Getenv("COPILOT_HOME")
	profile := filepath.Base(home)

	switch {
	case len(args) > 0 && args[0] == "--version":
		fmt.Println("GitHub Copilot CLI 0.0.0-fake.\nRun 'copilot update' to check for updates.")

		return 0

	case len(args) > 0 && args[0] == "--help":
		fmt.Println("Usage: copilot [OPTIONS] [COMMAND]\n\nCommands:\n  login        Authenticate with Copilot\n\nOptions:\n  -r, --resume [<value>]\n      --session-id <id>")

		return 0

	case len(args) > 0 && args[0] == "login":
		config := fmt.Sprintf("// User settings belong in settings.json.\n// This file is managed automatically.\n{\n  \"lastLoggedInUser\": {\"host\": %q, \"login\": %q}\n}\n", copilotDefaultHost, profile)
		os.WriteFile(filepath.Join(home, "config.json"), []byte(config), 0o600)

		return 0
	}

	sessionID, resume := NewSessionID(), false

	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--session-id":
			sessionID = args[i+1]
		case "--resume":
			sessionID, resume = args[i+1], true
		}
	}

	dir := copilotSessionDir(home, sessionID)

	if resume {
		if _, err := os.Stat(dir); err != nil {
			fmt.Fprintf(os.Stderr, "No session found with ID %s\n", sessionID)

			return 1
		}
	}

	cwd, _ := os.Getwd()
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "workspace.yaml"), fmt.Appendf(nil, "id: %s\ncwd: %s\n", sessionID, cwd), 0o600)
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)

	if err != nil {
		return 1
	}

	fmt.Fprintf(f, "{\"profile\":%q,\"resumed\":%t}\n", profile, resume)
	f.Close()

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
