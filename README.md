# csm — Code Session Manager

`csm` is a local profile and session manager for coding agents.

**Supported providers: Claude Code, Codex, Gemini CLI and GitHub Copilot CLI.**
Most of this document describes Claude Code. [Codex](#codex),
[Gemini CLI](#gemini-cli) and [GitHub Copilot CLI](#github-copilot-cli) list
what is different for each, including what has not yet been tested with a
signed-in account. Gemini CLI and Copilot CLI accounts are switched by hand;
csm cannot detect their limits.

It keeps several independently authenticated Claude Code accounts on one Mac,
each in its own isolated profile, and lets you move a working session between
them without logging out and back in. If an account hits its usage limit, `csm`
can checkpoint the session, stop Claude Code, restart it under the next account
in the same project, and resume the same conversation. Codex accounts work the
same way. An account is only ever switched with accounts of the same provider.

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
├── accounts/<name>/     CLAUDE_CONFIG_DIR, CODEX_HOME, GEMINI_CLI_HOME or COPILOT_HOME for each account (owned by the agent)
├── usage/<name>.json    last usage figures seen for each account
└── projects/<id>/       Claude Code state for a project (others add their name, e.g. <id>-codex)
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

Requires Go 1.25+ and at least one of Claude Code, Codex, Gemini CLI and GitHub Copilot CLI.

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

`setup` creates `~/.csm`, looks for every supported agent, reports their
versions, and runs the fresh-profile isolation check for each one it finds. An
agent that isn't installed is skipped. `setup` fails only if no installed agent
can isolate profiles.

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
├── provider.go     the Provider interface and shared vocabulary: StopReason, Identity, Features, Failure
├── claude.go       everything specific to Claude Code
├── claude_fake.go  a fake Claude Code for tests
├── codex.go        everything specific to Codex
├── codex_fake.go   a fake Codex for tests
├── gemini.go       everything specific to Gemini CLI
├── gemini_fake.go  a fake Gemini CLI for tests
├── copilot.go      everything specific to GitHub Copilot CLI
└── copilot_fake.go a fake GitHub Copilot CLI for tests
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
| `LaunchArgs(csmPath)` | `--settings` with `StopFailure`/`SessionStart` hooks and a status line |
| `FetchUsage` | one minimal `claude -p … --output-format stream-json` request, reading `rate_limit_event` |
| `ParseUsage`, `UserStatusLine` | status-line `rate_limits`; the user's own status line to pass through to |
| `ParseFailure`, `ParseSessionStart` | hook payloads, plus usage-limit classification |
| `NewSessionArgs`, `HasSessionFlag` | `--session-id`, `--append-system-prompt` |
| `CarrySession(from, to, id)` | copies `projects/*/<id>.jsonl` between profiles and returns `--resume <id>` |
| `LinkUserConfig`, `LogoutCommand` | `~/.claude` symlinks, `claude auth logout` |

This method set is the `provider.Provider` interface, implemented by
`provider.Claude`, `provider.Codex`, `provider.Gemini` and `provider.Copilot`.
Each provider's ID is also its command (`csm claude`, `csm codex`, `csm gemini`,
`csm copilot`). The hook payload parsers stay on `provider.Claude`, because only
Claude Code calls `csm hook`. `Version` and `Features` run every agent except
Claude Code with a throwaway profile, because they write to their home
directory even for `--version`.

Codex implements the same methods differently:

| Method | Codex implementation |
| --- | --- |
| `Env(profileDir)` | sets `CODEX_HOME` |
| `CheckEnv()` | rejects `CODEX_ACCESS_TOKEN` and `CODEX_API_KEY` |
| `Identity(profileDir)` | `account/read` on a short-lived `codex app-server` |
| `Login(profileDir)` | `codex login` |
| `LaunchArgs` | `--no-daemon`; no hooks |
| `FetchUsage` | `account/rateLimits/read` on the app-server |
| `LimitPollInterval` | one minute (Claude Code returns zero and uses its failure hook) |
| `CurrentSession` | `thread/list` for the project directory (Claude Code uses its `SessionStart` hook) |
| `CarrySession(from, to, id)` | `thread/read` for the rollout path, copies that file, and returns `resume <id>` |
| `LinkUserConfig`, `LogoutCommand` | `~/.codex` symlinks, `codex logout` |

## Testing

```bash
go test ./...
go vet ./...
csm test failover
csm test failover codex
```

The tests never need a real agent installed. The fake Codex
(`internal/provider/codex_fake.go`) implements `login`, `resume` and the
app-server requests csm sends. The fake Gemini CLI
(`internal/provider/gemini_fake.go`) signs in on its first launch and
implements `--session-id` and `--session-file`. The tests use a fake Claude Code
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

## Codex

Codex accounts use the same commands. The differences:

```bash
csm account add work-codex --provider codex   # runs `codex login` for the new profile
csm codex                                     # arguments after `codex` go to Codex
csm next codex                                # next ready Codex account
csm current codex
csm test failover codex
```

`csm use <name>` works for either provider, because account names are unique.
`csm next` and `csm current` without a provider mean Claude Code, unless a csm
session is running or Codex is the only provider with accounts. Claude Code and
Codex can run in the same project at the same time; each has its own session
state.

- **Profiles.** Each account's directory is its `CODEX_HOME`. A new profile
  starts with Codex's defaults, so Codex asks again whether it trusts each
  project. `--link-settings` symlinks `config.toml`, `AGENTS.md`, `hooks.json`,
  `prompts` and `rules` from `~/.codex`.
- **Identity.** csm starts a short-lived `codex app-server` for the profile and
  asks it `account/read`. The answer holds the account email and the Codex home
  in use, never a token. Only ChatGPT sign-ins are accepted. A profile signed in
  with an API key is rejected.
- **Environment.** csm refuses to run if `CODEX_ACCESS_TOKEN` or
  `CODEX_API_KEY` is set.
- **Usage.** `csm usage` asks the app-server `account/rateLimits/read`. It sends
  no message and uses no tokens.
- **Limits.** Codex has no hook that reports a failed turn. While `csm codex`
  runs, csm reads the account's limits once a minute, and once more when Codex
  exits. An account counts as limited when OpenAI reports a reached limit
  (`rateLimitReachedType`) and the account has no credits to continue on. With
  auto failover on, csm then switches; otherwise it offers to switch when Codex
  exits. Detection can lag the limit by up to a minute.
- **Background server.** csm starts Codex with `--no-daemon`, so stopping Codex
  for a switch also stops the session's work.
- **Session continuation.** csm asks the app-server which thread was last
  updated in the project directory since Codex started, copies that thread's
  rollout file (`sessions/YYYY/MM/DD/rollout-…-<id>.jsonl`) into the target
  profile, and starts `codex resume <id>`. If the thread is stored in Codex's
  paginated history instead of a single file, csm starts a new session. Codex
  takes no handoff note, so that new session starts without one.

### What has been checked

Checked against the real Codex CLI 0.160.0, signed out: profile isolation
through `CODEX_HOME`, the signed-out `account/read` answer, the feature probe,
`csm setup` and `csm doctor`, finding a session with `thread/list`, and copying
a real rollout file into a second profile where Codex then lists and reads it.

Not yet checked, because it needs a signed-in ChatGPT account: the signed-in
`account/read` and `account/rateLimits/read` answers (csm follows Codex's
published app-server schema), `codex resume` of a carried session in the
terminal UI, and how Codex reports a real reached limit.

## Gemini CLI

Gemini CLI gets profiles, manual switching and session carry-over. It does not
get usage figures or automatic failover.

```bash
csm account add work-gemini --provider gemini
csm gemini                                    # arguments after `gemini` go to Gemini CLI
csm next gemini                               # or: csm use <name>
csm current gemini
```

- **Profiles.** Each account's directory is its `GEMINI_CLI_HOME`. Gemini CLI
  keeps everything in a `.gemini` directory inside it. `--link-settings`
  symlinks `settings.json`, `GEMINI.md`, `commands`, `extensions` and `skills`
  from `~/.gemini`.
- **Login.** Gemini CLI has no login command. `csm account add` opens Gemini
  CLI for the new profile. Choose "Sign in with Google", finish in the browser,
  then type `/quit`. By default the login is the file
  `<profile>/.gemini/oauth_creds.json`, so it stays inside the profile.
- **Identity.** Gemini CLI has no command that reports the signed-in account.
  csm treats a profile as signed in when that login file exists (csm checks
  only that it exists and never reads it) and takes the email from
  `google_accounts.json`, which Gemini CLI writes at sign-in. This is weaker
  than the Claude Code and Codex checks: csm reads what was recorded, it does
  not ask the agent. The fresh-profile check cannot detect a shared login here.
- **Environment.** csm refuses to run if `GEMINI_API_KEY`,
  `GOOGLE_GENAI_USE_VERTEXAI` or `GOOGLE_GEMINI_BASE_URL` is set, because they
  select a login other than the profile's. It also refuses
  `GEMINI_FORCE_ENCRYPTED_FILE_STORAGE`, which moves the login into a Keychain
  entry that every profile would share.
- **Usage and limits.** Gemini CLI reports neither outside its own screen, and
  it has no hook for a failed turn. `csm usage` shows nothing for these
  accounts and `csm auto` has no effect on them. When Gemini CLI says a quota
  is used up, run `csm next gemini` from another terminal.
- **Session continuation.** csm starts new sessions with `--session-id`. On a
  switch it finds the session file last written for the project directory and
  starts the next account with `--session-file <that file>`. Gemini CLI imports
  the conversation's user and model messages into a new session. csm copies
  nothing into the target profile.

### What has been checked

Checked against the real Gemini CLI 0.62.0, signed out: isolation through
`GEMINI_CLI_HOME`, the feature probe, `csm setup` and `csm doctor`, the session
file layout and project hash, finding a session, and a real `--session-file`
import into a second profile.

Not yet checked, because it needs a signed-in Google account: the sign-in flow
that `csm account add` opens, the files Gemini CLI writes at sign-in (csm
follows Gemini CLI's source), and an imported session continuing in the
terminal UI.

## GitHub Copilot CLI

Copilot CLI gets the same reduced support as Gemini CLI: profiles, manual
switching and session carry-over, with no usage figures and no automatic
failover. It is the least verified provider; see below.

```bash
csm account add work-copilot --provider copilot   # runs `copilot login` for the new profile
csm copilot                                       # arguments after `copilot` go to Copilot CLI
csm next copilot                                  # or: csm use <name>
```

- **Profiles.** Each account's directory is its `COPILOT_HOME`.
  `--link-settings` symlinks `settings.json`, `mcp-config.json`,
  `copilot-instructions.md`, `agents` and `skills` from `~/.copilot`.
- **Login.** `copilot login` records the signed-in GitHub user in the profile's
  `config.json`. The token itself goes to the macOS Keychain, which all
  profiles share; the profile only decides which user is used.
- **Identity.** Copilot CLI has no command that reports the signed-in user. csm
  reads the GitHub login name from `config.json`, so an account shows a
  username, not an email. As with Gemini CLI, csm reads what was recorded and
  does not ask the agent.
- **The `gh` fallback.** A Copilot CLI with no login of its own asks the `gh`
  command for a token (`gh auth token`). csm never starts a profile it sees as
  signed out. But if a profile's stored token stops working, Copilot CLI can
  fall back to your `gh` user without csm noticing.
- **Environment.** csm refuses to run if `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`,
  `GITHUB_TOKEN` or `COPILOT_PROVIDER_BASE_URL` is set, because each overrides
  the profile's login.
- **Session continuation.** csm starts new sessions with `--session-id`. On a
  switch it copies the session's folder (`session-state/<id>/`) into the target
  profile and starts `copilot --resume <id>`.
- **Files outside the profile.** Copilot CLI keeps a package cache in
  `~/Library/Caches/copilot` and a device ID under
  `~/Library/Application Support/Microsoft`. Neither holds a login, and csm
  cannot move them.

### What has been checked

Checked against the real Copilot CLI 1.0.91, signed out: the flags csm uses,
`COPILOT_HOME`, `csm setup`, the `config.json` a signed-out launch writes, and
the `gh auth token` fallback.

Not checked, because it needs a signed-in GitHub account and Copilot CLI's
logic is in a closed binary: the shape of the signed-in user record in
`config.json`, the layout of `session-state/`, how the Keychain entry is named,
and a carried session resuming. If the user record has a different shape,
`csm account add` reports the profile as not logged in and nothing else runs.

## Supported platform

macOS (primary, tested). The code builds on Linux and uses the same profile
model, but it hasn't been verified there.
