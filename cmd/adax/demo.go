package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ada "github.com/serainox420/assertion-driven-architecture/ada"
)

// runDemo exercises the full flat loop offline with a scripted MockLLM against a
// real temp directory (so fs assertions are genuine independent checks). It walks
// the behaviors ADA exists to enforce: lazy-assertion rejection, a precondition
// guard that skips a doomed write, an fs-proven mkdir, a corroborated write, and a
// doomed readiness probe whose escalating directive forces a change of method.
func runDemo(ctx context.Context, logf func(string, ...any)) RunResult {
	work, err := os.MkdirTemp("", "ada-demo-")
	if err != nil {
		return RunResult{Err: err}
	}
	defer os.RemoveAll(work)

	appDir := filepath.Join(work, "app")
	configPath := filepath.Join(appDir, "config.yaml")
	readyPath := filepath.Join(appDir, "ready")

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

	lazyProbe := ada.Task{
		ID: "lazy_probe", Command: "echo DONE", Mode: ada.ModeBlocking, TimeoutSec: 5,
		Assertion: ada.Assertion{Type: "regex", Pattern: ".*", Channel: ada.ChannelStdout},
	}
	writeConfig := ada.Task{
		ID: "write_config", Description: fmt.Sprintf("config written at %s", configPath),
		Command: fmt.Sprintf("printf 'mode: prod\\n' > %s", configPath),
		Mode:    ada.ModeBlocking, TimeoutSec: 5,
		Preconditions:  []ada.Assertion{{Type: "dir", Pattern: appDir + "|dir", Channel: ada.ChannelFS}},
		Assertion:      ada.Assertion{Type: "exit", Pattern: "0", Channel: ada.ChannelExitCode},
		Postconditions: []ada.Assertion{{Type: "content", Pattern: configPath + "|contains:^mode: prod$", Channel: ada.ChannelFS}},
	}
	makeAppDir := ada.Task{
		ID: "make_appdir", Description: fmt.Sprintf("created app dir %s", appDir),
		Command: fmt.Sprintf("mkdir -p %s", appDir), Mode: ada.ModeBlocking, TimeoutSec: 5,
		Assertion: ada.Assertion{Type: "dir", Pattern: appDir + "|dir", Channel: ada.ChannelFS},
	}
	portProbe := ada.Task{
		ID: "prove_ready", Command: "true", Mode: ada.ModeBlocking, TimeoutSec: 5,
		Assertion: ada.Assertion{Type: "process", Pattern: "ada-demo-daemon-not-running", Channel: ada.ChannelProcess},
	}
	readyMarker := ada.Task{
		ID: "prove_ready", Description: fmt.Sprintf("readiness marker present at %s", readyPath),
		Command: fmt.Sprintf("touch %s", readyPath), Mode: ada.ModeBlocking, TimeoutSec: 5, Final: true,
		Assertion: ada.Assertion{Type: "file_exists", Pattern: readyPath, Channel: ada.ChannelFS},
	}

	llm := &ada.MockLLM{Respond: func(s ada.StateSnapshot) (ada.Task, error) {
		switch {
		case !hasFact(s, "write_config"):
			switch {
			case anomalyFor(s, "write_config") && s.Anomaly.FailureClass == ada.ClassPrecondition:
				return makeAppDir, nil
			case hasFact(s, "make_appdir"):
				return writeConfig, nil
			case anomalyFor(s, "lazy_probe"):
				return writeConfig, nil
			default:
				return lazyProbe, nil
			}
		case !hasFact(s, "prove_ready"):
			if s.Anomaly != nil && s.Anomaly.Directive != "" {
				return readyMarker, nil
			}
			return portProbe, nil
		}
		return readyMarker, nil
	}}

	rt := ada.NewRuntime()
	orch := ada.NewOrchestrator("provision the app config and prove the service is ready — verifying assumptions before acting and corroborating every change", llm, rt)
	orch.MaxSteps = 20
	orch.Log = logf
	orch.DoneCheck = func(s ada.StateSnapshot) bool {
		return hasFact(s, "write_config") && hasFact(s, "prove_ready")
	}

	outcome := orch.Run(ctx)
	return RunResult{Outcome: outcome, Snapshot: orch.Snapshot}
}

// runPlanDemo shows planning mode offline: a scripted planner decomposes an
// open-ended objective, the flat loop completes each sub-goal, and the planner
// declares completion once the facts prove it — mirroring Coordinator + a real LLM.
func runPlanDemo(ctx context.Context, logf func(string, ...any)) RunResult {
	work, err := os.MkdirTemp("", "ada-plandemo-")
	if err != nil {
		return RunResult{Err: err}
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
			Final:      true,
			Assertion:  ada.Assertion{Type: "file_exists", Pattern: target, Channel: ada.ChannelFS},
		}, nil
	}

	llm := &ada.MockLLM{Respond: respond, PlanFn: planFn}
	rt := ada.NewRuntime()
	coord := ada.NewCoordinator("set up the app workspace and verify all of its files", llm, llm, rt)
	coord.Log = logf

	outcome, dec := coord.Run(ctx)
	return RunResult{
		Outcome:    outcome,
		PlanReason: dec.Reason,
		Snapshot:   ada.StateSnapshot{Objective: coord.Objective, EstablishedFacts: coord.Facts()},
		LastError:  coord.LastError(),
	}
}
