package main

import (
	"context"

	ada "github.com/serainox420/assertion-driven-architecture/ada"
)

// RunResult is the outcome of a flat or planning run, in a form both the CLI
// summary and the TUI result screen can render.
type RunResult struct {
	Outcome    ada.Outcome
	PlanReason string
	Snapshot   ada.StateSnapshot
	LastError  string
	Err        error // a setup error (e.g. empty objective); nil on a normal run
}

// applyBudgets copies the shared budget/decoder knobs from config onto an
// orchestrator. Centralized so flat and planning runs stay in lockstep.
func applyOrchestratorBudgets(o *ada.Orchestrator, cfg *Config) {
	o.MaxSteps = cfg.MaxSteps
	o.MaxEntropy = cfg.MaxEntropy
	o.MaxFacts = cfg.MaxFacts
	o.MaxStuck = cfg.MaxStuck
	o.MaxAttemptsPerTask = cfg.MaxAttempts
	o.MaxRoutes = cfg.MaxRoutes
	o.StallBudget = cfg.Stall
	o.NormalTemp = cfg.Temperature
	o.ForkTemp = cfg.ForkTemp
}

// executeRun is the single entry point for driving an objective. It opens
// re-validated memory, seeds it, runs either the flat orchestrator or the planning
// coordinator, persists durable facts, and returns a uniform RunResult. logf
// receives the structured event stream (already pretty-rendered by the caller).
func executeRun(ctx context.Context, cfg *Config, client *Client, objective string, planMode bool, logf func(string, ...any)) RunResult {
	// Re-validated persistent memory (§5.3): learn durable facts once, reuse next run.
	mem := ada.OpenMemory(cfg.MemoryPath(), cfg.Memory)
	if mem != nil {
		mem.Log = logf
	}
	seed := mem.Load()

	if planMode {
		rt := ada.NewRuntime()
		coord := ada.NewCoordinator(objective, client, client, rt)
		coord.MaxRounds = cfg.MaxRounds
		coord.GoalSteps = cfg.GoalSteps
		coord.MaxEntropy = cfg.MaxEntropy
		coord.MaxStuck = cfg.MaxStuck
		coord.MaxAttemptsPerTask = cfg.MaxAttempts
		coord.MaxRoutes = cfg.MaxRoutes
		coord.StallBudget = cfg.Stall
		coord.MaxFacts = cfg.MaxFacts
		coord.NormalTemp = cfg.Temperature
		coord.ForkTemp = cfg.ForkTemp
		coord.PlanTemp = cfg.PlanTemp
		coord.Log = logf
		coord.Seed(seed)

		outcome, dec := coord.Run(ctx)
		mem.Save(coord.Facts())
		return RunResult{
			Outcome:    outcome,
			PlanReason: dec.Reason,
			Snapshot:   ada.StateSnapshot{Objective: objective, EstablishedFacts: coord.Facts()},
			LastError:  coord.LastError(),
		}
	}

	rt := ada.NewRuntime()
	orch := ada.NewOrchestrator(objective, client, rt)
	orch.Snapshot.Environment = ada.HostFacts()
	applyOrchestratorBudgets(orch, cfg)
	orch.Log = logf
	orch.Seed(seed)
	if cfg.Meta {
		orch.Meta = ada.HeuristicController{MaxEntropy: cfg.MaxEntropy}
	}

	outcome := orch.Run(ctx)
	mem.Save(orch.Snapshot.EstablishedFacts)
	return RunResult{
		Outcome:   outcome,
		Snapshot:  orch.Snapshot,
		LastError: orch.LastError,
	}
}
