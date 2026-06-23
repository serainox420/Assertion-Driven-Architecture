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
	for round := 1; round <= c.MaxRounds; round++ {
		if ctx.Err() != nil {
			return OutcomeExhausted, last
		}

		in := PlanInput{Objective: c.Objective, Environment: c.Environment, Facts: c.facts, Completed: c.completed, Failed: c.failed}
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
			return OutcomeFinished, dec
		}
		if len(dec.Subgoals) == 0 {
			c.logf("round=%d STUCK: planner produced no sub-goals yet is not done", round)
			return OutcomeStable, dec
		}

		startFacts := len(c.facts)
		roundFailures = nil
		for i, sg := range dec.Subgoals {
			if ctx.Err() != nil {
				return OutcomeExhausted, last
			}
			c.logf("round=%d goal=%d/%d START %q", round, i+1, len(dec.Subgoals), sg)
			outcome := c.runSubgoal(ctx, sg)
			// Tell the planner the TRUTH about each outcome: only FINISHED/STABLE
			// (achieved or already held) count as completed; EXHAUSTED/FAILED are
			// recorded as failures so the planner cannot mistake stuck work for done.
			switch outcome {
			case OutcomeExhausted, OutcomeFailed:
				c.failed = append(c.failed, sg)
				roundFailures = append(roundFailures, sg)
			default:
				c.completed = append(c.completed, sg)
			}
			c.logf("round=%d goal=%d %s %q", round, i+1, outcome, sg)
		}

		// If a whole round of sub-goals produced no new verified ground, we are not
		// converging — stop rather than re-plan the same dead strategy forever.
		if len(c.facts) == startFacts {
			c.logf("round=%d STUCK: a full round produced no new verified facts", round)
			return OutcomeStable, last
		}
	}
	c.logf("ROUND_BUDGET exhausted after %d rounds", c.MaxRounds)
	return OutcomeExhausted, last
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
