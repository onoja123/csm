package provider

import (
	"github.com/onoja123/csm/internal/handoff"
)

type HandoffInjector interface {
	HandoffArgs(f Features, instruction string) []string
}

func Extractor(p Provider) (handoff.Extractor, bool) {
	e, ok := p.(handoff.Extractor)

	return e, ok
}

func Injector(p Provider) (HandoffInjector, bool) {
	i, ok := p.(HandoffInjector)

	return i, ok
}

func CanInject(p Provider, f Features) bool {
	if _, ok := Injector(p); !ok {
		return false
	}

	if p.ID() == ClaudeID {
		return f.SystemPrompt
	}

	return f.InitialPrompt
}
