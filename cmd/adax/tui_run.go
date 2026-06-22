package main

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

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
					m.runState = rsInput
					m.runInput.SetValue("")
					return m, m.runInput.Focus()
				}
			}
			var cmd tea.Cmd
			m.runTypeList, cmd = m.runTypeList.Update(msg)
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
			head = m.spin.View() + " " + theme.Title.Render("running") + theme.Dim.Render("  ·  "+m.cfg.Model+"  ·  esc to stop")
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
