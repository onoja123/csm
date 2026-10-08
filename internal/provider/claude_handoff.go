package provider

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/onoja123/csm/internal/handoff"
)

func (c Claude) HandoffArgs(f Features, instruction string) []string {
	if !f.SystemPrompt || instruction == "" {
		return nil
	}

	return []string{"--append-system-prompt", instruction}
}

// claudeTranscriptPath finds the session's transcript the same way CarrySession does.
func claudeTranscriptPath(profileDir, sessionID string) (string, error) {
	if sessionID == "" || sessionID != filepath.Base(sessionID) {
		return "", fmt.Errorf("unexpected Claude Code session ID %q", sessionID)
	}

	matches, err := filepath.Glob(filepath.Join(profileDir, "projects", "*", sessionID+".jsonl"))

	if err != nil || len(matches) == 0 {
		return "", os.ErrNotExist
	}

	return matches[0], nil
}

type claudeRecord struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	Message     struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudeBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type claudeToolInput struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Command      string `json:"command"`
	Description  string `json:"description"`
	Path         string `json:"path"`
}

// SessionContext reads the user's prompts, the assistant's text and the files and commands it touched. Tool results are never read.
func (c Claude) SessionContext(profileDir, cwd, sessionID string) (handoff.Context, error) {
	path, err := claudeTranscriptPath(profileDir, sessionID)
	if err != nil {
		return handoff.Context{}, err
	}

	f, err := os.Open(path)
	if err != nil {
		return handoff.Context{}, err
	}

	defer f.Close()

	return parseClaudeTranscript(f)
}

func parseClaudeTranscript(f *os.File) (handoff.Context, error) {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)

	var ctx handoff.Context
	var lastAssistant strings.Builder
	read, modified, created := map[string]bool{}, map[string]bool{}, map[string]bool{}
	malformed := 0

	for sc.Scan() {
		var rec claudeRecord

		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			malformed++

			continue
		}

		if rec.IsSidechain || rec.IsMeta || (rec.Type != "user" && rec.Type != "assistant") {
			continue
		}

		blocks, text := claudeBlocks(rec.Message.Content)

		switch rec.Type {
		case "user":
			if text != "" && !strings.HasPrefix(text, "<") {
				ctx.UserRequests = append(ctx.UserRequests, text)
				ctx.Messages++
			}

		case "assistant":
			ctx.Messages++
			var turn strings.Builder

			for _, b := range blocks {
				switch b.Type {
				case "text":
					turn.WriteString(b.Text)
					turn.WriteString("\n")

				case "tool_use":
					var in claudeToolInput

					json.Unmarshal(b.Input, &in)
					file := in.FilePath

					if file == "" {
						file = in.NotebookPath
					}

					switch b.Name {
					case "Read":
						mark(read, file)
					case "Edit", "MultiEdit", "NotebookEdit":
						mark(modified, file)
					case "Write":
						if read[file] || modified[file] {
							mark(modified, file)
						} else {
							mark(created, file)
						}
					case "Bash":
						if in.Command != "" {
							ctx.Commands = append(ctx.Commands, in.Command)
						}
					}
				}
			}

			if s := strings.TrimSpace(turn.String()); s != "" {
				lastAssistant.Reset()
				lastAssistant.WriteString(s)
				ctx.Decisions = append(ctx.Decisions, handoff.DecisionsFrom(s)...)
			}
		}
	}

	if err := sc.Err(); err != nil {
		return ctx, err
	}

	ctx.LastAssistant = lastAssistant.String()
	ctx.FilesRead = keys(read)
	ctx.FilesModified = keys(modified)
	ctx.FilesCreated = keys(created)
	ctx.NextSteps = handoff.NextStepsFrom(ctx.LastAssistant)

	if ctx.Messages == 0 {
		if malformed > 0 {
			return ctx, errors.New("the Claude Code transcript could not be read")
		}

		ctx.Note = "The session had no recorded messages."
	}

	return ctx, nil
}

func claudeBlocks(raw json.RawMessage) ([]claudeBlock, string) {
	var text string

	if json.Unmarshal(raw, &text) == nil {
		return nil, strings.TrimSpace(text)
	}

	var blocks []claudeBlock

	if json.Unmarshal(raw, &blocks) != nil {
		return nil, ""
	}

	var b strings.Builder

	for _, block := range blocks {
		if block.Type == "text" {
			b.WriteString(block.Text)
			b.WriteString("\n")
		}
	}

	return blocks, strings.TrimSpace(b.String())
}

func mark(set map[string]bool, path string) {
	if path != "" {
		set[path] = true
	}
}

func keys(set map[string]bool) []string {
	var out []string

	for k := range set {
		out = append(out, k)
	}

	return out
}
