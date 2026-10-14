package provider

// Derived from the adapter and installed version, so no surface offers an action the agent cannot do.
type Capabilities struct {
	Interactive     bool `json:"interactive"`
	SessionTracking bool `json:"session_tracking"`
	SessionResume   bool `json:"session_resume"`
	// Tasks covers everything that starts an agent on an instruction: Build Room tasks, plans and reviews, and AutoFix.
	Tasks          bool `json:"tasks"`
	HandoffTarget  bool `json:"handoff_target"`
	HandoffContext bool `json:"handoff_context"`
	// UsageLimits means csm can tell a usage limit from any other stop, which automatic failover and handoff need.
	UsageLimits      bool `json:"usage_limits"`
	UsageReport      bool `json:"usage_report"`
	AccountSwitching bool `json:"account_switching"`
	MCP              bool `json:"mcp"`
}

func CapabilitiesOf(p Provider, f Features) Capabilities {
	_, extracts := Extractor(p)
	_, mcp := MCPAdapter(p)

	return Capabilities{
		Interactive:      f.CanIsolate(),
		SessionTracking:  f.Supervise,
		SessionResume:    f.Resume,
		Tasks:            CanStartTasks(p, f),
		HandoffTarget:    CanInject(p, f),
		HandoffContext:   extracts,
		UsageLimits:      detectsLimits(p, f),
		UsageReport:      f.UsageProbe,
		AccountSwitching: f.CanIsolate() && !f.SharedLogin,
		MCP:              mcp,
	}
}

func CanStartTasks(p Provider, f Features) bool {
	s, ok := Starter(p)

	return ok && len(s.TaskArgs(f, "task")) > 0
}

// Claude Code reports limits through its failure hook and Codex through the usage csm polls; the others give no reliable signal.
func detectsLimits(p Provider, f Features) bool {
	if p.ID() == ClaudeID {
		return f.Supervise
	}

	return f.UsageProbe && p.LimitPollInterval() > 0
}
