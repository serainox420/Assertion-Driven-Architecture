package ada

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// factStrength derives how much to trust a produced fact from the channel that
// observed it (§3.2). Independent channels are strong; the command's own output
// is weak because the actor authored it end-to-end.
func factStrength(a Assertion) string {
	switch a.Channel {
	case ChannelFS, ChannelProcess, ChannelService, ChannelExitCode:
		return StrengthStrong // observed independently of the command's stdout narration
	case ChannelStdout, ChannelStderr:
		return StrengthWeak // self-satisfiable; trust only when the goal IS the output
	default:
		return StrengthWeak
	}
}

// lazyPattern matches unanchored, low-specificity regexes that produce false
// positives by matching a stray character in an error message (§8.4). A bare
// `[0-9]+` happily matches the `1` in "error at line 1".
var lazyPattern = regexp.MustCompile(`^(\.\*|\.\+|\\d\+|\[0-9\]\+|\.\*\?)$`)

// isLazyAssertion flags self-satisfiable / under-specified stdout|stderr
// patterns. Anchored patterns (containing ^ or $) are exempt: the model did the
// work of pinning the match (§8.4).
func isLazyAssertion(a Assertion) bool {
	if a.Channel != ChannelStdout && a.Channel != ChannelStderr {
		return false
	}
	p := strings.TrimSpace(a.Pattern)
	if strings.Contains(p, "^") || strings.Contains(p, "$") {
		return false
	}
	return lazyPattern.MatchString(p)
}

// checkIndependentState evaluates a strong assertion by reading observable state
// through a channel the action did not author (§3.2). These are the checks that
// make ADA's correctness claim real rather than decorative.
func checkIndependentState(a Assertion) bool {
	switch a.Channel {
	case ChannelFS:
		return checkFS(a.Pattern)
	case ChannelProcess:
		return checkProcess(a.Pattern)
	case ChannelService:
		return checkService(a.Pattern)
	default:
		return false
	}
}

// checkFS verifies filesystem state. The pattern is a path, optionally with an
// expected octal mode after a '|': "/usr/bin/app|0755". A bare path asserts
// existence; the mode form additionally asserts the permission bits.
func checkFS(pattern string) bool {
	path, mode, hasMode := strings.Cut(pattern, "|")
	info, err := os.Stat(strings.TrimSpace(path))
	if err != nil {
		return false
	}
	if !hasMode {
		return true
	}
	want, err := strconv.ParseUint(strings.TrimSpace(mode), 8, 32)
	if err != nil {
		return false
	}
	return uint32(info.Mode().Perm()) == uint32(want)
}

// checkProcess verifies a process or listening socket exists in the kernel's
// tables. The pattern is matched (as a regex) against the full `ps` command
// lines and the `ss` socket listing — channels the action cannot narrate into.
func checkProcess(pattern string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	if out, err := exec.Command("ps", "-eo", "args").Output(); err == nil && re.Match(out) {
		return true
	}
	// `ss` may be absent on minimal hosts; failure to find it is not a match.
	if out, err := exec.Command("ss", "-tlnp").Output(); err == nil && re.Match(out) {
		return true
	}
	return false
}

// checkService asks the init system whether a unit is active (§3.2). The pattern
// is the unit name. Reads the service's reported state, not the actor's claim.
func checkService(unit string) bool {
	out, err := exec.Command("systemctl", "is-active", strings.TrimSpace(unit)).Output()
	if err != nil {
		// `systemctl is-active` exits non-zero for inactive units; treat the
		// trimmed stdout as authoritative regardless of exit status.
		return strings.TrimSpace(string(out)) == "active"
	}
	return strings.TrimSpace(string(out)) == "active"
}
