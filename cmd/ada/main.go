// Command ada runs the Assertion-Driven Architecture loop.
//
// Two modes:
//
//	-demo            run a self-contained, offline demonstration (no model needed)
//	(default)        drive a local Ollama / llama.cpp server
//
// Examples:
//
//	ada -demo
//	ada -objective "provision nginx and prove it is listening on :80" \
//	    -model qwen2.5-coder:14b -ollama http://localhost:11434
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	ada "github.com/serainox420/assertion-driven-architecture/ada"
)

func main() {
	var (
		objective   = flag.String("objective", "", "the pinned, immutable objective for the agent")
		model       = flag.String("model", "qwen2.5-coder:14b", "Ollama model tag for the worker")
		ollamaURL   = flag.String("ollama", "http://localhost:11434", "Ollama base URL")
		demo        = flag.Bool("demo", false, "run the offline, model-free demonstration")
		plan        = flag.Bool("plan", false, "planning mode: decompose an open-ended objective into sub-goals and work them")
		maxSteps    = flag.Int("max-steps", 200, "global step budget (hard stop)")
		maxRounds   = flag.Int("max-rounds", 8, "planning mode: max plan/execute rounds")
		goalSteps   = flag.Int("goal-steps", 40, "planning mode: step budget per sub-goal")
		maxEntropy  = flag.Int("max-entropy", 6, "entropy ceiling that triggers a Hard Context Fork")
		maxFacts    = flag.Int("max-facts", 15, "fact-folding cap")
		maxStuck    = flag.Int("max-stuck", 10, "abandon a goal after this many steps with no new verified fact (0 disables)")
		maxAttempts = flag.Int("max-attempts", 3, "per-proposition tries within a strategy before forcing a new approach (0 disables)")
		maxRoutes   = flag.Int("max-routes", 3, "bounded alternative strategies (forks) before abandoning a goal (0 disables)")
		stall       = flag.Int("stall", 2, "stop after this many consecutive no-progress successes (0 disables)")
		useMeta     = flag.Bool("meta", false, "enable the heuristic meta-controller")
		useMemory   = flag.Bool("memory", true, "persist & reuse re-validated durable facts across runs")
		memFile     = flag.String("memory-file", "", "persistent knowledge file (default: $XDG_STATE_HOME/ada/knowledge.json)")
		verbose     = flag.Bool("v", true, "log one structured line per loop event")
		colorMode   = flag.String("color", "auto", "colorize the live log: auto|always|never")
	)
	flag.Parse()

	color := resolveColor(*colorMode, os.Stderr)
	pretty := color || isTTY(os.Stderr) // pretty layout whenever interactive or forced
	logger := log.New(os.Stderr, "ada ", log.Ltime)
	logf := func(format string, args ...any) {}
	if *verbose {
		if pretty {
			logf = newPrettyLogf(os.Stderr, color)
		} else {
			logf = func(format string, args ...any) { logger.Printf(format, args...) }
		}
	}
	pnt := painter{color}

	// Ctrl-C produces a clean, observable shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *demo {
		if *plan {
			runPlanDemo(ctx, pnt, logf)
		} else {
			runDemo(ctx, pnt, logf)
		}
		return
	}

	if strings.TrimSpace(*objective) == "" {
		fmt.Fprintln(os.Stderr, "error: -objective is required (or pass -demo)")
		flag.Usage()
		os.Exit(2)
	}

	// Persistent, re-validated knowledge: learn durable facts once, reuse them next
	// run (§5.3). nil when disabled — every method is nil-safe.
	mem := ada.OpenMemory(*memFile, *useMemory)
	if mem != nil {
		mem.Log = logf
	}
	seed := mem.Load() // re-validated strong facts (or none)

	// Planning mode: decompose an open-ended objective into sub-goals and work
	// them until the planner judges the objective satisfied.
	if *plan {
		llm := ada.NewOllamaLLM(*ollamaURL, *model)
		rt := ada.NewRuntime()
		coord := ada.NewCoordinator(*objective, llm, llm, rt)
		coord.MaxRounds = *maxRounds
		coord.GoalSteps = *goalSteps
		coord.MaxEntropy = *maxEntropy
		coord.MaxStuck = *maxStuck
		coord.MaxAttemptsPerTask = *maxAttempts
		coord.MaxRoutes = *maxRoutes
		coord.StallBudget = *stall
		coord.MaxFacts = *maxFacts
		coord.Log = logf
		coord.Seed(seed)

		outcome, dec := coord.Run(ctx)
		mem.Save(coord.Facts())
		printSummary(pnt, "PLAN RUN COMPLETE", outcome, dec.Reason,
			ada.StateSnapshot{Objective: *objective, EstablishedFacts: coord.Facts()}, coord.LastError())
		return
	}

	llm := ada.NewOllamaLLM(*ollamaURL, *model)
	rt := ada.NewRuntime()
	orch := ada.NewOrchestrator(*objective, llm, rt)
	orch.Snapshot.Environment = ada.HostFacts() // tell the agent what host it's on (§5.3)
	orch.MaxSteps = *maxSteps
	orch.MaxEntropy = *maxEntropy
	orch.MaxFacts = *maxFacts
	orch.MaxStuck = *maxStuck
	orch.MaxAttemptsPerTask = *maxAttempts
	orch.MaxRoutes = *maxRoutes
	orch.StallBudget = *stall
	orch.Log = logf
	orch.Seed(seed)
	if *useMeta {
		orch.Meta = ada.HeuristicController{MaxEntropy: *maxEntropy}
	}

	outcome := orch.Run(ctx)
	mem.Save(orch.Snapshot.EstablishedFacts)
	printSummary(pnt, "RUN COMPLETE", outcome, "", orch.Snapshot, orch.LastError)
	switch outcome {
	case ada.OutcomeStable:
		fmt.Fprintln(os.Stderr, pnt.c("2",
			"\nnote: the agent reached a stable state — it kept re-verifying facts it had\n"+
				"      already established without making new progress, so the loop stopped.\n"+
				"      The objective is likely complete; the model just never set \"final\": true."))
	case ada.OutcomeExhausted:
		fmt.Fprintln(os.Stderr, pnt.c("2",
			"\nhint: the run hit the step budget without finishing or stabilizing. The agent\n"+
				"      ends on \"final\": true, an external check, or no progress (-stall). Adjust\n"+
				"      -max-steps / -stall / -goal-steps, or refine the objective."))
	case ada.OutcomeFailed:
		fmt.Fprintln(os.Stderr, pnt.c("2",
			"\nnote: the agent exhausted its bounded alternative strategies (-max-routes ×\n"+
				"      -max-attempts) without proving the objective. The last error above is the\n"+
				"      likeliest blocker; refine the objective or raise the recovery budget."))
	}
}
