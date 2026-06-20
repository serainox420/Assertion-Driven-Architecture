package ada

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// Outcome is the terminal state of a run — always explicit, never an ambiguous
// fall-through (the EXHAUSTED lesson from the source notes).
type Outcome string

const (
	OutcomeFinished  Outcome = "FINISHED"  // objective verified complete
	OutcomeStable    Outcome = "STABLE"    // reached a fixed point: re-proving existing ground, no new progress
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

	MaxEntropy  int     // entropy ceiling that triggers a Hard Context Fork (§8.3)
	MaxFacts    int     // fact-folding cap (§7.2)
	MaxSteps    int     // global budget — no runaway
	StallBudget int     // consecutive no-progress successes before stopping (0 disables)
	NormalTemp  float64 // decoder temperature for ordinary THINK steps (§9.3)
	ForkTemp    float64 // raised temperature on a fork, to force novelty (§9.3)

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

	// Debug, when non-nil, records full step details (task JSON, raw LLM response,
	// stdout/stderr, timing) to a per-run folder for post-mortem analysis.
	Debug *DebugSession

	// LastError holds a decoded snippet of the most recent failing command's
	// stderr (or stdout) — the "why did it fail" otherwise buried in the Base64
	// anomaly. Not cleared by a fork, so it survives to the run summary.
	LastError string

	// MaxStuck abandons the run when this many steps pass with NO new verified fact
	// (failures or re-proofs), regardless of Hard Context Forks. This is what bounds
	// a goal whose strategy is hopeless — without it a fork resets the failure
	// counters and the loop spins to the step budget. 0 disables.
	MaxStuck int

	// MaxThinkFails bounds CONSECUTIVE invalid/incomplete Task emissions before the
	// run is abandoned. A malformed-JSON error is the model's own fixable mistake —
	// re-prompting with the error usually fixes it — so these retries are FREE: they
	// do not consume the step budget (a single JSON hiccup must not kill a goal with
	// a tight -goal-steps). 0 disables the cap. Reset on any valid emission.
	MaxThinkFails int

	// MaxAttemptsPerTask bounds how many times the SAME proposition (same target) may
	// be attempted and fail WITHIN one strategy before the runtime forces a route
	// change. It is the deterministic enforcement of "don't bang on the same door":
	// each failure tells the model how many times this exact approach has failed, and
	// the cap converts persistent failure into a Hard Context Fork — a fresh strategy
	// — rather than an infinite re-send. 0 disables. (§8, bounded non-linear recovery.)
	MaxAttemptsPerTask int

	// MaxRoutes bounds the number of STRATEGIES (a route = the span between Hard
	// Context Forks) a single goal may try before it is abandoned as FAILED. With
	// MaxAttemptsPerTask this gives a finite, nested recovery budget — e.g. 3 routes ×
	// 3 attempts = at most 9 tries at a stuck proposition — so the agent improvises
	// only as much as the budget allows and never spins. 0 disables. (§8.)
	MaxRoutes int

	steps            int
	thinkFails       int // consecutive invalid Task emissions (free retries)
	consecutiveFails int
	forking          bool            // next generation should sample at ForkTemp (§9.3)
	completed        bool            // a Final task's assertion held — objective proven complete
	seen             map[string]bool // verified propositions already established (dedup + stall)
	attempts         map[string]int  // per-proposition failed attempts in the CURRENT route (reset by a fork)
	forks            int             // routes taken: number of Hard Context Forks so far this run
	dupSuccess       int             // consecutive successes that re-proved existing ground
	stuckSteps       int             // steps since the last NEW verified fact (not reset by a fork)
}

// NewOrchestrator returns an orchestrator with documented defaults.
func NewOrchestrator(objective string, llm LLM, rt *Runtime) *Orchestrator {
	return &Orchestrator{
		Snapshot:           StateSnapshot{Objective: objective},
		RT:                 rt,
		LLM:                llm,
		MaxEntropy:         6,
		MaxFacts:           15, // hard cap before folding (§7.2)
		MaxSteps:           200,
		MaxStuck:           10,  // backstop: abandon after this many no-progress steps (kept above the 3×3 recovery budget so the route budget governs first)
		MaxThinkFails:      4,   // consecutive invalid-JSON emissions before giving up (free retries)
		MaxAttemptsPerTask: 3,   // same-proposition tries within a route before forcing a new strategy
		MaxRoutes:          3,   // strategies (forks) before abandoning the goal — 3×3 = 9 tries max
		StallBudget:        2,   // stop after this many consecutive no-progress successes
		NormalTemp:         0.0, // determinism where we want reliability (§9.3)
		ForkTemp:           0.8, // entropy where we want exploration (§9.3)
		Log:                func(string, ...any) {},
		seen:               make(map[string]bool),
		attempts:           make(map[string]int),
	}
}

// Steps returns the number of steps taken in the last Run.
func (o *Orchestrator) Steps() int { return o.steps }

// Forks returns the number of Hard Context Forks taken in the last Run.
func (o *Orchestrator) Forks() int { return o.forks }

// Seed pre-loads externally supplied verified facts (e.g. re-validated persistent
// memory, §5.3) so the model treats them as already-known and does not waste steps
// re-deriving them. Seeded propositions are marked established for stall/dedup.
func (o *Orchestrator) Seed(facts []Fact) {
	o.Snapshot.EstablishedFacts = append(o.Snapshot.EstablishedFacts, facts...)
	for _, f := range facts {
		o.seen[factSeedKey(f)] = true
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

		// Out of routes: the bounded set of alternative strategies is exhausted and
		// none worked. Abandon as FAILED rather than keep improvising past the budget
		// — "improvise only when needed, and only within a norm" (§8).
		if o.MaxRoutes > 0 && o.forks >= o.MaxRoutes {
			o.logf("step=%d OUT_OF_ROUTES: exhausted %d strategies without success; abandoning", o.steps, o.forks)
			return OutcomeFailed
		}

		// The meta-controller manages strategy BEFORE the model thinks (§10).
		o.applyMetaAction()

		temp := o.NormalTemp
		if o.forking {
			temp = o.ForkTemp
		}

		thinkStart := time.Now()
		task, err := o.LLM.GenerateTask(ctx, o.Snapshot, temp)
		thinkMs := time.Since(thinkStart).Milliseconds()
		if err != nil {
			// A parse / contract violation is the model's own fixable mistake (§9.2).
			// Feed it back as an anomaly and RETRY for free — do NOT spend a step, or a
			// single bad JSON would kill a goal under a tight -goal-steps budget.
			o.thinkFails++
			o.logf("THINK_FAILED (%d) err=%v", o.thinkFails, err)
			o.Snapshot.Anomaly = &AnomalyPayload{
				FailedTaskID: "SYSTEM_JSON_PARSE_ERROR",
				Expected:     "valid Task JSON with non-empty id, command, and assertion.channel",
				ActualOutB64: base64.StdEncoding.EncodeToString([]byte(err.Error())),
				FailureClass: ClassModelError,
			}
			limit := o.MaxThinkFails
			if limit <= 0 {
				limit = 20 // safety floor — never spin forever on a model that can't format JSON
			}
			if o.thinkFails >= limit {
				o.logf("ABANDON: model emitted invalid Task JSON %d times in a row", o.thinkFails)
				return OutcomeExhausted
			}
			continue
		}
		o.thinkFails = 0
		o.steps++
		o.forking = false
		o.logf("step=%d THINK id=%s mode=%s channel=%s", o.steps, task.ID, task.Mode, task.Assertion.Channel)

		execStart := time.Now()
		res := o.RT.Execute(task)
		execMs := time.Since(execStart).Milliseconds()
		o.applyResult(task, res)
		if o.Debug != nil {
			o.Debug.LogStep(o.steps, o.Snapshot.Objective, task, thinkMs, res, execMs, o.Snapshot.EntropyLevel, o.forks)
			if !res.Passed && res.Anomaly != nil {
				o.Debug.LogAnomaly(o.steps, o.Snapshot.Objective, res.Anomaly)
			}
		}

		// Async job completions fold straight back in as strong facts (§4.1).
		for _, f := range o.RT.PollJobs() {
			o.addFact(f)
			o.stuckSteps = 0 // a completed job is progress
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

		// Deterministic stall guard: if the model keeps re-proving ground it has
		// already established and never advances or signals completion, stop rather
		// than spin to the step budget. Termination must not depend on the model.
		if o.StallBudget > 0 && o.dupSuccess >= o.StallBudget {
			o.logf("step=%d STABLE (re-verified existing facts %d× with no new progress; model did not signal completion)",
				o.steps, o.dupSuccess)
			return OutcomeStable
		}

		// Hopeless-strategy guard: if no new verified fact has appeared in MaxStuck
		// steps — across however many forks — the local approach is exhausted. Abandon
		// fast instead of grinding to the step budget (a Hard Context Fork resets the
		// entropy/failure counters, so without this a stuck goal never terminates).
		if o.MaxStuck > 0 && o.stuckSteps >= o.MaxStuck {
			o.logf("step=%d STUCK: no new verified fact in %d steps; abandoning this goal", o.steps, o.stuckSteps)
			return OutcomeExhausted
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
		if task.Final {
			o.completed = true // verified Final task ⇒ objective complete (checked in Run)
		}

		// Has this proposition already been proven? Re-verifying existing ground is
		// not progress — it's how a model that won't signal completion loops forever.
		// Record only genuinely new facts; count the repeats toward the stall budget.
		key := factKey(task)
		if o.seen[key] {
			o.dupSuccess++
			o.stuckSteps++ // re-proving old ground is not progress
			o.logf("step=%d NOPROGRESS id=%s key=%q dup=%d/%d",
				o.steps, task.ID, key, o.dupSuccess, o.StallBudget)
			return
		}
		o.seen[key] = true
		delete(o.attempts, key) // this proposition is established; clear its failure budget
		o.dupSuccess = 0
		o.stuckSteps = 0 // genuine progress
		o.addFact(Fact{
			Statement: factStatement(task),
			SourceID:  task.ID,
			Strength:  res.Strength,
			assertion: task.Assertion,
		})
		o.logf("step=%d ACK id=%s strength=%s", o.steps, task.ID, res.Strength)
		return
	}

	// Count how many times this exact proposition has now failed in the current
	// route, and tell the model — explicitly — so it stops re-sending a dead approach
	// and instead analyzes the autopsy and changes method ("pause, think, try a
	// different approach"). This directive is runtime-authored, so it is trusted,
	// plaintext guidance — never Base64-muzzled like the environment output.
	key := factKey(task)
	o.attempts[key]++
	n := o.attempts[key]
	res.Anomaly.Attempts = n
	if n >= 2 {
		res.Anomaly.Directive = fmt.Sprintf(
			"This exact approach to the same target has failed %d× in a row. Do NOT resend it — read the autopsy and change the METHOD: a different command or channel, or first establish a missing precondition.", n)
	}
	o.Snapshot.Anomaly = res.Anomaly

	// Decode the buried stderr/stdout so the human log says WHY it failed, not just
	// that it did. (The Base64 in the payload is the model's injection defense; the
	// operator log is allowed to read it.)
	errSnip := decodeForLog(res.Anomaly.ActualErrB64, 200)
	if errSnip == "" {
		errSnip = decodeForLog(res.Anomaly.ActualOutB64, 200)
	}
	if errSnip != "" {
		o.LastError = errSnip
	}
	o.logf("step=%d ANOMALY id=%s class=%s attempts=%d expected=%q exit=%d err=%q",
		o.steps, res.Anomaly.FailedTaskID, res.Anomaly.FailureClass, n, res.Anomaly.Expected, res.Anomaly.ExitCode, errSnip)
	o.afterFailure(res.Anomaly.FailureClass, key)
}

// afterFailure advances entropy (weighted by failure class) and forks when the
// local strategy is exhausted — either because this one proposition has been
// retried to its per-task cap (bang-the-same-door), or because accumulated
// frustration crossed the entropy ceiling (§8.2, §8.3).
func (o *Orchestrator) afterFailure(class, key string) {
	o.consecutiveFails++
	o.stuckSteps++ // a failure is not progress (and the fork won't reset this)
	o.Snapshot.EntropyLevel += entropyWeight(class)

	attemptCapped := o.MaxAttemptsPerTask > 0 && o.attempts[key] >= o.MaxAttemptsPerTask
	entropyCapped := o.Snapshot.EntropyLevel >= o.MaxEntropy
	if attemptCapped || entropyCapped {
		if attemptCapped {
			o.logf("step=%d ATTEMPTS_EXHAUSTED key=%q n=%d -> forcing a new strategy", o.steps, key, o.attempts[key])
		}
		o.HardContextFork()
	}
}

// factKey identifies the proposition a passing task establishes, so re-proving
// the same ground is detected as no-progress. For independent-state channels the
// assertion pattern *is* the proposition (the file/path/service/socket), so the
// command is irrelevant — proving "/tmp/out exists" twice is the same fact even
// via different commands. For generic channels (exit_code/stdout/stderr) the
// pattern is too coarse (every "exit 0" would collide), so the command is folded
// in — distinct actions that happen to share a generic assertion count as progress.
func factKey(t Task) string {
	a := t.Assertion
	switch a.Channel {
	case ChannelFS:
		path, _, _ := strings.Cut(a.Pattern, "|")
		return "fs|" + normalizeFSPath(path)
	case ChannelProcess, ChannelService:
		return a.Channel + "|" + strings.TrimSpace(a.Pattern)
	default:
		return a.Channel + "|" + a.Pattern + "|" + strings.TrimSpace(t.Command)
	}
}

// factStatement renders a human- and model-readable description of WHAT a passing
// task established. The model reads these back in EstablishedFacts; an
// uninformative "1 verified via fs" left it unable to tell the objective was
// already met (so it never signaled completion and looped). A self-describing
// statement ("verified file exists: out.log") lets the model recognize it is done.
func factStatement(t Task) string {
	if d := strings.TrimSpace(t.Description); d != "" {
		return d
	}
	a := t.Assertion
	switch a.Channel {
	case ChannelFS:
		path, spec, has := strings.Cut(a.Pattern, "|")
		path = normalizeFSPath(path)
		if has {
			return fmt.Sprintf("verified: %s (%s)", path, strings.TrimSpace(spec))
		}
		return "verified file exists: " + path
	case ChannelProcess:
		return "verified process/socket present: " + a.Pattern
	case ChannelService:
		return "verified service active: " + a.Pattern
	case ChannelExitCode:
		return fmt.Sprintf("ran `%s` (exit %s)", shorten(t.Command, 80), a.Pattern)
	case ChannelStdout, ChannelStderr:
		return fmt.Sprintf("output of `%s` matched /%s/", shorten(t.Command, 60), a.Pattern)
	default:
		return t.ID + " verified"
	}
}

// decodeForLog base64-decodes a payload field and renders it as a single short
// line suitable for the operator log.
func decodeForLog(b64 string, max int) string {
	if b64 == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return ""
	}
	return shorten(strings.Join(strings.Fields(string(raw)), " "), max)
}

func shorten(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// addFact appends a verified fact and folds the fact base if it overflows (§7.2).
func (o *Orchestrator) addFact(f Fact) {
	o.Snapshot.EstablishedFacts = append(o.Snapshot.EstablishedFacts, f)
	o.Debug.LogFact(o.steps, o.Snapshot.Objective, f)
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
	o.forks++ // a fork is a new strategy ("route"); MaxRoutes bounds how many we try
	o.logf("step=%d HARD_FORK entropy=%d route=%d", o.steps, o.Snapshot.EntropyLevel, o.forks)
	o.RT.KillDaemons()
	o.Snapshot.EstablishedFacts = o.revalidateWeakFacts(o.Snapshot.EstablishedFacts)
	o.Snapshot.Anomaly = nil
	o.Snapshot.EntropyLevel = 0
	o.consecutiveFails = 0
	o.attempts = make(map[string]int) // new strategy ⇒ fresh per-proposition budgets
	o.forking = true                  // §9.3: the next generation must perturb the sampler, or it isn't a fork
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
