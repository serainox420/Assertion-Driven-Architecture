package ada

import (
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// maxFSRead bounds how many bytes a `contains:` content assertion reads from a
// file, so asserting against a multi-gigabyte log can't exhaust memory (§5).
const maxFSRead = 1 << 20 // 1 MiB

// independentChannel reports whether a channel is an INDEPENDENT-STATE read — the
// only kind valid as a precondition or postcondition. These channels observe the
// world without trusting (or, for preconditions, even running) the command, which
// is exactly what makes a pre/post check meaningful rather than self-satisfiable.
func independentChannel(ch string) bool {
	switch ch {
	case ChannelFS, ChannelProcess, ChannelService:
		return true
	}
	return false
}

// assertionDesc renders an assertion as a short "channel pattern" string for the
// human-readable Expected field of a pre/postcondition anomaly.
func assertionDesc(a Assertion) string {
	if p := strings.TrimSpace(a.Pattern); p != "" {
		return a.Channel + " " + p
	}
	return a.Channel
}

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

// isLazyAssertion flags self-satisfiable / under-specified regex patterns on any
// channel that interprets its pattern as a regex: stdout/stderr, and the `fs`
// content predicate (`path|contains:<regex>`). Anchored patterns (containing ^ or
// $) are exempt: the model did the work of pinning the match (§8.4).
func isLazyAssertion(a Assertion) bool {
	switch a.Channel {
	case ChannelStdout, ChannelStderr:
		return lazyRegex(a.Pattern)
	case ChannelFS:
		// Only the content predicate is a regex; path/size/mode predicates are not.
		if _, spec, has := strings.Cut(a.Pattern, "|"); has {
			if re, ok := fsContentPattern(spec); ok {
				return lazyRegex(re)
			}
		}
	}
	return false
}

// lazyRegex reports whether a regex string is an unanchored, low-specificity
// pattern that would match a stray character and falsely report success (§8.4).
func lazyRegex(pattern string) bool {
	p := strings.TrimSpace(pattern)
	if strings.ContainsAny(p, "^$") {
		return false
	}
	return lazyPattern.MatchString(p)
}

// fsContentPattern detects (and unwraps) the `fs` content-match predicate. A model
// proves a file's CONTENTS — not merely its existence — with `path|contains:<regex>`
// (aliases: matches:/regex:), and the runtime reads the file and matches the regex
// against it. This is the bulletproof way to "read the file and confirm the change
// really landed": an INDEPENDENT, strong observation of content, not a trusted echo
// of the command's own stdout.
func fsContentPattern(spec string) (string, bool) {
	s := strings.TrimSpace(spec)
	for _, p := range []string{"contains:", "contains=", "matches:", "matches=", "regex:", "regex="} {
		if rest, ok := strings.CutPrefix(s, p); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
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
	// A content predicate proves WHAT is in the file, by reading it independently.
	if re, ok := fsContentPattern(spec); ok {
		data, err := readFileCapped(normalizeFSPath(path), maxFSRead)
		if err != nil {
			return false
		}
		return regexpMatchLine(re, string(data))
	}
	// Otherwise interpret the spec as an octal permission mode. Models phrase this
	// several ways — "0644", "644", "mode=0644", "mode:0644", "0o644" — so be lenient.
	if want, ok := parseOctalMode(spec); ok {
		return uint32(info.Mode().Perm()) == want
	}
	return false // unknown predicate → fail secure
}

// readFileCapped reads at most max bytes from a file, so a content assertion
// against an enormous file can't exhaust memory (§5).
func readFileCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}

// parseOctalMode leniently parses an fs mode spec into permission bits. It
// accepts the bare octal a model should emit ("0644") plus the variants models
// actually emit ("mode=0644", "mode:0644", "0o644", "644").
func parseOctalMode(spec string) (uint32, bool) {
	s := strings.ToLower(strings.TrimSpace(spec))
	for _, p := range []string{"mode=", "mode:", "perm=", "perm:", "0o"} {
		s = strings.TrimPrefix(s, p)
	}
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

// checkProcess verifies a process or listening socket exists in the kernel's
// tables. The pattern is matched (as a regex) against the `ps` command lines and
// the `ss` socket listing — channels the action cannot narrate into.
//
// It EXCLUDES the agent's own process. The agent's argv embeds the objective
// text, so a bare pattern lifted from the objective (e.g. "jq" from "verify jq is
// installed") would otherwise match the agent itself — a self-satisfying false
// positive that reports a tool as "running" when nothing of the sort is true (§3).
func checkProcess(pattern string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	self := strconv.Itoa(os.Getpid())
	// `pid=,args=` suppresses the header and lets us drop our own PID's line.
	if out, err := exec.Command("ps", "-eo", "pid=,args=").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			pid, args, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok || pid == self {
				continue
			}
			if re.MatchString(args) {
				return true
			}
		}
	}
	// Listening sockets (network services). The agent listens on nothing, so there
	// is no self-match here. `ss` may be absent on minimal hosts.
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
