package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	ada "github.com/serainox420/assertion-driven-architecture/ada"
)

// painter wraps text in ANSI SGR codes when enabled, and is a no-op otherwise.
type painter struct{ on bool }

func (p painter) c(code, s string) string {
	if !p.on || s == "" {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

// ── color/TTY detection ───────────────────────────────────────────────────────

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

// resolveColor decides whether to emit ANSI color, honoring -color and NO_COLOR.
func resolveColor(mode string, f *os.File) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	default: // auto
		return isTTY(f) && os.Getenv("NO_COLOR") == ""
	}
}

// ── log line parsing (we render our own structured lines) ─────────────────────

var (
	kvRe          = regexp.MustCompile(`(\w+)=("(?:[^"\\]|\\.)*"|\S+)`)
	trailQuoteRe  = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"\s*$`)
	goalOutcomeRe = regexp.MustCompile(`goal=\d+ (FINISHED|STABLE|EXHAUSTED)`)
)

func parseKV(line string) map[string]string {
	m := map[string]string{}
	for _, kv := range kvRe.FindAllStringSubmatch(line, -1) {
		v := kv[2]
		if len(v) >= 2 && v[0] == '"' {
			v = v[1 : len(v)-1]
		}
		m[kv[1]] = v
	}
	return m
}

func trailingQuoted(line string) string {
	if m := trailQuoteRe.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

func afterColon(line string) string {
	if i := strings.Index(line, ": "); i >= 0 {
		return line[i+2:]
	}
	return line
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// newPrettyLogf returns a Log function that renders ADA's structured event lines
// as a colorized, indented, live-readable stream.
func newPrettyLogf(w io.Writer, color bool) func(string, ...any) {
	p := painter{color}
	return func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		ts := p.c("2", time.Now().Format("15:04:05"))
		emit := func(body string) { fmt.Fprintf(w, "%s %s\n", ts, body) }
		cont := func(body string) { fmt.Fprintf(w, "%9s %s\n", "", body) }
		kv := parseKV(line)
		id := kv["id"]

		switch {
		// ── planning / round level ──────────────────────────────────────────────
		case strings.Contains(line, " PLAN done="):
			fmt.Fprintln(w)
			if kv["done"] == "true" {
				emit(p.c("1;32", "✓ round "+kv["round"]) + "  " + p.c("2", "objective satisfied"))
			} else {
				emit(p.c("1;36", "▸ round "+kv["round"]) + "  " + p.c("2", kv["subgoals"]+" sub-goals"))
			}
			if r := kv["reason"]; r != "" {
				emit(p.c("2", "  ↳ "+clip(r, 200)))
			}
		case strings.Contains(line, "PLAN_FAILED"):
			emit(p.c("31", "✗ plan failed") + " " + p.c("2", kv["err"]))
		case strings.Contains(line, " START "):
			fmt.Fprintln(w)
			emit(p.c("1;36", "● goal "+kv["goal"]) + "  " + trailingQuoted(line))
		case goalOutcomeRe.MatchString(line):
			out := goalOutcomeRe.FindStringSubmatch(line)[1]
			emit("  " + p.c(outcomeColor(out), "└ goal "+kv["goal"]+" → "+out))
		case strings.Contains(line, "ROUND_BUDGET"):
			emit(p.c("33", "⊘ round budget exhausted"))
		case strings.HasPrefix(line, "round=") && strings.Contains(line, "STUCK"):
			emit("  " + p.c("33", "⊘ "+afterColon(line)))

		// ── step level ──────────────────────────────────────────────────────────
		case strings.Contains(line, "THINK_FAILED"):
			emit("    " + p.c("31", "✗ think") + " " + p.c("2", "bad output: "+clip(kv["err"], 120)))
		case strings.Contains(line, " THINK "):
			emit("    " + p.c("2", "· think   "+id+"  ["+kv["channel"]+"]"))
		case strings.Contains(line, " ACK "):
			emit("    " + p.c("32", "✓ ok") + "      " + id + "  " + p.c("2", "("+kv["strength"]+")"))
		case strings.Contains(line, " ANOMALY "):
			emit("    " + p.c("31", "✗ fail") + "    " + id + "  " + p.c("2", kv["class"]+", exit "+kv["exit"]))
			if e := kv["err"]; e != "" {
				cont(p.c("2", "↳ "+clip(e, 160)))
			}
		case strings.Contains(line, " NOPROGRESS "):
			emit("    " + p.c("2", "◦ no-op   "+id+"  ("+kv["dup"]+")"))
		case strings.Contains(line, "HARD_FORK"):
			emit("    " + p.c("35", "↻ fork") + "    " + p.c("2", "entropy "+kv["entropy"]))
		case strings.Contains(line, "OBJECTIVE_COMPLETE"):
			emit("    " + p.c("1;32", "✔ done") + "    " + p.c("2", "objective complete"))
		case strings.Contains(line, "JOB_DONE"):
			emit("    " + p.c("32", "✓ job") + "     " + p.c("2", clip(afterJob(line), 120)))
		case strings.Contains(line, " STABLE"):
			emit("    " + p.c("33", "⏹ stable") + "  " + p.c("2", "no new progress"))
		case strings.Contains(line, " STUCK"):
			emit("    " + p.c("33", "⊘ stuck") + "   " + p.c("2", "abandoning goal"))
		default:
			emit("    " + p.c("2", "· "+line))
		}
	}
}

func afterJob(line string) string {
	if i := strings.Index(line, "JOB_DONE "); i >= 0 {
		return line[i+len("JOB_DONE "):]
	}
	return line
}

func outcomeColor(outcome string) string {
	switch outcome {
	case string(ada.OutcomeFinished):
		return "1;32" // bold green
	case string(ada.OutcomeStable):
		return "33" // yellow
	default: // EXHAUSTED / FAILED
		return "31" // red
	}
}

// ── run summary rendering ─────────────────────────────────────────────────────

func printSummary(p painter, label string, outcome ada.Outcome, planReason string, s ada.StateSnapshot, lastErr string) {
	bar := p.c("2", "────────────────────────────────────────")
	fmt.Printf("\n%s\n%s  %s\n", bar, p.c("1", label), p.c(outcomeColor(string(outcome)), string(outcome)))
	if planReason != "" {
		fmt.Printf("%s %s\n", p.c("2", "planner:"), planReason)
	}
	fmt.Printf("%s %s\n", p.c("2", "objective:"), s.Objective)
	fmt.Printf("%s\n", p.c("2", fmt.Sprintf("verified facts (%d):", len(s.EstablishedFacts))))
	for _, f := range s.EstablishedFacts {
		mark := p.c("32", "✓")
		tag := p.c("32", "strong")
		if f.Strength != ada.StrengthStrong {
			mark = p.c("33", "•")
			tag = p.c("33", "weak")
		}
		fmt.Printf("  %s [%s] %s\n", mark, tag, f.Statement)
	}
	if lastErr != "" && outcome != ada.OutcomeFinished {
		fmt.Printf("%s %s\n", p.c("31", "last error:"), p.c("2", clip(lastErr, 240)))
	}
}
