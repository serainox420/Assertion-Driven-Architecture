package ada

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ExecutionResult is what the runtime hands back to the orchestrator for a task.
type ExecutionResult struct {
	Passed   bool
	Anomaly  *AnomalyPayload
	Output   string // sanitized + truncated (§5)
	Strength string // strength of the fact this result would establish (§3)
	// Started is true for daemon/job tasks that launched but whose real work
	// continues in the background (their completion fact is injected later).
	Started bool
}

// job tracks a backgrounded "job"-mode task whose completion is polled (§4.1).
type job struct {
	taskID   string
	pid      int
	artifact string // path the runtime watches for; appearance ⇒ likely complete
}

// Runtime is the deterministic muscle: it executes commands, enforces limits,
// and adjudicates assertions. It has no opinions and never calls the model.
type Runtime struct {
	// MaxOutput bounds bytes forwarded per stream (0 ⇒ defaultMaxOutput).
	MaxOutput int
	// WarmupDaemon is how long a daemon is given to announce readiness (§4.1).
	WarmupDaemon time.Duration
	// MuzzleULimits, when true, prefixes commands with ulimit caps to bound the
	// blast radius before the action runs (§5.4).
	MuzzleULimits bool

	mu      sync.Mutex
	Daemons map[string]int // taskID → PID, for scorched-earth cleanup on fork (§8.3)
	jobs    []job
}

// NewRuntime returns a Runtime with sane defaults.
func NewRuntime() *Runtime {
	return &Runtime{
		MaxOutput:     defaultMaxOutput,
		WarmupDaemon:  2 * time.Second,
		MuzzleULimits: true,
		Daemons:       make(map[string]int),
	}
}

func (r *Runtime) maxOut() int {
	if r.MaxOutput > 0 {
		return r.MaxOutput
	}
	return defaultMaxOutput
}

// muzzle wraps a command in generous per-task hard limits so a runaway action
// can't fill the disk or spin a core indefinitely before the timeout fires (§5.4).
//
// Deliberately NO virtual-memory cap: the old `ulimit -v 524288` (512 MiB) was a
// footgun — virtual memory is a poor proxy for real usage, and it silently killed
// legitimate work (package installs, runtimes, anything that mmaps or reserves a
// large arena). We keep a large file-size cap (no accidental disk-fill) and a CPU
// cap; wall-clock is bounded by the per-task timeout, and real memory isolation
// belongs in cgroups/namespaces (§5.4), not a brittle `ulimit -v`.
func (r *Runtime) muzzle(command string) string {
	if !r.MuzzleULimits {
		return command
	}
	return "ulimit -f 8388608 2>/dev/null; ulimit -t 600 2>/dev/null; " + command
}

// Execute runs the prior → action → post lifecycle of a task: verify the
// preconditions (assumptions) hold, dispatch the action by its declared mode,
// then corroborate a passing result through the postconditions. Each phase is a
// deterministic state check, never an LLM call (§1).
func (r *Runtime) Execute(task Task) ExecutionResult {
	// PRIOR: if the assumptions the action depends on are not already true, do not
	// run the command at all — report the unmet assumption so the model establishes
	// it first. A wrong guess thus costs no action (§3, "verify before you act").
	if res, ok := r.checkPreconditions(task); !ok {
		return res
	}

	// ACTION.
	var res ExecutionResult
	switch task.Mode {
	case ModeDaemon:
		res = r.executeDaemon(task)
	case ModeJob:
		res = r.executeJob(task)
	default: // ModeBlocking and anything unspecified
		res = r.executeBlocking(task)
	}

	// POST: corroborate a passing result through independent channels. Job mode's
	// real work is asynchronous — its completion is proven later by the artifact —
	// so the launch itself is not post-checked here.
	if res.Passed && task.Mode != ModeJob && len(task.Postconditions) > 0 {
		res = r.applyPostconditions(task, res)
	}
	return res
}

// checkPreconditions evaluates a task's independent-state guards BEFORE the
// command runs. ok=false means the command must NOT run; the returned result
// carries the anomaly explaining which assumption failed (or that a precondition
// used an invalid, non-independent channel). Independent-state channels only:
// a precondition on stdout/exit_code is meaningless without running something, so
// it is a contract error rather than a check.
func (r *Runtime) checkPreconditions(task Task) (ExecutionResult, bool) {
	for _, p := range task.Preconditions {
		switch {
		case !independentChannel(p.Channel):
			res := r.failed(task, "PRECONDITION_INVALID_CHANNEL: preconditions must use fs/process/service (got "+p.Channel+")", nil, nil, -1)
			res.Anomaly.FailureClass = ClassModelError
			return res, false
		case isLazyAssertion(p):
			res := r.failed(task, "LAZY_ASSERTION: anchor your precondition pattern (^...$)", nil, nil, -1)
			res.Anomaly.FailureClass = ClassModelError
			return res, false
		case !checkIndependentState(p):
			res := r.failed(task, "PRECONDITION_UNMET: "+assertionDesc(p)+" — establish this first, then act", nil, nil, -1)
			res.Anomaly.FailureClass = ClassPrecondition
			return res, false
		}
	}
	return ExecutionResult{Passed: true}, true
}

// applyPostconditions re-checks a passing result against additional independent
// channels (§3.2). A failure here means the command reported success while an
// independent observation disagrees — the goal was not actually achieved.
// Surviving corroboration upgrades the fact to STRONG: the state was independently
// observed, not merely narrated.
func (r *Runtime) applyPostconditions(task Task, res ExecutionResult) ExecutionResult {
	for _, q := range task.Postconditions {
		switch {
		case !independentChannel(q.Channel):
			out := r.failed(task, "POSTCONDITION_INVALID_CHANNEL: postconditions must use fs/process/service (got "+q.Channel+")", nil, nil, 0)
			out.Anomaly.FailureClass = ClassModelError
			return out
		case isLazyAssertion(q):
			out := r.failed(task, "LAZY_ASSERTION: anchor your postcondition pattern (^...$)", nil, nil, 0)
			out.Anomaly.FailureClass = ClassModelError
			return out
		case !checkIndependentState(q):
			out := r.failed(task, "POSTCONDITION_FAILED: "+assertionDesc(q)+" — the command reported success but an independent check disagrees", nil, nil, 0)
			out.Anomaly.FailureClass = ClassTransient // the approach did not really work; cheap to re-think
			return out
		}
	}
	res.Strength = StrengthStrong // independently corroborated ⇒ strong (§3.2)
	return res
}

// executeBlocking runs to completion within the timeout, then asserts (§4.1).
// Every blocking task is a fresh `bash -c`: there is NO shell state carried
// between tasks (§4.2). The model must use absolute paths.
func (r *Runtime) executeBlocking(task Task) ExecutionResult {
	timeout := time.Duration(task.TimeoutSec) * time.Second
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "bash", "-c", r.muzzle(task.Command))
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	// Run the command in its own process group so a real timeout can reap the
	// WHOLE tree — including any process the command backgrounded (e.g. a stray
	// `sleep 600 &`) — instead of orphaning it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // negative PID == process group
		}
		return nil
	}
	// The classic exec hang: if the command backgrounds a child that inherits our
	// stdout/stderr pipe, Run() blocks until THAT child also closes the pipe —
	// potentially forever (observed with a daemon launched in blocking mode).
	// WaitDelay forces Run to reclaim the pipes shortly after bash itself exits.
	cmd.WaitDelay = 2 * time.Second

	_ = cmd.Run()
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() == context.DeadlineExceeded {
		res := r.failed(task, "TIMEOUT", stdout.Bytes(), stderr.Bytes(), -1)
		res.Anomaly.FailureClass = ClassTransient // a timeout is worth a backed-off retry
		return res
	}

	return r.adjudicate(task, exitCode, stdout.Bytes(), stderr.Bytes())
}

// adjudicate evaluates the assertion against the observed result. This is the
// EVALUATE "state" — but it is a function call, not an LLM call. That deletion
// is the whole idea (§1).
func (r *Runtime) adjudicate(task Task, exitCode int, out, errb []byte) ExecutionResult {
	a := task.Assertion
	outStr := sanitizeOutput(out, r.maxOut())
	errStr := sanitizeOutput(errb, r.maxOut())

	// A lazy, unanchored output pattern is a false-positive waiting to happen.
	// Fail secure and tell the model to write a hyper-specific anchored check (§8.4).
	if isLazyAssertion(a) {
		res := r.failed(task, "LAZY_ASSERTION: anchor your pattern (^...$)", out, errb, exitCode)
		res.Anomaly.FailureClass = ClassModelError
		return res
	}

	var passed bool
	switch a.Channel {
	case ChannelExitCode:
		passed = strconv.Itoa(exitCode) == strings.TrimSpace(a.Pattern)
	case ChannelStdout:
		passed = regexpMatchLine(a.Pattern, outStr)
	case ChannelStderr:
		passed = regexpMatchLine(a.Pattern, errStr)
	case ChannelFS, ChannelProcess, ChannelService:
		passed = checkIndependentState(a)
	default:
		// Unknown assertion → fail secure. Never let an unverifiable action be
		// called success (§4, runtime switch default).
		return r.failed(task, "UNKNOWN_ASSERTION", out, errb, exitCode)
	}

	if passed {
		return ExecutionResult{Passed: true, Output: outStr, Strength: factStrength(a)}
	}
	res := r.failed(task, a.Pattern, out, errb, exitCode)
	// When an independent-state assertion (fs/process/service) fails despite a zero
	// exit code, the environment's state is deterministically wrong relative to the
	// model's assertion — not a transient hiccup that a retry will fix. Reclassify so
	// entropy jumps fast (weight 3) and the model forks to a genuinely different
	// strategy rather than re-issuing the same command three times in vain.
	if exitCode == 0 && independentChannel(a.Channel) && res.Anomaly.FailureClass == ClassTransient {
		res.Anomaly.FailureClass = ClassEnvDeterministic
	}
	return res
}

// regexpMatchLine matches an output stream in multiline mode so the model's
// anchored patterns (^...$) bind to LINE boundaries — i.e. `^Listening on :80$`
// matches that line within multi-line output, and the trailing newline that
// every `echo` appends doesn't defeat the anchor. An invalid pattern simply does
// not match rather than crashing the loop.
func regexpMatchLine(pattern, s string) bool {
	re, err := regexp.Compile("(?m)" + pattern)
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

// failed builds the AnomalyPayload autopsy. Environment-sourced output is
// Base64-encoded as the injection defense (§6).
func (r *Runtime) failed(t Task, expected string, out, errb []byte, code int) ExecutionResult {
	return ExecutionResult{
		Passed: false,
		Anomaly: &AnomalyPayload{
			FailedTaskID: t.ID,
			Expected:     expected,
			ActualOutB64: base64.StdEncoding.EncodeToString(out),
			ActualErrB64: base64.StdEncoding.EncodeToString(errb),
			ExitCode:     code,
			FailureClass: classifyFailure(code, errb),
		},
	}
}

// executeDaemon starts a long-lived process, grants a warm-up window, asserts on
// the STARTUP signal (not lifetime), records the PID, and moves on (§4.1).
func (r *Runtime) executeDaemon(task Task) ExecutionResult {
	cmd := exec.Command("bash", "-c", r.muzzle(task.Command))
	if err := cmd.Start(); err != nil {
		return r.failed(task, "DAEMON_START_FAILED", nil, []byte(err.Error()), -1)
	}
	pid := cmd.Process.Pid
	// Reap the process state in the background so it doesn't become a zombie.
	go func() { _ = cmd.Wait() }()

	time.Sleep(r.WarmupDaemon) // warm-up window before checking the readiness signal

	// Daemon readiness is proven by independent state (a listening socket, a unit
	// going active, a pidfile). Output channels are meaningless for a process that
	// is supposed to keep running, so only strong channels are honored here.
	res := r.adjudicate(task, 0, nil, nil)
	if res.Passed {
		r.mu.Lock()
		r.Daemons[task.ID] = pid
		r.mu.Unlock()
		res.Started = true
		return res
	}
	// Failed to come up: don't leak the process.
	_ = cmd.Process.Kill()
	return res
}

// executeJob launches a backgrounded task and registers its expected artifact on
// a watchlist; completion is detected by PollJobs on subsequent iterations
// (§4.1). The immediate assertion validates only that the job launched.
func (r *Runtime) executeJob(task Task) ExecutionResult {
	res := r.executeBlocking(task) // the launching command is short (e.g. nohup … & echo $!)
	if !res.Passed {
		return res
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(strings.Fields(res.Output)[0]))
	r.mu.Lock()
	r.jobs = append(r.jobs, job{taskID: task.ID, pid: pid, artifact: task.Assertion.Pattern})
	r.mu.Unlock()
	res.Started = true
	return res
}

// PollJobs checks each watched job for completion and returns a Fact per job
// that has finished, letting the loop stay responsive while slow work runs
// (§4.1). Completed jobs are removed from the watchlist.
func (r *Runtime) PollJobs() []Fact {
	r.mu.Lock()
	defer r.mu.Unlock()

	var done []Fact
	var still []job
	for _, j := range r.jobs {
		if _, err := os.Stat(j.artifact); err == nil && !pidAlive(j.pid) {
			done = append(done, Fact{
				Statement: fmt.Sprintf("JOB %s COMPLETE, output at %s", j.taskID, j.artifact),
				SourceID:  j.taskID,
				Strength:  StrengthStrong, // artifact-on-disk is independently observable
				assertion: Assertion{Type: "file_exists", Pattern: j.artifact, Channel: ChannelFS},
			})
			continue
		}
		still = append(still, j)
	}
	r.jobs = still
	return done
}

// pidAlive reports whether a PID still refers to a live process.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 probes existence without affecting the process.
	return p.Signal(syscall.Signal(0)) == nil
}

// KillDaemons SIGKILLs every registered daemon — the scorched-earth cleanup run
// on a Hard Context Fork so orphaned processes don't leak resources (§8.3).
func (r *Runtime) KillDaemons() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, pid := range r.Daemons {
		_ = exec.Command("kill", "-9", strconv.Itoa(pid)).Run()
	}
	r.Daemons = make(map[string]int)
}
