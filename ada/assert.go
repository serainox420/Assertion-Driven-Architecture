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
		if _, spec, has := splitFSPattern(a.Pattern); has {
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

// canonicalFSPredicate recognizes the many spellings a model emits for an fs
// predicate and maps each to the canonical keyword checkFS understands. Octal-mode
// ("0644", "mode=0644") and content ("contains:<re>") predicates are returned
// verbatim, since checkFS parses those itself. ok=false means the string is NOT a
// predicate — so the caller must keep it as part of the path, never drop it.
func canonicalFSPredicate(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "exists", "exist", "present", "any", "any type":
		return "exists", true
	case "dir", "directory", "folder", "is a directory", "is a dir", "isdir", "a directory":
		return "dir", true
	case "file", "regular", "regular file", "is a file", "is a regular file", "a file", "isfile":
		return "file", true
	case "nonempty", "non-empty", "non empty", "notempty", "not empty", "is not empty", "is non-empty", "is nonempty", "has content":
		return "nonempty", true
	case "empty", "is empty":
		return "empty", true
	}
	if _, ok := parseOctalMode(s); ok {
		return strings.TrimSpace(s), true
	}
	if _, ok := fsContentPattern(s); ok {
		return strings.TrimSpace(s), true
	}
	return "", false
}

// splitFSPattern separates an fs pattern into a path and an optional predicate,
// tolerating every notation a model realistically emits. The canonical form is
// `path|predicate`, but models also write `path (predicate)` — the exact shape our
// own factStatement renders ("verified: /etc/app (dir)") and then reads back out of
// EstablishedFacts — plus `path predicate` and prose like `path is a directory`.
// Model output is untrusted input: parse it liberally here, then let checkFS
// validate strictly. A non-canonical predicate is normalized to the keyword checkFS
// understands; a bare path with no recognizable predicate is returned whole (an
// existence check), never mangled.
func splitFSPattern(pattern string) (path, spec string, hasSpec bool) {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return "", "", false
	}
	// 1. Canonical pipe form. Trust the '|' verbatim: a content regex may itself
	//    contain '|', and cutting on the FIRST '|' keeps that regex intact as spec.
	if before, after, ok := strings.Cut(p, "|"); ok {
		return strings.TrimSpace(before), strings.TrimSpace(after), true
	}
	// 2. Parenthesized suffix: `path (dir)`. This is the single most common
	//    malformed shape, because it mirrors factStatement's own rendering.
	if i := strings.LastIndexByte(p, '('); i > 0 && strings.HasSuffix(p, ")") {
		if pred, ok := canonicalFSPredicate(p[i+1 : len(p)-1]); ok {
			return strings.TrimSpace(p[:i]), pred, true
		}
	}
	// 3. Trailing predicate word/phrase: `path dir`, `path is a directory`,
	//    `path 0755`. Only a RECOGNIZED predicate is peeled off, so an ordinary path
	//    keeps its final segment.
	if before, pred, ok := splitTrailingFSPredicate(p); ok {
		return before, pred, true
	}
	// 4. A bare path: existence check.
	return p, "", false
}

// splitTrailingFSPredicate peels a recognized predicate off the end of a
// space-separated, pipe-less pattern. It scans spaces left-to-right and splits at
// the first whose suffix is a recognized predicate, so the LONGEST predicate wins
// (multi-word "is a regular file" binds before the bare "file") and the path prefix
// is taken from the original bytes, preserving any internal spacing.
func splitTrailingFSPredicate(p string) (path, spec string, ok bool) {
	for i := 1; i < len(p); i++ {
		if p[i] != ' ' {
			continue
		}
		prefix := strings.TrimSpace(p[:i])
		if prefix == "" {
			continue
		}
		if pred, found := canonicalFSPredicate(p[i+1:]); found {
			return prefix, pred, true
		}
	}
	return "", "", false
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
// Anchored / regex-escaped paths are normalized first (models over-anchor), and
// the path/predicate split itself is liberal (splitFSPattern), so `path (dir)`,
// `path dir`, and `path is a directory` all resolve to the same check as `path|dir`.
func checkFS(pattern string) bool {
	path, spec, hasSpec := splitFSPattern(pattern)
	info, err := os.Stat(normalizeFSPath(path))
	if err != nil {
		return false
	}
	if !hasSpec {
		return true
	}
	// A model may pipe a non-canonical predicate spelling straight through
	// (`path|folder`, `path|is a directory`); fold it to the keyword the switch
	// understands. Mode and content predicates pass through unchanged.
	if canon, ok := canonicalFSPredicate(spec); ok {
		spec = canon
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
