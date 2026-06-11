// Package ada implements the Assertion-Driven Architecture: a control pattern
// for LLM operations agents that relocates success-evaluation out of the model
// and into a deterministic runtime.
//
// The model proposes a hypothesis (an action paired with a machine-checkable
// assertion); the runtime disposes (executes, then checks the assertion against
// observable state). Truth, control flow, resource limits, and termination all
// live in deterministic code — never in the stochastic model.
//
// See the design report (ADAREADME) for the full rationale. Section references
// in the comments below (e.g. §3) point at that document.
package ada

// Task is the single hypothesis the model emits per turn.
type Task struct {
	ID          string    `json:"id"`          // unique, no spaces — for logs and dedup
	Description string    `json:"description"` // short rationale, for human logs only
	Command     string    `json:"command"`     // the action to execute
	Mode        string    `json:"mode"`        // "blocking" | "daemon" | "job" (see §4.1)
	TimeoutSec  int       `json:"timeout_sec"` // hard ceiling; runtime enforces
	Assertion   Assertion `json:"assertion"`   // how we will KNOW it worked
	// Final marks the Task whose passing assertion proves the OBJECTIVE itself is
	// complete. When such a task's assertion holds, the runtime records the fact
	// and the loop terminates with FINISHED. Completion still requires a passing
	// assertion — the model cannot declare victory by fiat (§1).
	Final bool `json:"final,omitempty"`
	// Compensation is the inverse action to run if a later assertion reveals this
	// (irreversible) step left the system in a bad state — the saga pattern (§15).
	Compensation string `json:"compensation,omitempty"`
}

// Execution modes (§4.1).
const (
	ModeBlocking = "blocking" // run to completion within the timeout, then assert
	ModeDaemon   = "daemon"   // long-lived process; assert on the startup signal only
	ModeJob      = "job"      // backgrounded task; completion polled asynchronously
)

// Assertion is a machine-checkable success condition. Binary. No ambiguity.
type Assertion struct {
	Type    string `json:"type"`    // human label for the check, e.g. "regex" | "file_exists"
	Pattern string `json:"pattern"` // expected code, regex, path, or state expression
	// Channel declares HOW the runtime observes the result — and critically whether
	// it observes through a DIFFERENT channel than the action itself produced. This
	// is the anti-self-satisfaction rule (§3): a state change must be proven by
	// reading the changed state, not by trusting the actor's narration of it.
	Channel string `json:"channel"` // see the Channel* constants
}

// Observation channels (§2, §3). The channel determines fact strength.
const (
	ChannelExitCode = "exit_code" // moderately strong: set by the program, spoofable via `true`
	ChannelStdout   = "stdout"    // weak: self-satisfiable narration
	ChannelStderr   = "stderr"    // weak: self-satisfiable narration
	ChannelFS       = "fs"        // strong: independent — stat the changed file
	ChannelProcess  = "process"   // strong: independent — read the process/socket table
	ChannelService  = "service"   // strong: independent — ask the init system
)

// AnomalyPayload is the autopsy report fed back to the model on failure.
//
// Environment-sourced output is Base64-encoded as a structural defense against
// prompt-injection-via-tool-output (§6): the encoding breaks the surface-form
// adjacency the model's instruction-following pathway keys on, so data stays data.
type AnomalyPayload struct {
	FailedTaskID string `json:"failed_task_id"`
	Expected     string `json:"expected"`
	ActualOutB64 string `json:"actual_out_b64"` // base64 — injection defense (§6)
	ActualErrB64 string `json:"actual_err_b64"`
	ExitCode     int    `json:"exit_code"`
	FailureClass string `json:"failure_class"` // see the Class* constants (§8.2)
}

// Failure classes (§8.2). Entropy is incremented with a per-class weight.
const (
	ClassTransient        = "transient"         // timeout / temporary contention — cheap retry
	ClassEnvDeterministic = "env_deterministic" // refused/denied/not-found — retrying is futile
	ClassModelError       = "model_error"       // malformed JSON / bad command — model can fix it
)

// StateSnapshot is the ENTIRE world the model sees on a given turn. The model is
// stateless between calls; this is its complete input. Keep it small (§7).
type StateSnapshot struct {
	Objective        string          `json:"objective"`                // goal in focus (a sub-goal in planning mode)
	MainObjective    string          `json:"main_objective,omitempty"` // pinned top goal when planning (§5.6)
	Environment      []string        `json:"environment,omitempty"`    // durable host facts: os/distro/package_manager/user (§5.3)
	EstablishedFacts []Fact          `json:"established_facts"`        // verified state changes (§7)
	Anomaly          *AnomalyPayload `json:"anomaly,omitempty"`        // nil when advancing
	EntropyLevel     int             `json:"entropy_level"`            // distance to a hard fork (§8)
	DiscoveredState  []string        `json:"discovered_state"`         // tracked side effects (§4.2)
}

// Fact strengths (§3). Downstream mechanisms (compaction, fork, distillation)
// must respect strength: weak facts are provisional.
const (
	StrengthStrong = "strong" // observed independently of the command's stdout narration
	StrengthWeak   = "weak"   // self-satisfiable; trust only when the goal IS the output
)

// Fact carries its own provenance and strength so context compaction and fork
// re-validation can reason about how much to trust it (§3, §7, §8).
type Fact struct {
	Statement string `json:"statement"`
	SourceID  string `json:"source_id"` // which Task established it
	Strength  string `json:"strength"`  // StrengthStrong | StrengthWeak

	// assertion is retained (unexported, never serialized to the model) so the
	// orchestrator can re-validate weak facts after a Hard Context Fork (§8.3).
	assertion Assertion
}
