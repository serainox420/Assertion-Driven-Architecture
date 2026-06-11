package ada

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestCoordinatorDecomposesAndCompletes: the planner breaks an objective into two
// sub-goals, the flat loop completes each, facts accumulate across sub-goals, and
// the planner declares the objective satisfied — FINISHED with both facts.
func TestCoordinatorDecomposesAndCompletes(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")

	has := func(facts []Fact, p string) bool {
		for _, f := range facts {
			if strings.Contains(f.Statement, p) {
				return true
			}
		}
		return false
	}
	planFn := func(in PlanInput) (PlanDecision, error) {
		if has(in.Facts, a) && has(in.Facts, b) {
			return PlanDecision{Done: true, Reason: "both present"}, nil
		}
		var subs []string
		if !has(in.Facts, a) {
			subs = append(subs, "make file a at "+a)
		}
		if !has(in.Facts, b) {
			subs = append(subs, "make file b at "+b)
		}
		return PlanDecision{Reason: "make the files", Subgoals: subs}, nil
	}
	respond := func(s StateSnapshot) (Task, error) {
		if s.MainObjective == "" {
			t.Error("executor should see the pinned main_objective in planning mode")
		}
		target := a
		if strings.Contains(s.Objective, "file b") {
			target = b
		}
		return Task{
			ID: "mk", Command: "touch " + target, Mode: ModeBlocking, TimeoutSec: 5, Final: true,
			Assertion: Assertion{Type: "fs", Pattern: target, Channel: ChannelFS},
		}, nil
	}

	llm := &MockLLM{Respond: respond, PlanFn: planFn}
	coord := NewCoordinator("make files a and b", llm, llm, NewRuntime())

	outcome, dec := coord.Run(context.Background())
	if outcome != OutcomeFinished {
		t.Fatalf("expected FINISHED, got %s", outcome)
	}
	if !dec.Done {
		t.Error("final decision should be done")
	}
	if len(coord.Facts()) != 2 {
		t.Errorf("expected 2 accumulated facts, got %d", len(coord.Facts()))
	}
}

// TestCoordinatorStopsWhenStuck: an objective the executor can never satisfy must
// terminate (STABLE/EXHAUSTED), not spin forever — termination cannot depend on
// the planner ever declaring done.
func TestCoordinatorStopsWhenStuck(t *testing.T) {
	planFn := func(in PlanInput) (PlanDecision, error) {
		return PlanDecision{Reason: "keep trying", Subgoals: []string{"do the impossible"}}, nil
	}
	respond := func(s StateSnapshot) (Task, error) {
		// assert a path that never exists → the sub-goal can never pass
		return Task{
			ID: "x", Command: "true", Mode: ModeBlocking, TimeoutSec: 5,
			Assertion: Assertion{Type: "fs", Pattern: "/no/such/ada-coordinator-xyz", Channel: ChannelFS},
		}, nil
	}
	llm := &MockLLM{Respond: respond, PlanFn: planFn}
	coord := NewCoordinator("impossible objective", llm, llm, NewRuntime())
	coord.GoalSteps = 3
	coord.MaxRounds = 3

	outcome, _ := coord.Run(context.Background())
	if outcome != OutcomeStable && outcome != OutcomeExhausted {
		t.Fatalf("a stuck run must end STABLE or EXHAUSTED, got %s", outcome)
	}
}

// TestCoordinatorRePlans: the planner adds a new sub-goal in a later round that it
// could not know about initially — the flat-list plan extends as facts accumulate.
func TestCoordinatorRePlans(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
	has := func(facts []Fact, p string) bool {
		for _, f := range facts {
			if strings.Contains(f.Statement, p) {
				return true
			}
		}
		return false
	}
	planFn := func(in PlanInput) (PlanDecision, error) {
		switch {
		case !has(in.Facts, first):
			return PlanDecision{Reason: "step 1", Subgoals: []string{"make first at " + first}}, nil
		case !has(in.Facts, second):
			// only revealed after the first sub-goal is done (a re-plan)
			return PlanDecision{Reason: "step 2", Subgoals: []string{"make second at " + second}}, nil
		default:
			return PlanDecision{Done: true, Reason: "all done"}, nil
		}
	}
	respond := func(s StateSnapshot) (Task, error) {
		target := first
		if strings.Contains(s.Objective, "second") {
			target = second
		}
		return Task{
			ID: "mk", Command: "touch " + target, Mode: ModeBlocking, TimeoutSec: 5, Final: true,
			Assertion: Assertion{Type: "fs", Pattern: target, Channel: ChannelFS},
		}, nil
	}
	llm := &MockLLM{Respond: respond, PlanFn: planFn}
	coord := NewCoordinator("two-phase objective", llm, llm, NewRuntime())

	if outcome, _ := coord.Run(context.Background()); outcome != OutcomeFinished {
		t.Fatalf("expected FINISHED across re-planned rounds, got %s", outcome)
	}
	if len(coord.Facts()) != 2 {
		t.Errorf("expected 2 facts from the two phases, got %d", len(coord.Facts()))
	}
}
