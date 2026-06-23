package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeBench(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadBenchmarkValidatesTasks(t *testing.T) {
	dir := t.TempDir()
	writeBench(t, dir, "empty.json", `{"name":"empty","tasks":[]}`)
	if _, err := LoadBenchmark(filepath.Join(dir, "empty.json")); err == nil {
		t.Fatal("expected error for a benchmark with no tasks")
	}

	writeBench(t, dir, "ok.json", `{"name":"ok","mode":"plan","tasks":[{"name":"t1","objective":"do a thing"}]}`)
	b, err := LoadBenchmark(filepath.Join(dir, "ok.json"))
	if err != nil {
		t.Fatalf("LoadBenchmark: %v", err)
	}
	if len(b.Tasks) != 1 || b.Tasks[0].Objective != "do a thing" {
		t.Fatalf("unexpected tasks: %+v", b.Tasks)
	}
	if !b.planMode(b.Tasks[0]) {
		t.Fatal("plan-mode suite should default tasks to planning")
	}
}

func TestBenchmarkPerTaskPlanOverride(t *testing.T) {
	flat := false
	b := Benchmark{Mode: "plan", Tasks: []BenchmarkTask{{Name: "x", Plan: &flat}}}
	if b.planMode(b.Tasks[0]) {
		t.Fatal("per-task plan=false should override the plan-mode suite")
	}
}

func TestListBenchmarksSortedAndSkipsBad(t *testing.T) {
	dir := t.TempDir()
	writeBench(t, dir, "zeta.json", `{"name":"zeta","tasks":[{"name":"a","objective":"o"}]}`)
	writeBench(t, dir, "alpha.json", `{"name":"alpha","tasks":[{"name":"a","objective":"o"}]}`)
	writeBench(t, dir, "broken.json", `{not json`)
	writeBench(t, dir, "notes.txt", `ignored`)

	suites, err := ListBenchmarks(dir)
	if err != nil {
		t.Fatalf("ListBenchmarks: %v", err)
	}
	if len(suites) != 2 {
		t.Fatalf("expected 2 valid suites, got %d (%+v)", len(suites), suites)
	}
	if suites[0].Name != "alpha" || suites[1].Name != "zeta" {
		t.Fatalf("suites not sorted by name: %v, %v", suites[0].Name, suites[1].Name)
	}
}

func TestListBenchmarksMissingDir(t *testing.T) {
	suites, err := ListBenchmarks(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil || suites != nil {
		t.Fatalf("missing dir should be (nil, nil), got (%v, %v)", suites, err)
	}
}

// The shipped example suite must load and be planner-mode with five tasks.
func TestShippedPlannerBasicsSuite(t *testing.T) {
	path := filepath.Join("..", "..", "benchmarks", "planner-basics.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("example suite not present: %v", err)
	}
	b, err := LoadBenchmark(path)
	if err != nil {
		t.Fatalf("LoadBenchmark(planner-basics): %v", err)
	}
	if len(b.Tasks) != 5 {
		t.Fatalf("planner-basics should have 5 tasks, got %d", len(b.Tasks))
	}
	for _, task := range b.Tasks {
		if !b.planMode(task) {
			t.Fatalf("task %q should be planner mode", task.Name)
		}
		if task.Objective == "" {
			t.Fatalf("task %q has an empty objective", task.Name)
		}
	}
}

// TestAllShippedSuitesValid guards every JSON suite in benchmarks/: it must load,
// have at least one task, and every task must carry a non-empty objective. This
// catches a typo'd config before it ever reaches a model run.
func TestAllShippedSuitesValid(t *testing.T) {
	dir := filepath.Join("..", "..", "benchmarks")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("benchmarks dir not present: %v", err)
	}
	suites, err := ListBenchmarks(dir)
	if err != nil {
		t.Fatalf("ListBenchmarks: %v", err)
	}
	if len(suites) < 5 {
		t.Fatalf("expected at least 5 shipped suites, got %d", len(suites))
	}
	for _, b := range suites {
		if len(b.Tasks) == 0 {
			t.Errorf("suite %q has no tasks", b.Name)
		}
		for i, task := range b.Tasks {
			if task.Objective == "" {
				t.Errorf("suite %q task %d (%q) has an empty objective", b.Name, i+1, task.Name)
			}
			if task.Name == "" {
				t.Errorf("suite %q task %d has an empty name", b.Name, i+1)
			}
		}
	}
}

func TestTUIBenchmarkSelectScreen(t *testing.T) {
	dir := t.TempDir()
	writeBench(t, dir, "demo.json", `{"name":"demo","tasks":[{"name":"t1","objective":"o"}]}`)
	t.Setenv("ADA_BENCH_DIR", dir)

	m := newTestModel(t)
	m = selectMenu(t, m, "run")
	// Move to the "Benchmark" run type and select it.
	for i, it := range m.runTypeList.Items() {
		if mi, ok := it.(menuItem); ok && mi.id == "bench" {
			m.runTypeList.Select(i)
		}
	}
	m = drive(t, m, key("enter"))
	if m.runState != rsBenchSelect {
		t.Fatalf("expected rsBenchSelect, got %v", m.runState)
	}
	if len(m.benchList.Items()) != 1 {
		t.Fatalf("expected 1 benchmark in the picker, got %d", len(m.benchList.Items()))
	}
	// esc returns to the run type selector.
	m = drive(t, m, key("esc"))
	if m.runState != rsTypeSelect {
		t.Fatalf("esc from bench select should return to rsTypeSelect, got %v", m.runState)
	}
}
