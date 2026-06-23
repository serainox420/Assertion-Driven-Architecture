package ada

import (
	"context"
)

// PlanDecision is the planner's structured output each round (research §5.6/§6.2).
// The planner re-plans from the CURRENT verified facts rather than committing to a
// rigid up-front tree, which is what keeps the approach robust to mutable state (§0).
type PlanDecision struct {
	Done     bool     `json:"done"`     // main objective verified satisfied by the facts
	Reason   string   `json:"reason"`   // one-line rationale (logged / shown to the human)
	Subgoals []string `json:"subgoals"` // ordered next sub-goals; empty when done
}

// PlanInput is everything the planner sees: the pinned objective, the verified
// facts so far, which sub-goals were achieved, and which FAILED. No raw history
// (§7). Completed and Failed are kept distinct on purpose: reporting a failed
// sub-goal as "completed" lets the planner declare victory on work that never
// landed, so the runtime tells the planner the truth about each outcome.
type PlanInput struct {
	Objective   string   `json:"objective"`
	Environment []string `json:"environment,omitempty"` // durable host facts (§5.3)
	Facts       []Fact   `json:"established_facts"`
	Completed   []string `json:"completed_subgoals"`
	Failed      []string `json:"failed_subgoals,omitempty"` // EXHAUSTED/FAILED — NOT achieved
	// LastError is the decoded diagnostic from the most recent failed command — the
	// CAUSE behind the latest blocked sub-goal. The planner uses it to repair the
	// root condition (e.g. refresh a stale package DB) instead of re-issuing the plan
	// that just failed. Meaningful only when failed_subgoals is non-empty.
	LastError string `json:"last_error,omitempty"`
}

// Planner turns a high-level objective + verified facts into the next concrete
// sub-goals and judges when the objective is satisfied. It is separate from LLM
// (execution) so a larger "driver" model can plan while a fast "worker" executes
// (§1.5, §11.2) — though one model can implement both.
type Planner interface {
	Plan(ctx context.Context, in PlanInput, temperature float64) (PlanDecision, error)
}

// factSeedKey mirrors factKey for a stored Fact, so when a sub-goal inherits the
// facts proven by earlier sub-goals it recognizes that ground as already-held and
// does not waste steps re-proving it. Exact for fs/process/service; best-effort
// for generic channels (the originating command is not retained on a Fact).
func factSeedKey(f Fact) string {
	return assertionKey(f.assertion)
}

// dedupFacts removes facts with duplicate statements, preserving order. Facts
// accumulate across sub-goals; this keeps the carried set clean.
func dedupFacts(facts []Fact) []Fact {
	seen := make(map[string]bool, len(facts))
	out := facts[:0:0]
	for _, f := range facts {
		if seen[f.Statement] {
			continue
		}
		seen[f.Statement] = true
		out = append(out, f)
	}
	return out
}
