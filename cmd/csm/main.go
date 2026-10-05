package main

import (
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"runtime"
	"slices"

	"github.com/onoja123/csm/internal/provider"
)

const usage = `csm - Code Session Manager

A local profile and session manager for coding agents. Supported providers: Claude Code, Codex, Gemini CLI, GitHub Copilot CLI.

Usage:
  csm [--debug] <command> [arguments]

Commands:
  setup                    Initialise ~/.csm and check the provider
  doctor                   Diagnose installation, isolation and session support
  status                   Show project, accounts and session state
  current [provider]       Print the active account name
  accounts                 List accounts and their order
  account add <name>       Create a profile and log in
                           (--provider codex|gemini|copilot for another agent, --link-settings to share your own agent settings)
  account login <name>     Log in to an existing profile again
  account remove <name>    Forget an account (its profile directory is kept)
  account enable <name>    Include an account in switching
  account disable <name>   Exclude an account from switching
  use <name|number>        Make an account active (switches a running session)
  next [provider]          Move to the next ready account
  usage [name...] [--cached]  Check 5-hour and 7-day plan usage per account
  auto on|off|status       Control automatic failover on usage limits
  claude [args...]         Run Claude Code under the active Claude Code account
  codex [args...]          Run Codex under the active Codex account
  gemini [args...]         Run Gemini CLI under the active Gemini CLI account
  copilot [args...]        Run GitHub Copilot CLI under the active Copilot account
  test failover [codex]    Simulate a failover with a fake agent (no real usage)
  version                  Print version information`

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.1.0"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {

	if len(args) > 0 && args[0] == "hook" {
		if len(args) > 1 {
			runHook(args[1], os.Stdin)
		}

		return 0
	}

	if len(args) > 0 && args[0] == "__fake-claude" {
		return provider.RunFakeClaude(args[1:])
	}

	if len(args) > 0 && args[0] == "__fake-codex" {
		return provider.RunFakeCodex(args[1:])
	}

	if len(args) > 0 && args[0] == "__fake-gemini" {
		return provider.RunFakeGemini(args[1:])
	}

	if len(args) > 0 && args[0] == "__fake-copilot" {
		return provider.RunFakeCopilot(args[1:])
	}

	log := &Logger{out: os.Stdout, err: os.Stderr}
	fs := flag.NewFlagSet("csm", flag.ContinueOnError)
	fs.Usage = func() { log.Error("%s", usage) }
	debug := fs.Bool("debug", false, "print diagnostic logs to stderr")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	logOutput := io.Discard

	if *debug {
		logOutput = os.Stderr
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: slog.LevelDebug})))

	args = fs.Args()
	if len(args) == 0 {
		log.Error("%s", usage)

		return 2
	}

	home, err := csmHome()
	if err != nil {
		log.Error("%v", err)

		return 1
	}

	code := 0
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "version":
		log.Info("csm %s\n%s\n%s/%s", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)

	case "setup":
		err = cmdSetup(log, home)

	case "doctor":
		err = cmdDoctor(log, home)

	case "status":
		err = cmdStatus(log, home)

	case "current":
		err = cmdCurrent(log, home, rest)

	case "accounts":
		err = cmdAccounts(log, home)

	case "account":
		err = cmdAccount(log, home, rest)

	case "use":
		if len(rest) != 1 {
			err = errors.New("usage: csm use <name|number>")
			break
		}

		err = cmdUse(log, home, rest[0])

	case "next":
		err = cmdNext(log, home, rest)

	case "usage":
		err = cmdUsage(log, home, rest)

	case "auto":
		err = cmdAuto(log, home, rest)

	case provider.ClaudeID, provider.CodexID, provider.GeminiID, provider.CopilotID:
		code, err = cmdRun(log, home, cmd, rest)

	case "test":
		providerID := provider.ClaudeID

		if len(rest) == 2 {
			providerID = rest[1]
		}

		if len(rest) < 1 || len(rest) > 2 || rest[0] != "failover" || !slices.Contains([]string{provider.ClaudeID, provider.CodexID}, providerID) {
			err = errors.New("usage: csm test failover [codex]")
			break
		}

		err = cmdTestFailover(log, providerID)

	case "help", "-h", "--help":
		log.Info("%s", usage)

	default:
		log.Error("unknown command %q\n\n%s", cmd, usage)

		return 2
	}

	if err != nil {
		log.Error("\n%v", err)

		if code == 0 {
			code = 1
		}
	}

	return code
}
