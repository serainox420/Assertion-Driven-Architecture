package ada

import (
	"regexp"
	"strings"
)

// envDeterministicSignals are stderr fragments that mean "re-running the same
// vector changes nothing" — the failure is in the environment's state, not the
// phrasing (§8.2). Banging on a locked door wastes tokens.
var envDeterministicSignals = []*regexp.Regexp{
	regexp.MustCompile(`(?i)connection refused`),
	regexp.MustCompile(`(?i)permission denied`),
	regexp.MustCompile(`(?i)command not found`),
	regexp.MustCompile(`(?i)no such file or directory`),
	regexp.MustCompile(`(?i)operation not permitted`),
	regexp.MustCompile(`(?i)name or service not known`),
	regexp.MustCompile(`(?i)address already in use`),
	regexp.MustCompile(`(?i)disk quota exceeded|no space left`),
	// systemd / D-Bus unavailable: in a container or chroot with no running init,
	// `systemctl` can NEVER succeed — the host simply isn't booted with systemd, so
	// re-running the same command is futile. This is a hard environment fact, not a
	// transient hiccup; classing it as transient is what let the nginx-on-a-non-booted
	// -host loop re-issue `systemctl enable/start/is-active` dozens of times in vain.
	regexp.MustCompile(`(?i)has not been booted with systemd`),
	regexp.MustCompile(`(?i)failed to connect to .*bus`),
}

// classifyFailure maps observable signals to a failure class so the entropy
// metric can weight them differently (§8.2). The class drives whether a retry is
// worthwhile at all.
func classifyFailure(exitCode int, stderr []byte) string {
	// A timeout is surfaced by the runtime as exit code -1 with no exec error;
	// the runtime sets ClassTransient directly for that path. Here we classify
	// from the observable shell signals.
	se := string(stderr)

	// 127 = command not found, 126 = not executable: deterministic environment facts.
	switch exitCode {
	case 127, 126:
		return ClassEnvDeterministic
	}

	for _, re := range envDeterministicSignals {
		if re.Match(stderr) {
			return ClassEnvDeterministic
		}
	}

	// Shell syntax errors are the model's own bug — it can fix the next emission.
	if strings.Contains(se, "syntax error") || strings.Contains(se, "unexpected token") {
		return ClassModelError
	}

	// Default: an honest failed hypothesis. Cheap; that's how search works.
	return ClassTransient
}

// entropyWeight is how much a failure of a given class advances the entropy
// counter toward MAX_ENTROPY (§8.2). env_deterministic jumps fast (stop banging
// on a locked door); model_error increments gently (let the model self-correct).
func entropyWeight(class string) int {
	switch class {
	case ClassEnvDeterministic:
		return 3
	case ClassTransient:
		return 1
	case ClassModelError:
		return 1
	case ClassPrecondition:
		return 1 // a fixable planning mistake: go establish the assumption, then act
	default:
		return 1
	}
}
