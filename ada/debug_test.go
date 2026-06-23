package ada

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helper: an all-on combined config rooted at a temp dir.
func combinedCfg(t *testing.T) DebugConfig {
	t.Helper()
	cfg := DefaultDebugConfig()
	cfg.Enabled = true
	cfg.Dir = t.TempDir()
	return cfg
}

func TestDefaultDebugConfigCombinedOn(t *testing.T) {
	if !DefaultDebugConfig().Combined {
		t.Fatal("DefaultDebugConfig should enable Combined by default")
	}
}

func TestCombinedSessionWritesSingleReport(t *testing.T) {
	cfg := combinedCfg(t)
	s, err := NewDebugSession(cfg, time.Now())
	if err != nil {
		t.Fatalf("NewDebugSession: %v", err)
	}
	if s == nil {
		t.Fatal("expected a session when enabled")
	}
	// In combined mode the location is a single .md file, not a directory.
	if !strings.HasSuffix(s.Location(), ".md") {
		t.Fatalf("combined location should be a .md file, got %q", s.Location())
	}

	s.WriteMeta(map[string]any{"mode": "flat", "objective": "demo"})
	s.LogStep(1, "demo", Task{ID: "t1", Command: "true"}, 5, ExecutionResult{Passed: true, Output: "ok"}, 7, 0, 0)
	s.LogFact(1, "demo", Fact{SourceID: "t1", Statement: "it works", Strength: StrengthStrong})
	s.WriteSummary(OutcomeFinished, "", "", []Fact{{SourceID: "t1", Statement: "it works", Strength: StrengthStrong}}, 1, 0)

	// Nothing should exist on disk until Close flushes the combined report.
	if _, err := os.Stat(s.Location()); !os.IsNotExist(err) {
		t.Fatalf("combined report should not exist before Close (err=%v)", err)
	}
	s.Close()

	data, err := os.ReadFile(s.Location())
	if err != nil {
		t.Fatalf("reading report: %v", err)
	}
	report := string(data)
	for _, want := range []string{"# ADA debug report", "## Meta", "## Steps", "## Facts", "## Summary", "FINISHED", "it works"} {
		if !strings.Contains(report, want) {
			t.Errorf("combined report missing %q\n---\n%s", want, report)
		}
	}

	// Only the single file should exist in the base dir — no run-* folder.
	entries, _ := os.ReadDir(cfg.Dir)
	if len(entries) != 1 || entries[0].IsDir() {
		t.Fatalf("combined mode should produce exactly one file, got %v", entries)
	}
}

func TestFolderSessionWritesSeparateFiles(t *testing.T) {
	cfg := combinedCfg(t)
	cfg.Combined = false
	s, err := NewDebugSession(cfg, time.Now())
	if err != nil {
		t.Fatalf("NewDebugSession: %v", err)
	}
	// Folder mode location is a directory containing the streamed files.
	if filepath.Ext(s.Location()) != "" {
		t.Fatalf("folder location should be a directory, got %q", s.Location())
	}
	s.LogStep(1, "demo", Task{ID: "t1", Command: "true"}, 1, ExecutionResult{Passed: true}, 1, 0, 0)
	s.Close()

	if _, err := os.Stat(filepath.Join(s.Dir(), "tasks.jsonl")); err != nil {
		t.Fatalf("expected tasks.jsonl in folder mode: %v", err)
	}
}

func TestNewDebugReportForcesCombined(t *testing.T) {
	cfg := DefaultDebugConfig()
	cfg.Enabled = true
	cfg.Combined = false // folder preference must be ignored by NewDebugReport
	report := filepath.Join(t.TempDir(), "bench", "01-task.md")
	s, err := NewDebugReport(cfg, report, time.Now())
	if err != nil {
		t.Fatalf("NewDebugReport: %v", err)
	}
	if s.Location() != report {
		t.Fatalf("report location = %q, want %q", s.Location(), report)
	}
	s.WriteSummary(OutcomeStable, "", "", nil, 0, 0)
	s.Close()
	if _, err := os.Stat(report); err != nil {
		t.Fatalf("report not written: %v", err)
	}
}

func TestNewBenchmarkDirPrefixed(t *testing.T) {
	cfg := combinedCfg(t)
	dir, err := NewBenchmarkDir(cfg, time.Now())
	if err != nil {
		t.Fatalf("NewBenchmarkDir: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(dir), "benchmark-") {
		t.Fatalf("benchmark dir should be prefixed benchmark-, got %q", filepath.Base(dir))
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("benchmark dir not created: %v", err)
	}
}

func TestDisabledSessionIsNil(t *testing.T) {
	s, err := NewDebugSession(DebugConfig{Enabled: false}, time.Now())
	if err != nil || s != nil {
		t.Fatalf("disabled config should yield (nil, nil), got (%v, %v)", s, err)
	}
	// All methods must be nil-safe.
	s.WriteMeta(nil)
	s.LogStep(0, "", Task{}, 0, ExecutionResult{}, 0, 0, 0)
	s.Close()
	if s.Location() != "" {
		t.Fatal("nil session Location should be empty")
	}
}
