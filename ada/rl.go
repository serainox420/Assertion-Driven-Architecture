package ada

// RLState is what the meta-controller observes — metrics, never raw output
// (§10). The policy operates above the loop, not on the command space.
type RLState struct {
	ConsecutiveFails int // assertion failures in a row
	EntropyLevel     int // current frustration (§8)
	FactsCount       int // verified progress so far
	StepsTaken       int // budget consumed
}

// MetaAction is the small, discrete, meta-level action space — a problem RL is
// good at, unlike the intractable space of arbitrary shell strings (§10).
type MetaAction int

const (
	ActionLetItCook  MetaAction = iota // agent is making progress; stay out of the way
	ActionForceFork                    // trigger a Hard Context Fork now, before the ceiling
	ActionInjectHint                   // append a tactical hint to the objective
	ActionSwapModel                    // swap driver↔worker for the next step (§11)
)

// MetaController decides how to manage the agent. Implementations may be a
// learned policy (Q-table / net) or a hand-tuned heuristic — the report notes a
// heuristic may beat an under-trained learned policy on a single workstation (§17).
type MetaController interface {
	Decide(RLState) MetaAction
}

// Reward shapes a benign, task-completion-oriented signal tied to VERIFIED
// progress so the policy can't be gamed by weak assertions (§10).
func Reward(res ExecutionResult, objectiveComplete bool) float64 {
	if objectiveComplete {
		return +100.0 // episode terminates on verified goal completion
	}
	switch {
	case res.Passed && res.Strength == StrengthStrong:
		return +10.0 // verified progress
	case res.Passed:
		return +2.0 // progress, but weakly verified — worth less (§3)
	case res.Anomaly != nil && res.Anomaly.FailureClass == ClassModelError:
		return -5.0 // emitting broken commands is the costly mistake
	default:
		return -1.0 // an honest failed hypothesis is cheap; that's how search works
	}
}

// HeuristicController is a hand-tuned meta-policy: force a fork a touch before
// the hard ceiling when frustration is high, nudge toward strong assertions when
// the model keeps failing, and otherwise let it cook. A reasonable baseline to
// beat before reaching for a learned policy (§17).
type HeuristicController struct {
	MaxEntropy int
}

// Decide implements MetaController.
func (h HeuristicController) Decide(s RLState) MetaAction {
	switch {
	case h.MaxEntropy > 0 && s.EntropyLevel >= h.MaxEntropy-1:
		return ActionForceFork
	case s.ConsecutiveFails >= 2:
		return ActionInjectHint
	default:
		return ActionLetItCook
	}
}
