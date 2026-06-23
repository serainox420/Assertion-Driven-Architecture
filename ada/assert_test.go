package ada

import (
	"os"
	"path/filepath"
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
