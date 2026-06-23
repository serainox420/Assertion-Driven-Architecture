package ada

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DebugConfig controls what ADA records during a run. Load from JSON via
// LoadDebugConfig; use DefaultDebugConfig() to get all logging on. The zero
// value is safe (all logging disabled, Enabled: false).
type DebugConfig struct {
	Enabled bool   `json:"enabled"`
	Dir     string `json:"dir"` // "" → $XDG_STATE_HOME/ada/debug or ~/.local/state/ada/debug

	// Combined writes a single consolidated report file per run instead of a
	// per-run folder of separate files. ON by default (DefaultDebugConfig).
	// When false, ADA falls back to the legacy folder layout (one file per
	// channel). The per-section toggles below apply identically in both modes —
	// they just select files vs. sections of the one report.
	Combined bool `json:"combined"`

	// Per-file / per-section toggles. All default to true via DefaultDebugConfig.
	LogMeta      bool `json:"log_meta"`      // meta    – model params, limits, env, all settings
	LogTasks     bool `json:"log_tasks"`     // tasks   – full task JSON + result per step
	LogRawLLM    bool `json:"log_raw_llm"`   // include raw LLM response string with each step
	LogStdout    bool `json:"log_stdout"`    // include decoded stdout with each step
	LogStderr    bool `json:"log_stderr"`    // include decoded stderr with each step
	LogAnomalies bool `json:"log_anomalies"` // anomalies – every anomaly payload, decoded
	LogFacts     bool `json:"log_facts"`     // facts     – every fact as it is established
	LogPlanner   bool `json:"log_planner"`   // planner   – every planner call (plan mode)
	LogSummary   bool `json:"log_summary"`   // summary   – written once at end of run
}

// DefaultDebugConfig returns a config with ALL logging enabled and the combined
// single-file report selected — the maximum-verbosity preset for debugging. Set
// Enabled = true and optionally restrict individual fields before passing to
// NewDebugSession.
func DefaultDebugConfig() DebugConfig {
	return DebugConfig{
		Combined:     true,
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

// debugBaseDir resolves the root directory for all debug run folders / reports.
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

// NewRunID mints a sortable, collision-resistant id for one run or benchmark.
func NewRunID(startedAt time.Time) string {
	return startedAt.UTC().Format("20060102T150405") + "-" + debugRandHex(6)
}

// NewBenchmarkDir creates and returns a benchmark-<id> directory under the debug
// base dir. Each task in the suite drops its own consolidated report inside it,
// mirroring how a single run drops run-<id>. Returns "" when debug is disabled.
func NewBenchmarkDir(cfg DebugConfig, startedAt time.Time) (string, error) {
	if !cfg.Enabled {
		return "", nil
	}
	dir := filepath.Join(debugBaseDir(cfg), "benchmark-"+NewRunID(startedAt))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("debug: create benchmark dir %s: %w", dir, err)
	}
	return dir, nil
}

// DebugSession manages the debug output for one run. In combined mode (the
// default) it accumulates every enabled section in memory and writes a single
// consolidated report file on Close. In folder mode it opens one file handle per
// channel and streams JSONL as it goes. All methods are nil-safe and safe for
// concurrent use.
//
// Folder layout (run-<timestamp>-<id>/):
//
//	meta.json       model params, limits, env, all settings
//	tasks.jsonl     full task JSON + execution result per step
//	anomalies.jsonl every anomaly payload (stdout/stderr decoded)
//	facts.jsonl     every fact as it is established
//	planner.jsonl   every planner call in planning mode
//	summary.json    final outcome, duration, all facts — written at end
//
// Combined layout: a single run-<timestamp>-<id>.md report with one section per
// enabled channel.
type DebugSession struct {
	Config    DebugConfig
	RunID     string
	RunDir    string // folder mode: the run directory (combined: the parent dir)
	Report    string // combined mode: the single report file path
	StartedAt time.Time

	combined bool

	mu         sync.Mutex
	pendingRaw string // set by CaptureRaw, consumed by the next LogStep

	// folder mode handles
	tasksFH   *os.File
	anomFH    *os.File
	factsFH   *os.File
	plannerFH *os.File

	// combined mode in-memory accumulators (rendered on Close)
	meta      map[string]any
	steps     []map[string]any
	anomalies []map[string]any
	facts     []map[string]any
	planner   []map[string]any
	summary   map[string]any
}

// NewDebugSession creates a debug session for one run, honoring cfg.Combined.
// Returns (nil, nil) when cfg.Enabled is false so callers can treat nil as a
// no-op without special-casing the enabled check.
func NewDebugSession(cfg DebugConfig, startedAt time.Time) (*DebugSession, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	runID := NewRunID(startedAt)
	base := debugBaseDir(cfg)
	if cfg.Combined {
		if err := os.MkdirAll(base, 0o755); err != nil {
			return nil, fmt.Errorf("debug: create debug dir %s: %w", base, err)
		}
		report := filepath.Join(base, "run-"+runID+".md")
		return newCombinedSession(cfg, runID, base, report, startedAt), nil
	}
	dir := filepath.Join(base, "run-"+runID)
	return newFolderSession(cfg, runID, dir, startedAt)
}

// NewDebugReport creates a combined-mode session whose single consolidated
// report is written to reportPath, regardless of cfg.Combined. The benchmark
// runner uses this to give every task its own file inside a benchmark-<id>/
// directory. Returns (nil, nil) when cfg.Enabled is false.
func NewDebugReport(cfg DebugConfig, reportPath string, startedAt time.Time) (*DebugSession, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
		return nil, fmt.Errorf("debug: create report dir: %w", err)
	}
	return newCombinedSession(cfg, NewRunID(startedAt), filepath.Dir(reportPath), reportPath, startedAt), nil
}

func newCombinedSession(cfg DebugConfig, runID, dir, report string, startedAt time.Time) *DebugSession {
	cfg.Combined = true
	return &DebugSession{
		Config: cfg, RunID: runID, RunDir: dir, Report: report,
		StartedAt: startedAt, combined: true,
	}
}

func newFolderSession(cfg DebugConfig, runID, dir string, startedAt time.Time) (*DebugSession, error) {
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

// Dir returns the run directory path (empty if s is nil). In combined mode this
// is the parent directory that holds the report file.
func (s *DebugSession) Dir() string {
	if s == nil {
		return ""
	}
	return s.RunDir
}

// Location returns the human-facing path of the debug output: the single report
// file in combined mode, or the run folder in folder mode. Empty when s is nil.
func (s *DebugSession) Location() string {
	if s == nil {
		return ""
	}
	if s.combined {
		return s.Report
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

// WriteMeta records run-level metadata once at run start. The caller supplies
// the map; RunID and started_at are injected automatically.
func (s *DebugSession) WriteMeta(meta map[string]any) {
	if s == nil || !s.Config.LogMeta {
		return
	}
	meta["run_id"] = s.RunID
	meta["started_at"] = s.StartedAt.UTC().Format(time.RFC3339Nano)
	meta["debug_config"] = s.Config
	if s.combined {
		s.mu.Lock()
		s.meta = meta
		s.mu.Unlock()
		return
	}
	debugWriteJSON(filepath.Join(s.RunDir, "meta.json"), meta)
}

// LogStep records one step entry (task + execution result). goal is the current
// sub-goal / objective label for disambiguation in plan mode.
func (s *DebugSession) LogStep(step int, goal string, task Task, thinkMs int64, res ExecutionResult, execMs int64, entropy, forks int) {
	if s == nil || !s.Config.LogTasks {
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
	if s.combined {
		s.steps = append(s.steps, entry)
	} else {
		debugAppendJSONL(s.tasksFH, entry)
	}
	s.mu.Unlock()
}

// LogAnomaly records a decoded anomaly entry.
func (s *DebugSession) LogAnomaly(step int, goal string, a *AnomalyPayload) {
	if s == nil || !s.Config.LogAnomalies || a == nil {
		return
	}
	entry := map[string]any{
		"ts":             time.Now().UTC().Format(time.RFC3339Nano),
		"step":           step,
		"goal":           goal,
		"task_id":        a.FailedTaskID,
		"failure_class":  a.FailureClass,
		"exit_code":      a.ExitCode,
		"expected":       a.Expected,
		"attempts":       a.Attempts,
		"directive":      a.Directive,
		"failed_command": a.Command,
		"already_tried":  a.Tried,
		"stdout":         debugDecodeB64(a.ActualOutB64),
		"stderr":         debugDecodeB64(a.ActualErrB64),
	}
	s.mu.Lock()
	if s.combined {
		s.anomalies = append(s.anomalies, entry)
	} else {
		debugAppendJSONL(s.anomFH, entry)
	}
	s.mu.Unlock()
}

// LogFact records a newly established fact.
func (s *DebugSession) LogFact(step int, goal string, f Fact) {
	if s == nil || !s.Config.LogFacts {
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
	if s.combined {
		s.facts = append(s.facts, entry)
	} else {
		debugAppendJSONL(s.factsFH, entry)
	}
	s.mu.Unlock()
}

// LogPlanner records a planner call. Only meaningful in planning mode where the
// Coordinator calls a Planner each round.
func (s *DebugSession) LogPlanner(round int, in PlanInput, dec PlanDecision, durationMs int64) {
	if s == nil || !s.Config.LogPlanner {
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
	if s.combined {
		s.planner = append(s.planner, entry)
	} else {
		debugAppendJSONL(s.plannerFH, entry)
	}
	s.mu.Unlock()
}

// WriteSummary records the end-of-run summary. In folder mode it is written
// immediately to summary.json; in combined mode it is held and folded into the
// consolidated report on Close.
func (s *DebugSession) WriteSummary(outcome Outcome, plannerReason, lastError string, facts []Fact, steps, forks int) {
	if s == nil || !s.Config.LogSummary {
		return
	}
	summary := map[string]any{
		"run_id":         s.RunID,
		"started_at":     s.StartedAt.UTC().Format(time.RFC3339Nano),
		"finished_at":    time.Now().UTC().Format(time.RFC3339Nano),
		"duration_ms":    time.Since(s.StartedAt).Milliseconds(),
		"outcome":        string(outcome),
		"planner_reason": plannerReason,
		"last_error":     lastError,
		"total_steps":    steps,
		"total_forks":    forks,
		"total_facts":    len(facts),
		"facts":          facts,
	}
	if s.combined {
		s.mu.Lock()
		s.summary = summary
		s.mu.Unlock()
		return
	}
	debugWriteJSON(filepath.Join(s.RunDir, "summary.json"), summary)
}

// Close flushes all output. In folder mode it closes the open file handles; in
// combined mode it renders and writes the single consolidated report. Safe to
// call on nil and idempotent enough for a deferred call.
func (s *DebugSession) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.combined {
		s.flushCombinedLocked()
		return
	}
	for _, fh := range []*os.File{s.tasksFH, s.anomFH, s.factsFH, s.plannerFH} {
		if fh != nil {
			_ = fh.Sync()
			_ = fh.Close()
		}
	}
}

// flushCombinedLocked renders the consolidated Markdown report. Caller holds mu.
func (s *DebugSession) flushCombinedLocked() {
	if s.Report == "" {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# ADA debug report — `%s`\n\n", s.RunID)
	fmt.Fprintf(&b, "_started %s_\n\n", s.StartedAt.UTC().Format(time.RFC3339))

	if s.summary != nil {
		fmt.Fprintf(&b, "**Outcome:** `%v`  ·  **steps:** %v  ·  **forks:** %v  ·  **facts:** %v  ·  **duration:** %vms\n\n",
			s.summary["outcome"], s.summary["total_steps"], s.summary["total_forks"],
			s.summary["total_facts"], s.summary["duration_ms"])
		if r, _ := s.summary["planner_reason"].(string); r != "" {
			fmt.Fprintf(&b, "**Planner:** %s\n\n", r)
		}
		if e, _ := s.summary["last_error"].(string); e != "" {
			fmt.Fprintf(&b, "**Last error:** %s\n\n", e)
		}
	}

	if s.meta != nil {
		b.WriteString("## Meta\n\n")
		debugJSONBlock(&b, s.meta)
	}
	if len(s.steps) > 0 {
		fmt.Fprintf(&b, "## Steps (%d)\n\n", len(s.steps))
		for _, st := range s.steps {
			raw, _ := st["raw_llm"].(string)
			delete(st, "raw_llm") // render the raw response separately below the JSON
			fmt.Fprintf(&b, "### step %v", st["step"])
			if g, _ := st["goal"].(string); g != "" {
				fmt.Fprintf(&b, " · %s", g)
			}
			b.WriteString("\n\n")
			debugJSONBlock(&b, st)
			if raw != "" {
				b.WriteString("Raw LLM response:\n\n```\n" + strings.TrimRight(raw, "\n") + "\n```\n\n")
			}
		}
	}
	if len(s.anomalies) > 0 {
		fmt.Fprintf(&b, "## Anomalies (%d)\n\n", len(s.anomalies))
		for _, a := range s.anomalies {
			fmt.Fprintf(&b, "### step %v · %v (`%v`)\n\n", a["step"], a["task_id"], a["failure_class"])
			debugJSONBlock(&b, a)
		}
	}
	if len(s.facts) > 0 {
		fmt.Fprintf(&b, "## Facts (%d)\n\n", len(s.facts))
		for _, f := range s.facts {
			fmt.Fprintf(&b, "- _(%v)_ **%v** — `%v` via %v `%v`\n",
				f["strength"], f["statement"], f["source_id"], f["channel"], f["pattern"])
		}
		b.WriteString("\n")
	}
	if len(s.planner) > 0 {
		fmt.Fprintf(&b, "## Planner calls (%d)\n\n", len(s.planner))
		for _, p := range s.planner {
			fmt.Fprintf(&b, "### round %v (%vms)\n\n", p["round"], p["plan_ms"])
			debugJSONBlock(&b, p)
		}
	}
	if s.summary != nil {
		b.WriteString("## Summary\n\n")
		debugJSONBlock(&b, s.summary)
	}

	_ = os.WriteFile(s.Report, []byte(b.String()), 0o644)
}

// internal helpers

func debugJSONBlock(b *strings.Builder, v any) {
	j, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	b.WriteString("```json\n")
	b.Write(j)
	b.WriteString("\n```\n\n")
}

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
