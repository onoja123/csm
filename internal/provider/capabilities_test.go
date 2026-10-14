package provider

import "testing"

func TestCapabilities(t *testing.T) {
	claude := CapabilitiesOf(Claude{}, Features{Auth: true, Supervise: true, Resume: true, SessionID: true, SystemPrompt: true, UsageProbe: true})

	if !claude.UsageLimits || !claude.AccountSwitching || !claude.HandoffContext || !claude.HandoffTarget || !claude.Tasks || !claude.MCP {
		t.Fatalf("claude = %+v", claude)
	}

	codex := CapabilitiesOf(Codex{}, Features{Auth: true, Supervise: true, Resume: true, InitialPrompt: true, UsageProbe: true})

	if !codex.UsageLimits || !codex.UsageReport || !codex.AccountSwitching {
		t.Fatalf("codex = %+v", codex)
	}

	for _, p := range []Provider{Gemini{}, Copilot{}} {
		c := CapabilitiesOf(p, Features{Auth: true, Supervise: true, Resume: true, SessionID: true, InitialPrompt: true})

		if c.UsageLimits || !c.AccountSwitching || !c.Tasks || !c.HandoffContext {
			t.Fatalf("%s = %+v", p.ID(), c)
		}
	}

	if old := CapabilitiesOf(Gemini{}, Features{Auth: true, Supervise: true}); old.Tasks || old.HandoffTarget {
		t.Fatalf("a version without an opening prompt must not take tasks: %+v", old)
	}
}
