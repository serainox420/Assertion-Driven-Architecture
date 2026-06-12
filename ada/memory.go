package ada

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Memory is a persistent, RE-VALIDATED knowledge store: the "learn it once"
// semantic memory (§5.3). Durable facts the agent proves in one run are written
// to a file and reloaded in the next, so it does not re-derive the same basic
// truths for every task.
//
// The bulletproof part is the re-validation: a persisted fact is NEVER trusted on
// faith. Only STRONG facts on INDEPENDENT-STATE channels (fs/process/service) are
// stored — precisely the facts that can be re-checked by reading the world — and
// every one is re-observed on load (exactly as a Hard Context Fork re-validates
// weak facts, §8.3). A fact that no longer holds is silently dropped. So memory
// saves the model the STEPS of rediscovery without ever letting a stale claim leak
// in as truth: trust still comes only from a fresh, deterministic observation.
type Memory struct {
	Path string // file backing the store
	Max  int    // cap on retained records (most-recent kept); 0 ⇒ defaultMemoryMax
	Log  func(format string, args ...any)
}

const defaultMemoryMax = 64

// memoryRecord is the on-disk form of a durable fact: enough to rebuild the
// assertion and re-validate it. Only re-checkable channels are ever written.
type memoryRecord struct {
	Statement string `json:"statement"`
	Type      string `json:"type"`
	Pattern   string `json:"pattern"`
	Channel   string `json:"channel"`
}

// OpenMemory returns a Memory bound to path (or the default location when empty),
// or nil when disabled or no usable path exists. A nil *Memory is fully safe to
// use — Load returns nothing and Save is a no-op — so callers need not branch.
func OpenMemory(path string, enabled bool) *Memory {
	if !enabled {
		return nil
	}
	if path == "" {
		path = DefaultMemoryPath()
	}
	if path == "" {
		return nil
	}
	return &Memory{Path: path, Max: defaultMemoryMax, Log: func(string, ...any) {}}
}

// DefaultMemoryPath resolves the persistent-knowledge file: $ADA_MEMORY if set,
// else $XDG_STATE_HOME/ada/knowledge.json, else ~/.local/state/ada/knowledge.json.
// Returns "" when no home can be determined (memory then disables itself).
func DefaultMemoryPath() string {
	if p := os.Getenv("ADA_MEMORY"); p != "" {
		return p
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "ada", "knowledge.json")
}

func (m *Memory) logf(format string, args ...any) {
	if m != nil && m.Log != nil {
		m.Log(format, args...)
	}
}

// Load reads the store and returns the facts that STILL hold — each one
// re-observed through its independent channel. Stale facts are dropped. All IO
// errors are swallowed: a missing or corrupt store simply yields no prior
// knowledge, never a crash.
func (m *Memory) Load() []Fact {
	if m == nil {
		return nil
	}
	data, err := os.ReadFile(m.Path)
	if err != nil {
		return nil
	}
	var recs []memoryRecord
	if json.Unmarshal(data, &recs) != nil {
		return nil
	}
	var out []Fact
	for _, r := range recs {
		a := Assertion{Type: r.Type, Pattern: r.Pattern, Channel: r.Channel}
		if !independentChannel(a.Channel) {
			continue // only re-checkable channels were ever persisted; ignore anything else
		}
		if !checkIndependentState(a) {
			m.logf("MEMORY_DROP stale %s", assertionDesc(a))
			continue // re-validation failed — never trust a stale claim (§8.3)
		}
		out = append(out, Fact{Statement: r.Statement, SourceID: "MEMORY", Strength: StrengthStrong, assertion: a})
	}
	if len(out) > 0 {
		m.logf("MEMORY_LOAD %d re-validated fact(s) from %s", len(out), m.Path)
	}
	return out
}

// Save persists the durable subset of facts: STRONG facts on independent-state
// channels, deduped, most-recent first, capped at Max. Transient/weak facts and
// non-re-checkable channels (exit_code/stdout/stderr, fold summaries) are never
// written — they cannot be re-validated, so persisting them would mean trusting a
// stale claim. IO errors are logged and swallowed.
func (m *Memory) Save(facts []Fact) {
	if m == nil {
		return
	}
	max := m.Max
	if max <= 0 {
		max = defaultMemoryMax
	}
	seen := make(map[string]bool, len(facts))
	recs := make([]memoryRecord, 0, len(facts))
	// Walk most-recent first so the cap retains the freshest knowledge.
	for i := len(facts) - 1; i >= 0; i-- {
		f := facts[i]
		if f.Strength != StrengthStrong || !independentChannel(f.assertion.Channel) {
			continue
		}
		key := f.assertion.Channel + "|" + f.assertion.Pattern
		if seen[key] {
			continue
		}
		seen[key] = true
		recs = append(recs, memoryRecord{
			Statement: f.Statement, Type: f.assertion.Type,
			Pattern: f.assertion.Pattern, Channel: f.assertion.Channel,
		})
		if len(recs) >= max {
			break
		}
	}
	if len(recs) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o755); err != nil {
		m.logf("MEMORY_SAVE_ERR %v", err)
		return
	}
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(m.Path, data, 0o644); err != nil {
		m.logf("MEMORY_SAVE_ERR %v", err)
		return
	}
	m.logf("MEMORY_SAVE %d durable fact(s) -> %s", len(recs), m.Path)
}
