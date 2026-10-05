# csm beta checklist

For testers with two or more accounts of one agent. Each step says what to expect. If something else happens, note the step number, what you saw, and the output of `csm --debug <command>` if you can.

Everything here is covered by csm's automated tests with fake agents. What this checklist adds is real accounts and your machine. Only Claude Code and Codex have automatic failover; for Gemini CLI and Copilot CLI skip section 6.

## 1. Install

- [ ] Install Go 1.25 or newer, then `go install github.com/onoja123/csm/cmd/csm@latest`
- [ ] `csm version` prints a version
- [ ] `csm setup` lists your installed agents with `Profile isolation ✓`

## 2. Accounts

- [ ] `csm account add a` (add `--provider codex`, `gemini` or `copilot` for another agent). The agent's own login opens; sign in with your first account
- [ ] It ends with `✓ a ready (<your first account>)`
- [ ] `csm account add b`, signed in with your second account, ends with `✓ b ready (<your second account>)`
- [ ] `csm accounts` lists both, with `a` marked active
- [ ] `csm doctor` ends with `Result: ready` and shows a different identity for each account
- [ ] Try `csm account add c` and sign in with the first account again. csm must refuse it as a duplicate of `a`

## 3. Launch and identity

- [ ] In a project directory, `csm claude` (or `csm codex`, `csm gemini`, `csm copilot`) starts the agent
- [ ] Inside the agent, check which account it is using (Claude Code: `/status`). It must be account `a`
- [ ] Quit the agent. `csm use b`, start it again, check the account. It must be `b`

## 4. Switch a running session

- [ ] `csm use a`, then start the agent and send one message that contains a phrase you will recognise, for example `remember the code CSM-TEST-847291`
- [ ] From a second terminal in the same project: `csm next`
- [ ] The first terminal shows `Switching a → b`, a checklist of steps, and the agent restarts
- [ ] Ask the agent what code you asked it to remember. It must know
- [ ] Inside the agent, check the account. It must be `b`
- [ ] `git status` in the project shows the same branch and the same changes as before
- [ ] `csm next` again moves back to `a` with the conversation still intact

## 5. Usage (Claude Code and Codex only)

- [ ] `csm usage` prints a 5-hour and a 7-day figure for each account
- [ ] `csm usage --cached` prints the same figures without a live check

## 6. Automatic failover (Claude Code and Codex only)

Only do this if one account is close to its limit anyway.

- [ ] `csm auto on`
- [ ] Start the agent as the account that is close to its limit and work until the limit is reached
- [ ] csm reports `<account> account became unavailable`, switches to the other account, and the conversation continues
- [ ] `csm accounts` shows the limited account as `cooldown until <time>`
- [ ] `csm auto off`

## 7. Recovery

- [ ] Start the agent, then from another terminal run `csm next` and immediately close the first terminal window
- [ ] `csm status` in the project either shows the switch completed or says `Previous switch did not complete`
- [ ] If it did not complete, start the agent again; csm offers to resume the switch

## 8. Clean up

- [ ] `csm account remove b` says the profile directory was kept and prints how to sign it out
- [ ] `csm accounts` no longer lists `b`
