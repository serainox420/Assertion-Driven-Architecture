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

// normalizeFSPath tolerates the anchored-regex form models routinely emit for fs
// patterns. The §16 "anchor your patterns (^...$)" rule is for stdout/stderr
// regexes, but models over-apply it to paths, producing "^/tmp/app/ready$" or
// "^out\.log$" — which can never stat. Strip a leading ^ / trailing $ and undo
// the common regex escapes so the path resolves.
func normalizeFSPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "^")
	p = strings.TrimSuffix(p, "$")
	return strings.NewReplacer(`\.`, `.`, `\/`, `/`, `\-`, `-`, `\_`, `_`, `\ `, ` `).Replace(p)
}

// checkFS verifies filesystem state. The pattern is a path, optionally with a
// predicate after a '|':
//
//	/usr/bin/app            exists (any type)
//	/usr/bin/app|0755       exists AND mode bits == 0755 (octal)
//	/var/log/app.log|nonempty   exists AND size > 0
//	/var/log/app.log|empty      exists AND size == 0
//	/etc/app|dir            exists AND is a directory
//	/etc/app.conf|file      exists AND is a regular file
//
// Models reach for `|nonempty` naturally; supporting it (rather than silently
// failing a ParseUint) is what makes "prove the file is non-empty" achievable.
// Anchored / regex-escaped paths are normalized first (models over-anchor).
func checkFS(pattern string) bool {
	path, spec, hasSpec := strings.Cut(pattern, "|")
	info, err := os.Stat(normalizeFSPath(path))
	if err != nil {
		return false
	}
	if !hasSpec {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(spec)) {
	case "exists":
		return true
	case "nonempty", "non-empty", "notempty":
		return info.Size() > 0
	case "empty":
		return info.Size() == 0
	case "dir", "directory":
		return info.IsDir()
	case "file", "regular":
		return info.Mode().IsRegular()
	}
	// Otherwise interpret the spec as an octal permission mode (e.g. 0644).
	if want, err := strconv.ParseUint(strings.TrimSpace(spec), 8, 32); err == nil {
		return uint32(info.Mode().Perm()) == uint32(want)
	}
	return false // unknown predicate → fail secure
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
