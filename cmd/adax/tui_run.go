package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
)

// benchItem is a benchmark suite shown in the run › benchmark picker.
type benchItem struct{ b Benchmark }

func (i benchItem) Title() string { return i.b.Name }
func (i benchItem) Description() string {
	mode := "plan"
	if strings.EqualFold(strings.TrimSpace(i.b.Mode), "flat") {
		mode = "flat"
	}
	return theme.Val.Render(fmt.Sprintf("%d tasks · %s", len(i.b.Tasks), mode)) +
		theme.Dim.Render("  — "+i.b.Description)
}
func (i benchItem) FilterValue() string { return i.b.Name }

// rebuildBenchmarks reloads the benchmark suites from the benchmarks/ folder.
func (m *tuiModel) rebuildBenchmarks() {
	suites, _ := ListBenchmarks(benchmarksDir())
	items := make([]list.Item, 0, len(suites))
	for _, b := range suites {
		items = append(items, benchItem{b})
	}
	m.benchList.SetItems(items)
}

// startBenchmark drives a whole suite in the background, streaming each task's
// events live and finishing with a benchDoneMsg scorecard.
func (m *tuiModel) startBenchmark(b Benchmark) tea.Cmd {
	m.runState = rsActive
	m.runResult = nil
	m.benchResult = nil
	m.runLog = m.runLog[:0]
	m.runVP.SetContent("")

	ch := make(chan tea.Msg, 256)
	m.events = ch
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelRun = cancel

	cfg, client := m.cfg, m.client
	go func() {
		logf := func(format string, args ...any) {
			ch <- logLineMsg(renderEvent(fmt.Sprintf(format, args...)))
		}
		res := runBenchmark(ctx, cfg, client, b, logf)
		ch <- benchDoneMsg(res)
	}()

	return tea.Batch(m.spin.Tick, waitForEvent(ch))
}

// startRun spins up a background goroutine that drives the objective and streams
// every structured event back over m.events as a tea.Msg, so the loop renders live.
func (m *tuiModel) startRun(objective string) tea.Cmd {
	m.runState = rsActive
	m.runResult = nil
	m.runLog = m.runLog[:0]
	m.runVP.SetContent("")

	ch := make(chan tea.Msg, 256)
	m.events = ch
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelRun = cancel

	cfg, client, mode := m.cfg, m.client, m.runMode
	go func() {
		logf := func(format string, args ...any) {
			ch <- logLineMsg(renderEvent(fmt.Sprintf(format, args...)))
		}
		res := executeRun(ctx, cfg, client, objective, mode == "plan", logf)
		ch <- runDoneMsg(res)
	}()

	return tea.Batch(m.spin.Tick, waitForEvent(ch))
}

func (m *tuiModel) updateRun(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case logLineMsg:
		m.runLog = append(m.runLog, string(msg))
		m.runVP.SetContent(strings.Join(m.runLog, "\n"))
		m.runVP.GotoBottom()
		return m, waitForEvent(m.events) // keep draining the stream

	case runDoneMsg:
		res := RunResult(msg)
		m.runResult = &res
		m.runState = rsDone
		m.cancelRun = nil
		if res.Err != nil {
			m.runLog = append(m.runLog, theme.Bad.Render("error: ")+res.Err.Error())
		} else {
			label := "RUN COMPLETE"
			if m.runMode == "plan" {
				label = "PLAN RUN COMPLETE"
			}
			m.runLog = append(m.runLog, "", renderSummary(label, res.Outcome, res.PlanReason, res.Snapshot, res.LastError))
		}
		m.runVP.SetContent(strings.Join(m.runLog, "\n"))
		m.runVP.GotoBottom()
		return m, nil

	case benchDoneMsg:
		res := BenchmarkResult(msg)
		m.benchResult = &res
		m.runState = rsDone
		m.cancelRun = nil
		m.runLog = append(m.runLog, "", renderBenchmarkSummary(res))
		m.runVP.SetContent(strings.Join(m.runLog, "\n"))
		m.runVP.GotoBottom()
		return m, nil

	case tea.KeyMsg:
		switch m.runState {
		case rsTypeSelect:
			switch msg.String() {
			case "esc":
				m.screen = scMenu
				return m, nil
			case "enter":
				if it, ok := m.runTypeList.SelectedItem().(menuItem); ok {
					m.runMode = it.id
					if it.id == "bench" {
						m.rebuildBenchmarks()
						m.runState = rsBenchSelect
						return m, nil
					}
					m.runState = rsInput
					m.runInput.SetValue("")
					return m, m.runInput.Focus()
				}
			}
			var cmd tea.Cmd
			m.runTypeList, cmd = m.runTypeList.Update(msg)
			return m, cmd

		case rsBenchSelect:
			switch msg.String() {
			case "esc":
				m.runState = rsTypeSelect
				return m, nil
			case "enter":
				if it, ok := m.benchList.SelectedItem().(benchItem); ok {
					return m, m.startBenchmark(it.b)
				}
				return m, m.setFlash(theme.Warning.Render("no benchmark configs in benchmarks/"))
			}
			var cmd tea.Cmd
			m.benchList, cmd = m.benchList.Update(msg)
			return m, cmd

		case rsInput:
			switch msg.String() {
			case "esc":
				m.runState = rsTypeSelect
				return m, nil
			case "enter":
				obj := strings.TrimSpace(m.runInput.Value())
				if obj == "" {
					return m, m.setFlash(theme.Warning.Render("enter an objective first"))
				}
				m.runInput.Blur()
				return m, m.startRun(obj)
			}
			var cmd tea.Cmd
			m.runInput, cmd = m.runInput.Update(msg)
			return m, cmd

		case rsActive:
			if msg.String() == "esc" {
				if m.cancelRun != nil {
					m.cancelRun()
				}
				return m, m.setFlash(theme.Warning.Render("stopping…"))
			}
			var cmd tea.Cmd
			m.runVP, cmd = m.runVP.Update(msg) // allow scroll-back mid-run
			return m, cmd

		case rsDone:
			switch msg.String() {
			case "esc", "q":
				m.screen = scMenu
				return m, nil
			case "n":
				m.runState = rsTypeSelect
				return m, nil
			}
			var cmd tea.Cmd
			m.runVP, cmd = m.runVP.Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

func (m *tuiModel) viewRun() (string, string) {
	switch m.runState {
	case rsTypeSelect:
		return m.runTypeList.View(), "↑/↓ move · enter select · esc back"

	case rsBenchSelect:
		if len(m.benchList.Items()) == 0 {
			title := theme.Title.Render("Benchmarks")
			hint := theme.Dim.Render("No benchmark configs found in " + benchmarksDir() + ".\n" +
				"Add a *.json suite (see benchmarks/planner-basics.json) and come back.")
			return title + "\n\n" + hint, "esc back"
		}
		title := theme.Title.Render("Benchmarks") + theme.Dim.Render("  ·  "+benchmarksDir())
		return title + "\n\n" + m.benchList.View(), "↑/↓ move · enter run · esc back"

	case rsInput:
		mode := "flat loop"
		if m.runMode == "plan" {
			mode = "planning mode"
		}
		title := theme.Title.Render("New run") + theme.Dim.Render("  ·  "+mode+"  ·  model "+m.cfg.Model)
		box := theme.FocusBox.Width(maxi(m.width-4, 20)).Render(m.runInput.View())
		hint := theme.Dim.Render("Describe a goal the agent can verify by reading real state\n" +
			"(a file, a process, a service). The runtime checks every claim.")
		body := title + "\n\n" + box + "\n\n" + hint
		return body, "enter run · esc back"

	default:
		head := theme.Title.Render("Run") + theme.Dim.Render("  ·  "+m.cfg.Model)
		if m.runState == rsActive {
			label := "running"
			if m.runMode == "bench" {
				label = "benchmarking"
			}
			head = m.spin.View() + " " + theme.Title.Render(label) + theme.Dim.Render("  ·  "+m.cfg.Model+"  ·  esc to stop")
		} else if m.benchResult != nil {
			head = theme.Title.Render(fmt.Sprintf("● %d/%d passed", m.benchResult.Passed, m.benchResult.Total)) +
				theme.Dim.Render("  ·  "+m.cfg.Model)
		} else if m.runResult != nil && m.runResult.Err == nil {
			head = theme.outcomeStyle(string(m.runResult.Outcome)).Render("● "+string(m.runResult.Outcome)) +
				theme.Dim.Render("  ·  "+m.cfg.Model)
		}
		footer := "↑/↓ scroll · esc stop"
		if m.runState == rsDone {
			footer = "↑/↓ scroll · n new run · esc back"
		}
		return head + "\n" + m.runVP.View(), footer
	}
}
