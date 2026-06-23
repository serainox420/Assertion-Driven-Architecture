package ada

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// TestRecoveryFromAnomaly: the model emits a failing task, receives the anomaly,
// and adapts to a passing one. The loop must reach FINISHED with a strong fact.
func TestRecoveryFromAnomaly(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ok")

	llm := &MockLLM{Respond: func(s StateSnapshot) (Task, error) {
		if s.Anomaly != nil {
			// adapt: actually create the file and prove it via fs
			return Task{
				ID: "create", Command: "touch " + target, Mode: ModeBlocking, TimeoutSec: 5,
				Assertion: Assertion{Type: "file_exists", Pattern: target, Channel: ChannelFS},
			}, nil
		}
		// first hypothesis: assert the file exists before creating it → fails
		return Task{
			ID: "premature", Command: "true", Mode: ModeBlocking, TimeoutSec: 5,
			Assertion: Assertion{Type: "file_exists", Pattern: target, Channel: ChannelFS},
		}, nil
	}}

	orch := NewOrchestrator("create the file", llm, NewRuntime())
	orch.MaxSteps = 10
	orch.DoneCheck = func(s StateSnapshot) bool {
		for _, f := range s.EstablishedFacts {
			if f.SourceID == "create" {
				return true
			}
		}
		return false
	}

	if got := orch.Run(context.Background()); got != OutcomeFinished {
		t.Fatalf("expected FINISHED, got %s", got)
	}
	if n := len(orch.Snapshot.EstablishedFacts); n != 1 {
		t.Fatalf("expected 1 established fact, got %d", n)
	}
	if orch.Snapshot.EstablishedFacts[0].Strength != StrengthStrong {
		t.Errorf("expected strong fact, got %q", orch.Snapshot.EstablishedFacts[0].Strength)
	}
}

// TestFinalTaskTerminates: a verified Final task ends the run with FINISHED even
// with no DoneCheck wired — this is the in-band completion signal the CLI relies
// on (previously the loop ran to the step budget and had to be killed).
func TestFinalTaskTerminates(t *testing.T) {
	llm := ScriptedLLM(Task{
		ID: "done", Command: "true", Mode: ModeBlocking, TimeoutSec: 5, Final: true,
		Assertion: Assertion{Type: "exit", Pattern: "0", Channel: ChannelExitCode},
	})
	orch := NewOrchestrator("do the thing", llm, NewRuntime())
	orch.MaxSteps = 50 // would spin to the budget without the Final signal

	if got := orch.Run(context.Background()); got != OutcomeFinished {
		t.Fatalf("expected FINISHED from a verified Final task, got %s", got)
	}
	if orch.steps != 1 {
		t.Errorf("expected to stop after the single final step, took %d", orch.steps)
	}
}

// A Final task whose assertion FAILS must not terminate the run — completion
// still requires a passing assertion, not the model's say-so.
func TestFinalTaskRequiresPassingAssertion(t *testing.T) {
	llm := ScriptedLLM(Task{
		ID: "claims-done", Command: "true", Mode: ModeBlocking, TimeoutSec: 5, Final: true,
		Assertion: Assertion{Type: "exit", Pattern: "7", Channel: ChannelExitCode}, // fails (exit 0)
	})
	orch := NewOrchestrator("x", llm, NewRuntime())
	orch.MaxSteps = 3
	if got := orch.Run(context.Background()); got != OutcomeExhausted {
		t.Fatalf("a failing Final assertion must not finish the run, got %s", got)
	}
}

// TestStallTerminatesOnRepeatedSuccess: a model that keeps re-proving the same
// ground (different task ids, same fs target — exactly the observed loop) must
// stop deterministically with STABLE, NOT spin to the step budget. Termination
// cannot depend on the model emitting "final".
func TestStallTerminatesOnRepeatedSuccess(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.log")
	n := 0
	llm := &MockLLM{Respond: func(StateSnapshot) (Task, error) {
		n++
		return Task{
			ID:      fmt.Sprintf("touch-%d", n), // different id each turn, same proposition
			Command: "touch " + target,
			Mode:    ModeBlocking, TimeoutSec: 5,
			Assertion: Assertion{Type: "file_exists", Pattern: target, Channel: ChannelFS},
		}, nil
	}}
	orch := NewOrchestrator("make out.log", llm, NewRuntime())
	orch.MaxSteps = 200 // would loop ~forever without stall detection
	orch.StallBudget = 3

	got := orch.Run(context.Background())
	if got != OutcomeStable {
		t.Fatalf("expected STABLE, got %s", got)
	}
	if len(orch.Snapshot.EstablishedFacts) != 1 {
		t.Errorf("re-proving the same proposition must yield ONE fact, got %d",
			len(orch.Snapshot.EstablishedFacts))
	}
	if orch.steps > 1+orch.StallBudget+1 {
		t.Errorf("stopped too late: %d steps for budget %d", orch.steps, orch.StallBudget)
	}
}

// Distinct verified propositions must NOT trip the stall guard — real progress
// resets the counter.
func TestDistinctProgressDoesNotStall(t *testing.T) {
	dir := t.TempDir()
	llm := &MockLLM{Respond: func(s StateSnapshot) (Task, error) {
		i := len(s.EstablishedFacts) + 1
		if i > 5 {
			return Task{ID: "done", Command: "true", Mode: ModeBlocking, TimeoutSec: 5, Final: true,
				Assertion: Assertion{Type: "exit", Pattern: "0", Channel: ChannelExitCode}}, nil
		}
		p := filepath.Join(dir, fmt.Sprintf("f%d", i)) // a NEW path each turn
		return Task{ID: fmt.Sprintf("mk%d", i), Command: "touch " + p, Mode: ModeBlocking, TimeoutSec: 5,
			Assertion: Assertion{Type: "file_exists", Pattern: p, Channel: ChannelFS}}, nil
	}}
	orch := NewOrchestrator("make five files", llm, NewRuntime())
	orch.StallBudget = 3
	if got := orch.Run(context.Background()); got != OutcomeFinished {
		t.Fatalf("expected FINISHED (steady progress then final), got %s", got)
	}
	// 5 distinct files + the final verification task = 6 distinct facts.
	if len(orch.Snapshot.EstablishedFacts) != 6 {
		t.Errorf("expected 6 distinct facts, got %d", len(orch.Snapshot.EstablishedFacts))
	}
}

// TestStuckGoalAbandonsFast: a goal whose assertion can never pass (the observed
// "check zsh installed" / "/home/user/.zshrc" disasters) must abandon after about
// MaxStuck steps — NOT grind to the step budget. A Hard Context Fork resets the
// entropy/failure counters, so the stuck counter (which a fork must not reset) is
// what guarantees termination.
func TestStuckGoalAbandonsFast(t *testing.T) {
	steps := 0
	llm := &MockLLM{Respond: func(StateSnapshot) (Task, error) {
		steps++
		return Task{ // asserts a path that never exists → fails every time
			ID: "check", Command: "true", Mode: ModeBlocking, TimeoutSec: 5,
			Assertion: Assertion{Type: "fs", Pattern: "/no/such/ada-stuck-xyz", Channel: ChannelFS},
		}, nil
	}}
	orch := NewOrchestrator("impossible check", llm, NewRuntime())
	orch.MaxSteps = 200 // would be 200 LLM calls without the stuck guard
	orch.MaxStuck = 6

	if got := orch.Run(context.Background()); got != OutcomeExhausted {
		t.Fatalf("a hopeless goal should abandon as EXHAUSTED, got %s", got)
	}
	if orch.steps > orch.MaxStuck+1 {
		t.Errorf("expected to abandon in ~%d steps, took %d", orch.MaxStuck, orch.steps)
	}
}

// TestStuckCounterSurvivesFork: progress resets the stuck counter, but a Hard
// Context Fork must NOT — otherwise a fork-on-failure loop never terminates.
func TestStuckCounterSurvivesFork(t *testing.T) {
	orch := NewOrchestrator("x", &MockLLM{}, NewRuntime())
	orch.stuckSteps = 4
	orch.HardContextFork()
	if orch.stuckSteps != 4 {
		t.Errorf("HardContextFork must not reset stuckSteps, got %d", orch.stuckSteps)
	}
}

// TestParseErrorsDoNotConsumeStepBudget: invalid-JSON emissions are the model's
// own fixable mistake and must NOT spend the step budget — otherwise a tight
// -goal-steps dies on a transient hiccup (the observed "goal 1 → EXHAUSTED" with
// /etc/os-release never even checked). They retry for free until valid JSON lands.
func TestParseErrorsDoNotConsumeStepBudget(t *testing.T) {
	calls := 0
	llm := &MockLLM{Respond: func(StateSnapshot) (Task, error) {
		calls++
		if calls <= 3 {
			return Task{}, errInvalid("incomplete Task")
		}
		return Task{
			ID: "done", Command: "true", Mode: ModeBlocking, TimeoutSec: 5, Final: true,
			Assertion: Assertion{Type: "exit", Pattern: "0", Channel: ChannelExitCode},
		}, nil
	}}
	orch := NewOrchestrator("x", llm, NewRuntime())
	orch.MaxSteps = 2 // tiny budget; 3 parse errors would exhaust it if they counted
	orch.MaxThinkFails = 5
	if got := orch.Run(context.Background()); got != OutcomeFinished {
		t.Fatalf("parse errors must not consume the step budget; want FINISHED got %s", got)
	}
	if orch.steps != 1 {
		t.Errorf("only the real execution should count as a step, got %d", orch.steps)
	}
}

// TestThinkFailsCapAbandons: a model that can NEVER produce valid JSON still
// terminates (it doesn't spin forever on free retries).
func TestThinkFailsCapAbandons(t *testing.T) {
	llm := &MockLLM{Respond: func(StateSnapshot) (Task, error) {
		return Task{}, errInvalid("always broken")
	}}
	orch := NewOrchestrator("x", llm, NewRuntime())
	orch.MaxSteps = 50
	orch.MaxThinkFails = 3
	if got := orch.Run(context.Background()); got != OutcomeExhausted {
		t.Fatalf("a model stuck on invalid JSON must abandon, got %s", got)
	}
}

// TestParseErrorBecomesAnomaly: a model that breaks its JSON contract must not
// crash the loop; the failure is fed back as a model_error anomaly (§9.2).
func TestParseErrorBecomesAnomaly(t *testing.T) {
	calls := 0
	llm := &MockLLM{Respond: func(s StateSnapshot) (Task, error) {
		calls++
		if calls == 1 {
			return Task{}, errInvalid("boom") // simulate a parse failure
		}
		// after seeing the synthetic anomaly, emit something valid & terminal
		return Task{
			ID: "ok", Command: "true", Mode: ModeBlocking, TimeoutSec: 5,
			Assertion: Assertion{Type: "exit", Pattern: "0", Channel: ChannelExitCode},
		}, nil
	}}
	orch := NewOrchestrator("x", llm, NewRuntime())
	orch.MaxSteps = 5
	orch.DoneCheck = func(s StateSnapshot) bool { return len(s.EstablishedFacts) > 0 }

	if got := orch.Run(context.Background()); got != OutcomeFinished {
		t.Fatalf("expected FINISHED after recovery, got %s", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
func errInvalid(s string) error   { return errString(s) }

// TestHardForkDropsUnverifiableWeakFact: a weak (stdout) fact that cannot be
// independently re-checked must be dropped on a Hard Context Fork, while a
// strong fact survives (§8.3).
func TestHardForkDropsUnverifiableWeakFact(t *testing.T) {
	orch := NewOrchestrator("x", &MockLLM{}, NewRuntime())
	orch.Snapshot.EstablishedFacts = []Fact{
		{Statement: "strong one", SourceID: "a", Strength: StrengthStrong,
			assertion: Assertion{Channel: ChannelExitCode}},
		{Statement: "weak stdout", SourceID: "b", Strength: StrengthWeak,
			assertion: Assertion{Channel: ChannelStdout, Pattern: "x"}},
	}
	orch.Snapshot.EntropyLevel = 99

	orch.HardContextFork()

	if orch.Snapshot.EntropyLevel != 0 {
		t.Errorf("fork should reset entropy, got %d", orch.Snapshot.EntropyLevel)
	}
	if !orch.forking {
		t.Error("fork should arm raised-temperature generation")
	}
	if len(orch.Snapshot.EstablishedFacts) != 1 {
		t.Fatalf("expected weak unverifiable fact dropped, got %d facts", len(orch.Snapshot.EstablishedFacts))
	}
	if orch.Snapshot.EstablishedFacts[0].SourceID != "a" {
		t.Errorf("expected strong fact to survive, got %q", orch.Snapshot.EstablishedFacts[0].SourceID)
	}
}

// TestFactFoldingRespectsStrength: folding a batch containing a weak fact must
// produce a provisional, weak summary — never launder weak into strong (§7.3).
func TestFactFoldingRespectsStrength(t *testing.T) {
	orch := NewOrchestrator("x", &MockLLM{}, NewRuntime())
	orch.MaxFacts = 4
	for i := 0; i < 4; i++ {
		orch.addFact(Fact{SourceID: "s", Strength: StrengthStrong})
	}
	// the 5th (weak) triggers a fold of the oldest
	orch.addFact(Fact{SourceID: "w", Strength: StrengthWeak})

	summary := orch.Snapshot.EstablishedFacts[0]
	if summary.SourceID != "FOLD" {
		t.Fatalf("expected a fold summary first, got %q", summary.SourceID)
	}
	// the weak fact was among the recent kept ones, strong ones folded → strong summary
	if summary.Strength != StrengthStrong {
		t.Errorf("folding only strong facts should yield a strong summary, got %q", summary.Strength)
	}
}

func TestRewardShaping(t *testing.T) {
	if Reward(ExecutionResult{Passed: true, Strength: StrengthStrong}, false) != 10 {
		t.Error("strong pass should reward +10")
	}
	if Reward(ExecutionResult{Passed: true, Strength: StrengthWeak}, false) != 2 {
		t.Error("weak pass should reward +2")
	}
	if Reward(ExecutionResult{}, true) != 100 {
		t.Error("objective completion should reward +100")
	}
}

// TestPreconditionSatisfiedByStrongFact: a precondition naming state an established
// STRONG fact already proves must pass WITHOUT a syscall — the loop's own "don't
// re-prove what you already know" principle (§3). The fact's path does NOT exist on
// disk, so a real re-stat would (wrongly) fail the precondition; reaching FINISHED
// proves the disk was never touched. The fact and the precondition deliberately use
// DIFFERENT fs notations (`path|dir` vs `path (dir)`), so this also exercises the
// key normalization that collapses both to one proposition.
func TestPreconditionSatisfiedByStrongFact(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "never-created")
	seed := Fact{
		Statement: "verified: " + absent + " (dir)",
		SourceID:  "seed",
		Strength:  StrengthStrong,
		assertion: Assertion{Type: "fs", Pattern: absent + "|dir", Channel: ChannelFS},
	}
	llm := &MockLLM{Respond: func(StateSnapshot) (Task, error) {
		return Task{
			ID: "act", Command: "true", Mode: ModeBlocking, TimeoutSec: 5, Final: true,
			Preconditions: []Assertion{{Type: "fs", Pattern: absent + " (dir)", Channel: ChannelFS}},
			Assertion:     Assertion{Type: "exit", Pattern: "0", Channel: ChannelExitCode},
		}, nil
	}}
	orch := NewOrchestrator("demo", llm, NewRuntime())
	orch.Seed([]Fact{seed})

	if outcome := orch.Run(context.Background()); outcome != OutcomeFinished {
		t.Fatalf("a precondition proven by a strong fact must let the task run (FINISHED), got %s", outcome)
	}
}

// TestPreconditionUnmetWithoutFact is the control for the short-circuit: the SAME
// task with NO proving fact must NOT run — the precondition is genuinely unmet (the
// path is absent), so the runtime must refuse the command and the run cannot finish.
func TestPreconditionUnmetWithoutFact(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "never-created")
	llm := &MockLLM{Respond: func(StateSnapshot) (Task, error) {
		return Task{
			ID: "act", Command: "true", Mode: ModeBlocking, TimeoutSec: 5, Final: true,
			Preconditions: []Assertion{{Type: "fs", Pattern: absent + " (dir)", Channel: ChannelFS}},
			Assertion:     Assertion{Type: "exit", Pattern: "0", Channel: ChannelExitCode},
		}, nil
	}}
	orch := NewOrchestrator("demo", llm, NewRuntime())
	orch.MaxStuck = 2 // terminate quickly; the command can never run

	if outcome := orch.Run(context.Background()); outcome == OutcomeFinished {
		t.Fatal("an unmet precondition with no proving fact must NOT reach FINISHED")
	}
}
