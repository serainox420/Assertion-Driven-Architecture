package ada

import (
	"context"
	"time"
)

// Coordinator drives OPEN-ENDED objectives that have no single obvious end. It
// asks a Planner to break the objective into a flat, re-plannable list of
// sub-goals, runs each sub-goal through the flat ADA loop (Orchestrator),
// accumulates verified facts, and re-plans from those facts each round —
// continuing until the planner judges the objective satisfied, or a safety
// budget is hit.
//
// This is the research notes' §5/§6 planning + goal-tracking layer, kept FLAT and
// re-plannable on purpose rather than a brittle hierarchical tree (§0): the next
// sub-goals are re-derived from the current verified state every round, so actions
// that change the environment don't shatter a pre-committed plan.
type Coordinator struct {
	Objective string   // pinned, immutable top objective
	LLM       LLM      // executes sub-goals (the flat loop)
	Planner   Planner  // decomposes + judges completion (may be the same model as LLM)
	RT        *Runtime // shared deterministic runtime

	MaxRounds          int      // plan/execute rounds before giving up (safety net)
	GoalSteps          int      // flat-loop step budget per sub-goal
	MaxEntropy         int      // forwarded to each sub-goal Orchestrator (§8.3)
	MaxStuck           int      // forwarded: abandon a sub-goal after N steps with no new fact
	MaxAttemptsPerTask int      // forwarded: same-proposition tries within a route (§8)
	MaxRoutes          int      // forwarded: bounded strategies per sub-goal (§8)
	StallBudget        int      // forwarded to each sub-goal Orchestrator (§ stall guard)
	MaxFacts           int      // fact-folding cap (§7.2)
	NormalTemp         float64  // executor temp for normal steps (§9.3)
	ForkTemp           float64  // executor temp on a fork (§9.3)
	PlanTemp           float64  // planner sampling temperature (a little creativity helps decomposition)
	Environment        []string // durable host facts shown to planner + executor (§5.3)
	Log                func(format string, args ...any)
	Debug              *DebugSession // when non-nil, writes planner calls + per-step details

	facts     []Fact
	completed []string // sub-goals achieved (FINISHED/STABLE)
	failed    []string // sub-goals the runtime could NOT achieve (EXHAUSTED/FAILED)
	lastError string
	steps     int // total flat-loop steps summed across every sub-goal (§ measurement)
	forks     int // total Hard Context Forks summed across every sub-goal
}

// LastError returns a decoded snippet of the most recent failing command's output
// across all sub-goals — the "why" behind a STABLE/EXHAUSTED run.
func (c *Coordinator) LastError() string { return c.lastError }

// Steps returns the total flat-loop steps executed across every sub-goal. Without
// this a planning run reports total_steps:0 and is unmeasurable — the aggregate is
// what makes a planning run comparable to a flat one when benchmarking models.
func (c *Coordinator) Steps() int { return c.steps }

// Forks returns the total Hard Context Forks taken across every sub-goal.
func (c *Coordinator) Forks() int { return c.forks }

// NewCoordinator returns a Coordinator with documented defaults. Pass the same
// model object as both llm and planner unless you want a driver/worker split.
func NewCoordinator(objective string, llm LLM, planner Planner, rt *Runtime) *Coordinator {
	return &Coordinator{
		Objective:          objective,
		LLM:                llm,
		Planner:            planner,
		RT:                 rt,
		Environment:        HostFacts(), // tell the agent what host it's on (§5.3)
		MaxRounds:          8,
		GoalSteps:          25,
		MaxEntropy:         6,
		MaxStuck:           10,
		MaxAttemptsPerTask: 3,
		MaxRoutes:          3,
		StallBudget:        2,
		MaxFacts:           15,
		NormalTemp:         0.0,
		ForkTemp:           0.8,
		PlanTemp:           0.4,
		Log:                func(string, ...any) {},
	}
}

// Seed pre-loads externally supplied verified facts (e.g. re-validated persistent
// memory, §5.3) so the planner and executors treat them as already-known.
func (c *Coordinator) Seed(facts []Fact) {
	c.facts = dedupFacts(append(c.facts, facts...))
}

func (c *Coordinator) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(format, args...)
	}
}

// Facts returns the verified facts accumulated across all sub-goals.
func (c *Coordinator) Facts() []Fact { return c.facts }

// Run drives the plan → execute → re-plan loop to a terminal Outcome and returns
// the planner's final decision (its reason explains why it stopped).
func (c *Coordinator) Run(ctx context.Context) (Outcome, PlanDecision) {
	var last PlanDecision
	var roundFailures []string // sub-goals the most recent round could NOT achieve
	stalledOnFailure := false  // a prior round hit a blocker and gained no new ground
	for round := 1; round <= c.MaxRounds; round++ {
		if ctx.Err() != nil {
			return OutcomeExhausted, last
		}

		// The planner sees the verified facts AND last_error — the most recent blocker
		// — so a failed round becomes a clue to repair, not just a dead end: it can
		// insert a corrective sub-goal (refresh a stale package DB, free a port) before
		// re-attempting the goal that failed (§ recover-from-blocker).
		in := PlanInput{
			Objective:   c.Objective,
			Environment: c.Environment,
			Facts:       c.facts,
			Completed:   c.completed,
			Failed:      c.failed,
			LastError:   c.lastError,
		}
		planStart := time.Now()
		dec, err := c.Planner.Plan(ctx, in, c.PlanTemp)
		planMs := time.Since(planStart).Milliseconds()
		if err != nil {
			// A planner contract violation is a survivable anomaly, not a crash.
			c.logf("round=%d PLAN_FAILED err=%v", round, err)
			continue
		}
		last = dec
		c.logf("round=%d PLAN done=%v subgoals=%d reason=%q", round, dec.Done, len(dec.Subgoals), dec.Reason)
		c.Debug.LogPlanner(round, in, dec, planMs)

		// "Until goal satisfied": the planner judges completion against verified
		// facts. But the planner is a stochastic model — it does not get to declare
		// victory while the deterministic runtime recorded sub-goals it could NOT
		// achieve. Completion must be earned against the record, not the model's hope
		// (§ ADA philosophy: relocate success-evaluation to the runtime). An honest
		// STABLE with accumulated facts beats a false FINISHED. A genuinely-already-
		// satisfied objective on a fresh start has no prior failures, so it still
		// returns FINISHED.
		if dec.Done {
			if len(roundFailures) > 0 {
				c.logf("round=%d PLAN claimed done but %d sub-goal(s) unresolved (EXHAUSTED/FAILED) last round; NOT finished -> STABLE", round, len(roundFailures))
				return OutcomeStable, dec
			}
			// Validate-on-use: a "done" may rest on facts carried from persistent
			// memory, which are NOT re-checked at load. Re-observe them now; if any
			// went stale, drop it and re-plan rather than declare a false FINISHED on a
			// claim that no longer holds (§8.3).
			if c.revalidateMemoryFacts() {
				c.logf("round=%d PLAN claimed done but a memory fact failed re-validation; re-planning", round)
				continue
			}
			return OutcomeFinished, dec
		}
		if len(dec.Subgoals) == 0 {
			c.logf("round=%d STUCK: planner produced no sub-goals yet is not done", round)
			return OutcomeStable, dec
		}

		startFacts := len(c.facts)
		roundFailures = nil
		failed := false
		for i, sg := range dec.Subgoals {
			if ctx.Err() != nil {
				return OutcomeExhausted, last
			}
			// A sub-goal already ACHIEVED in an earlier round is an idempotent end state
			// whose proving facts already carry forward — re-running it only burns budget
			// and invites the executor to fixate on a stale task (observed: the model kept
			// re-emitting a `write_index_html` task while the re-listed, already-done
			// "refresh databases" sub-goal was active, failing it spuriously and dragging
			// a completed goal into the failed list). The planner is told "never repeat a
			// sub-goal already in the facts"; when it does anyway, skip it here.
			if containsString(c.completed, sg) {
				c.logf("round=%d goal=%d/%d SKIP %q (already achieved in an earlier round)", round, i+1, len(dec.Subgoals), sg)
				continue
			}
			c.logf("round=%d goal=%d/%d START %q", round, i+1, len(dec.Subgoals), sg)
			before := len(c.facts)
			outcome := c.runSubgoal(ctx, sg)
			gained := len(c.facts) > before
			// Tell the planner the TRUTH about each outcome — but judge it by the RECORD,
			// not the label. FINISHED/STABLE are achieved. EXHAUSTED/FAILED normally mean
			// "could not achieve" and halt the round, since later sub-goals usually depend
			// on the blocked one. The exception: a sub-goal that recorded NEW verified facts
			// proved real state, it just never reached a clean terminal — the model satisfied
			// it in one step but never set final:true, then spent its think-budget on
			// malformed JSON, so the runtime returned EXHAUSTED with the goal already met.
			// That is verified PROGRESS, not a blocker. Counting it as a pure failure made
			// the coordinator re-list a sub-goal it had ACHIEVED and halt every round on it,
			// so dependent sub-goals never ran (the "/var/www/html is writable" dead-end: the
			// writable fact was recorded yet the goal sat in failed_subgoals forever). Keep
			// the two lists DEDUPED and DISJOINT — a sub-goal must never appear in both.
			progressed := gained && outcome != OutcomeFinished && outcome != OutcomeStable
			switch {
			case (outcome == OutcomeExhausted || outcome == OutcomeFailed) && !gained:
				c.failed = appendUnique(c.failed, sg)
				roundFailures = append(roundFailures, sg)
				failed = true
			default:
				c.completed = appendUnique(c.completed, sg)
				c.failed = removeString(c.failed, sg)
			}
			if progressed {
				c.logf("round=%d goal=%d %s %q (verified progress; counted as done)", round, i+1, outcome, sg)
			} else {
				c.logf("round=%d goal=%d %s %q", round, i+1, outcome, sg)
			}
			if failed {
				// Sub-goals are ORDERED, and the ones after a blocker usually DEPEND on
				// it (you cannot enable/start nginx if the install failed). Halt the
				// round and re-plan from the blocker — "stop, work out why it failed,
				// try a new approach" — instead of burning the budget on dependents
				// that cannot pass (§ recover-from-blocker).
				c.logf("round=%d goal=%d BLOCKED the round; halting to re-plan from the failure", round, i+1)
				break
			}
		}

		switch {
		case len(c.facts) > startFacts:
			// Gained verified ground this round — keep going (re-plan), even if a later
			// sub-goal was blocked: progress means the strategy is still moving.
			stalledOnFailure = false
		case failed:
			// No new facts AND a blocker. Give the planner ONE informed re-plan against
			// last_error; if the very next round ALSO stalls on a failure, it has found
			// no way through — stop honestly rather than spin the round budget.
			if stalledOnFailure {
				c.logf("round=%d STUCK: re-plan after the blocker still produced no progress -> STABLE", round)
				return OutcomeStable, last
			}
			stalledOnFailure = true
		default:
			// No new facts and no failure: the planner is re-proving settled ground.
			c.logf("round=%d STUCK: a full round produced no new verified facts -> STABLE", round)
			return OutcomeStable, last
		}
	}
	c.logf("ROUND_BUDGET exhausted after %d rounds", c.MaxRounds)
	return OutcomeExhausted, last
}

// revalidateMemoryFacts re-observes every fact carried from persistent memory and
// drops any that no longer holds, returning true if at least one was dropped. This
// is the "validate on use" safety net at the completion boundary: memory facts are
// not re-checked at load (so an unrelated previous run's facts cost no stat), so a
// planner "done" that leans on them must re-observe them before we accept FINISHED —
// preserving the §8.3 guarantee that a stale claim is never trusted on faith.
func (c *Coordinator) revalidateMemoryFacts() (dropped bool) {
	kept := c.facts[:0:0] // new backing array; don't alias the live slice
	for _, f := range c.facts {
		if f.SourceID == memorySourceID && !checkIndependentState(f.assertion) {
			c.logf("MEMORY_DROP stale %s", assertionDesc(f.assertion))
			dropped = true
			continue
		}
		kept = append(kept, f)
	}
	c.facts = kept
	return dropped
}

// runSubgoal executes a single sub-goal through the flat loop, seeded with the
// facts proven so far. FINISHED/STABLE mean the sub-goal was achieved (or already
// held); EXHAUSTED means it got stuck. Either way, accumulated facts carry forward.
func (c *Coordinator) runSubgoal(ctx context.Context, subgoal string) Outcome {
	o := NewOrchestrator(subgoal, c.LLM, c.RT)
	o.MaxSteps = c.GoalSteps
	o.MaxEntropy = c.MaxEntropy
	o.MaxStuck = c.MaxStuck
	o.MaxAttemptsPerTask = c.MaxAttemptsPerTask
	o.MaxRoutes = c.MaxRoutes
	o.StallBudget = c.StallBudget
	o.MaxFacts = c.MaxFacts
	o.NormalTemp = c.NormalTemp
	o.ForkTemp = c.ForkTemp
	o.Log = c.Log
	o.Debug = c.Debug
	o.Snapshot.MainObjective = c.Objective
	o.Snapshot.Environment = c.Environment
	o.Snapshot.EstablishedFacts = append([]Fact(nil), c.facts...)
	for _, f := range c.facts {
		o.seen[factSeedKey(f)] = true // don't re-prove ground earlier sub-goals already established
	}

	outcome := o.Run(ctx)
	c.steps += o.Steps() // aggregate so planning runs report real totals, not 0 (§ measurement)
	c.forks += o.Forks()
	c.facts = dedupFacts(o.Snapshot.EstablishedFacts)
	if o.LastError != "" {
		c.lastError = o.LastError
	}
	return outcome
}

// containsString reports whether list holds s.
func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// appendUnique appends s to list only when absent, preserving order — so the
// cumulative completed/failed sub-goal lists shown to the planner never pile up
// duplicates when the planner re-lists the same sub-goal round after round.
func appendUnique(list []string, s string) []string {
	if containsString(list, s) {
		return list
	}
	return append(list, s)
}

// removeString returns list with every element equal to s removed, preserving
// order. It keeps completed and failed DISJOINT: a sub-goal that just succeeded is
// cleared from the failed list, so the planner is never told it both passed and
// failed.
func removeString(list []string, s string) []string {
	out := list[:0:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}
