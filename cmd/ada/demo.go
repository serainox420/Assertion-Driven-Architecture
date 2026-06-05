package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	ada "github.com/serainox420/assertion-driven-architecture/ada"
)

// runDemo exercises the full loop offline with a scripted MockLLM, against a
// real temp directory so the fs assertions are genuinely independent checks.
//
// The scripted "model" deliberately walks through the failure modes ADA exists
// to handle:
//  1. a LAZY self-satisfiable assertion → fail-secure rejection (§3.1, §8.4)
//  2. real work proven by an independent fs check → a STRONG fact (§3.2)
//  3. an env_deterministic failure (read before create) → no value in retrying (§8.2)
//  4. adaptation → create the file, prove it independently → done.
func runDemo(ctx context.Context, logf func(string, ...any)) {
	work, err := os.MkdirTemp("", "ada-demo-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(work)

	configPath := filepath.Join(work, "config.yaml")
	markerPath := filepath.Join(work, "marker")

	hasFact := func(s ada.StateSnapshot, id string) bool {
		for _, f := range s.EstablishedFacts {
			if f.SourceID == id {
				return true
			}
		}
		return false
	}
	anomalyFor := func(s ada.StateSnapshot, id string) bool {
		return s.Anomaly != nil && s.Anomaly.FailedTaskID == id
	}

	llm := &ada.MockLLM{Respond: func(s ada.StateSnapshot) (ada.Task, error) {
		switch {
		// Phase 1: establish the config file.
		case !hasFact(s, "write_config"):
			if anomalyFor(s, "lazy_probe") {
				// The runtime rejected our lazy probe; do the real work and prove it.
				return ada.Task{
					ID:          "write_config",
					Description: fmt.Sprintf("config written at %s", configPath),
					Command:     fmt.Sprintf("printf 'mode: prod\\n' > %s", configPath),
					Mode:        ada.ModeBlocking,
					TimeoutSec:  5,
					Assertion:   ada.Assertion{Type: "file_exists", Pattern: configPath, Channel: ada.ChannelFS},
				}, nil
			}
			// A lazy, self-satisfiable assertion — the runtime must refuse this.
			return ada.Task{
				ID:         "lazy_probe",
				Command:    "echo DONE",
				Mode:       ada.ModeBlocking,
				TimeoutSec: 5,
				Assertion:  ada.Assertion{Type: "regex", Pattern: ".*", Channel: ada.ChannelStdout},
			}, nil

		// Phase 2: establish the marker file (with one env_deterministic detour).
		case !hasFact(s, "create_marker"):
			if anomalyFor(s, "read_marker") {
				// Reading a file that does not exist was futile; create it instead.
				return ada.Task{
					ID:          "create_marker",
					Description: fmt.Sprintf("marker created at %s", markerPath),
					Command:     fmt.Sprintf("touch %s", markerPath),
					Mode:        ada.ModeBlocking,
					TimeoutSec:  5,
					Assertion:   ada.Assertion{Type: "file_exists", Pattern: markerPath, Channel: ada.ChannelFS},
				}, nil
			}
			// Try to read before creating → "No such file or directory" → env_deterministic.
			return ada.Task{
				ID:         "read_marker",
				Command:    fmt.Sprintf("cat %s", markerPath),
				Mode:       ada.ModeBlocking,
				TimeoutSec: 5,
				Assertion:  ada.Assertion{Type: "exit", Pattern: "0", Channel: ada.ChannelExitCode},
			}, nil
		}
		// Nothing left — DoneCheck will terminate the run.
		return ada.Task{
			ID: "noop", Command: "true", Mode: ada.ModeBlocking, TimeoutSec: 5,
			Assertion: ada.Assertion{Type: "exit", Pattern: "0", Channel: ada.ChannelExitCode},
		}, nil
	}}

	rt := ada.NewRuntime()
	orch := ada.NewOrchestrator("create and independently verify a config and a marker file", llm, rt)
	orch.MaxSteps = 20
	orch.Log = logf
	orch.DoneCheck = func(s ada.StateSnapshot) bool {
		return hasFact(s, "write_config") && hasFact(s, "create_marker")
	}

	outcome := orch.Run(ctx)
	fmt.Printf("\n=== DEMO COMPLETE: %s ===\n", outcome)
	printFacts(orch.Snapshot)
}
