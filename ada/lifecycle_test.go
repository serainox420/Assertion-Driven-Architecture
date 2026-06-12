package ada

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The fs `contains:` predicate reads the file and matches an anchored regex —
// proving CONTENTS independently (strong), the bulletproof "read it back to be
// sure the change landed" check.
func TestFSContentPredicate(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("mode: prod\nport: 80\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime()
	run := func(pattern string) ExecutionResult {
		return rt.Execute(Task{
			ID: "t", Command: "true", Mode: ModeBlocking, TimeoutSec: 5,
			Assertion: Assertion{Type: "content", Pattern: pattern, Channel: ChannelFS},
		})
	}
	if r := run(cfg + "|contains:^mode: prod$"); !r.Passed {
		t.Errorf("anchored content match should pass, got %+v", r.Anomaly)
	} else if r.Strength != StrengthStrong {
		t.Errorf("fs content match must be strong, got %q", r.Strength)
	}
	if run(cfg + "|contains:^mode: dev$").Passed {
		t.Error("content that is not present must fail")
	}
	// alias spellings
	if !run(cfg + "|matches:^port: 80$").Passed {
		t.Error("matches: alias should work")
	}
	// a lazy, unanchored content regex is a false positive — reject it
	if run(cfg + "|contains:.*").Passed {
		t.Error("lazy unanchored content pattern must be rejected, not passed")
	}
}

// A precondition that does not hold must STOP the command from running — a wrong
// guess costs no action. We prove the command never ran by having it create a
// sentinel that must be absent afterward.
func TestPreconditionGatesCommand(t *testing.T) {
	dir := t.TempDir()
	guard := filepath.Join(dir, "guard-does-not-exist")
	sentinel := filepath.Join(dir, "sentinel")
	rt := NewRuntime()
	res := rt.Execute(Task{
		ID: "guarded", Command: "touch " + sentinel, Mode: ModeBlocking, TimeoutSec: 5,
		Preconditions: []Assertion{{Type: "fs", Pattern: guard, Channel: ChannelFS}},
		Assertion:     Assertion{Type: "fs", Pattern: sentinel, Channel: ChannelFS},
	})
	if res.Passed {
		t.Fatal("a task with an unmet precondition must not pass")
	}
	if res.Anomaly.FailureClass != ClassPrecondition {
		t.Errorf("expected precondition failure class, got %q", res.Anomaly.FailureClass)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("the command ran despite an unmet precondition — it must be skipped")
	}
}

// A met precondition lets the command run normally.
func TestPreconditionMetRunsCommand(t *testing.T) {
	dir := t.TempDir() // exists, so the precondition holds
	out := filepath.Join(dir, "made")
	rt := NewRuntime()
	res := rt.Execute(Task{
		ID: "ok", Command: "touch " + out, Mode: ModeBlocking, TimeoutSec: 5,
		Preconditions: []Assertion{{Type: "fs", Pattern: dir, Channel: ChannelFS}},
		Assertion:     Assertion{Type: "fs", Pattern: out, Channel: ChannelFS},
	})
	if !res.Passed {
		t.Fatalf("a met precondition should let the command run and pass, got %+v", res.Anomaly)
	}
}

// A precondition on a non-independent channel is a contract error (model_error):
// it can't be checked without running something.
func TestPreconditionInvalidChannel(t *testing.T) {
	rt := NewRuntime()
	res := rt.Execute(Task{
		ID: "bad", Command: "true", Mode: ModeBlocking, TimeoutSec: 5,
		Preconditions: []Assertion{{Type: "x", Pattern: "0", Channel: ChannelExitCode}},
		Assertion:     Assertion{Type: "exit", Pattern: "0", Channel: ChannelExitCode},
	})
	if res.Passed || res.Anomaly.FailureClass != ClassModelError {
		t.Fatalf("a precondition on a non-independent channel must be a model_error, got %+v", res)
	}
}

// A weak (stdout) primary corroborated by an independent postcondition becomes a
// STRONG fact — the state was genuinely observed, not just narrated (§3.2).
func TestPostconditionCorroboratesAndUpgrades(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.txt")
	rt := NewRuntime()
	res := rt.Execute(Task{
		ID: "write", Command: "printf hi > " + target, Mode: ModeBlocking, TimeoutSec: 5,
		Assertion:      Assertion{Type: "regex", Pattern: "^$", Channel: ChannelStdout}, // weak, trivially true (no stdout)
		Postconditions: []Assertion{{Type: "fs", Pattern: target + "|nonempty", Channel: ChannelFS}},
	})
	if !res.Passed {
		t.Fatalf("corroborated result should pass, got %+v", res.Anomaly)
	}
	if res.Strength != StrengthStrong {
		t.Errorf("an independent postcondition must upgrade a weak primary to strong, got %q", res.Strength)
	}
}

// A passing primary whose postcondition disagrees is a FAILURE: the command
// claimed success but the independent channel says the goal wasn't met.
func TestPostconditionFailureFailsStep(t *testing.T) {
	dir := t.TempDir()
	rt := NewRuntime()
	res := rt.Execute(Task{
		ID: "claims", Command: "true", Mode: ModeBlocking, TimeoutSec: 5,
		Assertion:      Assertion{Type: "exit", Pattern: "0", Channel: ChannelExitCode}, // passes
		Postconditions: []Assertion{{Type: "fs", Pattern: filepath.Join(dir, "absent"), Channel: ChannelFS}},
	})
	if res.Passed {
		t.Fatal("a failed postcondition must fail the step even though the primary passed")
	}
	if res.Anomaly == nil || res.Anomaly.Expected == "" {
		t.Fatalf("expected a POSTCONDITION_FAILED anomaly, got %+v", res)
	}
}

// Seeded facts are treated as already-known: re-proving one is a no-op, not a new
// fact (the basis of "learn it once").
func TestSeedMarksFactsKnown(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "known")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	llm := ScriptedLLM(Task{
		ID: "reprove", Command: "true", Mode: ModeBlocking, TimeoutSec: 5,
		Assertion: Assertion{Type: "fs", Pattern: target, Channel: ChannelFS},
	})
	orch := NewOrchestrator("prove the known file", llm, NewRuntime())
	orch.StallBudget = 1
	orch.Seed([]Fact{{Statement: "known file exists", SourceID: "MEMORY", Strength: StrengthStrong,
		assertion: Assertion{Type: "fs", Pattern: target, Channel: ChannelFS}}})

	orch.Run(context.Background())
	// The seeded fact is the only one; re-proving it must NOT add a duplicate.
	if n := len(orch.Snapshot.EstablishedFacts); n != 1 {
		t.Errorf("re-proving a seeded fact must not add a new fact, got %d", n)
	}
}
