package ada

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSplitFSPattern locks in the liberal path/predicate separation. The model
// emits an fs predicate in several notations — the canonical `path|pred`, the
// `path (pred)` shape our own factStatement renders (and the model mirrors back
// from EstablishedFacts), a bare trailing word, and prose — and all must resolve
// to the same (path, predicate). A bare path, or a path whose final segment is
// not a recognized predicate, must be returned whole and never mangled.
func TestSplitFSPattern(t *testing.T) {
	cases := []struct {
		in      string
		path    string
		spec    string
		hasSpec bool
	}{
		// dir, every notation the model realistically emits
		{"/opt/ada-demo|dir", "/opt/ada-demo", "dir", true},
		{"/opt/ada-demo (dir)", "/opt/ada-demo", "dir", true},
		{"/opt/ada-demo dir", "/opt/ada-demo", "dir", true},
		{"/opt/ada-demo is a directory", "/opt/ada-demo", "dir", true},
		{"/opt/ada-demo (directory)", "/opt/ada-demo", "dir", true},
		{"  /opt/ada-demo (dir)  ", "/opt/ada-demo", "dir", true}, // trims surrounding space
		// file
		{"/etc/app.conf (file)", "/etc/app.conf", "file", true},
		{"/etc/app.conf is a regular file", "/etc/app.conf", "file", true},
		// nonempty
		{"/var/log/app.log (nonempty)", "/var/log/app.log", "nonempty", true},
		{"/var/log/app.log is not empty", "/var/log/app.log", "nonempty", true},
		// mode
		{"/usr/bin/app (0755)", "/usr/bin/app", "0755", true},
		{"/usr/bin/app 0755", "/usr/bin/app", "0755", true},
		{"/usr/bin/app|mode=0755", "/usr/bin/app", "mode=0755", true},
		// content predicate carried verbatim through the canonical pipe (which itself
		// may contain '|' — Cut on the FIRST pipe keeps the regex intact)
		{"/data/file|contains:^OK$", "/data/file", "contains:^OK$", true},
		// bare path: no predicate
		{"/opt/ada-demo", "/opt/ada-demo", "", false},
		// a path whose final word is NOT a recognized predicate stays whole — even
		// with an internal space — so we never amputate a real path segment
		{"/home/user/My Documents", "/home/user/My Documents", "", false},
	}
	for _, c := range cases {
		path, spec, has := splitFSPattern(c.in)
		if path != c.path || spec != c.spec || has != c.hasSpec {
			t.Errorf("splitFSPattern(%q) = (%q, %q, %v); want (%q, %q, %v)",
				c.in, path, spec, has, c.path, c.spec, c.hasSpec)
		}
	}
}

// TestCheckFSToleratesPredicateNotations is the regression for the planning-mode
// post-mortem: with real fs objects on disk, every notation the model emits for a
// predicate must adjudicate the same as the canonical `path|pred`. Before the fix,
// `path (dir)` was stat'd as a literal path → ENOENT → a false PRECONDITION_UNMET.
func TestCheckFSToleratesPredicateNotations(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(dir, "full.txt")
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(full, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "nope")

	cases := []struct {
		pattern string
		want    bool
	}{
		// directory — pipe / paren / trailing-word / phrase / synonym
		{sub + "|dir", true},
		{sub + " (dir)", true},
		{sub + " dir", true},
		{sub + " is a directory", true},
		{sub + " (directory)", true},
		{sub + "|directory", true},
		{sub + "|folder", true}, // synonym only reachable once checkFS canonicalizes
		// regular file
		{full + "|file", true},
		{full + " (file)", true},
		{full + " is a file", true},
		{full + " is a regular file", true},
		{full + "|regular", true},
		// nonempty / empty
		{full + " (nonempty)", true},
		{full + " is not empty", true},
		{full + "|non-empty", true},
		{empty + " (empty)", true},
		{empty + " is empty", true},
		// existence
		{sub, true},
		{sub + " (exists)", true},
		{full + " exists", true},
		// mode (full is 0644)
		{full + "|0644", true},
		{full + " (0644)", true},
		{full + " 0644", true},
		{full + " mode=0644", true},
		// negatives — the notation resolves but the predicate is genuinely false
		{sub + " (file)", false}, // a dir is not a regular file
		{full + " (dir)", false}, // a file is not a dir
		{empty + " (nonempty)", false},
		{full + "|0600", false}, // wrong mode
		{full + " (0600)", false},
		{absent + " (dir)", false},        // missing path, regardless of notation
		{full + "|boguspredicate", false}, // unknown predicate fails secure
	}
	for _, c := range cases {
		if got := checkFS(c.pattern); got != c.want {
			t.Errorf("checkFS(%q) = %v; want %v", c.pattern, got, c.want)
		}
	}
}

// TestCheckFSCompoundConjunction locks in the fix for the stuck-then-STABLE
// failure seen in the planner benchmark: a model crammed two `contains:` checks
// for one file into a single assertion, joined by a newline. Fed whole to one
// matcher it could never pass, so the (correctly written) file failed forever and
// the run forked until it stalled. Such a compound must be a conjunction: every
// clause is evaluated independently and all must hold.
func TestCheckFSCompoundConjunction(t *testing.T) {
	dir := t.TempDir()
	yaml := filepath.Join(dir, "app.yaml")
	if err := os.WriteFile(yaml, []byte("name: ada-demo\nmode: prod\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The exact shape emitted in the benchmark (two clauses, newline-joined).
	both := yaml + "|contains:^name: ada-demo$\n" + yaml + "|contains:^mode: prod$"
	if !checkFS(both) {
		t.Errorf("compound assertion with both lines present should pass:\n%q", both)
	}

	// If any clause is false, the whole conjunction fails.
	missing := yaml + "|contains:^name: ada-demo$\n" + yaml + "|contains:^mode: dev$"
	if checkFS(missing) {
		t.Errorf("compound assertion with a false clause must fail:\n%q", missing)
	}

	// Mixed predicate kinds across clauses (existence + content + mode) all AND.
	mixed := yaml + "\n" + yaml + "|nonempty\n" + yaml + "|0644"
	if !checkFS(mixed) {
		t.Errorf("compound of existence+nonempty+mode clauses should pass:\n%q", mixed)
	}

	// A single (non-compound) pattern is unaffected — no accidental splitting.
	if !checkFS(yaml + "|contains:^mode: prod$") {
		t.Error("single-clause content assertion regressed")
	}

	// splitFSClauses must not split an ordinary single pattern.
	if got := splitFSClauses(yaml + "|contains:^mode: prod$"); len(got) != 1 {
		t.Errorf("single pattern split into %d clauses, want 1", len(got))
	}

	// A compound assertion must render as a clean, single-line conjunction fact —
	// not the raw multi-line pattern — since it is persisted and shown to the planner.
	stmt := factStatement(Task{Assertion: Assertion{Channel: ChannelFS, Pattern: both}})
	if strings.ContainsAny(stmt, "\n\r") {
		t.Errorf("compound fact statement leaked a newline: %q", stmt)
	}
	if !strings.Contains(stmt, "name: ada-demo") || !strings.Contains(stmt, "mode: prod") {
		t.Errorf("compound fact statement should mention both clauses: %q", stmt)
	}
}

// TestCheckFSAccessPredicates covers the writable/readable/executable predicates.
// They were added after a run looped to its route budget: the model proved a dir
// writable with a real command (exit 0) but asserted fs `|writable`, which checkFS
// did not understand and so failed every time. access(2) reflects REAL access, so
// these hold whether the tests run as root or as an unprivileged user.
func TestCheckFSAccessPredicates(t *testing.T) {
	dir := t.TempDir() // owned by us, writable + readable + searchable
	plain := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "nope")

	cases := []struct {
		pattern string
		want    bool
	}{
		{dir + "|writable", true},
		{dir + "|readable", true},
		{dir + " is writable", true}, // notation tolerance
		{dir + " (writable)", true},
		{plain + "|writable", true}, // a 0644 file we own is writable
		{plain + "|readable", true},
		{plain + "|executable", false}, // no exec bit (reliable even as root)
		{script + "|executable", true}, // has exec bits
		{absent + "|writable", false},  // a missing path is not writable
		{absent + "|readable", false},
	}
	for _, c := range cases {
		if got := checkFS(c.pattern); got != c.want {
			t.Errorf("checkFS(%q) = %v; want %v", c.pattern, got, c.want)
		}
	}
}

// TestFSWritableThroughRuntime drives the exact loop bug end to end: the model
// asserts an existing writable directory via fs `|writable`. The runtime must
// adjudicate PASS — previously the unknown predicate failed every time, so the
// sub-goal forked until its route budget was spent and the run died on step one.
func TestFSWritableThroughRuntime(t *testing.T) {
	dir := t.TempDir()
	rt := NewRuntime()
	res := rt.Execute(Task{
		ID: "w", Command: "true", Mode: ModeBlocking, TimeoutSec: 5,
		Assertion: Assertion{Type: "fs", Pattern: dir + "|writable", Channel: ChannelFS},
	})
	if !res.Passed {
		t.Error("an existing writable dir must satisfy fs|writable through the runtime")
	}
}

// TestFSPredicateNotationThroughRuntime drives the exact post-mortem bug end to
// end: a model asserts an existing directory with the "(dir)" notation our own
// factStatement taught it. The runtime must adjudicate it PASS, not stat the
// literal string "<dir> (dir)" into an ENOENT.
func TestFSPredicateNotationThroughRuntime(t *testing.T) {
	dir := t.TempDir()
	rt := NewRuntime()
	res := rt.Execute(Task{
		ID: "t", Command: "true", Mode: ModeBlocking, TimeoutSec: 5,
		Assertion: Assertion{Type: "fs", Pattern: dir + " (dir)", Channel: ChannelFS},
	})
	if !res.Passed {
		t.Fatalf("`path (dir)` on an existing directory must pass, got anomaly %+v", res.Anomaly)
	}
}
