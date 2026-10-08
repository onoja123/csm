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

If a `csm` session is running, `use` and `next` switch it: `csm` saves a checkpoint, stops the agent, and restarts it under the new account with the same conversation and its MCP servers (see [MCP failover](#mcp-failover)). Otherwise they change which account the next run uses.

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

### Handing a task to another agent

```bash
csm handoff <account|number>      # move the current task to that account, even of another agent
csm handoff --provider codex      # to that agent's active account
```

With a `csm` session running in this project, the running session checkpoints, stops its agent, and starts the target agent in the same terminal and directory. Without one, `csm handoff` starts the target here from the last checkpoint. See [Cross-agent handoff](#cross-agent-handoff).

### MCP servers

```bash
csm mcp                               # the MCP servers each account would start in this directory
csm mcp <account>                     # one account
csm mcp handoff <from> <to>           # preview moving MCP servers to another account, even of another agent
csm mcp handoff <from> <to> --apply   # write the compatible ones into the target profile
```

See [MCP failover](#mcp-failover) for what moves and what does not.

### Diagnostics

```bash
csm doctor           # check the install, isolation, every account and its MCP servers
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

## Cross-agent handoff

`csm` does not try to make Claude Code's conversation readable by Codex, or any agent's by another. Instead it writes a provider-neutral **task handoff**: what was asked, what the agent did, which files it touched, the exact repository state and the MCP verdicts, and gives that to the next agent as its opening instruction. The new agent reads it, checks the repository, and continues.

```
$ csm handoff work-codex
CSM handoff

Current agent                                Claude Code / personal
Target agent                                 Codex / work-codex
Session                                      974c0410-e320-44aa-bf5b-cd6e04a53e22
Task context                                 captured at the switch
Git state                                    captured at the switch

Continue with Codex? [Y/n]
```

and in the terminal running the session:

```
Code Session Manager

Switching personal (Claude Code) → work-codex (Codex).

  Saving checkpoint                  ✓
  Stopping Claude Code               ✓
  Selecting work-codex               ✓
  Capturing task context             ✓
  Capturing git state                ✓
  Carrying MCP configuration         ✓
  Saving handoff                     ✓
  Handing the task to Codex          ✓
  Starting Codex as work-codex       ✓

Codex cannot read a Claude Code conversation. It starts with a task handoff instead:

    ~/.csm/handoffs/my-project-3f2a9c1b7e04/20261008T110102Z-claude-codex/handoff.md

MCP handoff:
  ✓ filesystem
  ✓ github reads GITHUB_TOKEN from the environment
  ⚠ linear requires authentication: may need a login in work-codex
```

### What the handoff contains

Each handoff is a directory under `~/.csm/handoffs/<project>/<id>/`, mode `0700`, files `0600`:

| File | Contents |
| --- | --- |
| `handoff.md` | the summary the agent reads: current task, earlier requests, last progress note, files modified/created/read, commands run, decisions, git state, MCP verdicts, next steps |
| `handoff.json` | the same, structured and versioned (`"version": 1`), plus the lifecycle stage |
| `git-status.txt` | full `git status --short` |
| `diff.patch` | `git diff HEAD`, up to 1 MB; referenced from the summary, never put in a prompt |

The task context comes from the agent's own session records, read deterministically, no model involved:

| Agent | Read from | Extracted |
| --- | --- | --- |
| Claude Code | the session transcript csm already locates for `--resume` | user prompts, assistant text, files from `Read`/`Edit`/`Write` calls, `Bash` commands, "next steps" lists, sentences announcing a decision |
| Codex | the rollout file the app-server reports | user and assistant messages, shell calls |
| Gemini CLI | the session file under `.gemini/tmp` | user and model messages |
| Copilot CLI | `events.jsonl` in the session folder | user and assistant messages, tool calls |

Tool *output* is never read: it is untrusted and the usual place for secrets. Everything kept is run through a redactor (tokens, bearer headers, URLs with passwords, `NAME=value` with a secret-looking name) and bounded: the last six requests, one progress note, forty files, fifteen commands. The repository is the source of truth; the handoff tells the agent where to look.

The Claude Code format was verified against a real transcript. The Codex, Gemini CLI and Copilot CLI readers follow their documented session shapes and degrade to "repository state only" when a file does not match.

### How each agent receives it

| Agent | Mechanism |
| --- | --- |
| Claude Code | `--append-system-prompt`, alongside `--resume` when the session itself is carried |
| Codex | the positional prompt, also after `codex resume <id>` |
| Gemini CLI | `--prompt-interactive` |
| Copilot CLI | `--interactive <prompt>` |

All four were checked against the installed CLIs' help. If an installed version lacks the flag, `csm` still writes the handoff and prints the path to read.

### Same agent, different account

`csm next`, `csm use` and automatic failover run through the same engine. When the agent can resume its own session (all four can), the conversation is carried as before and the handoff is written as a safety copy; the opening note just says which account the session moved from. When it cannot, the new session starts with the full handoff instruction.

### Lifecycle and recovery

`handoff.json` records the stage: `checkpointed`, `context_extracted`, `mcp_prepared`, `target_started`. Handoffs are kept; `csm status` shows the latest one for the project. If `csm` dies between stopping one agent and starting the other, the transition is recorded under the target agent, and `csm <agent>` in that project offers to finish it.

### Limitations

- The conversation does not move between agents. The new agent knows the task, the files and the repository state, not the previous model's reasoning; it re-reads what it needs.
- The handoff is only as good as the session records. A session with no user prompts yields repository state only.
- Decisions and next steps are found by pattern, not understanding: a "Next steps" heading followed by a list, sentences beginning "I'll" or "instead of". Absent those, the sections fall back to generic guidance.
- Nothing here resets, stashes, cleans or checks out anything in your repository.

## MCP failover

MCP servers are configured per agent profile, so a fresh profile would start without them. When `csm` switches accounts it carries the MCP *configuration* along. It distinguishes four things:

| | Moves with a switch? |
| --- | --- |
| csm's own state (checkpoint, session ID, project, branch) | yes |
| MCP server configuration (name, transport, command, arguments, URL) | yes, where the target agent supports it |
| MCP runtime state (what a server has stored while running) | **no**; it belongs to the server |
| Secrets (API keys, OAuth logins, header values, environment values) | **never** between agents; see below |

**MCP runtime state is not portable between agents.** `csm` recreates configuration; it never copies a server's databases, caches or credential stores.

### What csm reads

| Agent | Configuration read |
| --- | --- |
| Claude Code | `<profile>/.claude.json` (user scope, and the local scope entry for the current directory), `<cwd>/.mcp.json` (project scope, with its approval state) |
| Codex | `[mcp_servers.*]` tables in `<profile>/config.toml` |
| Gemini CLI | `mcpServers` in `<profile>/.gemini/settings.json` |
| GitHub Copilot CLI | `mcpServers` in `<profile>/mcp-config.json` |

Before a switch `csm` writes a snapshot to `~/.csm/projects/<id>/mcp-snapshot.json`, and the result to `mcp-handoff.json`. Both hold server names, transports, commands, arguments, URLs and the *names* of environment variables and headers. Values are never written; an argument that looks like a credential (a URL with a password, the value of `--token`) is replaced by `<redacted>`.

### Claude → Claude (and any same-agent switch)

Both profiles belong to the same agent, so each server's own definition is copied into the target profile as it is, environment values included, the way you would recreate it with `claude mcp add`. Rules:

- A server the target profile already has **wins**; the source definition is not written.
- Project scope servers in `.mcp.json` are shared by both profiles already. Only a recorded approval moves, and Claude Code only honours it in a profile that has already trusted the directory; `csm` never grants that trust. Otherwise the server is marked ⚠ and Claude Code asks for approval on the next start.
- OAuth logins made with `claude mcp login` do not move. The server is carried and marked ⚠; log in again in the new profile.
- The configuration file is rewritten atomically with its existing mode (Claude Code uses `0600`). Everything else in the file is preserved.
- `csm` never writes through a symlink. If a profile shares its MCP file with your own settings (`--link-settings`), the handoff reports a failure for that server and nothing is changed.

### Claude → Codex, or any other pair of agents

A switch between agents is not automatic in `csm`; run it by hand:

```bash
csm mcp handoff personal work-codex          # preview
csm mcp handoff personal work-codex --apply  # write
```

Each server is reduced to a normalised description (name, transport, command, arguments, URL, environment variable names) and rebuilt in the target agent's own format. Then:

- **Environment and header values are not copied.** Codex and Gemini CLI can read a variable from their own environment, so the server is written with the variable *names* (`env_vars = [...]` for Codex, `"$NAME"` references for Gemini CLI) and marked ✓ if the variable is set in your shell, ⚠ if it is not. Claude Code and Copilot CLI have no such form, so the server is written without its environment and marked ⚠ with the names you need to set there.
- Header values never move; a server that needs one is marked ⚠.
- A server whose arguments contain a credential is not written at all; add it yourself.
- A transport the target does not support (for example SSE for Codex) is marked ✗ and skipped.
- HTTP servers without a stored header may need a login in the target agent.

```
MCP handoff personal (Claude Code) → work-codex (Codex)

  ✓ filesystem
  ⚠ github requires authentication: MCP requires missing environment variable: GITHUB_TOKEN
  ✗ legacy unsupported by work-codex: sse transport is not supported by Codex
  ⚠ linear requires authentication: may need a login in work-codex
  ⚠ postgres requires authentication: its arguments contain a credential; add it to work-codex yourself; runtime state stays with the server
```

### Secrets

`csm` never puts an MCP secret value in a snapshot, a handoff file, a handoff note for the agent, its output or its debug log. It only tells you *which* variable is needed:

```
⚠ github requires authentication: MCP requires missing environment variable: GITHUB_TOKEN
```

That message appears when a configuration refers to `${GITHUB_TOKEN}` and the variable is not set in the shell `csm` runs in. Export it before starting the agent, or set the value in the target agent's own configuration.

### Stateful servers

`csm` guesses from the configuration whether a server keeps local state (a database file in its arguments, a `*_PATH`, `*_DIR` or `DATABASE_URL` variable, servers such as `memory`, `sqlite` or `postgres`) and says so: `runtime state stays with the server`. The configuration moves; whatever the server stored does not. Servers that offer their own export and import can be supported later through the same per-agent adapter; nothing is copied blindly today.

### Diagnosing

- `csm mcp` lists the servers every account would start in the current directory, with ⚠ for missing variables or logins.
- `csm doctor` includes one `MCP <account>` row per account and reports an unreadable MCP configuration as a problem.
- A failed MCP handoff never stops the account switch. The agent starts anyway; the report says what needs attention.

## Security

- `csm` does not read, copy or store tokens, cookies or Keychain secrets. Logging in is always the agent's own login flow.
- MCP snapshots and handoff reports hold names, never values. Between two profiles of the same agent a server's definition is copied as it is; between different agents only the shape of the server moves and you add the credentials yourself.
- Task handoffs never include tool output, and everything they do include is redacted for tokens, bearer headers, passwords in URLs and secret-looking assignments before it is written. The stored diff is your own working tree, kept under `~/.csm` with mode `0600` and never placed in a prompt.
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
- MCP handoff was verified against the real CLIs (Claude Code 2.1.294, Codex 0.161, Gemini CLI 0.63, Copilot CLI 1.0.93): each lists the servers csm wrote. A live switch between two signed-in accounts of another agent has only been exercised with csm's fake agents.
- MCP runtime state is never carried. A server that remembers things between calls starts over in the new profile unless it keeps that state somewhere both profiles reach.
- `csm` does not read project-level MCP files of Codex, Gemini CLI or Copilot CLI, only the profile's own.

## Troubleshooting

| Message | What to do |
| --- | --- |
| "a brand-new profile directory already reports a logged-in account" | The agent shares credentials across profiles. Upgrade the agent. |
| "identifies as X, but it was set up as Y" | The profile's login changed. Run `csm account login <name>`. |
| "Previous switch did not complete" | Run the agent command again in that project to resume the switch. |
| "... is set in your environment" | Unset the variable named in the message. |
| "This … version cannot take an opening instruction" | The agent started without the handoff. Ask it to read the `handoff.md` path printed above. |
| the new agent asks what you were working on | The handoff had no task context (no session was recorded). Tell it; the repository state was still captured. |

`csm doctor` reports most problems with a reason and a next step.

## Development

```bash
go build ./...
go vet ./...
go test -race ./...
```

The tests do not need a real agent. Each agent has a fake that runs through the `csm` binary. CI runs the suite on macOS and Ubuntu.

```
cmd/csm/              the CLI: commands, state, runner, logger, MCP and task handoff
internal/provider/    one file per agent, plus its fake, its MCP adapter and its session reader
internal/mcp/         portable MCP server description, snapshot and handoff report
internal/handoff/     provider-neutral task handoff: context, git state, rendering, storage
```

To take part in task handoff, a provider implements `SessionContext` (read its own session into a `handoff.Context`) and `HandoffArgs` (pass the opening instruction).

To give an agent MCP handoff, implement `mcp.Adapter` (parse, validate, prepare) next to its provider and expose it with an `MCP()` method.

To add an agent, implement the `Provider` interface in `internal/provider`, register it in `provider.go`, and add its command to the switch in `cmd/csm/main.go`.

## Contributing

Issues and pull requests are welcome. Please run `go vet ./...` and `go test ./...` before opening a pull request.
