package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/chroma/v2/quick"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	ada "github.com/serainox420/assertion-driven-architecture/ada"
)

// Global render state, initialized once from the resolved color decision. Keeping
// it package-global lets every helper (and the TUI) share one theme + profile.
var (
	theme    = NewTheme()
	useColor = true
)

// initRender resolves the effective color mode against the given output stream and
// configures lipgloss + chroma accordingly. Call once at startup.
func initRender(mode string, out *os.File) {
	useColor = resolveColor(mode, out)
	if !useColor {
		lipgloss.SetColorProfile(termenv.Ascii)
	}
}

// resolveColor honors -color/Color (auto|always|never) and NO_COLOR.
func resolveColor(mode string, f *os.File) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	default:
		return isTTY(f) && os.Getenv("NO_COLOR") == ""
	}
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

// ── status helpers (the script-style ==>/ok/warn/fail lines) ──────────────────

func infof(format string, a ...any) {
	fmt.Fprintln(os.Stderr, theme.Crumb.Render("==>")+" "+fmt.Sprintf(format, a...))
}
func okf(format string, a ...any) {
	fmt.Fprintln(os.Stderr, theme.OK.Render(" ok ")+" "+fmt.Sprintf(format, a...))
}
func warnf(format string, a ...any) {
	fmt.Fprintln(os.Stderr, theme.Warning.Render("warn")+" "+fmt.Sprintf(format, a...))
}
func errf(format string, a ...any) {
	fmt.Fprintln(os.Stderr, theme.Bad.Render("fail")+" "+fmt.Sprintf(format, a...))
}

// banner renders the ADA wordmark plus a one-line subtitle.
func banner(subtitle string) string {
	var b strings.Builder
	b.WriteString(theme.Banner.Render(asciiLogo))
	if subtitle != "" {
		b.WriteString("\n  " + theme.Subtitle.Render(subtitle))
	}
	return b.String()
}

// ── markdown + JSON rendering ─────────────────────────────────────────────────

var (
	mdOnce sync.Once
	mdR    *glamour.TermRenderer
)

func markdownRenderer() *glamour.TermRenderer {
	mdOnce.Do(func() {
		style := "dark"
		if !useColor {
			style = "notty"
		}
		w := termWidth()
		if w > 100 {
			w = 100
		}
		mdR, _ = glamour.NewTermRenderer(
			glamour.WithStandardStyle(style),
			glamour.WithWordWrap(w),
		)
	})
	return mdR
}

// renderMarkdown styles a markdown string for the terminal (headings, lists, code
// blocks with syntax highlighting). Falls back to the raw text if glamour fails or
// color is off in a way that makes styling pointless.
func renderMarkdown(s string) string {
	r := markdownRenderer()
	if r == nil {
		return s
	}
	out, err := r.Render(s)
	if err != nil {
		return s
	}
	return strings.TrimRight(out, "\n")
}

// highlightJSON syntax-highlights a JSON blob with chroma. No-op when color is off.
func highlightJSON(s string) string {
	if !useColor {
		return s
	}
	var buf bytes.Buffer
	if err := quick.Highlight(&buf, s, "json", "terminal256", "monokai"); err != nil {
		return s
	}
	return strings.TrimRight(buf.String(), "\n")
}

// ── structured event log renderer ─────────────────────────────────────────────
//
// The Orchestrator/Coordinator emit one terse "key=value" line per event. We parse
// those lines and re-render them as an indented, colorized, live-readable stream —
// the same idea as the original cmd/ada pretty logger, widened to cover folding,
// memory, and recovery events.

var (
	kvRe          = regexp.MustCompile(`(\w+)=("(?:[^"\\]|\\.)*"|\S+)`)
	trailQuoteRe  = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"\s*$`)
	goalOutcomeRe = regexp.MustCompile(`goal=\d+ (FINISHED|STABLE|EXHAUSTED|FAILED)`)
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
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// renderEvent turns one structured log line into a pretty, indented string (no
// trailing newline). Exported shape is a pure function so the TUI can reuse it to
// build its scrollback.
func renderEvent(line string) string {
	kv := parseKV(line)
	id := kv["id"]
	ts := theme.Dim.Render(time.Now().Format("15:04:05"))
	dim := theme.Dim.Render
	row := func(body string) string { return ts + " " + body }
	cont := func(body string) string { return strings.Repeat(" ", 9) + body }

	switch {
	// ── planning / round level ────────────────────────────────────────────────
	case strings.Contains(line, " PLAN done="):
		var head string
		if kv["done"] == "true" {
			head = theme.OK.Render("✓ round "+kv["round"]) + "  " + dim("objective satisfied")
		} else {
			head = lipgloss.NewStyle().Bold(true).Foreground(theme.Accent).Render("▸ round "+kv["round"]) +
				"  " + dim(kv["subgoals"]+" sub-goals")
		}
		out := "\n" + row(head)
		if r := kv["reason"]; r != "" {
			out += "\n" + row(dim("  ↳ "+clip(r, 200)))
		}
		return out
	case strings.Contains(line, "PLAN_FAILED"):
		return row(theme.Bad.Render("✗ plan failed") + " " + dim(kv["err"]))
	case strings.Contains(line, " START "):
		return "\n" + row(lipgloss.NewStyle().Bold(true).Foreground(theme.Accent).Render("● goal "+kv["goal"])+"  "+trailingQuoted(line))
	case goalOutcomeRe.MatchString(line):
		out := goalOutcomeRe.FindStringSubmatch(line)[1]
		return row("  " + theme.outcomeStyle(out).Render("└ goal "+kv["goal"]+" → "+out))
	case strings.Contains(line, "ROUND_BUDGET"):
		return row(theme.Warning.Render("⊘ round budget exhausted"))

	// ── memory / lifecycle ────────────────────────────────────────────────────
	case strings.HasPrefix(line, "MEMORY_LOAD"):
		return row(theme.Key.Render("◆ memory") + "  " + dim("loaded "+kv["re-validated"]) + dim(afterWord(line, "MEMORY_LOAD")))
	case strings.HasPrefix(line, "MEMORY_SAVE"):
		return row(theme.Key.Render("◆ memory") + "  " + dim("saved"+afterWord(line, "MEMORY_SAVE")))
	case strings.HasPrefix(line, "MEMORY_DROP"), strings.Contains(line, "FORK_DROP"):
		return row("    " + dim("× drop    "+afterColonOrWord(line)))

	// ── step level ────────────────────────────────────────────────────────────
	case strings.Contains(line, "ABANDON"):
		return row("    " + theme.Bad.Render("✗ abandon") + " " + dim("model could not produce valid JSON"))
	case strings.Contains(line, "THINK_FAILED"):
		return row("    " + theme.Warning.Render("↻ retry") + "  " + dim("invalid JSON, re-prompting: "+clip(kv["err"], 100)))
	case strings.Contains(line, " THINK "):
		return row("    " + dim("· think   "+id+"  ["+kv["channel"]+"]"))
	case strings.Contains(line, " ACK "):
		strong := kv["strength"] == ada.StrengthStrong
		mark := theme.OK.Render("✓ ok")
		tag := theme.OK.Render("(" + kv["strength"] + ")")
		if !strong {
			tag = theme.Warning.Render("(" + kv["strength"] + ")")
		}
		return row("    " + mark + "      " + id + "  " + tag)
	case strings.Contains(line, " ANOMALY "):
		out := row("    " + theme.Bad.Render("✗ fail") + "    " + id + "  " + dim(kv["class"]+", exit "+kv["exit"]))
		if e := kv["err"]; e != "" {
			out += "\n" + row(cont(dim("↳ "+clip(e, 160))))
		}
		return out
	case strings.Contains(line, " NOPROGRESS "):
		return row("    " + dim("◦ no-op   "+id+"  ("+kv["dup"]+")"))
	case strings.Contains(line, "ATTEMPTS_EXHAUSTED"):
		return row("    " + theme.Warning.Render("⚑ exhausted") + " " + dim("forcing a new strategy"))
	case strings.Contains(line, "HARD_FORK"):
		return row("    " + lipgloss.NewStyle().Foreground(lipgloss.Color("171")).Render("↻ fork") + "    " + dim("entropy "+kv["entropy"]+", route "+kv["route"]))
	case strings.Contains(line, "OUT_OF_ROUTES"):
		return row("    " + theme.Bad.Render("⊘ no routes") + " " + dim("strategies exhausted"))
	case strings.Contains(line, " FOLD "):
		return row("    " + theme.Key.Render("⚙ fold") + "    " + dim("folded "+kv["folded"]+" facts ("+kv["strength"]+")"))
	case strings.Contains(line, "OBJECTIVE_COMPLETE"):
		return row("    " + theme.OK.Render("✔ done") + "    " + dim("objective complete"))
	case strings.Contains(line, "JOB_DONE"):
		return row("    " + theme.OK.Render("✓ job") + "     " + dim(clip(afterWord(line, "JOB_DONE"), 120)))
	case strings.Contains(line, " STABLE"):
		return row("    " + theme.Warning.Render("⏹ stable") + "  " + dim("no new progress"))
	case strings.Contains(line, " STUCK"):
		return row("    " + theme.Warning.Render("⊘ stuck") + "   " + dim("abandoning goal"))
	default:
		return row("    " + dim("· "+line))
	}
}

func afterWord(line, word string) string {
	if i := strings.Index(line, word); i >= 0 {
		return line[i+len(word):]
	}
	return ""
}
func afterColonOrWord(line string) string {
	if i := strings.Index(line, ": "); i >= 0 {
		return line[i+2:]
	}
	return strings.TrimSpace(line)
}

// newLogRenderer returns a Log function suitable for Orchestrator.Log /
// Coordinator.Log that streams pretty events to w.
func newLogRenderer(w io.Writer) func(string, ...any) {
	return func(format string, args ...any) {
		fmt.Fprintln(w, renderEvent(fmt.Sprintf(format, args...)))
	}
}

// ── run summary ───────────────────────────────────────────────────────────────

// renderSummary builds the end-of-run summary block (outcome, objective, verified
// facts with strength, last error). Returned as a string so the CLI prints it and
// the TUI embeds it in a viewport.
func renderSummary(label string, outcome ada.Outcome, planReason string, s ada.StateSnapshot, lastErr string) string {
	var b strings.Builder
	bar := theme.Dim.Render(strings.Repeat("─", 48))
	b.WriteString("\n" + bar + "\n")
	b.WriteString(theme.Title.Render(label) + "  " + theme.outcomeStyle(string(outcome)).Render(string(outcome)) + "\n")
	if planReason != "" {
		b.WriteString(theme.Dim.Render("planner: ") + planReason + "\n")
	}
	b.WriteString(theme.Dim.Render("objective: ") + s.Objective + "\n")
	b.WriteString(theme.Dim.Render(fmt.Sprintf("verified facts (%d):", len(s.EstablishedFacts))) + "\n")
	for _, f := range s.EstablishedFacts {
		mark := theme.OK.Render("✓")
		tag := theme.OK.Render("strong")
		if f.Strength != ada.StrengthStrong {
			mark = theme.Warning.Render("•")
			tag = theme.Warning.Render("weak")
		}
		b.WriteString(fmt.Sprintf("  %s [%s] %s\n", mark, tag, f.Statement))
	}
	if lastErr != "" && outcome != ada.OutcomeFinished {
		b.WriteString(theme.Bad.Render("last error: ") + theme.Dim.Render(clip(lastErr, 240)) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderBenchmarkSummary builds the end-of-suite scorecard: per-task outcomes
// and the aggregate pass count, plus where the per-task reports landed.
func renderBenchmarkSummary(res BenchmarkResult) string {
	var b strings.Builder
	bar := theme.Dim.Render(strings.Repeat("─", 48))
	b.WriteString("\n" + bar + "\n")
	b.WriteString(theme.Title.Render("BENCHMARK COMPLETE") + "  " +
		theme.Key.Render(fmt.Sprintf("%d/%d passed", res.Passed, res.Total)) + "\n")
	b.WriteString(theme.Dim.Render("suite: ") + res.Name + "\n")
	for _, t := range res.Tasks {
		mark := theme.OK.Render("✓")
		if !benchmarkPass(t.Outcome) {
			mark = theme.Bad.Render("✗")
		}
		b.WriteString(fmt.Sprintf("  %s %s %s\n", mark,
			padRight(t.Name, 24), theme.outcomeStyle(string(t.Outcome)).Render(string(t.Outcome))))
	}
	if res.LogDir != "" {
		b.WriteString(theme.Dim.Render("reports: ") + res.LogDir + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// outcomeHint returns the human "what just happened / what to try" note printed
// after a non-FINISHED run, matching the original CLI's guidance.
func outcomeHint(outcome ada.Outcome) string {
	switch outcome {
	case ada.OutcomeStable:
		return "note: the agent reached a stable state — it kept re-verifying facts it had\n" +
			"      already established without making new progress, so the loop stopped.\n" +
			"      The objective is likely complete; the model just never set \"final\": true."
	case ada.OutcomeExhausted:
		return "hint: the run hit the step budget without finishing or stabilizing. The agent\n" +
			"      ends on \"final\": true, an external check, or no progress (-stall). Adjust\n" +
			"      max-steps / stall / goal-steps, or refine the objective."
	case ada.OutcomeFailed:
		return "note: the agent exhausted its bounded alternative strategies (max-routes ×\n" +
			"      max-attempts) without proving the objective. The last error above is the\n" +
			"      likeliest blocker; refine the objective or raise the recovery budget."
	default:
		return ""
	}
}

// termWidth returns the terminal width (cols), defaulting to 80 when unknown.
func termWidth() int {
	if w, _, err := term_GetSize(); err == nil && w > 0 {
		return w
	}
	return 80
}
