package ada

import (
	"context"
	"testing"
)

// alwaysFailsLLM emits a task that can never pass (asserts an absent path), and
// records every snapshot it is handed so a test can inspect the anomaly the model
// actually sees.
func alwaysFailsLLM(seen *[]*AnomalyPayload) *MockLLM {
	return &MockLLM{Respond: func(s StateSnapshot) (Task, error) {
		if seen != nil {
			*seen = append(*seen, s.Anomaly)
		}
		return Task{
			ID: "doomed", Command: "true", Mode: ModeBlocking, TimeoutSec: 5,
			Assertion: Assertion{Type: "fs", Pattern: "/no/such/ada-recovery-xyz", Channel: ChannelFS},
		}, nil
	}}
}

// After MaxAttemptsPerTask failures of the SAME proposition, the runtime forces a
// route change (Hard Context Fork) rather than letting the model re-send forever.
func TestAttemptBudgetForcesFork(t *testing.T) {
	orch := NewOrchestrator("impossible", alwaysFailsLLM(nil), NewRuntime())
	orch.MaxStuck = 0 // isolate the attempt/route budget from the coarse stuck guard
	orch.MaxAttemptsPerTask = 3
	orch.MaxRoutes = 1 // one fork is enough to exhaust the routes

	if got := orch.Run(context.Background()); got != OutcomeFailed {
		t.Fatalf("exhausting the only route should be FAILED, got %s", got)
	}
	if orch.forks != 1 {
		t.Errorf("expected exactly one forced fork, got %d", orch.forks)
	}
	if orch.steps != 3 {
		t.Errorf("expected the fork after %d attempts, took %d steps", orch.MaxAttemptsPerTask, orch.steps)
	}
}

// The nested budget is finite and predictable: MaxRoutes × MaxAttemptsPerTask
// total tries at a stuck proposition, then FAILED. (3 × 3 = 9, the user's "norm".)
func TestRouteBudgetBoundsTotalAttempts(t *testing.T) {
	orch := NewOrchestrator("impossible", alwaysFailsLLM(nil), NewRuntime())
	orch.MaxStuck = 0
	orch.MaxEntropy = 100 // keep entropy from forking first; isolate the attempt budget
	orch.MaxAttemptsPerTask = 3
	orch.MaxRoutes = 3

	if got := orch.Run(context.Background()); got != OutcomeFailed {
		t.Fatalf("exhausting all routes should be FAILED, got %s", got)
	}
	if orch.forks != 3 {
		t.Errorf("expected %d routes, got %d", orch.MaxRoutes, orch.forks)
	}
	if want := orch.MaxRoutes * orch.MaxAttemptsPerTask; orch.steps != want {
		t.Errorf("expected exactly %d total tries (routes×attempts), took %d", want, orch.steps)
	}
}

// With STOCK defaults (no per-test tuning), the nested recovery budget — not the
// coarse stuck guard — must govern a "banging the same door" goal: it abandons as
// FAILED at exactly MaxRoutes × MaxAttemptsPerTask tries. This pins MaxStuck above
// the budget so a future tweak can't silently shadow the principled terminal.
func TestDefaultBudgetGovernsSameDoor(t *testing.T) {
	orch := NewOrchestrator("impossible", alwaysFailsLLM(nil), NewRuntime())
	// defaults only: MaxStuck=10, MaxAttemptsPerTask=3, MaxRoutes=3, MaxEntropy=6

	if got := orch.Run(context.Background()); got != OutcomeFailed {
		t.Fatalf("the route budget should govern and abandon as FAILED, got %s", got)
	}
	if want := orch.MaxRoutes * orch.MaxAttemptsPerTask; orch.steps != want {
		t.Errorf("expected the 3×3 budget (%d tries) to govern under defaults, took %d", want, orch.steps)
	}
}

// A repeated failure must hand the model an explicit, escalating directive so it
// knows to change method — the deterministic "stop, think, try something else".
func TestRepeatedFailureSurfacesDirective(t *testing.T) {
	var seen []*AnomalyPayload
	orch := NewOrchestrator("impossible", alwaysFailsLLM(&seen), NewRuntime())
	orch.MaxStuck = 0
	orch.MaxAttemptsPerTask = 0 // disable forced forks so we observe several plain repeats
	orch.MaxRoutes = 0
	orch.MaxEntropy = 100
	orch.MaxSteps = 4

	orch.Run(context.Background())

	// By the last turn the model has been told the approach failed ≥2× and given a directive.
	last := seen[len(seen)-1]
	if last == nil {
		t.Fatal("expected an anomaly on the final turn")
	}
	if last.Attempts < 2 {
		t.Errorf("expected escalating attempts count, got %d", last.Attempts)
	}
	if last.Directive == "" {
		t.Error("a repeated failure must carry a directive to change approach")
	}
}

// A genuine fix mid-run resets the per-proposition budget: succeeding clears the
// attempts so a later, unrelated failure starts fresh (no premature give-up).
func TestSuccessClearsAttemptBudget(t *testing.T) {
	dir := t.TempDir()
	made := dir + "/made"
	n := 0
	llm := &MockLLM{Respond: func(s StateSnapshot) (Task, error) {
		n++
		switch {
		case n <= 2: // two failures on proposition A
			return Task{ID: "a", Command: "true", Mode: ModeBlocking, TimeoutSec: 5,
				Assertion: Assertion{Type: "fs", Pattern: dir + "/absent", Channel: ChannelFS}}, nil
		default: // then succeed on proposition B and finish
			return Task{ID: "b", Command: "touch " + made, Mode: ModeBlocking, TimeoutSec: 5, Final: true,
				Assertion: Assertion{Type: "fs", Pattern: made, Channel: ChannelFS}}, nil
		}
	}}
	orch := NewOrchestrator("x", llm, NewRuntime())
	orch.MaxStuck = 0
	if got := orch.Run(context.Background()); got != OutcomeFinished {
		t.Fatalf("two early failures then a real success should FINISH, got %s", got)
	}
	if orch.forks != 0 {
		t.Errorf("two failures (< cap of %d) must not force a fork, got %d forks", orch.MaxAttemptsPerTask, orch.forks)
	}
}
