package ada

import (
	"context"
	"os"
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

// TestCoordinatorRejectsDoneWithUnresolvedFailures: the planner must not be able to
// declare victory on work the runtime could not achieve. A sub-goal the executor can
// never satisfy is reported as failed; if the planner then claims done, the runtime
// overrides it to STABLE (honest) rather than FINISHED (false). This locks in the
// deterministic completion backstop — completion is earned against the record, not
// the model's say-so.
func TestCoordinatorRejectsDoneWithUnresolvedFailures(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok") // a sub-goal that genuinely succeeds (so the round makes progress)
	round := 0
	planFn := func(in PlanInput) (PlanDecision, error) {
		round++
		if round == 1 {
			// First round: one achievable sub-goal (produces a fact so the round is
			// not a no-op) plus one the executor can never satisfy.
			return PlanDecision{Reason: "attempt", Subgoals: []string{
				"make file ok at " + ok,
				"do the impossible",
			}}, nil
		}
		// Later rounds: the planner (wrongly) believes it is done. The runtime must
		// refuse FINISHED because the previous round left an unresolved failure, and
		// must surface that failure to the planner.
		if len(in.Failed) == 0 {
			t.Errorf("planner should be told about the failed sub-goal, got none")
		}
		return PlanDecision{Done: true, Reason: "i think we're done"}, nil
	}
	respond := func(s StateSnapshot) (Task, error) {
		if strings.Contains(s.Objective, "file ok") {
			return Task{
				ID: "mk", Command: "touch " + ok, Mode: ModeBlocking, TimeoutSec: 5, Final: true,
				Assertion: Assertion{Type: "fs", Pattern: ok, Channel: ChannelFS},
			}, nil
		}
		return Task{
			ID: "x", Command: "true", Mode: ModeBlocking, TimeoutSec: 5,
			Assertion: Assertion{Type: "fs", Pattern: "/no/such/ada-backstop-xyz", Channel: ChannelFS},
		}, nil
	}
	llm := &MockLLM{Respond: respond, PlanFn: planFn}
	coord := NewCoordinator("objective the executor cannot fully meet", llm, llm, NewRuntime())
	coord.GoalSteps = 3
	coord.MaxRounds = 4

	outcome, dec := coord.Run(context.Background())
	if outcome == OutcomeFinished {
		t.Fatalf("must NOT report FINISHED while a sub-goal is unresolved; got FINISHED (done=%v)", dec.Done)
	}
	if outcome != OutcomeStable {
		t.Fatalf("expected STABLE backstop, got %s", outcome)
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

// TestCoordinatorAccumulatesSteps: planning mode must report the real total steps
// summed across sub-goals, not the hardcoded 0 the debug summary used to record
// (which made every planning run unmeasurable). Two sub-goals that each complete in
// one step ⇒ Steps()==2, and a clean run forks 0 times.
func TestCoordinatorAccumulatesSteps(t *testing.T) {
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

	if outcome, _ := coord.Run(context.Background()); outcome != OutcomeFinished {
		t.Fatalf("expected FINISHED, got %s", outcome)
	}
	if coord.Steps() != 2 {
		t.Errorf("expected 2 total steps across two one-step sub-goals, got %d", coord.Steps())
	}
	if coord.Forks() != 0 {
		t.Errorf("a clean run forks 0 times, got %d", coord.Forks())
	}
}

// TestCoordinatorAccumulatesForks: a sub-goal the executor can never satisfy burns
// through its route budget, and those Hard Context Forks must surface in the
// aggregate (forks were invisible while planning summaries hardcoded total_forks:0).
func TestCoordinatorAccumulatesForks(t *testing.T) {
	planFn := func(in PlanInput) (PlanDecision, error) {
		return PlanDecision{Reason: "try", Subgoals: []string{"do the impossible"}}, nil
	}
	respond := func(s StateSnapshot) (Task, error) {
		// exit 0 but an fs assertion on a path that never exists → a deterministic
		// failure that drives attempts/entropy to a Hard Context Fork, repeatedly.
		return Task{
			ID: "x", Command: "true", Mode: ModeBlocking, TimeoutSec: 5,
			Assertion: Assertion{Type: "fs", Pattern: "/no/such/ada-fork-xyz", Channel: ChannelFS},
		}, nil
	}
	llm := &MockLLM{Respond: respond, PlanFn: planFn}
	coord := NewCoordinator("impossible", llm, llm, NewRuntime())
	coord.GoalSteps = 12
	coord.MaxRounds = 1
	coord.MaxAttemptsPerTask = 2
	coord.MaxRoutes = 2
	coord.MaxStuck = 0 // disable the stuck backstop so the route budget governs

	coord.Run(context.Background())
	if coord.Forks() < 1 {
		t.Errorf("a sub-goal that exhausts its routes must report >=1 fork, got %d", coord.Forks())
	}
	if coord.Steps() < 1 {
		t.Errorf("expected steps to accumulate, got %d", coord.Steps())
	}
}

// TestCoordinatorHaltsRoundOnFailure: when an ordered sub-goal fails, the sub-goals
// AFTER it must not run that round — they usually depend on it. The coordinator
// halts the round and re-plans instead of burning the budget on doomed dependents.
func TestCoordinatorHaltsRoundOnFailure(t *testing.T) {
	var ran []string
	planFn := func(in PlanInput) (PlanDecision, error) {
		if len(in.Failed) > 0 { // after the failure, give up so the test terminates
			return PlanDecision{Done: true, Reason: "give up"}, nil
		}
		return PlanDecision{Reason: "go", Subgoals: []string{"do A", "do B"}}, nil
	}
	respond := func(s StateSnapshot) (Task, error) {
		if strings.Contains(s.Objective, "do A") {
			ran = append(ran, "A")
			return Task{ID: "a", Command: "true", Mode: ModeBlocking, TimeoutSec: 5,
				Assertion: Assertion{Type: "fs", Pattern: "/no/such/ada-halt-xyz", Channel: ChannelFS}}, nil
		}
		ran = append(ran, "B")
		return Task{ID: "b", Command: "true", Mode: ModeBlocking, TimeoutSec: 5, Final: true,
			Assertion: Assertion{Type: "exit", Pattern: "0", Channel: ChannelExitCode}}, nil
	}
	llm := &MockLLM{Respond: respond, PlanFn: planFn}
	coord := NewCoordinator("ordered objective", llm, llm, NewRuntime())
	coord.GoalSteps = 2
	coord.MaxStuck = 2
	coord.MaxRounds = 3

	coord.Run(context.Background())
	for _, g := range ran {
		if g == "B" {
			t.Fatal("sub-goal B ran even though A failed — the round must halt on the blocker")
		}
	}
}

// TestCoordinatorRePlansFromBlocker is the nginx-mirror scenario in miniature: the
// install fails with a 404-style error, the coordinator halts and re-plans, the
// planner READS last_error and inserts a corrective "refresh" sub-goal, and the
// retry then succeeds. This is "stop, work out why it failed, try a new approach."
func TestCoordinatorRePlansFromBlocker(t *testing.T) {
	dir := t.TempDir()
	refreshed := filepath.Join(dir, "refreshed")
	installed := filepath.Join(dir, "installed")
	enabled := filepath.Join(dir, "enabled")
	has := func(facts []Fact, p string) bool {
		for _, f := range facts {
			if strings.Contains(f.Statement, p) {
				return true
			}
		}
		return false
	}

	sawBlocker := false
	planFn := func(in PlanInput) (PlanDecision, error) {
		switch {
		case has(in.Facts, installed) && has(in.Facts, enabled):
			return PlanDecision{Done: true, Reason: "installed and enabled"}, nil
		case len(in.Failed) > 0 && !has(in.Facts, refreshed):
			// Diagnose the blocker: the install failed, so REPAIR (refresh) then retry.
			if strings.Contains(in.LastError, "404") {
				sawBlocker = true
			}
			return PlanDecision{Reason: "refresh then retry", Subgoals: []string{
				"ensure the package db is refreshed at " + refreshed,
				"ensure widget is installed at " + installed,
				"ensure widget is enabled at " + enabled,
			}}, nil
		default:
			return PlanDecision{Reason: "install then enable", Subgoals: []string{
				"ensure widget is installed at " + installed,
				"ensure widget is enabled at " + enabled,
			}}, nil
		}
	}
	respond := func(s StateSnapshot) (Task, error) {
		switch {
		case strings.Contains(s.Objective, "refreshed"):
			return Task{ID: "refresh", Command: "touch " + refreshed, Mode: ModeBlocking, TimeoutSec: 5, Final: true,
				Assertion: Assertion{Type: "fs", Pattern: refreshed, Channel: ChannelFS}}, nil
		case strings.Contains(s.Objective, "installed"):
			// The install fails with a 404-style error until the db has been refreshed.
			cmd := "if [ ! -f " + refreshed + " ]; then echo '404 stale db' >&2; exit 1; fi; touch " + installed
			return Task{ID: "install", Command: cmd, Mode: ModeBlocking, TimeoutSec: 5, Final: true,
				Assertion: Assertion{Type: "fs", Pattern: installed, Channel: ChannelFS}}, nil
		default: // enable
			return Task{ID: "enable", Command: "touch " + enabled, Mode: ModeBlocking, TimeoutSec: 5, Final: true,
				Assertion: Assertion{Type: "fs", Pattern: enabled, Channel: ChannelFS}}, nil
		}
	}
	llm := &MockLLM{Respond: respond, PlanFn: planFn}
	coord := NewCoordinator("install and enable widget", llm, llm, NewRuntime())
	coord.GoalSteps = 4
	coord.MaxStuck = 3

	outcome, _ := coord.Run(context.Background())
	if outcome != OutcomeFinished {
		t.Fatalf("expected FINISHED after re-planning around the blocker, got %s", outcome)
	}
	if !sawBlocker {
		t.Error("planner should have received last_error (the 404) to drive the corrective re-plan")
	}
	if !has(coord.Facts(), installed) || !has(coord.Facts(), enabled) {
		t.Errorf("widget should be installed AND enabled after recovery, facts=%+v", coord.Facts())
	}
}

// TestCoordinatorRevalidatesMemoryAtCompletion: a planner "done" resting on a fact
// carried from memory must be re-validated before FINISHED is accepted. The seeded
// memory fact is stale (its file does not exist), so the first "done" is rejected,
// the fact is dropped, and the run only finishes once the state is genuinely
// re-established — never a false completion on a stale claim.
func TestCoordinatorRevalidatesMemoryAtCompletion(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "thing") // the memory fact CLAIMS this exists
	has := func(facts []Fact, p string) bool {
		for _, f := range facts {
			if strings.Contains(f.Statement, p) {
				return true
			}
		}
		return false
	}
	planCalls := 0
	planFn := func(in PlanInput) (PlanDecision, error) {
		planCalls++
		if has(in.Facts, absent) {
			return PlanDecision{Done: true, Reason: "the fact says it exists"}, nil
		}
		return PlanDecision{Reason: "re-establish it", Subgoals: []string{"ensure thing at " + absent}}, nil
	}
	respond := func(s StateSnapshot) (Task, error) {
		return Task{ID: "mk", Command: "touch " + absent, Mode: ModeBlocking, TimeoutSec: 5, Final: true,
			Assertion: Assertion{Type: "fs", Pattern: absent, Channel: ChannelFS}}, nil
	}
	llm := &MockLLM{Respond: respond, PlanFn: planFn}
	coord := NewCoordinator("ensure thing exists", llm, llm, NewRuntime())
	coord.Seed([]Fact{{Statement: "verified: " + absent + " (file)", SourceID: memorySourceID,
		Strength: StrengthStrong, assertion: Assertion{Type: "fs", Pattern: absent + "|file", Channel: ChannelFS}}})

	outcome, _ := coord.Run(context.Background())
	if planCalls < 2 {
		t.Errorf("a stale memory fact must force a re-plan, not an instant FINISHED (planCalls=%d)", planCalls)
	}
	if outcome == OutcomeFinished {
		if _, err := os.Stat(absent); err != nil {
			t.Fatal("reported FINISHED but the asserted file does not exist — false completion from a stale memory fact")
		}
	}
}
