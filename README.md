# csm

Run several accounts of the same coding agent on one machine and switch between them without logging out.

`csm` (Code Session Manager) keeps each account in its own profile directory, starts the agent with the right one, and can move a running session to another account, keeping the conversation. When an account hits its usage limit, it can do that switch for you.

## Supported agents

| Agent | Command | Manual switch | Usage check | Auto failover |
| --- | --- | --- | --- | --- |
| Claude Code | `csm claude` | yes | yes | yes |
| Codex | `csm codex` | yes | yes | yes |
| Gemini CLI | `csm gemini` | yes | no | no |
| GitHub Copilot CLI | `csm copilot` | yes | no | no |

Accounts only switch with accounts of the same agent. Different agents can run in the same project at the same time.

## Requirements

- macOS or Linux. Windows is not supported: csm does not build there yet.
- Go 1.25 or newer
- At least one of the agents above, installed and on your `PATH`

csm is developed on macOS. On Linux its test suite runs on every push (GitHub Actions, Ubuntu); it has not yet been used with a real agent on Linux.

## Installation

```bash
go install github.com/onoja123/csm/cmd/csm@latest
```

Or from source:

```bash
git clone https://github.com/onoja123/csm.git
cd csm
go build -o csm ./cmd/csm
mv csm ~/.local/bin/
```

## Quick start

```bash
csm setup                       # create ~/.csm and check the installed agents

csm account add personal        # log in to a first Claude Code account
csm account add work            # and a second one

cd ~/code/my-project
csm claude                      # run Claude Code as the active account

csm next                        # from another terminal: move the session to the next account
```

For another agent, add `--provider`:

```bash
csm account add work-codex --provider codex
csm codex
```

## Usage

```
csm [--debug] <command> [arguments]
```

### Accounts

```bash
csm account add <name> [--provider claude|codex|gemini|copilot] [--link-settings]
csm account login <name>      # log in again
csm account disable <name>    # skip it when switching
csm account enable <name>
csm account remove <name>     # forget it; the profile directory is kept
csm accounts                  # list accounts and their order
csm current [provider]        # print the active account
```

`--link-settings` symlinks your existing agent settings (for example `~/.claude/settings.json`, `CLAUDE.md`, `skills`) into the new profile.

### Running an agent

```bash
csm claude [args...]
csm codex [args...]
csm gemini [args...]
csm copilot [args...]
```

Arguments are passed to the agent. It runs in your terminal as usual.

### Switching

```bash
csm use <name|number>    # make an account active
csm next [provider]      # move to the next ready account
csm status               # project, accounts, running session, last checkpoint
```

If a `csm` session is running, `use` and `next` switch it: `csm` saves a checkpoint, stops the agent, and restarts it under the new account with the same conversation. Otherwise they change which account the next run uses.

### Usage

```bash
csm usage              # live check of every signed-in account
csm usage work backup  # only these accounts
csm usage --cached     # last saved figures, no check
```

```
● personal     updated 0s ago
    5-hour     0%   resets 05:10
    7-day     13%   resets Sun 01:00
```

For Claude Code a live check sends one small Haiku request per account (about 500 tokens). For Codex it sends no request and uses no tokens.

### Automatic failover

```bash
csm auto on
csm auto off
csm auto status
```

With auto failover on, `csm` switches to the next ready account when the running one reports a usage limit. The limited account gets a one-hour cooldown. If every account is limited, `csm` stops instead of cycling. The request that hit the limit is not retried; send it again or type "continue".

`csm` only switches on a known usage limit. A plain rate limit, an authentication error, or a server error is reported and nothing is switched.

### Diagnostics

```bash
csm doctor           # check the install, isolation, and every account
csm test failover    # simulate a failover with a fake agent (add `codex` for Codex)
csm --debug <cmd>    # log what csm is doing to stderr
```

## How it works

Each account is a directory under `~/.csm/accounts/<name>`. `csm` points the agent at it with the agent's own variable:

| Agent | Variable | Identity comes from |
| --- | --- | --- |
| Claude Code | `CLAUDE_CONFIG_DIR` | `claude auth status --json` |
| Codex | `CODEX_HOME` | the Codex app-server (`account/read`) |
| Gemini CLI | `GEMINI_CLI_HOME` | `google_accounts.json` in the profile |
| GitHub Copilot CLI | `COPILOT_HOME` | `config.json` in the profile |

Before every launch and switch `csm` checks that the profile still reports the account it was set up with. Two profiles that report the same account are rejected.

```
~/.csm/
├── config.json          active accounts, order, auto failover
├── accounts.json        names, providers, profile paths, verified identity
├── accounts/<name>/     the agent's profile directory
├── usage/<name>.json    last usage figures
└── projects/<id>/       session, checkpoint and switch state per project
```

### Carrying a session over

| Agent | How the conversation moves |
| --- | --- |
| Claude Code | the transcript is copied to the new profile, then `--resume <id>` |
| Codex | the rollout file is copied, then `codex resume <id>` |
| Gemini CLI | the new profile imports the old session file with `--session-file` |
| GitHub Copilot CLI | the session folder is copied, then `--resume <id>` |

Your working directory, branch and uncommitted changes are never touched. If a conversation cannot be carried, `csm` says so and starts a new session in the same directory.

### Detecting a limit

- Claude Code: a `StopFailure` hook, registered for the launch with `--settings`. Your settings files are not modified.
- Codex: `csm` reads the account's limits from the app-server once a minute, and once more when Codex exits. Detection can lag by up to a minute.
- Gemini CLI and Copilot CLI: no signal is available. Switch by hand with `csm next`.

## Security

- `csm` does not read, copy or store tokens, cookies or Keychain secrets. Logging in is always the agent's own login flow.
- `accounts.json` holds names, paths, timestamps and the account email or username.
- `~/.csm` is created with mode `0700`, files with `0600`.
- `csm` refuses to run when an environment variable would override the profile's login:

  | Agent | Variables |
  | --- | --- |
  | Claude Code | `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN` |
  | Codex | `CODEX_ACCESS_TOKEN`, `CODEX_API_KEY` |
  | Gemini CLI | `GEMINI_API_KEY`, `GOOGLE_GENAI_USE_VERTEXAI`, `GOOGLE_GEMINI_BASE_URL`, `GEMINI_FORCE_ENCRYPTED_FILE_STORAGE` |
  | GitHub Copilot CLI | `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, `GITHUB_TOKEN`, `COPILOT_PROVIDER_BASE_URL` |

- The only git commands `csm` runs are read-only (`rev-parse`, `branch --show-current`, `status --short`).

`csm` does not bypass authentication, rate limits or usage limits. Using several accounts must still follow the terms that apply to each of them.

## Limitations

- Codex, Gemini CLI and Copilot CLI support has not yet been tested with signed-in accounts. It was built against each CLI signed out and against fake agents.
- Switching a running session between two real Claude Code accounts has been exercised with csm's fake agent, not yet with two signed-in accounts.
- Windows does not build. csm relies on Unix signals, `/bin/sh` and symlinks.
- If csm itself is killed with `SIGKILL`, the agent it started keeps running on its own and must be closed by hand. Ctrl+C, `SIGTERM` and closing the terminal stop the agent cleanly.
- A second `csm use` or `csm next` sent while a switch is already in progress is acknowledged but ignored.
- Outside a git repository, a project reached through a symlink gets its own state, separate from the same directory reached by its real path. Inside a git repository both resolve to the repository root.
- Gemini CLI and Copilot CLI have no command that reports the signed-in account, so `csm` reads what the agent recorded at sign-in.
- Gemini CLI has no login command. `csm account add` opens Gemini CLI; choose "Sign in with Google", then type `/quit`.
- Copilot CLI keeps tokens in the macOS Keychain and falls back to your `gh` login when a profile has none of its own.
- A Codex session stored in paginated history cannot be carried; `csm` starts a new session instead.
- Flags you pass on the first launch (for example `--model`) are not repeated after a switch.

## Troubleshooting

| Message | What to do |
| --- | --- |
| "a brand-new profile directory already reports a logged-in account" | The agent shares credentials across profiles. Upgrade the agent. |
| "identifies as X, but it was set up as Y" | The profile's login changed. Run `csm account login <name>`. |
| "Previous switch did not complete" | Run the agent command again in that project to resume the switch. |
| "... is set in your environment" | Unset the variable named in the message. |

`csm doctor` reports most problems with a reason and a next step.

## Development

```bash
go build ./...
go vet ./...
go test -race ./...
```

The tests do not need a real agent. Each agent has a fake that runs through the `csm` binary. CI runs the suite on macOS and Ubuntu.

```
cmd/csm/              the CLI: commands, state, runner, logger
internal/provider/    one file per agent, plus its fake
```

To add an agent, implement the `Provider` interface in `internal/provider`, register it in `provider.go`, and add its command to the switch in `cmd/csm/main.go`.

## Contributing

Issues and pull requests are welcome. Please run `go vet ./...` and `go test ./...` before opening a pull request.
