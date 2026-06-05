package ada

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
)

// Outcome is the terminal state of a run — always explicit, never an ambiguous
// fall-through (the EXHAUSTED lesson from the source notes).
type Outcome string

const (
	OutcomeFinished  Outcome = "FINISHED"  // objective verified complete
	OutcomeExhausted Outcome = "EXHAUSTED" // step budget consumed before a clean terminal
	OutcomeFailed    Outcome = "FAILED"    // unrecoverable
)

// Orchestrator wires the stochastic model to the deterministic runtime and owns
// everything load-bearing: control flow, entropy, the hard fork, context
// compaction, and termination.
type Orchestrator struct {
	Snapshot StateSnapshot
	RT       *Runtime
	LLM      LLM

	MaxEntropy int     // entropy ceiling that triggers a Hard Context Fork (§8.3)
	MaxFacts   int     // fact-folding cap (§7.2)
	MaxSteps   int     // global budget — no runaway
	NormalTemp float64 // decoder temperature for ordinary THINK steps (§9.3)
	ForkTemp   float64 // raised temperature on a fork, to force novelty (§9.3)

	// DoneCheck reports whether the objective is verifiably complete. It runs
	// against established (preferably strong) facts, never against model claims.
	DoneCheck func(StateSnapshot) bool

	// Meta is the optional meta-controller (RL or heuristic) that manages strategy
	// over a small discrete action space (§10). Nil ⇒ pure entropy control.
	Meta MetaController

	// SwapModel, if set, is invoked on the meta-action ActionSwapModel (§10/§11).
	SwapModel func()

	// Log receives one structured line per significant event for observability.
	Log func(format string, args ...any)

	steps            int
	consecutiveFails int
	forking          bool // next generation should sample at ForkTemp (§9.3)
	completed        bool // a Final task's assertion held — objective proven complete
}

// NewOrchestrator returns an orchestrator with documented defaults.
func NewOrchestrator(objective string, llm LLM, rt *Runtime) *Orchestrator {
	return &Orchestrator{
		Snapshot:   StateSnapshot{Objective: objective},
		RT:         rt,
		LLM:        llm,
		MaxEntropy: 6,
		MaxFacts:   15, // hard cap before folding (§7.2)
		MaxSteps:   200,
		NormalTemp: 0.0, // determinism where we want reliability (§9.3)
		ForkTemp:   0.8, // entropy where we want exploration (§9.3)
		Log:        func(string, ...any) {},
	}
}

func (o *Orchestrator) logf(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// Run drives the loop to a terminal Outcome.
func (o *Orchestrator) Run(ctx context.Context) Outcome {
	for o.steps < o.MaxSteps {
		if err := ctx.Err(); err != nil {
			return OutcomeExhausted
		}
		o.steps++

		// The meta-controller manages strategy BEFORE the model thinks (§10).
		o.applyMetaAction()

		temp := o.NormalTemp
		if o.forking {
			temp = o.ForkTemp
		}

		task, err := o.LLM.GenerateTask(ctx, o.Snapshot, temp)
		if err != nil {
			// A parse / contract violation is a normal anomaly, not a crash (§9.2).
			o.logf("step=%d THINK_FAILED err=%v", o.steps, err)
			o.Snapshot.Anomaly = &AnomalyPayload{
				FailedTaskID: "SYSTEM_JSON_PARSE_ERROR",
				Expected:     "valid Task JSON",
				ActualOutB64: base64.StdEncoding.EncodeToString([]byte(err.Error())),
				FailureClass: ClassModelError,
			}
			o.afterFailure(ClassModelError)
			continue
		}
		o.forking = false
		o.logf("step=%d THINK id=%s mode=%s channel=%s", o.steps, task.ID, task.Mode, task.Assertion.Channel)

		res := o.RT.Execute(task)
		o.applyResult(task, res)

		// Async job completions fold straight back in as strong facts (§4.1).
		for _, f := range o.RT.PollJobs() {
			o.addFact(f)
			o.logf("step=%d JOB_DONE %s", o.steps, f.Statement)
		}

		// Completion via a verified Final task (the model's in-band "done" signal),
		// or an externally supplied DoneCheck. Both require passing assertions.
		if o.completed {
			o.logf("step=%d OBJECTIVE_COMPLETE (final task verified)", o.steps)
			return OutcomeFinished
		}
		if o.DoneCheck != nil && o.DoneCheck(o.Snapshot) {
			o.logf("step=%d OBJECTIVE_COMPLETE", o.steps)
			return OutcomeFinished
		}
	}
	return OutcomeExhausted
}

// applyResult routes a result: record a fact on success, or build an anomaly and
// advance the entropy machinery on failure (§1, §8).
func (o *Orchestrator) applyResult(task Task, res ExecutionResult) {
	if res.Passed {
		o.consecutiveFails = 0
		o.Snapshot.Anomaly = nil
		stmt := task.Description
		if strings.TrimSpace(stmt) == "" {
			stmt = fmt.Sprintf("%s verified via %s", task.ID, task.Assertion.Channel)
		}
		o.addFact(Fact{
			Statement: stmt,
			SourceID:  task.ID,
			Strength:  res.Strength,
			assertion: task.Assertion,
		})
		o.logf("step=%d ACK id=%s strength=%s", o.steps, task.ID, res.Strength)
		if task.Final {
			o.completed = true // verified Final task ⇒ objective complete (checked in Run)
		}
		return
	}

	o.Snapshot.Anomaly = res.Anomaly
	o.logf("step=%d ANOMALY id=%s class=%s expected=%q exit=%d",
		o.steps, res.Anomaly.FailedTaskID, res.Anomaly.FailureClass, res.Anomaly.Expected, res.Anomaly.ExitCode)
	o.afterFailure(res.Anomaly.FailureClass)
}

// afterFailure advances entropy (weighted by failure class) and forks if the
// local strategy is exhausted (§8.2, §8.3).
func (o *Orchestrator) afterFailure(class string) {
	o.consecutiveFails++
	o.Snapshot.EntropyLevel += entropyWeight(class)
	if o.Snapshot.EntropyLevel >= o.MaxEntropy {
		o.HardContextFork()
	}
}

// addFact appends a verified fact and folds the fact base if it overflows (§7.2).
func (o *Orchestrator) addFact(f Fact) {
	o.Snapshot.EstablishedFacts = append(o.Snapshot.EstablishedFacts, f)
	if len(o.Snapshot.EstablishedFacts) > o.MaxFacts {
		o.foldFacts()
	}
}

// foldFacts is context garbage collection (§7.2). In a full system this issues a
// compression prompt to the model ("fold these 15 tactical facts into ≤3 dense
// operational conclusions"). Here we fold deterministically, and — crucially —
// we carry Fact.Strength through (§7.3): a folded conclusion is only as strong
// as its weakest input, so compaction never launders a weak fact into a strong
// one. Weak inputs are explicitly marked provisional.
func (o *Orchestrator) foldFacts() {
	facts := o.Snapshot.EstablishedFacts
	keep := 3
	if len(facts) <= keep {
		return
	}
	oldCount := len(facts) - keep
	old := facts[:oldCount]
	recent := facts[oldCount:]

	strength := StrengthStrong
	anyWeak := false
	for _, f := range old {
		if f.Strength == StrengthWeak {
			anyWeak = true
			strength = StrengthWeak // weakest-link
		}
	}
	stmt := fmt.Sprintf("Folded %d earlier verified facts into a dense conclusion.", oldCount)
	if anyWeak {
		stmt = "[PROVISIONAL] " + stmt + " (contains output-asserted facts; re-verify before irreversible use)"
	}
	summary := Fact{Statement: stmt, SourceID: "FOLD", Strength: strength}

	folded := append([]Fact{summary}, recent...)
	o.Snapshot.EstablishedFacts = folded
	o.logf("step=%d FOLD folded=%d strength=%s", o.steps, oldCount, strength)
}

// HardContextFork is the reset when the local strategy is declared exhausted
// (§8.3): scorched-earth daemon cleanup, re-validate weak facts, discard the
// dead causal chain, and force a fresh attack vector from verified ground at a
// raised temperature.
func (o *Orchestrator) HardContextFork() {
	o.logf("step=%d HARD_FORK entropy=%d", o.steps, o.Snapshot.EntropyLevel)
	o.RT.KillDaemons()
	o.Snapshot.EstablishedFacts = o.revalidateWeakFacts(o.Snapshot.EstablishedFacts)
	o.Snapshot.Anomaly = nil
	o.Snapshot.EntropyLevel = 0
	o.consecutiveFails = 0
	o.forking = true // §9.3: the next generation must perturb the sampler, or it isn't a fork
}

// revalidateWeakFacts re-checks weak (output-asserted) facts before they seed a
// new strategy — otherwise the fork preserves the poison and deletes the
// antidote (§8.3). Weak facts backed by an independent channel are re-observed;
// weak facts backed only by stdout/stderr cannot be re-checked without re-running
// the action, so they are dropped rather than trusted. Strong facts pass through.
func (o *Orchestrator) revalidateWeakFacts(facts []Fact) []Fact {
	out := facts[:0:0] // new backing array; don't alias the input
	for _, f := range facts {
		if f.Strength != StrengthWeak {
			out = append(out, f)
			continue
		}
		switch f.assertion.Channel {
		case ChannelFS, ChannelProcess, ChannelService:
			if checkIndependentState(f.assertion) {
				out = append(out, f) // independently re-confirmed
			} else {
				o.logf("step=%d FORK_DROP id=%s (weak fact failed re-validation)", o.steps, f.SourceID)
			}
		default:
			o.logf("step=%d FORK_DROP id=%s (weak fact, no independent channel to re-check)", o.steps, f.SourceID)
		}
	}
	return out
}

// applyMetaAction consults the meta-controller, if any, and applies its choice
// over the small discrete action space (§10).
func (o *Orchestrator) applyMetaAction() {
	if o.Meta == nil {
		return
	}
	action := o.Meta.Decide(RLState{
		ConsecutiveFails: o.consecutiveFails,
		EntropyLevel:     o.Snapshot.EntropyLevel,
		FactsCount:       len(o.Snapshot.EstablishedFacts),
		StepsTaken:       o.steps,
	})
	switch action {
	case ActionForceFork:
		o.logf("step=%d META=FORCE_FORK", o.steps)
		o.HardContextFork()
	case ActionInjectHint:
		const hint = " [HINT: verify via service/fs state, not stdout]"
		if !strings.Contains(o.Snapshot.Objective, hint) {
			o.Snapshot.Objective += hint
			o.logf("step=%d META=INJECT_HINT", o.steps)
		}
	case ActionSwapModel:
		if o.SwapModel != nil {
			o.logf("step=%d META=SWAP_MODEL", o.steps)
			o.SwapModel()
		}
	default: // ActionLetItCook — stay out of the way
	}
}
