package ada

import (
	"os"
	"path/filepath"
	"testing"
)

// Memory round-trips durable facts and RE-VALIDATES them on load: a persisted fact
// whose state no longer holds is dropped, never trusted on faith (§8.3).
func TestMemoryRoundTripAndRevalidation(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present")
	if err := os.WriteFile(present, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "absent") // never created

	m := &Memory{Path: filepath.Join(dir, "knowledge.json"), Log: func(string, ...any) {}}
	m.Save([]Fact{
		{Statement: "present file", Strength: StrengthStrong, assertion: Assertion{Type: "fs", Pattern: present, Channel: ChannelFS}},
		{Statement: "absent file", Strength: StrengthStrong, assertion: Assertion{Type: "fs", Pattern: absent, Channel: ChannelFS}},
		{Statement: "weak narration", Strength: StrengthWeak, assertion: Assertion{Type: "regex", Pattern: "^x$", Channel: ChannelStdout}},
		{Statement: "ran ok", Strength: StrengthStrong, assertion: Assertion{Type: "exit", Pattern: "0", Channel: ChannelExitCode}},
	})

	got := m.Load()
	// Only the still-true, independently-re-checkable fact survives: weak and
	// exit_code facts are never persisted, and the absent file fails re-validation.
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 re-validated fact, got %d: %+v", len(got), got)
	}
	if got[0].Statement != "present file" || got[0].Strength != StrengthStrong {
		t.Errorf("unexpected surviving fact: %+v", got[0])
	}
}

// A nil *Memory (disabled) is fully safe to use — no panics, no writes.
func TestMemoryNilSafe(t *testing.T) {
	var m *Memory
	if got := m.Load(); got != nil {
		t.Errorf("nil memory Load should return nil, got %+v", got)
	}
	m.Save([]Fact{{Statement: "x", Strength: StrengthStrong,
		assertion: Assertion{Channel: ChannelFS, Pattern: "/tmp"}}}) // must not panic
}

// OpenMemory honors the disable switch.
func TestOpenMemoryDisabled(t *testing.T) {
	if OpenMemory("/tmp/whatever.json", false) != nil {
		t.Error("disabled memory must be nil")
	}
	if OpenMemory("/tmp/explicit.json", true) == nil {
		t.Error("an explicit path with memory enabled must open")
	}
}

// Nothing durable to persist ⇒ no file is written (no empty-store clutter).
func TestMemorySkipsWhenNothingDurable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "knowledge.json")
	m := &Memory{Path: path, Log: func(string, ...any) {}}
	m.Save([]Fact{
		{Statement: "weak", Strength: StrengthWeak, assertion: Assertion{Channel: ChannelStdout, Pattern: "^x$"}},
		{Statement: "exit", Strength: StrengthStrong, assertion: Assertion{Channel: ChannelExitCode, Pattern: "0"}},
	})
	if _, err := os.Stat(path); err == nil {
		t.Error("no durable facts should mean no file is written")
	}
}
