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
		maxSteps   = flag.Int("max-steps", 200, "global step budget (hard stop)")
		maxEntropy = flag.Int("max-entropy", 6, "entropy ceiling that triggers a Hard Context Fork")
		maxFacts   = flag.Int("max-facts", 15, "fact-folding cap")
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
		runDemo(ctx, logf)
		return
	}

	if strings.TrimSpace(*objective) == "" {
		fmt.Fprintln(os.Stderr, "error: -objective is required (or pass -demo)")
		flag.Usage()
		os.Exit(2)
	}

	llm := ada.NewOllamaLLM(*ollamaURL, *model)
	rt := ada.NewRuntime()
	orch := ada.NewOrchestrator(*objective, llm, rt)
	orch.MaxSteps = *maxSteps
	orch.MaxEntropy = *maxEntropy
	orch.MaxFacts = *maxFacts
	orch.StallBudget = *stall
	orch.Log = logf
	if *useMeta {
		orch.Meta = ada.HeuristicController{MaxEntropy: *maxEntropy}
	}

	outcome := orch.Run(ctx)
	fmt.Printf("\n=== RUN COMPLETE: %s ===\n", outcome)
	printFacts(orch.Snapshot)
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
