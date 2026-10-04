# Cleanup audit

Audit of csm against the 79 rules in `go-deslop.md`. Rule numbers are in
brackets.

csm is a single-binary CLI. It has no HTTP server, database or queue. Data
enters in five places:

- command-line arguments (`cmd/csm/main.go`, `commands.go`)
- environment variables (`CSM_*`, and the agents' own auth variables)
- csm's own state files under `~/.csm` (`state.go`, `project.go`, `usage.go`)
- agent output: `claude auth status --json`, Claude Code hook payloads and
  stream output, the Codex app-server (JSON-RPC over stdio)
- agent files: Gemini CLI session and account files, Copilot CLI `config.json`
  and `session-state/`

## Baseline (before any change)

| Check | Result |
| --- | --- |
| `gofmt -l cmd internal` | clean |
| `go build ./...` | ok |
| `go vet ./...` | ok |
| `go test ./...` | ok (both packages) |
| `staticcheck ./...` | 2 findings, both ST1005 (see "Left alone") |

staticcheck is not set up in this repo. It was run once with
`go run honnef.co/go/tools/cmd/staticcheck@latest`, which added nothing to
`go.mod`.

## Search patterns [73]

| Pattern | Matches | Verdict |
| --- | --- | --- |
| `any` | 24 | 8 in production code, see findings 2.1 and 2.2; the rest are in fakes and tests |
| `map[string]any` | 16 | 4 in `codex.go` (finding 2.2), 12 in `codex_fake.go` (finding 2.5) |
| `interface{}`, `reflect.`, `recover(`, `panic(`, `sync.Pool`, `context.TODO` | 0 | nothing to do |
| `type .* interface` | 1 | `provider.Provider`, four implementations; kept (see "Left alone") |
| `context.Background` | 3 | all at the top of a CLI operation that has no caller context; kept |
| `go func` | 3 | process wait, usage poll, parallel usage refresh; all needed |
| `make([]` | 4 | 3 are indexed writes; 1 disappears with finding 2.1 |
| ignored results (`, _ :=`, bare calls) | 41 + 13 | findings 3.1, 3.2, 5.2; the rest are listed under "Left alone" |
| `TODO`, `FIXME` | 0 | none |

## Findings and planned fixes

### 1. Provider discovery (`internal/provider/provider.go`, each `Find*`)

1.1 `Find` repeats the same five lines for every provider, only to turn
`(Claude, error)` into `(Provider, error)`. The `Find*` functions have no other
caller. Fix: the `Find*` functions return `Provider`, and `Find` returns their
result directly. [10]

### 2. Codex app-server client (`internal/provider/codex.go`, `codex_fake.go`)

2.1 `rpc` accepts a list of calls and returns a list of replies, with
bookkeeping to match them up. Every caller sends exactly one call. `call` and
`callContext` are two wrappers on top. Fix: `rpc` sends one request and decodes
one result; `call` only adds the default timeout; `callContext`, `codexCall`
and `codexReply` are deleted. [6, 9, 74]

2.2 Request parameters are built as `map[string]any` literals although every
shape is known. Fix: one small struct per request. [1, 18]

2.3 The decoded rate-limit reply uses pointers for `primary`, `secondary`,
`individualLimit`, `credits` and `rateLimitReachedType`. For all five, JSON
`null` and the zero value mean the same thing to csm. Fix: plain values; the
nil checks go away. `account` in `account/read` stays a pointer, because
"signed out" is a real state. [4, 5]

2.4 `CurrentSession` treats a failed `thread/list` call as "no session" with no
trace. See finding 3.1.

2.5 `codex_fake.go` builds every reply from `map[string]any`. Fix: use the same
typed structs the client decodes into. The reply envelope keeps an `any`
result, because it carries a different payload per method. [1, 68]

### 3. Session lookup and carry-over (all providers, `cmd/csm/run.go`)

3.1 `CurrentSession` returns only a string, so Codex swallows an app-server
error and Gemini CLI and Copilot CLI swallow a bad glob pattern. A profile path
containing `[` is enough to produce one. Fix: `CurrentSession` returns
`(string, error)`; the runner logs the error with `--debug` and continues
exactly as today. No caller behaviour changes. [12]

3.2 `geminiSessions` and Copilot's session scan discard the `filepath.Glob`
error. Fixed together with 3.1. [12]

3.3 `Carried` is a struct holding two return values and is used at one call
site. Fix: `CarrySession` returns `(resumeArgs []string, sessionID string, err
error)`. [55]

3.4 `ResumeArgs` on Claude, Codex and Copilot has one caller each and returns a
two-element slice. Fix: inline. [6]

### 4. Copilot CLI identity (`internal/provider/copilot.go`)

4.1 `copilotConfig` holds two `*copilotUser` pointers. An absent user and an
empty login mean the same thing. Fix: plain values. [5]

### 5. Runner (`cmd/csm/run.go`)

5.1 `if r.features.Supervise` guards the launch arguments, but `cmdRun` has
already refused to run when `CanIsolate()` is false, so the branch is always
taken. Fix: remove the guard. [74]

5.2 `r.state.save()` after recording a cooldown and `r.state.saveUsage()` after
a usage check discard their errors. The checkpoint and transition saves next to
them log theirs. Fix: log at debug level the same way. Nothing new can fail.
[12]

### 6. Commands (`cmd/csm/commands.go`, `hook.go`)

6.1 When no agent is installed, `cmdDoctor` calls `provider.Find` a second time
just to get an error message. Fix: keep the first error from the loop. [74]

6.2 `cmdSetup` picks its error with `slices.Concat(broken, missing)[0]`. Fix:
two plain branches. [28, 70]

6.3 `stdinIsTerminal()` only forwards to `isTerminal(os.Stdin.Fd())` and has
two callers. Fix: inline. [6, 10]

6.4 `runStatusLine` takes a zero-value `provider.Claude` parameter only to call
a method on it. Fix: drop the parameter. [53]

### 7. Comments

Handled in a separate comment-only commit. Candidates: doc comments that repeat
the identifier, and comments that restate the next line.

### 8. Spacing and layout

Handled in a separate spacing-only commit, across every non-generated Go file.

## Left alone, and why

- **`provider.Provider` (22 methods).** Four real implementations, chosen at
  run time. Several methods are no-ops for some agents (`UserStatusLine`,
  `RedactArgs`, `UsageCheckNote`, `LimitPollInterval`). Splitting it into
  optional interfaces would replace them with type assertions, which is worse.
  [7, 8, 79]
- **`writeJSON(path, v any)` and `readJSON(path, v any)`.** They mirror
  `encoding/json`; every caller passes a concrete struct. [1]
- **`rpc(..., params, result any)` in the Codex client.** Same reason: it is
  the JSON boundary. [1]
- **`*int` request and reply IDs and the `*struct` error in the Codex client.**
  A notification has no ID and a successful reply has no error; both are real
  absences. [5, 69]
- **`recovery` returned as a pointer from `recoverTransition`.** `nil` means
  "nothing to recover"; the alternative is four return values. [69]
- **Named results on `setupProvider` and `handoffArgs`.** They name a bare
  `bool` and a `(args, id, resumed)` triple; no naked returns are used. [56]
- **Ignored errors in `cmd/csm/hook.go`.** Hooks run inside the agent and must
  never fail it; the comment on `runHook` says so. [12, 77]
- **Ignored `os.Remove` / `os.RemoveAll` results.** Best-effort removal of
  files that may not exist. [12]
- **`json.Marshal` of the hook settings and `filepath.Rel` inside `WalkDir`.**
  Neither can fail for the values passed. [12]
- **`filepath.Glob` in `liveSessions`.** The only possible error is a malformed
  pattern; returning it would change `liveSessions`' signature and every caller
  for a path csm creates itself. [12]
- **`detectProject`, `gitStatusShort`, `loadUsage` returning empty values on
  error.** "Not a git repository" and "no usage recorded yet" are normal
  states, not failures. [3, 69]
- **`cmd/csm/selftest.go` ignoring setup errors.** It is a simulation harness;
  a failed step shows up as a failed assertion. [68]
- **Per-provider one-line methods (`CheckEnv`, `Env`, `ID`, `Name`).** Required
  by the interface. [10]
- **`accountNamePattern` regular expression.** Boundary validation of user
  input. [47, 50]
- **Two staticcheck ST1005 findings** (`"Result: not ready"` in
  `reportDoctor`, and the "already managing" error ending in a full stop).
  Both strings are printed to the user as written; changing them changes CLI
  output. See "Open questions".

## Open questions

None of these were changed. Each needs a decision.

1. **Two staticcheck ST1005 findings.** `"Result: not ready"` (capitalised)
   and the "already managing ... first." error (ends in a full stop). Fixing
   them changes text the user sees.
2. **`csm status` and a corrupt transition file.** `cmdStatus` ignores the
   error from `loadTransition`, so a damaged `transition.json` is silently not
   shown. The next `csm <agent>` run does report it. Showing it in `status`
   would change that command's output. [12]
3. **Unknown provider in `accounts.json`.** `providerName` falls back to the
   raw ID when the provider is unknown. Validating the provider when the file
   is loaded would let that fallback go, but a hand-edited file would then fail
   at load instead of at use. [3, 50]
4. **Spacing rules where the brief is ambiguous.** The brief's example puts a
   blank line between `x, err := ...` and `if err != nil`, and after an opening
   `for {`; its written rules forbid both. The written rules were followed.
   "A blank line after a group of variable declarations" was applied to `var`
   statements only, not to every `:=`. Every `defer` gets blank lines around
   it, including `defer cancel()` directly after the context is created.
   One-line methods are now separated by blank lines.
5. **Struct field grouping.** Left as it was; no struct looked clearer with
   extra blank lines.
6. **Size of `provider.Provider`.** Kept as one interface (see "Left alone").
   Worth revisiting if more agents are added.

## Result

All findings in sections 1 to 6 were fixed. Commits, oldest first:

| Commit | Findings | Rules |
| --- | --- | --- |
| Return Provider from the Find functions | 1.1 | 10 |
| Send one typed request per Codex app-server call | 2.1, 2.2, 2.3, 2.5 | 1, 4, 5, 6, 9, 18, 68, 74 |
| Return errors from session lookup and plain values from session carry | 2.4, 3.1, 3.2, 3.3, 3.4 | 6, 12, 55 |
| Use plain values for the Copilot CLI signed-in user | 4.1 | 5 |
| Drop an always-true guard and log two ignored saves in the runner | 5.1, 5.2 | 12, 74 |
| Simplify four spots in the commands | 6.1, 6.2, 6.3, 6.4 | 6, 10, 28, 53, 70, 74 |
| Remove two comments that repeat the name and correct a stale one | 7 | 41 |
| Apply the spacing and layout rules | 8 | brief section 6 |

Behaviour: no command output, file format, flag or environment variable
changed. The only visible difference is under `--debug`, which now logs a
failed session lookup, a failed cooldown save and a failed usage save.

Comments: 2 removed (they repeated the identifier), 1 rewritten (it described
only two of the four agents). The rest explain a reason, an external quirk or
what a returned value means, and were kept.

Layout: blank lines were applied with a throwaway tool that is not part of the
repo, then checked: ignoring whitespace and line order, only the three files
with regrouped `const`/`var` blocks differ from the commit before.

### Final checks, compared with the baseline

| Check | Baseline | Final |
| --- | --- | --- |
| `gofmt -l cmd internal` | clean | clean |
| `go build ./...` | ok | ok |
| `go vet ./...` | ok | ok |
| `go test ./...` | ok | ok |
| `go test -race ./...` | ok | ok |
| `csm test failover`, `csm test failover codex` | pass | pass |
| `staticcheck ./...` | 2 findings (ST1005) | the same 2 findings |

## TODO and FIXME notes

None in the repository.
