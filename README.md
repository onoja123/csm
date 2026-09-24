# csm — Code Session Manager

`csm` is a local profile and session manager for coding agents.

**Supported provider: Claude Code.** Other agents (Codex and others) are
possible later through the provider boundary described under
[Development](#development). They are not implemented.

It keeps several independently authenticated Claude Code accounts on one Mac,
each in its own isolated profile, and lets you move a working session between
them without logging out and back in. If an account hits its usage limit, `csm`
can checkpoint the session, stop Claude Code, restart it under the next account
in the same project, and resume the same conversation.

`csm` does not bypass authentication, rate limits or usage limits. Every
account logs in through Claude Code's own `claude auth login` flow, and `csm`
never reads, copies or stores credentials. It only decides which profile
directory Claude Code starts with. Using several accounts must still follow the
terms that apply to each of them.

## Why it exists

If you have a personal, a work and a backup Claude account, switching between
them normally means `/logout`, `/login`, a browser round-trip and a restarted
session each time. `csm` does that authentication once per account, then makes
switching a local operation.

## How account profiles work

Each account is a directory under `~/.csm/accounts/<name>`. `csm` launches
Claude Code with `CLAUDE_CONFIG_DIR` pointing at that directory, so each profile
gets its own settings, history and login.

On macOS Claude Code keeps credentials in the Keychain. `csm` doesn't trust a
changed config directory alone to isolate credentials. It checks isolation with
Claude Code's own `claude auth status --json`:

- **Fresh profile check.** Before the first login, a new profile must report
  *not logged in*. If an empty directory already shows a login, credentials are
  shared across profiles. `csm` stops and changes nothing.
- **Config directory check.** Claude Code must report the profile's directory
  as its `configDirectory`.
- **Identity check.** After login, `csm` records the email the profile reports.
  Before every launch and switch it checks the profile still reports that same
  email. Two profiles that report the same identity are rejected.

An account counts as *ready* only after these checks pass.

```
~/.csm/
├── config.json          active account, order, auto failover
├── accounts.json        names, profile paths, verified identity, cooldowns
├── accounts/<name>/     CLAUDE_CONFIG_DIR for each account (owned by Claude Code)
└── projects/<id>/
    ├── session.json     the running csm-managed session (removed on exit)
    ├── checkpoint.json  last handoff metadata
    ├── transition.json  present only while a switch is in progress
    └── history/         every checkpoint
```

## Security model

- Tokens, cookies and Keychain secrets are never read, printed, logged or
  written by `csm`. `accounts.json` contains names, paths, timestamps and the
  account email that `claude auth status` reports.
- Authentication is always Claude Code's own `claude auth login`.
- If `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` or `CLAUDE_CODE_OAUTH_TOKEN` is
  set, `csm` refuses to run, because those override per-profile login.
- `~/.csm` is created with mode `0700` and files with `0600`.
- `csm` never runs a git command that modifies anything. It reads
  `rev-parse --show-toplevel`, `branch --show-current` and `status --short`.

## Installation

Requires Go 1.25+ and Claude Code.

```bash
go build -o csm ./cmd/csm
mv csm ~/.local/bin/    # or any directory on your PATH
```

To stamp a version:

```bash
go build -ldflags "-X main.version=0.1.0" -o csm ./cmd/csm
```

## Setup

```bash
csm setup
```

`setup` creates `~/.csm`, finds Claude Code, reports its version, and runs the
fresh-profile isolation check. If the installed Claude Code can't isolate
profiles, `setup` says so and stops.

## Adding accounts

```bash
csm account add personal
csm account add work
csm account add backup
```

Each command creates the profile, confirms it starts logged out, runs
`claude auth login` for it (log in in the browser as usual), and then verifies
it. The first account added becomes active.

New profiles start with Claude Code's defaults. To reuse your existing
`~/.claude` settings, `CLAUDE.md`, agents, commands and skills in a profile,
add it with `--link-settings`. This symlinks those files into the profile.
Changes made from any linked profile then write through to `~/.claude`.

```bash
csm account add work --link-settings
```

Other account commands:

```bash
csm account login work     # log in again, e.g. after the login expired
csm account disable backup # skip it when switching
csm account enable backup
csm account remove backup  # forget it; the profile directory is kept
```

## Running Claude

```bash
cd ~/Documents/polishpad
csm claude                 # arguments after `claude` go to Claude Code
```

Claude Code runs in your terminal exactly as `claude` would, with the same
stdin, stdout, stderr, working directory and exit status. `csm` doesn't capture
its output.

A switch relaunches Claude Code with only the resume flags. Flags you passed on
the first launch, such as `--model`, are not repeated.

## Switching accounts

```bash
csm use work      # or: csm use 2
csm next          # next ready account in the configured order
csm current       # prints the active account name
csm accounts
csm status
```

If no session is running, these change the account the next `csm claude` uses.

If `csm claude` is running (run the command from the project directory, or from
anywhere when only one session is running), `use` and `next` switch the running
session. The managing terminal saves a checkpoint, stops Claude Code, and
restarts it under the new account with the same conversation.

`next` skips accounts that are disabled, not authenticated, or cooling down.

## Checking usage

```bash
csm usage              # live check of every signed-in account
csm usage work backup  # only these accounts (names or numbers)
csm usage --cached     # last saved figures, no check
```

```
● personal     updated 0s ago
    5-hour     0%   resets 05:10
    7-day     13%   resets Sun 01:00
```

The live check doesn't need a running session. For each enabled, signed-in
account, in parallel, csm first confirms the profile still reports the same
identity. It then runs one minimal request:

```
claude -p ok --model haiku --tools "" --system-prompt "Reply with: ok" \
  --strict-mcp-config --disable-slash-commands --setting-sources "" \
  --no-session-persistence --output-format stream-json --verbose
```

It reads the `rate_limit_event` Claude Code includes in that output
(`unifiedWindows.five_hour` and `seven_day`). Each check sends about 500 tokens
on Haiku per account (about $0.0009 at list price). On a Pro or Max plan that's
a tiny part of the allowance, not a charge. Nothing is saved to the account's
session history.

This is the only supported way to get the numbers outside a session. `/usage`
only works in the interactive screen, and anything more direct would mean
reading the account's login token, which csm never does.

While `csm claude` runs, csm also records usage for free from the status line
(`rate_limits` in the status-line input). Inside Claude the status line shows
`csm · work · 5h 42% · 7d 17%`. If you configured your own status line (in the
project's `.claude/settings*.json` or the profile's `settings.json`), csm runs
yours instead and still records usage.

`csm accounts` shows the last saved summary, such as `(5h 42% · 7d 17%)`.
Claude Code reports plan usage only for Pro and Max accounts.

## Automatic failover

```bash
csm auto on
csm auto status
csm auto off
```

### What triggers a switch

`csm` registers a
[`StopFailure` hook](https://code.claude.com/docs/en/hooks.md#stopfailure) for
each launch (through `--settings`, leaving your settings files untouched).
Claude Code runs this hook when a turn fails with an API error, and passes it a
structured `error` type and the error text it showed you.

An automatic switch happens only when **all** of these are true:

1. auto failover is on,
2. the hook reports `error: "rate_limit"` **and** the error text contains
   "limit" together with "usage" or "reset" (for example *"You've hit your
   limit · resets 5pm"*),
3. another enabled, authenticated account is not cooling down and has not hit
   a limit during this run,
4. you didn't stop the session yourself (SIGTERM/SIGHUP).

`classifyClaudeFailure` in `internal/provider/claude.go` implements these rules. Everything
else pauses and tells you what happened:

| Claude Code `error`                                          | csm reason       | Switches? |
| ------------------------------------------------------------ | ---------------- | --------- |
| `rate_limit` + usage-limit wording                           | `usage_limit`    | yes, if auto is on |
| `rate_limit` otherwise (e.g. "API Error: Rate limit reached") | `rate_limited`   | no        |
| `authentication_failed`, `oauth_org_not_allowed`, `account_on_hold` | `authentication` | no |
| `overloaded`, `server_error`                                 | `network`        | no        |
| anything else, including `billing_error`                     | `unknown`        | no        |
| Ctrl+C / Claude exits normally                               | —                | no        |

When an account hits a usage limit, it gets a local one-hour cooldown. `csm`
doesn't try to work out the server's real reset time. If every account hits a
limit in one run, `csm` stops with *"All configured accounts are currently
unavailable. Automatic failover paused."* instead of cycling.

If auto failover is off and a usage limit is reported, Claude Code stays open
and shows its own message. When you exit it, `csm` asks whether to switch to
the next account and resume.

The request that hit the limit is not retried automatically. After a switch,
send it again or type "continue".

## Session continuation: what is and isn't restored

**Always:** the same working directory, filesystem, git branch and uncommitted
changes. `csm` never touches them.

**Usually: the same conversation.** Claude Code stores each conversation as a
transcript in `<profile>/projects/<project>/<session-id>.jsonl`. `csm` learns
the session ID from the `SessionStart` hook and copies that transcript (and its
`<session-id>/` directory) into the target profile. It then starts Claude Code
with the supported `--resume <session-id>` flag, and the new account continues
the same conversation with its full history.

That transcript location is where Claude Code documents it keeps transcripts.
The file format is internal, and `csm` only copies it as-is. If the transcript
can't be found, `csm` says so. It starts a new session in the same directory and
adds a short handoff note (previous account, current `git status`) to the system
prompt with `--append-system-prompt`. It never claims a conversation was carried
over when it wasn't.

## Recovery

- **Interrupted switch:** each switch writes `transition.json` before it stops
  Claude and deletes it once the new process is running. If `csm` dies in
  between, `csm status` reports *"Previous switch did not complete"*. The next
  `csm claude` in that project resumes the handoff (after asking, unless auto
  failover is on).
- **Reboot:** all state is on disk, and logins belong to each profile's Claude
  Code credentials, so accounts survive a restart.
- **Expired login:** the pre-launch identity check fails with instructions to
  run `csm account login <name>`. `csm` never tries to repair credentials.

## Troubleshooting and `csm doctor`

```bash
csm doctor
```

checks the csm state directory and permissions, the Claude executable and
version, auth-overriding environment variables, profile and credential
isolation (the fresh-profile probe), every account's login and identity
(including duplicates), the terminal, session continuation flags, and git.
Every failure says why and what to do next. `csm doctor` never modifies
credentials.

Common problems:

- *"a brand-new profile directory already reports a logged-in account"*: this
  Claude Code build shares credentials across config directories. Upgrade
  Claude Code. `csm` won't work around it.
- *"identifies as X, but it was set up as Y"*: the profile's login changed.
  Run `csm account login <name>`.
- Add `--debug` to any command (`csm --debug claude`) to log account selection,
  config directory, arguments, exit status and classification to stderr. Values
  of `--settings` and `--append-system-prompt` are omitted from the log.

## Development

```
cmd/csm/
├── main.go       command dispatch
├── commands.go   setup, doctor, status, accounts, use, next, auto
├── state.go      config.json / accounts.json, account order and selection
├── project.go    project detection, session, checkpoint, transition files
├── profile.go    profile verification and the isolation probe
├── run.go        the managed agent process and the switch loop
├── hook.go       `csm hook …`, invoked by the agent's hooks and status line
├── usage.go      `csm usage` and the saved usage records
└── selftest.go   `csm test failover`

internal/provider/
├── provider.go     shared vocabulary: StopReason, Identity, Features, Failure
├── claude.go       everything specific to Claude Code
└── claude_fake.go  a fake Claude Code for tests
```

The only dependency is the standard library.

### Provider boundary

The core (`cmd/csm`) handles accounts, order, checkpoints, transitions,
failover, and process supervision. It never mentions `CLAUDE_CONFIG_DIR`, hook
names, CLI flags, or transcript paths. It asks the provider for:

| Method | Claude Code implementation |
| --- | --- |
| `Env(profileDir)` | sets `CLAUDE_CONFIG_DIR` |
| `CheckEnv()` | rejects `ANTHROPIC_API_KEY` and similar overrides |
| `Version()`, `Features()` | `--version`, `--help` |
| `Identity(profileDir)` | `claude auth status --json` |
| `Login(profileDir)` | `claude auth login` |
| `SettingsArgs(csmPath)` | `--settings` with `StopFailure`/`SessionStart` hooks and a status line |
| `FetchUsage` | one minimal `claude -p … --output-format stream-json` request, reading `rate_limit_event` |
| `ParseUsage`, `UserStatusLine` | status-line `rate_limits`; the user's own status line to pass through to |
| `ParseFailure`, `ParseSessionStart` | hook payloads, plus usage-limit classification |
| `ResumeArgs`, `NewSessionArgs`, `HasSessionFlag` | `--resume`, `--session-id`, `--append-system-prompt` |
| `CarrySession(from, to, id)` | copies `projects/*/<id>.jsonl` between profiles |
| `LinkUserConfig`, `LogoutCommand` | `~/.claude` symlinks, `claude auth logout` |

`provider.Claude` is a concrete type because it's the only provider. When a
second provider is added, this method set becomes a Go interface and
`csm claude` gets a sibling command such as `csm codex`.

## Testing

```bash
go test ./...
go vet ./...
csm test failover
```

The tests never need Claude Code installed. They use a fake Claude Code
(`internal/provider/claude_fake.go`, run through the `csm` binary in a hidden mode) that implements `auth login`,
`auth status --json`, `--resume`, `--session-id`, and runs the hooks it's given.
The failover tests drive the real runner through:

- a usage limit on account A → checkpoint → stop → switch to B → `--resume` of
  the same session, with the git branch and uncommitted changes unchanged,
- every account limited → one switch, then pause,
- an `overloaded` error → no switch,
- a manual `csm use` while a session is running,
- recovery of an interrupted switch.

`csm test failover` runs the same simulation from the installed binary.

### Real-machine verification

Automated tests can't prove real Keychain isolation. After installing:

1. `csm account add a`, `csm account add b`, `csm account add c`, each logged
   in as a different account.
2. `csm doctor`: every account must show a distinct email.
3. `csm use a && csm claude`, then run `/status` inside Claude to confirm the
   account. Repeat for b, c, then a, b, c again.
4. With a session running, `csm next` from another terminal: a → b → c → a.
   Confirm the conversation continues each time.
5. Check `git status` in the project before and after: nothing changes.

Don't deliberately exhaust real usage to test failover. `csm test failover`
covers that path.

## Supported platform

macOS (primary, tested). The code builds on Linux and uses the same profile
model, but it hasn't been verified there.
