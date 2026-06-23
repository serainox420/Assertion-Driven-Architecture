package ada

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// recordingLLM emits a caller-supplied task each turn and records the snapshot the
// model is handed, so a test can assert on exactly what the stateless model sees —
// the anomaly's failed_command / already_tried, any notice, and blocked_approaches.
func recordingLLM(seen *[]StateSnapshot, next func(n int) Task) *MockLLM {
	n := 0
	return &MockLLM{Respond: func(s StateSnapshot) (Task, error) {
		if seen != nil {
			*seen = append(*seen, s)
		}
		n++
		return next(n), nil
	}}
}

// The model is stateless between turns; on a failure the runtime must hand back the
// EXACT command that just failed and a growing list of distinct commands already
// tried for this target — otherwise the model cannot see what to avoid and (at temp
// 0) regenerates the identical command. This is the core fix for "it keeps trying
// the exact same thing when it failed".
func TestAnomalySurfacesFailedCommandAndTried(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "never")
	var snaps []StateSnapshot
	// Each turn emits a DIFFERENT command, all asserting the same (absent) path, so
	// the proposition is constant while the tried list accumulates distinct commands.
	llm := recordingLLM(&snaps, func(n int) Task {
		return Task{
			ID: "try", Command: "echo attempt-" + itoa(n), Mode: ModeBlocking, TimeoutSec: 5,
			Assertion: Assertion{Type: "fs", Pattern: absent, Channel: ChannelFS},
		}
	})
	orch := NewOrchestrator("impossible", llm, NewRuntime())
	// Isolate the messaging from the budgets: no forks, no stuck guard — just observe
	// several plain failures so the tried list can grow within one route.
	orch.MaxStuck = 0
	orch.MaxAttemptsPerTask = 0
	orch.MaxRoutes = 0
	orch.MaxEntropy = 100
	orch.MaxSteps = 4

	orch.Run(context.Background())

	// Turn 2 onward must show the previous turn's command and a non-empty tried list.
	if len(snaps) < 3 {
		t.Fatalf("expected at least 3 turns, got %d", len(snaps))
	}
	second := snaps[1].Anomaly
	if second == nil {
		t.Fatal("turn 2 must carry the anomaly from turn 1")
	}
	if second.Command != "echo attempt-1" {
		t.Errorf("anomaly must echo the failed command, got %q", second.Command)
	}
	if len(second.Tried) < 1 || !strings.Contains(second.Tried[0], "attempt-1") {
		t.Errorf("tried list must record the first attempt, got %v", second.Tried)
	}
	// By the last turn the tried list has grown with the distinct commands.
	last := snaps[len(snaps)-1].Anomaly
	if last == nil || len(last.Tried) < 2 {
		t.Fatalf("tried list must accumulate distinct commands, got %+v", last)
	}
}

// Re-sending a byte-identical command (the "banging the same door" loop) earns the
// sharpest directive, regardless of attempt count — and the tried list does NOT
// double-count it.
func TestVerbatimResendGetsSharpDirective(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "never")
	var snaps []StateSnapshot
	llm := recordingLLM(&snaps, func(int) Task { // SAME command every turn
		return Task{
			ID: "same", Command: "echo identical", Mode: ModeBlocking, TimeoutSec: 5,
			Assertion: Assertion{Type: "fs", Pattern: absent, Channel: ChannelFS},
		}
	})
	orch := NewOrchestrator("impossible", llm, NewRuntime())
	orch.MaxStuck = 0
	orch.MaxAttemptsPerTask = 0
	orch.MaxRoutes = 0
	orch.MaxEntropy = 100
	orch.MaxSteps = 3

	orch.Run(context.Background())

	last := snaps[len(snaps)-1].Anomaly
	if last == nil {
		t.Fatal("expected an anomaly")
	}
	if !strings.Contains(last.Directive, "IDENTICAL") {
		t.Errorf("a verbatim resend must get the IDENTICAL-command directive, got %q", last.Directive)
	}
	// One distinct command ⇒ exactly one tried entry, no matter how many resends.
	if len(last.Tried) != 1 {
		t.Errorf("a verbatim resend must not grow the tried list, got %v", last.Tried)
	}
}

// A Hard Context Fork must distill the dead route's failed commands into
// blocked_approaches and carry them ACROSS the fork — so the next strategy can
// avoid them, instead of the fork wiping all memory and rediscovering the same
// dead command (the "pointless strategy change" the user reported).
func TestForkPopulatesBlockedApproaches(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "never")
	var snaps []StateSnapshot
	llm := recordingLLM(&snaps, func(n int) Task {
		return Task{
			ID: "try", Command: "echo route-cmd-" + itoa(n), Mode: ModeBlocking, TimeoutSec: 5,
			Assertion: Assertion{Type: "fs", Pattern: absent, Channel: ChannelFS},
		}
	})
	orch := NewOrchestrator("impossible", llm, NewRuntime())
	// Stock defaults: fs-fail-with-exit-0 is env_deterministic (weight 3), so entropy
	// hits the ceiling and forks after 2 failures; 3 routes ⇒ FAILED.
	if got := orch.Run(context.Background()); got != OutcomeFailed {
		t.Fatalf("a hopeless goal should abandon as FAILED, got %s", got)
	}
	if orch.forks == 0 {
		t.Fatal("expected at least one fork")
	}
	if len(orch.Snapshot.BlockedApproaches) == 0 {
		t.Fatal("a fork must record the dead route's commands in blocked_approaches")
	}
	// The very first command must still be remembered as blocked after several forks.
	joined := strings.Join(orch.Snapshot.BlockedApproaches, "\n")
	if !strings.Contains(joined, "route-cmd-1") {
		t.Errorf("blocked_approaches must retain abandoned commands, got %v", orch.Snapshot.BlockedApproaches)
	}
}

// Re-proving a fact the agent already holds passes, but adds nothing. The runtime
// must flag this with a NOTICE on the next turn — telling the model to finalize or
// advance — so a model that never emits final:true does not burn StallBudget+1
// steps re-proving every already-satisfied sub-goal.
func TestDupSuccessSetsNotice(t *testing.T) {
	target := filepath.Join(t.TempDir(), "out.log")
	var snaps []StateSnapshot
	llm := recordingLLM(&snaps, func(int) Task { // same proposition every turn
		return Task{
			ID: "mk", Command: "touch " + target, Mode: ModeBlocking, TimeoutSec: 5,
			Assertion: Assertion{Type: "fs", Pattern: target, Channel: ChannelFS},
		}
	})
	orch := NewOrchestrator("make out.log", llm, NewRuntime())
	orch.StallBudget = 5 // room to observe the notice before the stall guard stops us

	orch.Run(context.Background())

	sawNotice := false
	for _, s := range snaps {
		if strings.Contains(s.Notice, "ALREADY PROVEN") {
			sawNotice = true
		}
	}
	if !sawNotice {
		t.Error("re-proving an established fact must surface an ALREADY PROVEN notice")
	}
	// The first turn (nothing proven yet) must NOT carry the notice.
	if snaps[0].Notice != "" {
		t.Errorf("the first turn must have no notice, got %q", snaps[0].Notice)
	}
}

// A command blocked only by an unmet PRECONDITION was never actually tested — it may
// be exactly right once the prerequisite exists. A fork must therefore NOT add it to
// blocked_approaches, and its directive must steer toward establishing the prerequisite
// rather than abandoning the command.
func TestPreconditionFailureNotBlocked(t *testing.T) {
	absentPrereq := filepath.Join(t.TempDir(), "prereq") // never created → precondition always unmet
	target := filepath.Join(t.TempDir(), "target")
	var snaps []StateSnapshot
	llm := recordingLLM(&snaps, func(int) Task {
		return Task{
			ID: "act", Command: "touch " + target, Mode: ModeBlocking, TimeoutSec: 5,
			Preconditions: []Assertion{{Type: "fs", Pattern: absentPrereq, Channel: ChannelFS}},
			Assertion:     Assertion{Type: "fs", Pattern: target, Channel: ChannelFS},
		}
	})
	orch := NewOrchestrator("blocked on a prerequisite", llm, NewRuntime())
	// Precondition failures (weight 1) reach the attempt cap (3) and force forks; 3
	// routes ⇒ FAILED. We only care that the command never becomes a blocked approach.
	orch.Run(context.Background())

	for _, b := range orch.Snapshot.BlockedApproaches {
		if strings.Contains(b, "touch "+target) {
			t.Fatalf("a precondition-blocked command must NOT be a blocked approach, got %v", orch.Snapshot.BlockedApproaches)
		}
	}
	// The directive the model sees must point at the precondition, not "don't resend".
	last := snaps[len(snaps)-1].Anomaly
	if last == nil || !strings.Contains(last.Directive, "precondition") {
		t.Errorf("a precondition failure must direct the model to establish the prerequisite, got %+v", last)
	}
}

// The recovery directive is specialized by failure class and escalates on a resend —
// the deterministic "stop, think, change METHOD" guidance, actionable from the first
// failure for the deterministic classes.
func TestRecoveryDirectiveByClass(t *testing.T) {
	if d := recoveryDirective(ClassPrecondition, 1, false); !strings.Contains(d, "precondition") {
		t.Errorf("precondition directive should name the precondition, got %q", d)
	}
	if d := recoveryDirective(ClassEnvDeterministic, 1, false); d == "" {
		t.Error("env_deterministic should give guidance from the first failure")
	}
	if d := recoveryDirective(ClassModelError, 1, false); !strings.Contains(d, "malformed") {
		t.Errorf("model_error directive should name the defect, got %q", d)
	}
	// A genuinely transient FIRST failure is not nagged (an honest retry is fine).
	if d := recoveryDirective(ClassTransient, 1, false); d != "" {
		t.Errorf("a transient first failure should not be nagged, got %q", d)
	}
	// But a repeat is.
	if d := recoveryDirective(ClassTransient, 2, false); d == "" {
		t.Error("a repeated transient failure must carry a directive")
	}
	// A resend always escalates to the sharpest signal.
	if d := recoveryDirective(ClassTransient, 1, true); !strings.Contains(d, "IDENTICAL") {
		t.Errorf("a resend must escalate, got %q", d)
	}
}

// A raised temperature explores nothing if the nucleus stays pinned low. NucleusForTemp
// must widen top_p when sampling hot (so a fork can actually reach a different command)
// and leave a deterministic low temperature untouched.
func TestNucleusWidensWhenHot(t *testing.T) {
	if got := NucleusForTemp(0.0, 0.1); got != 0.1 {
		t.Errorf("cold sampling must keep the deterministic nucleus, got %v", got)
	}
	if got := NucleusForTemp(0.8, 0.1); got < 0.9 {
		t.Errorf("hot sampling must widen the nucleus to explore, got %v", got)
	}
	// An operator who already set a wide nucleus is not narrowed.
	if got := NucleusForTemp(0.8, 0.95); got != 0.95 {
		t.Errorf("an already-wide nucleus must be preserved, got %v", got)
	}
}

// tempRecordingLLM emits a fixed task each turn and records the temperature it is
// asked to sample at, so a test can prove the orchestrator perturbs the sampler.
type tempRecordingLLM struct {
	task  Task
	temps *[]float64
}

func (l tempRecordingLLM) GenerateTask(_ context.Context, _ StateSnapshot, temperature float64) (Task, error) {
	*l.temps = append(*l.temps, temperature)
	return l.task, nil
}
func (l tempRecordingLLM) Plan(context.Context, PlanInput, float64) (PlanDecision, error) {
	return PlanDecision{Done: true}, nil
}

// A verbatim resend must immediately raise the NEXT generation's sampling temperature —
// without spending the fork/route budget — so the runtime breaks a repeat loop instead
// of merely advising a model that ignores the advice. This is the behavioral half of the
// fix; NucleusForTemp is the sampling half.
func TestResendPerturbsNextGeneration(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "never")
	var temps []float64
	llm := tempRecordingLLM{
		temps: &temps,
		task: Task{ // identical every turn → a resend after the first failure
			ID: "same", Command: "echo identical", Mode: ModeBlocking, TimeoutSec: 5,
			Assertion: Assertion{Type: "fs", Pattern: absent, Channel: ChannelFS},
		},
	}
	orch := NewOrchestrator("impossible", llm, NewRuntime())
	orch.NormalTemp = 0.0
	orch.ForkTemp = 0.8
	// Isolate the perturbation from real forks: no attempt/route/entropy fork can fire,
	// so any elevated temperature MUST come from the resend perturbation.
	orch.MaxStuck = 0
	orch.MaxAttemptsPerTask = 0
	orch.MaxRoutes = 0
	orch.MaxEntropy = 100
	orch.MaxSteps = 4

	orch.Run(context.Background())

	if orch.forks != 0 {
		t.Fatalf("no fork should fire in this setup; got %d", orch.forks)
	}
	if len(temps) < 3 {
		t.Fatalf("expected at least 3 generations, got %d", len(temps))
	}
	if temps[0] != orch.NormalTemp {
		t.Errorf("first generation must sample cold, got %v", temps[0])
	}
	// After the first verbatim resend (turn 2), a later generation must sample hot.
	hot := false
	for _, tp := range temps[2:] {
		if tp == orch.ForkTemp {
			hot = true
		}
	}
	if !hot {
		t.Errorf("a resend must perturb a later generation to ForkTemp; temps=%v", temps)
	}
}

// itoa is a tiny local int→string to avoid pulling strconv into the test for one use.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
