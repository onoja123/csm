package engrun

import (
	"fmt"
	"slices"
)

// State is the lifecycle of an engineering run, whatever started it: a Build Room task or an incoming signal. CSM and
// CSM Hub both validate transitions with CanTransition, so a run cannot jump stages on either side.
type State string

const (
	StateReceived      State = "received"
	StateTriaging      State = "triaging"
	StateReady         State = "ready"
	StateInvestigating State = "investigating"
	StateReproducing   State = "reproducing"
	StatePlanning      State = "planning"
	StateAwaitApproval State = "awaiting_approval"
	StateImplementing  State = "implementing"
	StateIntegrating   State = "integrating"
	StateVerifying     State = "verifying"
	StateReviewing     State = "reviewing"
	StateCorrecting    State = "correcting"
	StateAttention     State = "awaiting_attention"
	StateAwaitPR       State = "awaiting_pr_authorization"
	StatePROpened      State = "pr_opened"
	StateCompleted     State = "completed"
	StateFailed        State = "failed"
	StateCancelled     State = "cancelled"
)

var working = []State{StateTriaging, StateReady, StateInvestigating, StateReproducing, StatePlanning, StateAwaitApproval, StateImplementing, StateIntegrating, StateVerifying, StateReviewing, StateCorrecting, StateAttention, StateAwaitPR}

var transitions = map[State][]State{
	StateReceived:      {StateTriaging, StateReady, StateCancelled, StateFailed},
	StateTriaging:      {StateReady, StateAttention, StateCancelled, StateFailed},
	StateReady:         {StateInvestigating, StatePlanning, StateImplementing, StateAttention, StateCancelled, StateFailed},
	StateInvestigating: {StateReproducing, StateImplementing, StateAttention, StateCancelled, StateFailed},
	StateReproducing:   {StateImplementing, StateAttention, StateCancelled, StateFailed},
	StatePlanning:      {StateAwaitApproval, StateImplementing, StateAttention, StateCancelled, StateFailed},
	StateAwaitApproval: {StatePlanning, StateImplementing, StateCancelled},
	StateImplementing:  {StateIntegrating, StateAttention, StateCancelled, StateFailed},
	StateIntegrating:   {StateVerifying, StateAttention, StateCancelled, StateFailed},
	StateVerifying:     {StateReviewing, StateCorrecting, StateAwaitPR, StateAttention, StateCancelled, StateFailed},
	StateReviewing:     {StateCorrecting, StateAwaitPR, StateAttention, StateCancelled, StateFailed},
	StateCorrecting:    {StateIntegrating, StateAttention, StateCancelled, StateFailed},
	StateAttention:     working,
	StateAwaitPR:       {StatePROpened, StateCompleted, StateAttention, StateCancelled},
	StatePROpened:      {StateCompleted},
}

func init() {
	transitions[StateAttention] = append(slices.Clone(working), StateCancelled, StateFailed)
}

func (s State) Terminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCancelled
}

func (s State) Valid() bool {
	_, ok := transitions[s]

	return ok || s.Terminal()
}

func CanTransition(from, to State) bool {
	return from == to || slices.Contains(transitions[from], to)
}

func Transition(from, to State) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("an engineering run cannot move from %s to %s", from, to)
	}

	return nil
}

// Reproduction is what an attempt to reproduce the failure established, and only that.
type Reproduction string

const (
	ReproNotAttempted  Reproduction = "not_attempted"
	ReproReproduced    Reproduction = "reproduced"
	ReproNotReproduced Reproduction = "not_reproduced"
	ReproInconclusive  Reproduction = "inconclusive"
	ReproBlocked       Reproduction = "blocked"
)

// Verdict summarizes verification. A green test suite alone is TestsPassedUnconfirmed, never Verified: Verified needs the
// failure reproduced on the base and gone after the change under the same conditions.
type Verdict string

const (
	VerdictNotRun                 Verdict = "not_run"
	VerdictVerified               Verdict = "fixed_and_verified"
	VerdictTestsPassedUnconfirmed Verdict = "tests_passed_reproduction_unconfirmed"
	VerdictFailed                 Verdict = "verification_failed"
	VerdictBlocked                Verdict = "verification_blocked"
)

// Classification is the investigator's call on whether the signal is something a code change should fix.
type Classification string

const (
	ClassCodeDefect   Classification = "code_defect"
	ClassEnvironment  Classification = "environment"
	ClassDependency   Classification = "dependency"
	ClassInsufficient Classification = "insufficient_evidence"
	ClassUnsafe       Classification = "unsafe_change"
)

func (c Classification) Valid() bool {
	return slices.Contains([]Classification{ClassCodeDefect, ClassEnvironment, ClassDependency, ClassInsufficient, ClassUnsafe}, c)
}

// Actionable is true only for a code defect: anything else stops for a person instead of producing a speculative fix.
func (c Classification) Actionable() bool { return c == ClassCodeDefect }
