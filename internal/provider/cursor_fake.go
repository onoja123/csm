package provider

import (
	"cmp"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

const fakeCursorHelp = `Usage: agent [options] [command] [prompt...]

Start the Cursor Agent

Arguments:
  prompt                       Initial prompt for the agent

Options:
  -v, --version                Output the version number
  -p, --print                  Print responses to console (for scripts or
                               non-interactive use).
  --resume [chatId]            Select a session to resume (default: false)
  --continue                   Continue previous session (default: false)
  --model <model>              Model to use
  -h, --help                   Display help for command

Commands:
  login                        Authenticate with Cursor.
  logout                       Sign out and clear stored authentication
  status|whoami [options]      View authentication status
  about [options]              Display version, system, and account information`

const fakeCursorOldHelp = `Usage: cursor-agent [options] [command]

Options:
  -v, --version                Output the version number
  -h, --help                   Display help for command

Commands:
  login                        Authenticate with Cursor.
  status                       View authentication status`

// The sign-in sits beside the profiles, not in one, because the real CLI keeps one per user.
func RunFakeCursor(args []string) int {
	config := os.Getenv("CURSOR_CONFIG_DIR")
	profile := filepath.Base(config)
	login := filepath.Join(filepath.Dir(config), ".fake-cursor-login")
	email, signedIn := "", false

	if data, err := os.ReadFile(login); err == nil {
		email, signedIn = strings.TrimSpace(string(data)), true
	}

	command := ""

	if len(args) > 0 {
		command = args[0]
	}

	switch command {
	case "--version", "-v":
		fmt.Println("2026.10.14-fake")

		return 0

	case "--help", "-h":
		if os.Getenv("CSM_FAKE_CURSOR_OLD") != "" {
			fmt.Println(fakeCursorOldHelp)
		} else {
			fmt.Println(fakeCursorHelp)
		}

		return 0

	case "login":
		email = cmp.Or(os.Getenv("CSM_FAKE_CURSOR_EMAIL"), "dev@example.test")
		os.WriteFile(login, []byte(email), 0o600)
		fmt.Println("Logged in as " + email)

		return 0

	case "logout":
		os.Remove(login)

		return 0

	case "status", "whoami":
		if !signedIn {
			fmt.Println(`{"status":"unauthenticated","isAuthenticated":false,"hasAccessToken":false,"hasRefreshToken":false,"message":"Not logged in"}`)
		} else {
			fmt.Printf("{\"status\":\"authenticated\",\"isAuthenticated\":true,\"hasAccessToken\":true,\"hasRefreshToken\":true,\"userInfo\":{\"email\":%q}}\n", email)
		}

		return 0

	case "about":
		if signedIn {
			fmt.Printf("{\"cliVersion\":\"2026.10.14-fake\",\"userEmail\":%q}\n", email)
		} else {
			fmt.Println(`{"cliVersion":"2026.10.14-fake","userEmail":null}`)
		}

		return 0
	}

	if !signedIn {
		fmt.Fprintln(os.Stderr, "Error: Authentication required. Please run 'agent login' first, or set CURSOR_API_KEY environment variable.")

		return 1
	}

	chatID, resume, prompt := NewSessionID(), false, ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--resume", "--model":
			if i+1 < len(args) {
				if args[i] == "--resume" {
					chatID, resume = args[i+1], true
				}

				i++
			}

		default:
			if !strings.HasPrefix(args[i], "-") {
				prompt = args[i]
			}
		}
	}

	cwd, _ := os.Getwd()

	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}

	chat := filepath.Join(cursorChatsDir(config, cwd), chatID)

	if resume {
		if _, err := os.Stat(chat); err != nil {
			fmt.Fprintf(os.Stderr, "Chat %s not found\n", chatID)

			return 1
		}
	}

	os.MkdirAll(chat, 0o700)
	f, err := os.OpenFile(filepath.Join(chat, "store.db"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)

	if err != nil {
		return 1
	}

	fmt.Fprintf(f, "{\"profile\":%q,\"resumed\":%t,\"prompt\":%q}\n", profile, resume, prompt)
	f.Close()

	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	fakeEcho("Cursor CLI", profile, chatID, prompt)

	// Cursor's limit wording is its own and unreadable to csm; this proves csm does not act on such text.
	if fakeLimited(profile) {
		fakePartial()
		fmt.Fprintln(os.Stderr, "Simulated usage limit (CSM_FAKE_LIMIT): You've hit your usage limit.")

		return 1
	}

	if code, ok := fakeBuild(term); ok {
		return code
	}

	if !slices.Contains(strings.Split(os.Getenv("CSM_FAKE_HOLD"), ","), profile) {
		return 0
	}

	select {
	case <-term:
		return 143
	case <-time.After(fakeHold()):
		return 0
	}
}
