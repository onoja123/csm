#!/bin/sh
# End-to-end check of the csm binary against the fake agents it ships with.
# Usage: scripts/smoke.sh /absolute/path/to/csm
set -u

CSM_BIN="${1:?usage: smoke.sh /absolute/path/to/csm}"
export CSM_BIN
WORK="$(mktemp -d)"
WORK="$(cd "$WORK" && pwd -P)"
trap 'rm -rf "$WORK"' EXIT

export CSM_HOME="$WORK/home"
PROJECT="$WORK/my project"
FAILED=0

for agent in claude codex gemini copilot; do
	printf '#!/bin/sh\nexec "$CSM_BIN" __fake-%s "$@"\n' "$agent" >"$WORK/$agent"
	chmod +x "$WORK/$agent"
done
export CSM_CLAUDE="$WORK/claude" CSM_CODEX="$WORK/codex" CSM_GEMINI="$WORK/gemini" CSM_COPILOT="$WORK/copilot"
unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN CLAUDE_CODE_OAUTH_TOKEN CODEX_ACCESS_TOKEN CODEX_API_KEY \
	GEMINI_API_KEY GOOGLE_GENAI_USE_VERTEXAI GOOGLE_GEMINI_BASE_URL GEMINI_FORCE_ENCRYPTED_FILE_STORAGE \
	COPILOT_GITHUB_TOKEN GH_TOKEN GITHUB_TOKEN COPILOT_PROVIDER_BASE_URL CSM_FAKE_LIMIT CSM_FAKE_HOLD CSM_FAKE_ERROR

csm() { "$CSM_BIN" "$@" </dev/null; }

pass() { printf 'PASS  %s\n' "$1"; }
fail() {
	printf 'FAIL  %s\n' "$1"
	FAILED=1
}

# check NAME COMMAND...: the command must succeed.
check() {
	name="$1"
	shift
	if "$@" >"$WORK/out" 2>&1; then pass "$name"; else
		fail "$name"
		sed 's/^/      /' "$WORK/out"
	fi
}

# refuse NAME COMMAND...: the command must fail.
refuse() {
	name="$1"
	shift
	if "$@" >"$WORK/out" 2>&1; then
		fail "$name (succeeded but should not have)"
		sed 's/^/      /' "$WORK/out"
	else pass "$name"; fi
}

# says NAME TEXT: the last command's output must contain TEXT.
says() { if grep -qF -- "$2" "$WORK/out"; then pass "$1"; else
	fail "$1 (output lacks: $2)"
	sed 's/^/      /' "$WORK/out"
fi; }

mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }
is() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (got '$2', want '$3')"; fi; }

wait_for() {
	i=0
	while [ $i -lt 100 ]; do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 0.1
		i=$((i + 1))
	done
	return 1
}

mkdir -p "$PROJECT"
git -C "$PROJECT" init -q -b main
printf 'package main\n' >"$PROJECT/main.go"
git -C "$PROJECT" add .
git -C "$PROJECT" -c user.name=csm -c user.email=csm@example.test commit -q -m init
printf 'package main // uncommitted\n' >"$PROJECT/main.go"
cd "$PROJECT" || exit 1
GIT_BEFORE="$(git status --short --branch)"

echo "== setup"
check "setup succeeds" csm setup
for name in "Claude Code" "Codex" "Gemini CLI" "GitHub Copilot CLI"; do says "setup finds $name" "Provider: $name"; done
is "state directory is private" "$(mode "$CSM_HOME")" 700
is "config.json is private" "$(mode "$CSM_HOME/config.json")" 600

echo "== accounts"
check "add Claude Code account a1" csm account add a1
check "add Claude Code account a2" csm account add a2
check "add Codex account cx1" csm account add cx1 --provider codex
check "add Codex account cx2" csm account add cx2 --provider codex
check "add Gemini CLI account g1" csm account add g1 --provider gemini
check "add Gemini CLI account g2" csm account add g2 --provider gemini
check "add Copilot CLI account cp1" csm account add cp1 --provider copilot
check "add Copilot CLI account cp2" csm account add cp2 --provider copilot
is "accounts.json is private" "$(mode "$CSM_HOME/accounts.json")" 600
is "profile directory is private" "$(mode "$CSM_HOME/accounts/a1")" 700
is "one profile directory per account" "$(ls "$CSM_HOME/accounts" | tr '\n' ' ')" "a1 a2 cp1 cp2 cx1 cx2 g1 g2 "
refuse "duplicate account name is rejected" csm account add a1
refuse "unknown provider is rejected" csm account add x1 --provider nope
for bad in "../../evil" "test;echo evil" '$(whoami)' "with space" '"quoted"' "Upper" ""; do
	refuse "account name [$bad] is rejected" csm account add "$bad"
done
is "no stray profile was created" "$(ls "$CSM_HOME/accounts" | wc -l | tr -d ' ')" 8
is "nothing was written outside the state directory" "$(ls "$WORK" | tr '\n' ' ')" "claude codex copilot gemini home my project out "

echo "== switching with no session running"
is "first account is active" "$(csm current)" a1
is "each provider has its own active account" "$(csm current codex)" cx1
check "next moves to the next account" csm next
is "active account after next" "$(csm current)" a2
is "next did not touch another provider" "$(csm current codex)" cx1
check "use by position" csm use 1
is "active account after use 1" "$(csm current)" a1
refuse "use of an unknown account fails" csm use nobody
refuse "use of an out-of-range position fails" csm use 99
check "disable a2" csm account disable a2
refuse "next skips a disabled account" csm next
refuse "use of a disabled account fails" csm use a2
check "enable a2" csm account enable a2
check "next works again after enable" csm next
check "back to a1" csm use a1

echo "== identity"
cp "$CSM_HOME/accounts/a1/fake-auth" "$WORK/a1-auth"
printf 'intruder@example.test' >"$CSM_HOME/accounts/a1/fake-auth"
refuse "a profile that changed identity cannot be launched" csm claude
says "the mismatch is named" "identifies as intruder@example.test"
refuse "doctor reports the mismatch" csm doctor
says "doctor names the account" "Account a1"
cp "$WORK/a1-auth" "$CSM_HOME/accounts/a1/fake-auth"
rm -f "$CSM_HOME/accounts/a2/fake-auth"
refuse "a logged-out profile cannot be selected" csm use a2
says "the user is told to log in" "csm account login a2"
check "log in again" csm account login a2
rm "$WORK/a1-auth"

echo "== environment overrides"
for pair in claude:ANTHROPIC_API_KEY claude:ANTHROPIC_AUTH_TOKEN claude:CLAUDE_CODE_OAUTH_TOKEN \
	codex:CODEX_ACCESS_TOKEN codex:CODEX_API_KEY \
	gemini:GEMINI_API_KEY gemini:GOOGLE_GENAI_USE_VERTEXAI gemini:GOOGLE_GEMINI_BASE_URL gemini:GEMINI_FORCE_ENCRYPTED_FILE_STORAGE \
	copilot:COPILOT_GITHUB_TOKEN copilot:GH_TOKEN copilot:GITHUB_TOKEN copilot:COPILOT_PROVIDER_BASE_URL; do
	agent="${pair%%:*}"
	var="${pair#*:}"
	refuse "$agent refuses to run with $var set" env "$var=s3cret-value-123" "$CSM_BIN" "$agent"
	says "$var is named in the error" "$var"
	if grep -q "s3cret-value-123" "$WORK/out"; then fail "$var value was printed"; else pass "$var value is not printed"; fi
done

echo "== running each agent"
for agent in claude codex gemini copilot; do check "csm $agent runs and exits cleanly" csm "$agent"; done
check "arguments reach the agent unchanged" csm claude --model "some model" "a prompt with spaces" --flag=value -x
is "git state is untouched after runs" "$(git status --short --branch)" "$GIT_BEFORE"

echo "== usage"
check "usage check succeeds" csm usage
says "Claude Code usage is shown" "5-hour"
says "Gemini CLI usage is reported as unavailable" "cannot report usage outside a session"
check "cached usage succeeds" csm usage --cached

echo "== switching a running session"
running() { ls "$CSM_HOME"/projects/*/session.json && grep -rqs '"profile":"a1"' "$CSM_HOME/accounts/a1"; }
CSM_FAKE_HOLD=a1 "$CSM_BIN" claude </dev/null >"$WORK/run.out" 2>&1 &
pid=$!
if wait_for running; then pass "the running session is recorded"; else fail "the running session is recorded"; fi
refuse "a second session in the same project is refused" csm claude
check "next reaches the running session" csm next
wait "$pid"
is "the managed session exited cleanly" "$?" 0
is "the active account moved" "$(csm current)" a2
if grep -rqs '"profile":"a2","resumed":true' "$CSM_HOME/accounts/a2/projects"; then pass "the conversation was resumed under a2"; else
	fail "the conversation was resumed under a2"
	sed 's/^/      /' "$WORK/run.out"
fi
if ls "$CSM_HOME"/projects/*/checkpoint.json >/dev/null 2>&1; then pass "a checkpoint was saved"; else fail "a checkpoint was saved"; fi
if ls "$CSM_HOME"/projects/*/transition.json >/dev/null 2>&1; then fail "the switch record was cleaned up"; else pass "the switch record was cleaned up"; fi
if ls "$CSM_HOME"/projects/*/session.json >/dev/null 2>&1; then fail "the session record was cleaned up"; else pass "the session record was cleaned up"; fi
is "git state is untouched after a switch" "$(git status --short --branch)" "$GIT_BEFORE"
check "back to a1" csm use a1

echo "== two agents in one project"
CSM_FAKE_HOLD=a1,cx1 "$CSM_BIN" claude </dev/null >"$WORK/claude.out" 2>&1 &
claude_pid=$!
CSM_FAKE_HOLD=a1,cx1 "$CSM_BIN" codex </dev/null >"$WORK/codex.out" 2>&1 &
codex_pid=$!
both() { [ "$(ls "$CSM_HOME"/projects/*/session.json | wc -l | tr -d ' ')" = 2 ] && grep -rqs '"profile":"cx1"' "$CSM_HOME/accounts/cx1" && grep -rqs '"profile":"a1"' "$CSM_HOME/accounts/a1"; }
if wait_for both; then pass "both sessions run side by side"; else
	fail "both sessions run side by side"
	sed 's/^/      claude: /' "$WORK/claude.out"
	sed 's/^/      codex: /' "$WORK/codex.out"
fi
refuse "next without a provider is refused as ambiguous" csm next
check "next codex switches only Codex" csm next codex
wait "$codex_pid"
is "Codex moved to cx2" "$(csm current codex)" cx2
is "Claude Code stayed on a1" "$(csm current)" a1
if kill -0 "$claude_pid" 2>/dev/null; then pass "the Claude Code session kept running"; else fail "the Claude Code session kept running"; fi
check "use a2 switches the Claude Code session" csm use a2
wait "$claude_pid"
is "Claude Code moved to a2" "$(csm current)" a2
if grep -rqs '"profile":"cx2","resumed":true' "$CSM_HOME/accounts/cx2/sessions"; then pass "the Codex conversation was resumed under cx2"; else fail "the Codex conversation was resumed under cx2"; fi
if grep -rqs '"profile":"cx1"' "$CSM_HOME/accounts/a2" "$CSM_HOME/accounts/a1"; then fail "a Codex session leaked into a Claude Code profile"; else pass "no session crossed between providers"; fi

echo "== Gemini CLI and Copilot CLI running-session switch"
for pair in gemini:g1:g2 copilot:cp1:cp2; do
	agent="${pair%%:*}"
	rest="${pair#*:}"
	from="${rest%%:*}"
	to="${rest#*:}"
	CSM_FAKE_HOLD="$from" "$CSM_BIN" "$agent" </dev/null >"$WORK/$agent.out" 2>&1 &
	pid=$!
	one() { ls "$CSM_HOME"/projects/*-"$agent"/session.json && grep -rqs "\"profile\":\"$from\"" "$CSM_HOME/accounts/$from"; }
	wait_for one
	check "next $agent reaches the running session" csm next "$agent"
	wait "$pid"
	is "$agent moved to $to" "$(csm current "$agent")" "$to"
	if grep -rqs "\"profile\":\"$to\",\"resumed\":true" "$CSM_HOME/accounts/$to"; then pass "the $agent conversation was resumed under $to"; else fail "the $agent conversation was resumed under $to"; fi
done

echo "== interrupted switch"
check "use a1" csm use a1
check "auto failover on" csm auto on
state="$(ls -d "$CSM_HOME"/projects/* | grep -v -e '-codex$' -e '-gemini$' -e '-copilot$' | head -1)"
printf '{"version":1,"project_dir":"%s","from":"a1","to":"a2","session_id":"","reason":"manual_switch","csm_pid":999999999}\n' "$PROJECT" >"$state/transition.json"
check "status still works" csm status
says "status reports the unfinished switch" "switch did not complete"
check "the next run finishes the switch" csm claude
is "the switch target became active" "$(csm current)" a2
if [ -e "$state/transition.json" ]; then fail "the switch record was cleaned up"; else pass "the switch record was cleaned up"; fi
check "auto failover off" csm auto off

echo "== damaged state"
cp "$CSM_HOME/config.json" "$WORK/config.bak"
printf '{not json' >"$CSM_HOME/config.json"
refuse "a damaged config stops accounts" csm accounts
says "the damaged file is named" "config.json"
refuse "a damaged config stops a launch" csm claude
printf '{"version": 99}' >"$CSM_HOME/config.json"
refuse "a config from a newer version is refused" csm accounts
says "the version is named" "version 99"
cp "$WORK/config.bak" "$CSM_HOME/config.json"
rm "$WORK/config.bak"
check "accounts works again after repair" csm accounts

echo "== removal"
check "remove an account" csm account remove a2
if [ -d "$CSM_HOME/accounts/a2" ]; then pass "its profile directory is kept"; else fail "its profile directory is kept"; fi
refuse "a removed account cannot be used" csm use a2

echo "== failover simulation"
check "csm test failover" csm test failover
check "csm test failover codex" csm test failover codex

echo "== leftovers"
if pgrep -f "$CSM_BIN __fake-" >/dev/null 2>&1; then
	fail "a fake agent process was left running"
	pgrep -fl "$CSM_BIN __fake-" | cut -c1-120 | sed 's/^/      /'
else pass "no agent process was left running"; fi
is "git state is untouched at the end" "$(git status --short --branch)" "$GIT_BEFORE"

if [ "$FAILED" -ne 0 ]; then
	echo
	echo "SMOKE TEST FAILED"
	exit 1
fi
echo
echo "SMOKE TEST PASSED"
