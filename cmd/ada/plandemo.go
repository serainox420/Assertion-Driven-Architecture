package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ada "github.com/serainox420/assertion-driven-architecture/ada"
)

// runPlanDemo shows planning mode offline: a scripted planner decomposes an
// open-ended objective into sub-goals, the flat loop completes each, and the
// planner declares the objective satisfied once the facts prove it — no model
// server needed. Mirrors how the real Coordinator + OllamaLLM behave.
func runPlanDemo(ctx context.Context, logf func(string, ...any)) {
	work, err := os.MkdirTemp("", "ada-plandemo-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(work)

	cfg := filepath.Join(work, "config.yaml")
	marker := filepath.Join(work, "marker")

	factsHave := func(facts []ada.Fact, path string) bool {
		for _, f := range facts {
			if strings.Contains(f.Statement, path) {
				return true
			}
		}
		return false
	}

	// Planner: re-plans from the verified facts each round; done when both files exist.
	planFn := func(in ada.PlanInput) (ada.PlanDecision, error) {
		haveCfg, haveMarker := factsHave(in.Facts, cfg), factsHave(in.Facts, marker)
		if haveCfg && haveMarker {
			return ada.PlanDecision{Done: true, Reason: "both workspace files exist and are verified"}, nil
		}
		var subs []string
		if !haveCfg {
			subs = append(subs, "create the config file at "+cfg)
		}
		if !haveMarker {
			subs = append(subs, "create the marker file at "+marker)
		}
		return ada.PlanDecision{Reason: "provision the workspace files", Subgoals: subs}, nil
	}

	// Executor: do the action the current sub-goal asks for, prove it via fs.
	respond := func(s ada.StateSnapshot) (ada.Task, error) {
		target := cfg
		if strings.Contains(s.Objective, "marker") {
			target = marker
		}
		return ada.Task{
			ID:         "provision",
			Command:    fmt.Sprintf("mkdir -p %s && touch %s", work, target),
			Mode:       ada.ModeBlocking,
			TimeoutSec: 5,
			Final:      true, // completes the current SUB-GOAL
			Assertion:  ada.Assertion{Type: "file_exists", Pattern: target, Channel: ada.ChannelFS},
		}, nil
	}

	llm := &ada.MockLLM{Respond: respond, PlanFn: planFn}
	rt := ada.NewRuntime()
	coord := ada.NewCoordinator("set up the app workspace and verify all of its files", llm, llm, rt)
	coord.Log = logf

	outcome, dec := coord.Run(ctx)
	fmt.Printf("\n=== PLAN DEMO COMPLETE: %s ===\n", outcome)
	fmt.Printf("Planner: %s\n", dec.Reason)
	printFacts(ada.StateSnapshot{Objective: coord.Objective, EstablishedFacts: coord.Facts()})
}
