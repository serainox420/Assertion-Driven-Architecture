package main

import (
	"os"
	"path/filepath"

	"golang.org/x/term"
)

// term_GetSize returns the controlling terminal's (width, height). It tries stdout
// then stderr so it works whether either is the TTY.
func term_GetSize() (int, int, error) {
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		if w, h, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
			return w, h, nil
		}
	}
	return 0, 0, os.ErrInvalid
}

// repoRoot locates the project checkout so script-wrapping commands (build, test,
// package, sandbox …) can find scripts/. Resolution order: $ADA_HOME / $ADA_ROOT,
// then walk up from the current directory looking for a go.mod with our module
// path, then the directory holding the running executable (walking up the same
// way). Returns "" when nothing matches.
func repoRoot() string {
	for _, k := range []string{"ADA_HOME", "ADA_ROOT"} {
		if v := os.Getenv(k); v != "" {
			if isRepo(v) {
				return v
			}
		}
	}
	if wd, err := os.Getwd(); err == nil {
		if r := walkUpForRepo(wd); r != "" {
			return r
		}
	}
	if exe, err := os.Executable(); err == nil {
		if r := walkUpForRepo(filepath.Dir(exe)); r != "" {
			return r
		}
	}
	return ""
}

func walkUpForRepo(dir string) string {
	for {
		if isRepo(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// isRepo reports whether dir looks like our checkout (has scripts/ and a go.mod).
func isRepo(dir string) bool {
	if st, err := os.Stat(filepath.Join(dir, "scripts")); err != nil || !st.IsDir() {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return false
	}
	return true
}
