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
		objective  = flag.String("objective", "", "the pinned, immutable objective for the agent")
		model      = flag.String("model", "qwen2.5-coder:14b", "Ollama model tag for the worker")
		ollamaURL  = flag.String("ollama", "http://localhost:11434", "Ollama base URL")
		demo       = flag.Bool("demo", false, "run the offline, model-free demonstration")
		plan       = flag.Bool("plan", false, "planning mode: decompose an open-ended objective into sub-goals and work them")
		maxSteps   = flag.Int("max-steps", 200, "global step budget (hard stop)")
		maxRounds  = flag.Int("max-rounds", 8, "planning mode: max plan/execute rounds")
		goalSteps  = flag.Int("goal-steps", 40, "planning mode: step budget per sub-goal")
		maxEntropy = flag.Int("max-entropy", 6, "entropy ceiling that triggers a Hard Context Fork")
		maxFacts   = flag.Int("max-facts", 15, "fact-folding cap")
		maxStuck   = flag.Int("max-stuck", 6, "abandon a goal after this many steps with no new verified fact (0 disables)")
		stall      = flag.Int("stall", 2, "stop after this many consecutive no-progress successes (0 disables)")
		useMeta    = flag.Bool("meta", false, "enable the heuristic meta-controller")
		verbose    = flag.Bool("v", true, "log one structured line per loop event")
	)
	flag.Parse()

	logger := log.New(os.Stderr, "ada ", log.Ltime)
	logf := func(format string, args ...any) {}
	if *verbose {
		logf = func(format string, args ...any) { logger.Printf(format, args...) }
	}

	// Ctrl-C produces a clean, observable shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *demo {
		if *plan {
			runPlanDemo(ctx, logf)
		} else {
			runDemo(ctx, logf)
		}
		return
	}

	if strings.TrimSpace(*objective) == "" {
		fmt.Fprintln(os.Stderr, "error: -objective is required (or pass -demo)")
		flag.Usage()
		os.Exit(2)
	}

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
		coord.StallBudget = *stall
		coord.MaxFacts = *maxFacts
		coord.Log = logf

		outcome, dec := coord.Run(ctx)
		fmt.Printf("\n=== PLAN RUN COMPLETE: %s ===\n", outcome)
		fmt.Printf("Planner: %s\n", dec.Reason)
		printFacts(ada.StateSnapshot{Objective: *objective, EstablishedFacts: coord.Facts()})
		if outcome != ada.OutcomeFinished {
			if e := coord.LastError(); e != "" {
				fmt.Printf("Last command error: %s\n", e)
			}
		}
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
	orch.StallBudget = *stall
	orch.Log = logf
	if *useMeta {
		orch.Meta = ada.HeuristicController{MaxEntropy: *maxEntropy}
	}

	outcome := orch.Run(ctx)
	fmt.Printf("\n=== RUN COMPLETE: %s ===\n", outcome)
	printFacts(orch.Snapshot)
	if outcome != ada.OutcomeFinished && orch.LastError != "" {
		fmt.Printf("Last command error: %s\n", orch.LastError)
	}
	switch outcome {
	case ada.OutcomeStable:
		fmt.Fprintln(os.Stderr,
			"\nnote: the agent reached a stable state — it kept re-verifying facts it had\n"+
				"      already established without making new progress, so the loop stopped.\n"+
				"      The objective is likely complete; the model just never set \"final\": true.\n"+
				"      The established facts above are the verified result.")
	case ada.OutcomeExhausted:
		fmt.Fprintln(os.Stderr,
			"\nhint: the run hit the step budget without finishing or stabilizing.\n"+
				"      The agent ends when a Task sets \"final\": true, when an external check\n"+
				"      passes, or when it stops making progress (-stall). Adjust -max-steps,\n"+
				"      -stall, or refine the objective.")
	}
}

func printFacts(s ada.StateSnapshot) {
	fmt.Printf("Objective: %s\n", s.Objective)
	fmt.Printf("Established facts (%d):\n", len(s.EstablishedFacts))
	for _, f := range s.EstablishedFacts {
		fmt.Printf("  [%s] %s (via %s)\n", f.Strength, f.Statement, f.SourceID)
	}
}
