package provider

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/onoja123/csm/internal/handoff"
)

// Codex takes the instruction as its positional prompt, also after `resume <id>`.
func (c Codex) HandoffArgs(f Features, instruction string) []string {
	if !f.InitialPrompt || instruction == "" {
		return nil
	}

	return []string{instruction}
}

func (g Gemini) HandoffArgs(f Features, instruction string) []string {
	if !f.InitialPrompt || instruction == "" {
		return nil
	}

	return []string{"--prompt-interactive", instruction}
}

func (c Copilot) HandoffArgs(f Features, instruction string) []string {
	if !f.InitialPrompt || instruction == "" {
		return nil
	}

	return []string{"--interactive", instruction}
}

// genericRecord is the common ground of the Codex rollout, Gemini chat and Copilot event formats: a role or type, text somewhere, and tool calls with arguments.
type genericRecord struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Text    string          `json:"text"`
	Payload json.RawMessage `json:"payload"`
	Data    json.RawMessage `json:"data"`
	Name    string          `json:"name"`
	Args    json.RawMessage `json:"arguments"`
}

func textOf(raw json.RawMessage) string {
	var s string

	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}

	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}

	var b strings.Builder

	for _, block := range blocks {
		if block.Text != "" {
			b.WriteString(block.Text)
			b.WriteString("\n")
		}
	}

	return strings.TrimSpace(b.String())
}

// consume folds one record into the context; nested payload/data records are unwrapped first.
func consume(ctx *handoff.Context, last *string, rec genericRecord) {
	for _, nested := range []json.RawMessage{rec.Payload, rec.Data} {
		var inner genericRecord

		if len(nested) == 0 || json.Unmarshal(nested, &inner) != nil {
			continue
		}

		if inner.Role == "" && inner.Type == "" && inner.Text == "" && len(inner.Content) == 0 && len(inner.Args) == 0 {
			continue
		}

		if inner.Role == "" {
			inner.Role = rec.Role
		}

		if inner.Type == "" {
			inner.Type = rec.Type
		}

		consume(ctx, last, inner)
	}

	role := strings.ToLower(rec.Role + " " + rec.Type)
	text := textOf(rec.Content)

	if text == "" {
		text = strings.TrimSpace(rec.Text)
	}

	switch {
	case strings.Contains(role, "user") && text != "" && !strings.Contains(role, "tool"):
		ctx.UserRequests = append(ctx.UserRequests, text)
		ctx.Messages++

	case (strings.Contains(role, "assistant") || strings.Contains(role, "gemini") || strings.Contains(role, "agent_message")) && text != "":
		*last = text
		ctx.Messages++
		ctx.Decisions = append(ctx.Decisions, handoff.DecisionsFrom(text)...)

	case strings.Contains(role, "function_call") || strings.Contains(role, "tool_call") || strings.Contains(role, "tool.execution"):
		var args struct {
			Command any    `json:"command"`
			Cmd     string `json:"cmd"`
			Path    string `json:"path"`
			File    string `json:"file_path"`
		}

		raw := rec.Args

		var asString string

		if json.Unmarshal(raw, &asString) == nil {
			raw = json.RawMessage(asString)
		}

		json.Unmarshal(raw, &args)

		switch cmd := args.Command.(type) {
		case string:
			ctx.Commands = append(ctx.Commands, cmd)
		case []any:
			var parts []string

			for _, p := range cmd {
				if s, ok := p.(string); ok {
					parts = append(parts, s)
				}
			}

			if len(parts) > 0 {
				ctx.Commands = append(ctx.Commands, strings.Join(parts, " "))
			}
		}

		if args.Cmd != "" {
			ctx.Commands = append(ctx.Commands, args.Cmd)
		}

		for _, p := range []string{args.Path, args.File} {
			if p != "" {
				ctx.FilesModified = append(ctx.FilesModified, p)
			}
		}
	}
}

// parseGenericSession reads a JSONL stream, or one JSON document holding a messages array, with the same tolerant rules.
func parseGenericSession(r io.Reader) (handoff.Context, error) {
	data, err := io.ReadAll(io.LimitReader(r, 64<<20))
	if err != nil {
		return handoff.Context{}, err
	}

	var ctx handoff.Context
	var last string

	var doc struct {
		Messages []genericRecord `json:"messages"`
		Items    []genericRecord `json:"items"`
	}

	if json.Unmarshal(data, &doc) == nil && len(doc.Messages)+len(doc.Items) > 0 {
		for _, rec := range append(doc.Messages, doc.Items...) {
			consume(&ctx, &last, rec)
		}
	} else {
		sc := bufio.NewScanner(strings.NewReader(string(data)))
		sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
		malformed := 0

		for sc.Scan() {
			var rec genericRecord

			if json.Unmarshal(sc.Bytes(), &rec) != nil {
				malformed++

				continue
			}

			consume(&ctx, &last, rec)
		}

		if ctx.Messages == 0 && malformed > 0 {
			return ctx, errors.New("the session file could not be read")
		}
	}

	ctx.LastAssistant = last
	ctx.NextSteps = handoff.NextStepsFrom(last)

	if ctx.Messages == 0 {
		ctx.Note = "The session had no recorded messages."
	}

	return ctx, nil
}

func parseSessionFile(path string) (handoff.Context, error) {
	f, err := os.Open(path)
	if err != nil {
		return handoff.Context{}, err
	}

	defer f.Close()

	return parseGenericSession(f)
}

func (c Codex) SessionContext(profileDir, cwd, sessionID string) (handoff.Context, error) {
	var res codexThreadRead

	if _, err := c.call(profileDir, "thread/read", codexThreadReadParams{ThreadID: sessionID, IncludeTurns: false}, &res); err != nil {
		return handoff.Context{}, err
	}

	if res.Thread.Path == "" {
		return handoff.Context{}, errors.New("codex did not report where the session is stored")
	}

	return parseSessionFile(res.Thread.Path)
}

func (g Gemini) SessionContext(profileDir, cwd, sessionID string) (handoff.Context, error) {
	sessions, err := geminiSessions(profileDir)
	if err != nil {
		return handoff.Context{}, err
	}

	for _, s := range sessions {
		if s.meta.SessionID == sessionID {
			return parseSessionFile(s.path)
		}
	}

	return handoff.Context{}, os.ErrNotExist
}

func (c Copilot) SessionContext(profileDir, cwd, sessionID string) (handoff.Context, error) {
	if sessionID == "" || sessionID != filepath.Base(sessionID) {
		return handoff.Context{}, errors.New("unexpected Copilot CLI session ID")
	}

	return parseSessionFile(filepath.Join(copilotSessionDir(profileDir, sessionID), "events.jsonl"))
}
