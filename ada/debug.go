package ada

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DebugConfig controls what ADA records during a run. Load from JSON via
// LoadDebugConfig; use DefaultDebugConfig() to get all logging on. The zero
// value is safe (all logging disabled, Enabled: false).
type DebugConfig struct {
	Enabled bool   `json:"enabled"`
	Dir     string `json:"dir"` // "" → $XDG_STATE_HOME/ada/debug or ~/.local/state/ada/debug

	// Per-file toggles. All default to true when using DefaultDebugConfig.
	LogMeta      bool `json:"log_meta"`      // meta.json    – model params, limits, env, all settings
	LogTasks     bool `json:"log_tasks"`     // tasks.jsonl  – full task JSON + result per step
	LogRawLLM    bool `json:"log_raw_llm"`   // include raw LLM response string in tasks.jsonl
	LogStdout    bool `json:"log_stdout"`    // include decoded stdout in tasks.jsonl
	LogStderr    bool `json:"log_stderr"`    // include decoded stderr in tasks.jsonl
	LogAnomalies bool `json:"log_anomalies"` // anomalies.jsonl – every anomaly payload, decoded
	LogFacts     bool `json:"log_facts"`     // facts.jsonl     – every fact as it is established
	LogPlanner   bool `json:"log_planner"`   // planner.jsonl   – every planner call (plan mode)
	LogSummary   bool `json:"log_summary"`   // summary.json    – written once at end of run
}

// DefaultDebugConfig returns a config with ALL logging enabled — the maximum-
// verbosity preset for debugging. Set Enabled = true and optionally restrict
// individual fields before passing to NewDebugSession.
func DefaultDebugConfig() DebugConfig {
	return DebugConfig{
		LogMeta:      true,
		LogTasks:     true,
		LogRawLLM:    true,
		LogStdout:    true,
		LogStderr:    true,
		LogAnomalies: true,
		LogFacts:     true,
		LogPlanner:   true,
		LogSummary:   true,
	}
}

// LoadDebugConfig reads a DebugConfig from a JSON file. If the file does not
// exist, DefaultDebugConfig is returned (with Enabled from the file, or false).
// A malformed file is a hard error.
func LoadDebugConfig(path string) (DebugConfig, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return DefaultDebugConfig(), nil
	}
	if err != nil {
		return DebugConfig{}, err
	}
	defer f.Close()
	cfg := DefaultDebugConfig() // start with all-on; file overrides individual fields
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return DebugConfig{}, fmt.Errorf("debug config %s: %w", path, err)
	}
	return cfg, nil
}

// debugBaseDir resolves the root directory for all debug run folders.
func debugBaseDir(cfg DebugConfig) string {
	if cfg.Dir != "" {
		return cfg.Dir
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "ada", "debug")
}

// DebugSession manages one per-run debug directory and writes structured JSONL
// logs to separate files inside it. All methods are nil-safe. All public
// methods are safe for concurrent use.
//
// Layout (run-<timestamp>-<id>/):
//
//	meta.json       model params, limits, env, all settings
//	tasks.jsonl     full task JSON + execution result per step
//	anomalies.jsonl every anomaly payload (stdout/stderr decoded)
//	facts.jsonl     every fact as it is established
//	planner.jsonl   every planner call in planning mode
//	summary.json    final outcome, duration, all facts — written at end
type DebugSession struct {
	Config    DebugConfig
	RunID     string
	RunDir    string
	StartedAt time.Time

	mu          sync.Mutex
	tasksFH     *os.File
	anomFH      *os.File
	factsFH     *os.File
	plannerFH   *os.File
	pendingRaw  string // set by CaptureRaw, consumed by LogStep
}

// NewDebugSession creates the per-run directory and opens the log file handles.
// Returns (nil, nil) when cfg.Enabled is false so callers can treat nil as a
// no-op without special-casing the enabled check.
func NewDebugSession(cfg DebugConfig, startedAt time.Time) (*DebugSession, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	runID := startedAt.UTC().Format("20060102T150405") + "-" + debugRandHex(6)
	dir := filepath.Join(debugBaseDir(cfg), "run-"+runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("debug: create run dir %s: %w", dir, err)
	}
	s := &DebugSession{Config: cfg, RunID: runID, RunDir: dir, StartedAt: startedAt}
	if cfg.LogTasks {
		s.tasksFH, _ = os.Create(filepath.Join(dir, "tasks.jsonl"))
	}
	if cfg.LogAnomalies {
		s.anomFH, _ = os.Create(filepath.Join(dir, "anomalies.jsonl"))
	}
	if cfg.LogFacts {
		s.factsFH, _ = os.Create(filepath.Join(dir, "facts.jsonl"))
	}
	if cfg.LogPlanner {
		s.plannerFH, _ = os.Create(filepath.Join(dir, "planner.jsonl"))
	}
	return s, nil
}

// Dir returns the run directory path (empty if s is nil).
func (s *DebugSession) Dir() string {
	if s == nil {
		return ""
	}
	return s.RunDir
}

// CaptureRaw is wired as OllamaLLM.DebugHook to grab the raw LLM response
// before JSON parsing. Stored in a one-shot field, consumed by the next
// LogStep call so it appears alongside the parsed Task.
func (s *DebugSession) CaptureRaw(raw string) {
	if s == nil || !s.Config.LogRawLLM {
		return
	}
	s.mu.Lock()
	s.pendingRaw = raw
	s.mu.Unlock()
}

// WriteMeta writes run-level metadata to meta.json once at run start. The
// caller supplies the map; RunID and started_at are injected automatically.
func (s *DebugSession) WriteMeta(meta map[string]any) {
	if s == nil || !s.Config.LogMeta {
		return
	}
	meta["run_id"] = s.RunID
	meta["started_at"] = s.StartedAt.UTC().Format(time.RFC3339Nano)
	meta["debug_config"] = s.Config
	debugWriteJSON(filepath.Join(s.RunDir, "meta.json"), meta)
}

// LogStep appends one step entry (task + execution result) to tasks.jsonl.
// goal is the current sub-goal / objective label for disambiguation in plan mode.
func (s *DebugSession) LogStep(step int, goal string, task Task, thinkMs int64, res ExecutionResult, execMs int64, entropy, forks int) {
	if s == nil || s.tasksFH == nil {
		return
	}
	s.mu.Lock()
	raw := s.pendingRaw
	s.pendingRaw = ""
	s.mu.Unlock()

	result := map[string]any{
		"passed":        res.Passed,
		"failure_class": debugAnomalyField(res.Anomaly, "class"),
		"exit_code":     debugAnomalyField(res.Anomaly, "exit"),
		"strength":      res.Strength,
		"started":       res.Started,
	}
	if !res.Passed && res.Anomaly != nil {
		if s.Config.LogStdout {
			result["stdout"] = debugDecodeB64(res.Anomaly.ActualOutB64)
		}
		if s.Config.LogStderr {
			result["stderr"] = debugDecodeB64(res.Anomaly.ActualErrB64)
		}
	} else if res.Passed {
		if s.Config.LogStdout {
			result["stdout"] = res.Output
		}
	}

	entry := map[string]any{
		"ts":       time.Now().UTC().Format(time.RFC3339Nano),
		"step":     step,
		"goal":     goal,
		"think_ms": thinkMs,
		"exec_ms":  execMs,
		"entropy":  entropy,
		"forks":    forks,
		"task":     task,
		"result":   result,
	}
	if s.Config.LogRawLLM && raw != "" {
		entry["raw_llm"] = raw
	}

	s.mu.Lock()
	debugAppendJSONL(s.tasksFH, entry)
	s.mu.Unlock()
}

// LogAnomaly appends a decoded anomaly entry to anomalies.jsonl.
func (s *DebugSession) LogAnomaly(step int, goal string, a *AnomalyPayload) {
	if s == nil || s.anomFH == nil || a == nil {
		return
	}
	entry := map[string]any{
		"ts":            time.Now().UTC().Format(time.RFC3339Nano),
		"step":          step,
		"goal":          goal,
		"task_id":       a.FailedTaskID,
		"failure_class": a.FailureClass,
		"exit_code":     a.ExitCode,
		"expected":      a.Expected,
		"attempts":      a.Attempts,
		"directive":     a.Directive,
		"stdout":        debugDecodeB64(a.ActualOutB64),
		"stderr":        debugDecodeB64(a.ActualErrB64),
	}
	s.mu.Lock()
	debugAppendJSONL(s.anomFH, entry)
	s.mu.Unlock()
}

// LogFact appends a newly established fact to facts.jsonl.
func (s *DebugSession) LogFact(step int, goal string, f Fact) {
	if s == nil || s.factsFH == nil {
		return
	}
	entry := map[string]any{
		"ts":        time.Now().UTC().Format(time.RFC3339Nano),
		"step":      step,
		"goal":      goal,
		"source_id": f.SourceID,
		"statement": f.Statement,
		"strength":  f.Strength,
		"channel":   f.assertion.Channel,
		"pattern":   f.assertion.Pattern,
	}
	s.mu.Lock()
	debugAppendJSONL(s.factsFH, entry)
	s.mu.Unlock()
}

// LogPlanner appends a planner call to planner.jsonl. Only meaningful in
// planning mode where the Coordinator calls a Planner each round.
func (s *DebugSession) LogPlanner(round int, in PlanInput, dec PlanDecision, durationMs int64) {
	if s == nil || s.plannerFH == nil {
		return
	}
	entry := map[string]any{
		"ts":       time.Now().UTC().Format(time.RFC3339Nano),
		"round":    round,
		"plan_ms":  durationMs,
		"input":    in,
		"decision": dec,
	}
	s.mu.Lock()
	debugAppendJSONL(s.plannerFH, entry)
	s.mu.Unlock()
}

// WriteSummary writes summary.json at the very end of the run.
func (s *DebugSession) WriteSummary(outcome Outcome, plannerReason, lastError string, facts []Fact, steps, forks int) {
	if s == nil || !s.Config.LogSummary {
		return
	}
	summary := map[string]any{
		"run_id":          s.RunID,
		"started_at":      s.StartedAt.UTC().Format(time.RFC3339Nano),
		"finished_at":     time.Now().UTC().Format(time.RFC3339Nano),
		"duration_ms":     time.Since(s.StartedAt).Milliseconds(),
		"outcome":         string(outcome),
		"planner_reason":  plannerReason,
		"last_error":      lastError,
		"total_steps":     steps,
		"total_forks":     forks,
		"total_facts":     len(facts),
		"facts":           facts,
	}
	debugWriteJSON(filepath.Join(s.RunDir, "summary.json"), summary)
}

// Close flushes and closes all open file handles. Safe to call on nil.
func (s *DebugSession) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, fh := range []*os.File{s.tasksFH, s.anomFH, s.factsFH, s.plannerFH} {
		if fh != nil {
			_ = fh.Sync()
			_ = fh.Close()
		}
	}
}

// internal helpers

func debugAppendJSONL(fh *os.File, v any) {
	if fh == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = fh.Write(append(b, '\n'))
}

func debugWriteJSON(path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, append(b, '\n'), 0o644)
}

func debugDecodeB64(s string) string {
	if s == "" {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(b)
}

func debugAnomalyField(a *AnomalyPayload, field string) any {
	if a == nil {
		switch field {
		case "exit":
			return 0
		default:
			return ""
		}
	}
	switch field {
	case "class":
		return a.FailureClass
	case "exit":
		return a.ExitCode
	default:
		return ""
	}
}

func debugRandHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)[:n]
}
