package provider

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/onoja123/csm/internal/handoff"
)

// A trimmed real Claude Code 2.1.294 transcript: string and block content, tool calls, a tool result, a sidechain and other record types.
const claudeTranscriptFixture = `{"type":"file-history-snapshot","messageId":"x","snapshot":{}}
{"type":"user","isSidechain":false,"message":{"role":"user","content":"Add retry logic to the webhook"},"cwd":"/repo","sessionId":"abc"}
{"type":"assistant","isSidechain":false,"message":{"role":"assistant","content":[{"type":"thinking","thinking":"secret internal thoughts with TOKEN=abc"},{"type":"text","text":"I'll add exponential backoff in the handler instead of the client."},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/repo/internal/webhook.go"}}]}}
{"type":"user","isSidechain":false,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"package webhook // contains GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123 do not leak"}]}}
{"type":"assistant","isSidechain":false,"message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":"/repo/internal/webhook.go","old_string":"a","new_string":"b"}},{"type":"tool_use","id":"t3","name":"Write","input":{"file_path":"/repo/internal/backoff.go","content":"package webhook"}},{"type":"tool_use","id":"t4","name":"Bash","input":{"command":"go test ./internal/...","description":"Run tests"}}]}}
{"type":"user","isSidechain":true,"message":{"role":"user","content":"subagent prompt that must be ignored"}}
{"type":"user","isSidechain":false,"message":{"role":"user","content":[{"type":"text","text":"Make the retries exponential and cap at 5"}]}}
{"type":"assistant","isSidechain":false,"message":{"role":"assistant","content":[{"type":"text","text":"Backoff is in place and tests pass.\n\n## Next steps\n\n1. Cap retries at 5\n2. Add a metric for retried deliveries\n"}]}}
{"type":"user","isSidechain":false,"message":{"role":"user","content":"<command-name>/clear</command-name>"}}
`

func TestClaudeSessionContext(t *testing.T) {
	profile := t.TempDir()
	project := filepath.Join(profile, "projects", "-repo")
	os.MkdirAll(project, 0o700)
	os.WriteFile(filepath.Join(project, "abc.jsonl"), []byte(claudeTranscriptFixture), 0o600)

	ctx, err := Claude{}.SessionContext(profile, "/repo", "abc")
	if err != nil {
		t.Fatal(err)
	}

	ctx = handoff.Clean(ctx)

	if !slices.Equal(ctx.UserRequests, []string{"Add retry logic to the webhook", "Make the retries exponential and cap at 5"}) {
		t.Fatalf("requests = %q", ctx.UserRequests)
	}

	if !strings.HasPrefix(ctx.LastAssistant, "Backoff is in place") {
		t.Fatalf("last assistant = %q", ctx.LastAssistant)
	}

	if !slices.Equal(ctx.FilesRead, []string{"/repo/internal/webhook.go"}) || !slices.Equal(ctx.FilesModified, []string{"/repo/internal/webhook.go"}) || !slices.Equal(ctx.FilesCreated, []string{"/repo/internal/backoff.go"}) {
		t.Fatalf("files read %q modified %q created %q", ctx.FilesRead, ctx.FilesModified, ctx.FilesCreated)
	}

	if !slices.Equal(ctx.Commands, []string{"go test ./internal/..."}) {
		t.Fatalf("commands = %q", ctx.Commands)
	}

	if !slices.Equal(ctx.NextSteps, []string{"Cap retries at 5", "Add a metric for retried deliveries"}) {
		t.Fatalf("next steps = %q", ctx.NextSteps)
	}

	if len(ctx.Decisions) != 1 || !strings.Contains(ctx.Decisions[0], "exponential backoff in the handler") {
		t.Fatalf("decisions = %q", ctx.Decisions)
	}

	all := strings.Join(append(append(ctx.UserRequests, ctx.LastAssistant), ctx.Decisions...), "\n")

	for _, leak := range []string{"ghp_", "TOKEN=abc", "secret internal", "subagent prompt", "do not leak"} {
		if strings.Contains(all, leak) {
			t.Fatalf("context leaks %q", leak)
		}
	}

	if ctx.Messages != 5 {
		t.Fatalf("messages = %d", ctx.Messages)
	}
}

func TestClaudeSessionContextEdgeCases(t *testing.T) {
	profile := t.TempDir()
	project := filepath.Join(profile, "projects", "-repo")
	os.MkdirAll(project, 0o700)

	if _, err := (Claude{}).SessionContext(profile, "/repo", "missing"); err == nil {
		t.Fatal("a missing transcript was accepted")
	}

	if _, err := (Claude{}).SessionContext(profile, "/repo", "../abc"); err == nil {
		t.Fatal("a session ID with a path was accepted")
	}

	os.WriteFile(filepath.Join(project, "empty.jsonl"), nil, 0o600)
	ctx, err := Claude{}.SessionContext(profile, "/repo", "empty")

	if err != nil || !ctx.Empty() || ctx.Note == "" {
		t.Fatalf("empty transcript: %+v %v", ctx, err)
	}

	os.WriteFile(filepath.Join(project, "bad.jsonl"), []byte("{not json\n{\n"), 0o600)

	if _, err := (Claude{}).SessionContext(profile, "/repo", "bad"); err == nil {
		t.Fatal("a malformed transcript was accepted")
	}

	os.WriteFile(filepath.Join(project, "mixed.jsonl"), []byte("garbage\n"+`{"type":"user","message":{"role":"user","content":"hello"}}`+"\n"), 0o600)
	ctx, err = Claude{}.SessionContext(profile, "/repo", "mixed")

	if err != nil || len(ctx.UserRequests) != 1 {
		t.Fatalf("a readable record after a bad line should be kept: %+v %v", ctx, err)
	}
}

func TestGenericSessionParsing(t *testing.T) {
	codexRollout := `{"timestamp":"t","type":"session_meta","payload":{"id":"s1","cwd":"/repo"}}
{"timestamp":"t","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the flaky test"}]}}
{"timestamp":"t","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"bash\",\"-lc\",\"go test ./...\"]}"}}
{"timestamp":"t","type":"response_item","payload":{"type":"function_call_output","output":"FAIL secret=xyz"}}
{"timestamp":"t","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The race is in cache.go.\n\nNext steps:\n- Add a mutex around the map"}]}}
{"timestamp":"t","type":"event_msg","payload":{"type":"agent_message","message":"ignored duplicate"}}
`
	ctx, err := parseGenericSession(strings.NewReader(codexRollout))
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(ctx.UserRequests, []string{"Fix the flaky test"}) || !strings.HasPrefix(ctx.LastAssistant, "The race is in cache.go") {
		t.Fatalf("codex: %+v", ctx)
	}

	if !slices.Equal(ctx.Commands, []string{"bash -lc go test ./..."}) || !slices.Equal(ctx.NextSteps, []string{"Add a mutex around the map"}) {
		t.Fatalf("codex commands %q steps %q", ctx.Commands, ctx.NextSteps)
	}

	geminiChat := `{"sessionId":"g1","projectHash":"h","messages":[{"type":"user","content":"Rename the module"},{"type":"gemini","content":"Renamed it in go.mod; imports still need updating.","toolCalls":[{"name":"write_file","args":{"file_path":"go.mod"}}]}]}`
	ctx, err = parseGenericSession(strings.NewReader(geminiChat))

	if err != nil || !slices.Equal(ctx.UserRequests, []string{"Rename the module"}) || !strings.HasPrefix(ctx.LastAssistant, "Renamed it") {
		t.Fatalf("gemini: %+v %v", ctx, err)
	}

	copilotEvents := `{"type":"user.message","data":{"content":"Add a health endpoint"}}
{"type":"tool.execution_start","data":{"toolName":"bash","arguments":{"cmd":"npm test"}}}
{"type":"assistant.message","data":{"content":"Added /healthz; tests pass."}}
`
	ctx, err = parseGenericSession(strings.NewReader(copilotEvents))

	if err != nil || !slices.Equal(ctx.UserRequests, []string{"Add a health endpoint"}) || ctx.LastAssistant != "Added /healthz; tests pass." || !slices.Equal(ctx.Commands, []string{"npm test"}) {
		t.Fatalf("copilot: %+v %v", ctx, err)
	}

	ctx, err = parseGenericSession(strings.NewReader(`{"profile":"alpha","resumed":false}` + "\n"))

	if err != nil || !ctx.Empty() || ctx.Note == "" {
		t.Fatalf("a fake agent's transcript has no task context: %+v %v", ctx, err)
	}

	if _, err := parseGenericSession(strings.NewReader("{{{\n")); err == nil {
		t.Fatal("malformed session accepted")
	}
}

func TestHandoffArgsPerProvider(t *testing.T) {
	instruction := "Read /state/handoff.md"
	all := Features{SystemPrompt: true, InitialPrompt: true}

	if got := (Claude{}).HandoffArgs(all, instruction); !slices.Equal(got, []string{"--append-system-prompt", instruction}) {
		t.Fatalf("claude = %q", got)
	}

	if got := (Codex{}).HandoffArgs(all, instruction); !slices.Equal(got, []string{instruction}) {
		t.Fatalf("codex = %q", got)
	}

	if got := (Gemini{}).HandoffArgs(all, instruction); !slices.Equal(got, []string{"--prompt-interactive", instruction}) {
		t.Fatalf("gemini = %q", got)
	}

	if got := (Copilot{}).HandoffArgs(all, instruction); !slices.Equal(got, []string{"--interactive", instruction}) {
		t.Fatalf("copilot = %q", got)
	}

	for _, id := range IDs {
		p, _ := New(id, "")

		if got, _ := Injector(p); got.HandoffArgs(Features{}, instruction) != nil {
			t.Errorf("%s injected without the feature", id)
		}

		if _, ok := Extractor(p); !ok {
			t.Errorf("%s has no session extractor", id)
		}

		if !CanInject(p, all) || CanInject(p, Features{}) {
			t.Errorf("%s CanInject is wrong", id)
		}
	}
}
