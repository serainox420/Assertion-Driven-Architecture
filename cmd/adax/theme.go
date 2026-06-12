package main

import "github.com/charmbracelet/lipgloss"

// Theme is the single source of truth for color. Both the streaming CLI logger and
// the full-screen TUI draw from it, so the tool looks like one product everywhere.
type Theme struct {
	Primary  lipgloss.Color // brand accent (violet)
	Accent   lipgloss.Color // secondary accent (cyan)
	Success  lipgloss.Color
	Warn     lipgloss.Color
	Error    lipgloss.Color
	Muted    lipgloss.Color
	Subtle   lipgloss.Color
	Fg       lipgloss.Color
	StrongFg lipgloss.Color

	// Reusable styles.
	Title    lipgloss.Style
	Subtitle lipgloss.Style
	Banner   lipgloss.Style
	Key      lipgloss.Style
	Val      lipgloss.Style
	OK       lipgloss.Style
	Bad      lipgloss.Style
	Warning  lipgloss.Style
	Dim      lipgloss.Style
	Pill     lipgloss.Style
	SelPill  lipgloss.Style
	Box      lipgloss.Style
	FocusBox lipgloss.Style
	Help     lipgloss.Style
	Crumb    lipgloss.Style
}

// NewTheme builds the palette. When color is false (a pipe, NO_COLOR, -color never)
// lipgloss still renders, but we force the no-color profile at the renderer level so
// every style degrades to plain text automatically.
func NewTheme() *Theme {
	t := &Theme{
		Primary:  lipgloss.Color("99"),  // violet
		Accent:   lipgloss.Color("44"),  // cyan
		Success:  lipgloss.Color("42"),  // green
		Warn:     lipgloss.Color("214"), // amber
		Error:    lipgloss.Color("203"), // red
		Muted:    lipgloss.Color("244"), // gray
		Subtle:   lipgloss.Color("240"), // dark gray
		Fg:       lipgloss.Color("252"),
		StrongFg: lipgloss.Color("231"),
	}
	t.Title = lipgloss.NewStyle().Bold(true).Foreground(t.StrongFg)
	t.Subtitle = lipgloss.NewStyle().Foreground(t.Muted)
	t.Banner = lipgloss.NewStyle().Bold(true).Foreground(t.Primary)
	t.Key = lipgloss.NewStyle().Foreground(t.Accent)
	t.Val = lipgloss.NewStyle().Foreground(t.Fg)
	t.OK = lipgloss.NewStyle().Bold(true).Foreground(t.Success)
	t.Bad = lipgloss.NewStyle().Bold(true).Foreground(t.Error)
	t.Warning = lipgloss.NewStyle().Bold(true).Foreground(t.Warn)
	t.Dim = lipgloss.NewStyle().Foreground(t.Subtle)
	t.Pill = lipgloss.NewStyle().Foreground(t.StrongFg).Background(t.Subtle).Padding(0, 1)
	t.SelPill = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("16")).Background(t.Primary).Padding(0, 1)
	t.Box = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(t.Subtle).Padding(0, 1)
	t.FocusBox = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(t.Primary).Padding(0, 1)
	t.Help = lipgloss.NewStyle().Foreground(t.Muted)
	t.Crumb = lipgloss.NewStyle().Foreground(t.Primary).Bold(true)
	return t
}

// outcomeStyle maps an ADA terminal outcome to a color: FINISHED green, STABLE
// amber, EXHAUSTED/FAILED red.
func (t *Theme) outcomeStyle(outcome string) lipgloss.Style {
	switch outcome {
	case "FINISHED":
		return t.OK
	case "STABLE":
		return t.Warning
	default:
		return t.Bad
	}
}

// asciiLogo is the wordmark shown at the top of the TUI and `ada` with no args.
const asciiLogo = `   █████╗ ██████╗  █████╗
  ██╔══██╗██╔══██╗██╔══██╗
  ███████║██║  ██║███████║
  ██╔══██║██║  ██║██╔══██║
  ██║  ██║██████╔╝██║  ██║
  ╚═╝  ╚═╝╚═════╝ ╚═╝  ╚═╝`
