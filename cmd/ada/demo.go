package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	ada "github.com/serainox420/assertion-driven-architecture/ada"
)

// runDemo exercises the full loop offline with a scripted MockLLM, against a real
// temp directory so the fs assertions are genuinely independent checks.
//
// The scripted "model" walks the behaviors ADA exists to enforce — now including
// the richer, more flexible assertion lifecycle:
//  1. a LAZY self-satisfiable assertion → fail-secure rejection (§3.1, §8.4)
//  2. a PRECONDITION guard: it assumes the app dir exists; the runtime checks that
//     BEFORE running and gates the command — a wrong guess costs no action (§3)
//  3. it establishes the missing precondition (mkdir), proven via fs → a STRONG fact
//  4. it writes the config and CORROBORATES the change with a postcondition that
//     reads the file's contents back — multi-channel proof, recorded STRONG (§3.2)
//  5. a doomed approach fails repeatedly; the runtime's escalating DIRECTIVE makes
//     the model change method (bounded non-linear recovery), and it proves readiness
//     a different, independent way → done.
func runDemo(ctx context.Context, p painter, logf func(string, ...any)) {
	work, err := os.MkdirTemp("", "ada-demo-")
	if err != nil {
		panic(err)
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

	// Task constructors for the narrative beats.
	lazyProbe := ada.Task{
		ID: "lazy_probe", Command: "echo DONE", Mode: ada.ModeBlocking, TimeoutSec: 5,
		Assertion: ada.Assertion{Type: "regex", Pattern: ".*", Channel: ada.ChannelStdout},
	}
	// write_config ASSUMES the app dir exists (a precondition) and proves the write
	// two ways: exit_code 0 AND a postcondition that reads the file's contents back.
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
	// The doomed approach: prove readiness by looking for a daemon that isn't running.
	portProbe := ada.Task{
		ID: "prove_ready", Command: "true", Mode: ada.ModeBlocking, TimeoutSec: 5,
		Assertion: ada.Assertion{Type: "process", Pattern: "ada-demo-daemon-not-running", Channel: ada.ChannelProcess},
	}
	// The adaptation: prove readiness a different, independent way — a marker file.
	readyMarker := ada.Task{
		ID: "prove_ready", Description: fmt.Sprintf("readiness marker present at %s", readyPath),
		Command: fmt.Sprintf("touch %s", readyPath), Mode: ada.ModeBlocking, TimeoutSec: 5, Final: true,
		Assertion: ada.Assertion{Type: "file_exists", Pattern: readyPath, Channel: ada.ChannelFS},
	}

	llm := &ada.MockLLM{Respond: func(s ada.StateSnapshot) (ada.Task, error) {
		switch {
		// Phase 1: get the config written (lazy rejection, precondition guard, write+corroborate).
		case !hasFact(s, "write_config"):
			switch {
			case anomalyFor(s, "write_config") && s.Anomaly.FailureClass == ada.ClassPrecondition:
				return makeAppDir, nil // the precondition revealed the dir is missing — make it
			case hasFact(s, "make_appdir"):
				return writeConfig, nil // dir now exists; the precondition holds → write + corroborate
			case anomalyFor(s, "lazy_probe"):
				return writeConfig, nil // after the lazy rejection, attempt the guarded write
			default:
				return lazyProbe, nil // very first turn: a lazy assertion the runtime must refuse
			}

		// Phase 2: prove readiness — the first method is doomed; heed the directive and adapt.
		case !hasFact(s, "prove_ready"):
			if s.Anomaly != nil && s.Anomaly.Directive != "" {
				return readyMarker, nil // runtime told us to change method — prove it independently instead
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
	printSummary(p, "DEMO COMPLETE", outcome, "", orch.Snapshot, "")
}
