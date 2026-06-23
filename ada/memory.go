package ada

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Memory is a persistent, RE-VALIDATED knowledge store: the "learn it once"
// semantic memory (§5.3). Durable facts the agent proves in one run are written
// to a file and reloaded in the next, so it does not re-derive the same basic
// truths for every task.
//
// Two rules keep it from poisoning a fresh run. (1) SCOPE: only facts relevant to
// the CURRENT objective are loaded — an "install nginx" run does not drag in (or
// re-check) directory facts left by an unrelated previous objective. (2) VALIDATE
// ON USE: a persisted fact is never trusted on faith, but it is no longer re-stat'd
// eagerly at load; it is re-observed exactly when the run leans on it (as a
// precondition short-circuit, or before the planner's completion is accepted). Only
// STRONG facts on INDEPENDENT-STATE channels (fs/process/service) are ever stored —
// precisely the facts that can be re-checked by reading the world — so a stale claim
// is dropped the moment it is relied on, never laundered into truth. Trust still
// comes only from a fresh, deterministic observation (§8.3).
type Memory struct {
	Path string // file backing the store
	Max  int    // cap on retained records (most-recent kept); 0 ⇒ defaultMemoryMax
	Log  func(format string, args ...any)
}

const defaultMemoryMax = 64

// memorySourceID tags facts carried in from the persistent store, so the runtime
// knows to re-validate them ON USE (a current-run fact was just observed and is
// trusted directly; a memory fact may be stale and must be re-checked when relied on).
const memorySourceID = "MEMORY"

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

// loadRecords reads the raw on-disk records — no objective scoping, no channel
// filtering — so both Load (which then scopes them) and Save (which MERGES into
// them) work from the same store. Any IO/parse error yields nil: a missing or
// corrupt store simply contributes no prior records, never a crash.
func (m *Memory) loadRecords() []memoryRecord {
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
	return recs
}

// Load reads the store and returns the facts RELEVANT to objective, tagged so the
// runtime re-validates them on use (§ validate-on-use). Facts left by an unrelated
// previous objective are skipped — never loaded, never re-checked — so they neither
// cost a syscall nor pollute the planner's context. An empty objective disables
// scoping (load everything). All IO errors are swallowed: a missing or corrupt store
// simply yields no prior knowledge, never a crash.
func (m *Memory) Load(objective string) []Fact {
	if m == nil {
		return nil
	}
	recs := m.loadRecords()
	objTokens := significantTokens(objective)
	var out, skipped int
	var facts []Fact
	for _, r := range recs {
		a := Assertion{Type: r.Type, Pattern: r.Pattern, Channel: r.Channel}
		if !independentChannel(a.Channel) {
			continue // only re-checkable channels were ever persisted; ignore anything else
		}
		if !relevantToObjective(objTokens, r) {
			skipped++
			continue // an unrelated objective's fact: don't load OR re-validate it
		}
		// Validate ON USE, not here: tag as MEMORY and let the precondition check /
		// completion guard re-observe it the moment the run actually relies on it.
		facts = append(facts, Fact{Statement: r.Statement, SourceID: memorySourceID, Strength: StrengthStrong, assertion: a})
		out++
	}
	if out > 0 || skipped > 0 {
		m.logf("MEMORY_LOAD %d relevant fact(s) (skipped %d off-objective) from %s", out, skipped, m.Path)
	}
	return facts
}

// significantTokens lowercases s, splits it on any non-alphanumeric byte, and keeps
// tokens of length >= 3 that are not generic filler (English connectives plus ADA's
// own fact-rendering words like "verified"/"dir"). It is a deliberately small, fast
// bag-of-words — enough to tell an "nginx" fact apart from an "/opt/ada-demo" one.
func significantTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, tok := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(tok) >= 3 && !memoryStopwords[tok] {
			out[tok] = true
		}
	}
	return out
}

// relevantToObjective reports whether a stored fact pertains to the current
// objective, by sharing at least one significant token with it. An empty objective
// (no tokens) keeps everything, preserving callers that don't scope. This is what
// stops a run from re-validating and re-injecting an unrelated previous objective's
// facts (§ scope-to-objective).
func relevantToObjective(objTokens map[string]bool, r memoryRecord) bool {
	if len(objTokens) == 0 {
		return true
	}
	for tok := range significantTokens(r.Statement + " " + r.Pattern) {
		if objTokens[tok] {
			return true
		}
	}
	return false
}

// memoryStopwords are tokens too generic to signal relevance: common English filler,
// frequent objective verbs, and the predicate / render words ADA bakes into every
// fact statement ("verified: /x (dir)") — which would otherwise cross-match any two
// facts. Discriminating nouns (nginx, zsh, a path component) are never in this set.
var memoryStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"via": true, "are": true, "its": true, "not": true, "but": true, "all": true,
	"any": true, "has": true, "have": true, "was": true, "will": true, "then": true,
	"you": true, "your": true, "from": true, "into": true, "onto": true,
	"ensure": true, "confirm": true, "verified": true, "exists": true, "exist": true,
	"prove": true, "make": true, "set": true, "run": true, "install": true,
	"installed": true, "equivalent": true, "target": true, "distro": true,
	"dir": true, "directory": true, "file": true, "regular": true, "folder": true,
	"nonempty": true, "empty": true, "mode": true, "contains": true,
}

// Save persists the durable subset of facts: STRONG facts on independent-state
// channels, deduped, most-recent first, capped at Max. Transient/weak facts and
// non-re-checkable channels (exit_code/stdout/stderr, fold summaries) are never
// written — they cannot be re-validated, so persisting them would mean trusting a
// stale claim. IO errors are logged and swallowed.
//
// The store is a CUMULATIVE knowledge base spanning many objectives (Load scopes
// it per run), so a save MERGES this run's facts into what previous runs proved —
// it does NOT replace the file. Without the merge, finishing an "install zsh" run
// would wipe the "install nginx" facts a prior run learned, defeating the whole
// point of persistent memory. Fresh facts win on a key collision (they were just
// observed); older records fill in behind them, newest first, capped at Max.
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
	// Walk THIS run's facts most-recent first so the cap retains the freshest knowledge.
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
	}
	fresh := len(recs)
	if fresh == 0 {
		return // nothing new to persist — leave any existing store untouched (never truncate it)
	}

	// MERGE in records previous runs already proved, skipping any the fresh facts
	// just superseded (same channel|pattern). The fresh facts sit first, so the cap
	// keeps the most recent knowledge across runs rather than dropping it.
	for _, r := range m.loadRecords() {
		if !independentChannel(r.Channel) {
			continue // a hand-edited store could hold non-durable records; ignore them
		}
		key := r.Channel + "|" + r.Pattern
		if seen[key] {
			continue
		}
		seen[key] = true
		recs = append(recs, r)
	}
	if len(recs) > max {
		recs = recs[:max]
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
	m.logf("MEMORY_SAVE %d durable fact(s) (%d new) -> %s", len(recs), fresh, m.Path)
}
