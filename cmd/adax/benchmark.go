package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	ada "github.com/serainox420/assertion-driven-architecture/ada"
)

// BenchmarkTask is one objective in a benchmark suite. Optional per-task budgets
// override the active config for that task only; zero means "use the config".
type BenchmarkTask struct {
	Name      string `json:"name"`           // short label → report filename slug
	Objective string `json:"objective"`      // the goal handed to the agent
	Plan      *bool  `json:"plan,omitempty"` // override the suite mode for this task
	MaxSteps  int    `json:"max_steps,omitempty"`
	MaxRounds int    `json:"max_rounds,omitempty"`
	GoalSteps int    `json:"goal_steps,omitempty"`
}

// Benchmark is a named list of objectives run one after another. It is just a
// config file in the benchmarks/ folder; running it drives every task through
// the same ADA loop a normal run uses, and (when debug is on) drops one
// consolidated report per task into a benchmark-<id>/ directory.
type Benchmark struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Mode        string          `json:"mode"` // "plan" (default) | "flat"
	Tasks       []BenchmarkTask `json:"tasks"`

	path string `json:"-"` // source file (not serialized)
}

// planMode reports whether the given task should run in planning mode, honoring a
// per-task override and falling back to the suite mode.
func (b Benchmark) planMode(t BenchmarkTask) bool {
	if t.Plan != nil {
		return *t.Plan
	}
	return !strings.EqualFold(strings.TrimSpace(b.Mode), "flat")
}

// Path returns the file the benchmark was loaded from.
func (b Benchmark) Path() string { return b.path }

// benchmarksDir resolves where benchmark config files live: $ADA_BENCH_DIR, else
// <repo>/benchmarks, else ./benchmarks.
func benchmarksDir() string {
	if v := strings.TrimSpace(os.Getenv("ADA_BENCH_DIR")); v != "" {
		return v
	}
	if r := repoRoot(); r != "" {
		return filepath.Join(r, "benchmarks")
	}
	return "benchmarks"
}

// LoadBenchmark reads and validates a single benchmark config file.
func LoadBenchmark(path string) (Benchmark, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Benchmark{}, err
	}
	var b Benchmark
	if err := json.Unmarshal(data, &b); err != nil {
		return Benchmark{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	b.path = path
	if strings.TrimSpace(b.Name) == "" {
		b.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if len(b.Tasks) == 0 {
		return Benchmark{}, fmt.Errorf("%s: benchmark has no tasks", filepath.Base(path))
	}
	return b, nil
}

// ListBenchmarks loads every *.json benchmark config in dir, sorted by name. A
// missing directory is not an error — it yields an empty list.
func ListBenchmarks(dir string) ([]Benchmark, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Benchmark
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := LoadBenchmark(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // skip malformed files rather than failing the whole list
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// BenchmarkTaskResult records how one task fared.
type BenchmarkTaskResult struct {
	Name    string
	Outcome ada.Outcome
	Report  string // path to the per-task consolidated report ("" when debug off)
}

// BenchmarkResult is the aggregate outcome of a whole suite.
type BenchmarkResult struct {
	Name   string
	LogDir string // benchmark-<id>/ directory ("" when debug off)
	Tasks  []BenchmarkTaskResult
	Passed int // tasks that reached FINISHED or STABLE
	Total  int
	Err    error
}

// benchmarkPass reports whether an outcome counts as a pass for scoring: the
// agent either signaled completion (FINISHED) or reached a verified fixed point
// (STABLE). EXHAUSTED / FAILED are misses.
func benchmarkPass(o ada.Outcome) bool {
	return o == ada.OutcomeFinished || o == ada.OutcomeStable
}

// runBenchmark drives every task in a suite one after another, streaming events
// through logf. When debug is enabled it creates a benchmark-<id>/ directory and
// gives each task its own consolidated report inside it. ctx cancellation stops
// the suite between (and during) tasks.
func runBenchmark(ctx context.Context, cfg *Config, client *Client, b Benchmark, logf func(string, ...any)) BenchmarkResult {
	res := BenchmarkResult{Name: b.Name, Total: len(b.Tasks)}

	// Per-suite log directory, prefixed benchmark- instead of run-.
	var benchDir string
	if cfg.DebugEnabled {
		var err error
		benchDir, err = ada.NewBenchmarkDir(cfg.ToDebugConfig(), time.Now())
		if err != nil {
			res.Err = err
			return res
		}
		res.LogDir = benchDir
		logf("benchmark %q · %d tasks · logs %s", b.Name, len(b.Tasks), benchDir)
	} else {
		logf("benchmark %q · %d tasks", b.Name, len(b.Tasks))
	}

	for i, task := range b.Tasks {
		if ctx.Err() != nil {
			break
		}
		planMode := b.planMode(task)
		mode := "flat"
		if planMode {
			mode = "plan"
		}
		logf("── task %d/%d %q (%s) ──", i+1, len(b.Tasks), task.Name, mode)
		logf("objective: %s", task.Objective)

		// Per-task config: copy the suite config and overlay any task budgets.
		taskCfg := *cfg
		if task.MaxSteps > 0 {
			taskCfg.MaxSteps = task.MaxSteps
		}
		if task.MaxRounds > 0 {
			taskCfg.MaxRounds = task.MaxRounds
		}
		if task.GoalSteps > 0 {
			taskCfg.GoalSteps = task.GoalSteps
		}

		// Per-task consolidated report inside the benchmark directory.
		var dbg *ada.DebugSession
		if benchDir != "" {
			name := fmt.Sprintf("%02d-%s.md", i+1, debugSlugName(task.Name))
			dbg, _ = ada.NewDebugReport(taskCfg.ToDebugConfig(), filepath.Join(benchDir, name), time.Now())
			if dbg != nil {
				client.DebugHook = dbg.CaptureRaw
			}
		}

		runRes := runObjective(ctx, &taskCfg, client, task.Objective, planMode, dbg, logf)
		report := dbg.Location()
		dbg.Close()
		client.DebugHook = nil // the session is closed; don't keep a dangling hook

		tr := BenchmarkTaskResult{Name: task.Name, Outcome: runRes.Outcome, Report: report}
		res.Tasks = append(res.Tasks, tr)
		if benchmarkPass(runRes.Outcome) {
			res.Passed++
		}
		logf("task %d/%d %q → %s", i+1, len(b.Tasks), task.Name, runRes.Outcome)
	}
	return res
}

// cmdBenchmark powers `ada benchmark [list|<name>]`: with no args (or `list`) it
// prints the available suites; with a suite name it runs every task in order and
// prints an aggregate scorecard. Honors the same flags as `ada run`.
func cmdBenchmark(cfg *Config, args []string) int {
	dir := benchmarksDir()
	suites, err := ListBenchmarks(dir)
	if err != nil {
		errf("reading benchmarks from %s: %v", dir, err)
		return 1
	}

	if len(args) == 0 || args[0] == "list" || args[0] == "ls" {
		initRender(cfg.Color, os.Stderr)
		if len(suites) == 0 {
			warnf("no benchmark configs in %s — add a *.json suite (see benchmarks/planner-basics.json)", dir)
			return 0
		}
		fmt.Println(theme.Title.Render("Benchmarks") + theme.Dim.Render("  ("+dir+")"))
		for _, b := range suites {
			fmt.Printf("  %s  %s\n", theme.Key.Render(padRight(b.Name, 20)),
				theme.Dim.Render(fmt.Sprintf("%d tasks · %s", len(b.Tasks), clip(b.Description, 70))))
		}
		fmt.Println(theme.Dim.Render("\nrun one: ada benchmark <name>"))
		return 0
	}

	name := args[0]
	cfg.normalize()
	initRender(cfg.Color, os.Stderr)

	var bench *Benchmark
	for i := range suites {
		if strings.EqualFold(suites[i].Name, name) {
			bench = &suites[i]
			break
		}
	}
	if bench == nil {
		errf("no benchmark named %q in %s (try `ada benchmark list`)", name, dir)
		return 2
	}

	ctx, stop := signalContext()
	defer stop()

	client := NewClient(cfg)
	infof("%s · model %s", theme.Title.Render("benchmark "+bench.Name), theme.Key.Render(cfg.Model))
	logf := newLogRenderer(os.Stderr)
	res := runBenchmark(ctx, cfg, client, *bench, logf)
	if res.Err != nil {
		errf("%v", res.Err)
		return 1
	}

	fmt.Println("\n" + theme.Title.Render("BENCHMARK COMPLETE") + "  " +
		theme.Key.Render(fmt.Sprintf("%d/%d passed", res.Passed, res.Total)))
	for _, t := range res.Tasks {
		mark := theme.OK.Render("✓")
		if !benchmarkPass(t.Outcome) {
			mark = theme.Bad.Render("✗")
		}
		fmt.Printf("  %s %s  %s\n", mark, padRight(t.Name, 24), theme.outcomeStyle(string(t.Outcome)).Render(string(t.Outcome)))
	}
	if res.LogDir != "" {
		fmt.Println(theme.Dim.Render("\nreports: " + res.LogDir))
	}
	return 0
}

// debugSlugName mirrors the ada package's slugging for per-task report names,
// kept here so the adax binary doesn't need to export it from ada.
func debugSlugName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = strings.Trim(out[:40], "-")
	}
	if out == "" {
		out = "task"
	}
	return out
}
