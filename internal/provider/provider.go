// Package provider holds everything specific to a coding agent; Claude Code is the only one in v1.
package provider

import (
	"crypto/rand"
	"fmt"
	"time"
)

func NewSessionID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type StopReason string

const (
	StopReasonUsageLimit      StopReason = "usage_limit"
	StopReasonRateLimited     StopReason = "rate_limited"
	StopReasonAuthentication  StopReason = "authentication"
	StopReasonNetwork         StopReason = "network"
	StopReasonUserInterrupted StopReason = "user_interrupted"
	StopReasonProcessExited   StopReason = "process_exited"
	StopReasonManualSwitch    StopReason = "manual_switch"
	StopReasonUnknown         StopReason = "unknown"
)

func (r StopReason) Describe() string {
	switch r {
	case StopReasonUsageLimit:
		return "known usage limit"
	case StopReasonRateLimited:
		return "temporary API rate limit (not a usage limit)"
	case StopReasonAuthentication:
		return "authentication required"
	case StopReasonNetwork:
		return "network or service error"
	case StopReasonUserInterrupted:
		return "stopped by user"
	case StopReasonProcessExited:
		return "the agent exited"
	case StopReasonManualSwitch:
		return "manual switch"
	default:
		return "unknown error"
	}
}

// Identity is who the agent says it is logged in as for one profile directory.
type Identity struct {
	LoggedIn   bool
	Email      string
	ProfileDir string
}

type Features struct {
	Auth         bool
	Hooks        bool
	Resume       bool
	SessionID    bool
	SystemPrompt bool
	UsageProbe   bool
}

func (f Features) CanIsolate() bool  { return f.Auth && f.Hooks }
func (f Features) CanContinue() bool { return f.Resume && f.SessionID }

// Failure is an API error the agent reported through its failure hook.
type Failure struct {
	SessionID string     `json:"session_id"`
	Error     string     `json:"error"`
	Details   string     `json:"details,omitempty"`
	Message   string     `json:"message"`
	Reason    StopReason `json:"reason"`
}

// UsageWindow is one plan limit window; a zero ResetsAt means the agent did not report it.
type UsageWindow struct {
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at"`
}

// Usage is the plan usage the agent last reported for the account it runs as.
type Usage struct {
	FiveHour   UsageWindow `json:"five_hour,omitzero"`
	SevenDay   UsageWindow `json:"seven_day,omitzero"`
	SpendLimit UsageWindow `json:"spend_limit,omitzero"`
	// Limited is true when the agent reported that requests are currently being rejected.
	Limited bool `json:"limited,omitempty"`
}

func (u Usage) Empty() bool {
	return u.FiveHour.ResetsAt.IsZero() && u.SevenDay.ResetsAt.IsZero() && u.SpendLimit.ResetsAt.IsZero()
}
