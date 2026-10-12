package main

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"
)

const approvalWait = 10 * time.Minute

// hookRequest is what `csm hook pre-tool-use` sends: the agent's hook payload plus the identity of the csm process running it.
type hookRequest struct {
	CSMPID   int             `json:"csm_pid"`
	Account  string          `json:"account"`
	StateDir string          `json:"state_dir"`
	Payload  json.RawMessage `json:"payload"`
}

type hookPayload struct {
	SessionID string          `json:"session_id"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

type Approval struct {
	ID          string          `json:"id"`
	RunID       string          `json:"run_id,omitempty"`
	Provider    string          `json:"provider"`
	Account     string          `json:"account"`
	SessionID   string          `json:"session_id,omitempty"`
	ToolName    string          `json:"tool_name"`
	Command     string          `json:"command,omitempty"`
	Description string          `json:"description,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
	Destructive bool            `json:"destructive"`
	RequestedAt time.Time       `json:"requested_at"`
	Decision    string          `json:"decision,omitempty"`
	DecidedAt   time.Time       `json:"decided_at,omitzero"`

	decided chan struct{}
}

type approvalQueue struct {
	mu      sync.Mutex
	pending map[string]*Approval
	history []*Approval
	seq     int
	bus     *eventBus
	log     *activityLog
}

func newApprovalQueue(bus *eventBus, log *activityLog) *approvalQueue {
	return &approvalQueue{pending: map[string]*Approval{}, bus: bus, log: log}
}

var destructivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\brm\s+(-[a-zA-Z]*r[a-zA-Z]*f|-[a-zA-Z]*f[a-zA-Z]*r)\b`),
	regexp.MustCompile(`\bgit\s+(push\s+.*--force|push\s+-f\b|reset\s+--hard|clean\s+-[a-z]*f|checkout\s+--\s|branch\s+-D)`),
	regexp.MustCompile(`(?i)\b(drop\s+(table|database|schema)|truncate\s+table)\b`),
	regexp.MustCompile(`\bmkfs\b|\bdd\s+if=|>\s*/dev/sd`),
	regexp.MustCompile(`\b(sudo|chmod\s+-R\s+777|chown\s+-R)\b`),
	regexp.MustCompile(`\bkubectl\s+delete\b|\bterraform\s+destroy\b|\baws\s+\S+\s+delete\b`),
}

func looksDestructive(command string) bool {
	for _, p := range destructivePatterns {
		if p.MatchString(command) {
			return true
		}
	}

	return false
}

// ask registers the request and blocks until the desktop decides, the wait ends, or the caller gives up. An empty decision means: let the agent ask the user itself.
func (q *approvalQueue) ask(r *agentRun, req hookRequest, done <-chan struct{}) string {
	var payload hookPayload

	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		return ""
	}

	var input struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}

	json.Unmarshal(payload.ToolInput, &input)

	q.mu.Lock()
	q.seq++
	a := &Approval{
		ID:          time.Now().UTC().Format("20060102T150405Z") + "-" + itoa(q.seq),
		Provider:    "claude",
		Account:     req.Account,
		SessionID:   payload.SessionID,
		ToolName:    payload.ToolName,
		Command:     input.Command,
		Description: input.Description,
		Input:       payload.ToolInput,
		Destructive: looksDestructive(input.Command),
		RequestedAt: time.Now(),
		decided:     make(chan struct{}),
	}

	if r != nil {
		a.RunID = r.ID
		a.Provider = r.Provider
	}

	q.pending[a.ID] = a
	q.mu.Unlock()

	title := "Claude Code requested approval"
	q.log.add(Activity{Kind: "approval.requested", Provider: a.Provider, Account: a.Account, Title: title, Detail: firstLine(a.Command), RunID: a.RunID, Refs: map[string]string{"approval_id": a.ID}})
	q.bus.publish("approval.requested", a)

	select {
	case <-a.decided:
	case <-time.After(approvalWait):
		q.resolve(a.ID, "", "")
	case <-done:
		q.resolve(a.ID, "", "")
	}

	return a.Decision
}

func (q *approvalQueue) resolve(id, decision, reason string) (*Approval, error) {
	q.mu.Lock()
	a, ok := q.pending[id]

	if !ok {
		q.mu.Unlock()

		return nil, errors.New("no pending approval with that id")
	}

	delete(q.pending, id)
	a.Decision = decision
	a.DecidedAt = time.Now()
	q.history = append(q.history, a)

	if len(q.history) > 200 {
		q.history = q.history[len(q.history)-200:]
	}

	close(a.decided)
	q.mu.Unlock()

	if decision != "" {
		kind, title := "approval.allowed", "Command approved"

		if decision == "deny" {
			kind, title = "approval.denied", "Command rejected"
		}

		q.log.add(Activity{Kind: kind, Provider: a.Provider, Account: a.Account, Title: title, Detail: firstLine(a.Command), RunID: a.RunID, Refs: map[string]string{"approval_id": a.ID}})
	} else {
		q.log.add(Activity{Kind: "approval.expired", Provider: a.Provider, Account: a.Account, Title: "Approval left to the agent's own prompt", Detail: firstLine(a.Command), RunID: a.RunID})
	}

	q.bus.publish("approval.resolved", a)

	return a, nil
}

func (q *approvalQueue) list() (pending, recent []*Approval) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, a := range q.pending {
		pending = append(pending, a)
	}

	for i := len(q.history) - 1; i >= 0 && len(recent) < 20; i-- {
		recent = append(recent, q.history[i])
	}

	return pending, recent
}

// hookResponse is the JSON Claude Code expects from a PreToolUse hook; nothing is returned when the agent should ask the user itself.
func hookResponse(decision string) []byte {
	if decision == "" {
		return nil
	}

	reason := "Approved in CSM Desktop"

	if decision == "deny" {
		reason = "Rejected in CSM Desktop"
	}

	out := map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": "PreToolUse", "permissionDecision": decision, "permissionDecisionReason": reason}}
	data, _ := json.Marshal(out)

	return data
}

func validDecision(d string) bool {
	return strings.EqualFold(d, "allow") || strings.EqualFold(d, "deny")
}
