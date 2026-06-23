package ada

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Memory round-trips the DURABLE subset: only STRONG, independent-state facts are
// persisted (weak and exit_code are not), and they reload tagged as MEMORY. Load no
// longer re-stat's them — validation is deferred to first use — so BOTH the present
// and the (now absent) file come back; neither is trusted yet (that happens on use).
func TestMemoryRoundTrip(t *testing.T) {
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

	got := m.Load("") // empty objective ⇒ no scoping, load all durable facts
	// Both fs facts persist and reload; weak + exit_code are never written. Validation
	// is on-use now, so the absent file is NOT dropped at load.
	if len(got) != 2 {
		t.Fatalf("expected 2 persisted fs facts (validation deferred to use), got %d: %+v", len(got), got)
	}
	for _, f := range got {
		if f.SourceID != memorySourceID || f.Strength != StrengthStrong {
			t.Errorf("memory facts must be tagged MEMORY/strong, got %+v", f)
		}
	}
}

// Load is scoped to the objective: facts left by an UNRELATED previous objective are
// neither loaded nor (later) re-validated, while facts pertinent to the current
// objective survive. This is what keeps an "install nginx" run from dragging in (and
// re-checking) the "/opt/ada-demo" directory facts of a prior objective.
func TestMemoryScopesToObjective(t *testing.T) {
	dir := t.TempDir()
	m := &Memory{Path: filepath.Join(dir, "knowledge.json"), Log: func(string, ...any) {}}
	m.Save([]Fact{
		{Statement: "verified: /usr/bin/nginx (file)", Strength: StrengthStrong,
			assertion: Assertion{Type: "fs", Pattern: "/usr/bin/nginx|file", Channel: ChannelFS}},
		{Statement: "verified: /opt/ada-demo/config (dir)", Strength: StrengthStrong,
			assertion: Assertion{Type: "fs", Pattern: "/opt/ada-demo/config|dir", Channel: ChannelFS}},
	})

	got := m.Load("Install nginx, enable and start the service, prove it is listening on port 80")
	if len(got) != 1 {
		t.Fatalf("expected only the nginx-relevant fact to load, got %d: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Statement, "nginx") {
		t.Errorf("the surviving fact should be the nginx one, got %q", got[0].Statement)
	}
}

// Saving NEW facts MERGES into the store rather than overwriting it: the knowledge
// base is cumulative across objectives (Load scopes per run), so a later run must not
// wipe what an earlier one proved. Here an "nginx" save followed by a "zsh" save must
// leave BOTH facts on disk — the bug being fixed truncated the file to just the last
// run's facts.
func TestMemorySaveMergesAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	m := &Memory{Path: filepath.Join(dir, "knowledge.json"), Log: func(string, ...any) {}}

	m.Save([]Fact{{Statement: "verified: /usr/bin/nginx (file)", Strength: StrengthStrong,
		assertion: Assertion{Type: "fs", Pattern: "/usr/bin/nginx|file", Channel: ChannelFS}}})
	// A separate, later run proves a different durable fact.
	m.Save([]Fact{{Statement: "verified: /usr/bin/zsh (file)", Strength: StrengthStrong,
		assertion: Assertion{Type: "fs", Pattern: "/usr/bin/zsh|file", Channel: ChannelFS}}})

	all := m.Load("") // empty objective ⇒ no scoping, return every stored fact
	if len(all) != 2 {
		t.Fatalf("merge should retain both runs' facts, got %d: %+v", len(all), all)
	}
	var haveNginx, haveZsh bool
	for _, f := range all {
		haveNginx = haveNginx || strings.Contains(f.Statement, "nginx")
		haveZsh = haveZsh || strings.Contains(f.Statement, "zsh")
	}
	if !haveNginx || !haveZsh {
		t.Errorf("expected BOTH nginx and zsh facts after merge, got %+v", all)
	}
}

// Re-saving the SAME proposition does not duplicate it, and the freshest statement
// wins — the merge dedups by channel|pattern, it does not append blindly.
func TestMemorySaveDedupsOnReSave(t *testing.T) {
	dir := t.TempDir()
	m := &Memory{Path: filepath.Join(dir, "knowledge.json"), Log: func(string, ...any) {}}

	m.Save([]Fact{{Statement: "old wording", Strength: StrengthStrong,
		assertion: Assertion{Type: "fs", Pattern: "/usr/bin/nginx|file", Channel: ChannelFS}}})
	m.Save([]Fact{{Statement: "fresh wording", Strength: StrengthStrong,
		assertion: Assertion{Type: "fs", Pattern: "/usr/bin/nginx|file", Channel: ChannelFS}}})

	all := m.Load("")
	if len(all) != 1 {
		t.Fatalf("re-saving the same proposition must not duplicate it, got %d: %+v", len(all), all)
	}
	if all[0].Statement != "fresh wording" {
		t.Errorf("the freshest statement should win on a key collision, got %q", all[0].Statement)
	}
}

// A save with nothing durable must NOT truncate an existing store (it returns early,
// before any write), so prior runs' facts survive an objective that proved nothing.
func TestMemorySaveNoDurableKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	m := &Memory{Path: filepath.Join(dir, "knowledge.json"), Log: func(string, ...any) {}}

	m.Save([]Fact{{Statement: "verified: /usr/bin/nginx (file)", Strength: StrengthStrong,
		assertion: Assertion{Type: "fs", Pattern: "/usr/bin/nginx|file", Channel: ChannelFS}}})
	// A run that proves only non-durable facts must leave the store intact.
	m.Save([]Fact{
		{Statement: "weak", Strength: StrengthWeak, assertion: Assertion{Channel: ChannelStdout, Pattern: "^x$"}},
		{Statement: "exit", Strength: StrengthStrong, assertion: Assertion{Channel: ChannelExitCode, Pattern: "0"}},
	})

	all := m.Load("")
	if len(all) != 1 || !strings.Contains(all[0].Statement, "nginx") {
		t.Fatalf("a no-durable save must preserve the existing store, got %+v", all)
	}
}

// A nil *Memory (disabled) is fully safe to use — no panics, no writes.
func TestMemoryNilSafe(t *testing.T) {
	var m *Memory
	if got := m.Load(""); got != nil {
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
