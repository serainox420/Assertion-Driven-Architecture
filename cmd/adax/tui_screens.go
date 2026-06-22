package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// ── settings ──────────────────────────────────────────────────────────────────

type kvItem struct{ key, val, desc string }

func (i kvItem) Title() string { return i.key }
func (i kvItem) Description() string {
	return theme.Val.Render(i.val) + theme.Dim.Render("  — "+i.desc)
}
func (i kvItem) FilterValue() string { return i.key }

func (m *tuiModel) rebuildSettings() {
	var items []list.Item
	for _, f := range m.cfg.Fields() {
		items = append(items, kvItem{f.Key, f.Value, f.Desc})
	}
	m.settings.SetItems(items)
}

func (m *tuiModel) updateSettings(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.editing {
		if k, ok := msg.(tea.KeyMsg); ok {
			switch k.String() {
			case "esc":
				m.editing = false
				return m, nil
			case "enter":
				err := m.cfg.SetField(m.editKey, strings.TrimSpace(m.editInput.Value()))
				if err != nil {
					return m, m.setFlash(theme.Bad.Render("✗ " + err.Error()))
				}
				if serr := m.cfg.Save(); serr != nil {
					return m, m.setFlash(theme.Bad.Render("✗ " + serr.Error()))
				}
				m.editing = false
				m.rebuildSettings()
				// A color/host change can affect rendering or connectivity.
				m.cfg.normalize()
				return m, tea.Batch(m.setFlash(theme.OK.Render("✓ saved "+m.editKey)), m.cmdStatus())
			}
		}
		var cmd tea.Cmd
		m.editInput, cmd = m.editInput.Update(msg)
		return m, cmd
	}

	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "esc":
			m.screen = scSettingsMenu
			return m, nil
		case "enter":
			if it, ok := m.settings.SelectedItem().(kvItem); ok {
				m.editing, m.editKey = true, it.key
				m.editInput.SetValue(it.val)
				m.editInput.CursorEnd()
				return m, m.editInput.Focus()
			}
		}
	}
	var cmd tea.Cmd
	m.settings, cmd = m.settings.Update(msg)
	return m, cmd
}

func (m *tuiModel) viewSettings() (string, string) {
	if m.editing {
		title := theme.Title.Render("Edit ") + theme.Key.Render(m.editKey)
		box := theme.FocusBox.Width(maxi(m.width-4, 20)).Render(m.editInput.View())
		hint := theme.Dim.Render("Type a new value. Booleans: true/false. Color: auto|always|never.")
		return title + "\n\n" + box + "\n\n" + hint, "enter save · esc cancel"
	}
	return m.settings.View(), "↑/↓ move · enter edit · esc back"
}

func (m *tuiModel) rebuildDebugSettings() {
	var items []list.Item
	for _, f := range m.cfg.DebugFields() {
		items = append(items, kvItem{f.Key, f.Value, f.Desc})
	}
	m.debugSettings.SetItems(items)
}

func (m *tuiModel) updateSettingsMenu(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "esc":
			m.screen = scMenu
			return m, nil
		case "enter":
			if it, ok := m.settingsMenu.SelectedItem().(menuItem); ok {
				return m.dispatchSettingsMenu(it.id)
			}
		}
	}
	var cmd tea.Cmd
	m.settingsMenu, cmd = m.settingsMenu.Update(msg)
	return m, cmd
}

func (m *tuiModel) viewSettingsMenu() (string, string) {
	return m.settingsMenu.View(), "↑/↓ move · enter select · esc back"
}

func (m *tuiModel) updateDebugSettings(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.editing {
		if k, ok := msg.(tea.KeyMsg); ok {
			switch k.String() {
			case "esc":
				m.editing = false
				return m, nil
			case "enter":
				err := m.cfg.SetField(m.editKey, strings.TrimSpace(m.editInput.Value()))
				if err != nil {
					return m, m.setFlash(theme.Bad.Render("✗ " + err.Error()))
				}
				if serr := m.cfg.Save(); serr != nil {
					return m, m.setFlash(theme.Bad.Render("✗ " + serr.Error()))
				}
				m.editing = false
				m.rebuildDebugSettings()
				return m, m.setFlash(theme.OK.Render("✓ saved " + m.editKey))
			}
		}
		var cmd tea.Cmd
		m.editInput, cmd = m.editInput.Update(msg)
		return m, cmd
	}

	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "esc":
			m.screen = scSettingsMenu
			return m, nil
		case "enter":
			if it, ok := m.debugSettings.SelectedItem().(kvItem); ok {
				m.editing, m.editKey = true, it.key
				m.editInput.SetValue(it.val)
				m.editInput.CursorEnd()
				return m, m.editInput.Focus()
			}
		}
	}
	var cmd tea.Cmd
	m.debugSettings, cmd = m.debugSettings.Update(msg)
	return m, cmd
}

func (m *tuiModel) viewDebugSettings() (string, string) {
	if m.editing {
		title := theme.Title.Render("Edit ") + theme.Key.Render(m.editKey)
		box := theme.FocusBox.Width(maxi(m.width-4, 20)).Render(m.editInput.View())
		hint := theme.Dim.Render("Booleans: true/false. Leave debug_dir blank to use the default.")
		return title + "\n\n" + box + "\n\n" + hint, "enter save · esc cancel"
	}
	return m.debugSettings.View(), "↑/↓ move · enter edit · esc back"
}

// ── models ────────────────────────────────────────────────────────────────────

type modelItem struct {
	name, meta string
	active     bool
}

func (i modelItem) Title() string {
	if i.active {
		return theme.OK.Render("▶ ") + i.name
	}
	return i.name
}
func (i modelItem) Description() string { return theme.Dim.Render(i.meta) }
func (i modelItem) FilterValue() string { return i.name }

func (m *tuiModel) setModelItems(ms []ModelInfo) {
	sort.Slice(ms, func(a, b int) bool { return ms[a].ModifiedAt.After(ms[b].ModifiedAt) })
	var items []list.Item
	for _, mi := range ms {
		meta := strings.TrimSpace(humanBytes(mi.Size) + "  " + mi.Details.ParameterSize + " " + mi.Details.QuantizationLevel)
		items = append(items, modelItem{name: mi.Name, meta: meta, active: mi.Name == m.cfg.Model})
	}
	m.models.SetItems(items)
}

func (m *tuiModel) cmdDeleteModel(name string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.DeleteModel(ctx, name); err != nil {
			return errMsg(err.Error())
		}
		ms, err := client.ListModels(ctx)
		if err != nil {
			return errMsg(err.Error())
		}
		return modelsLoadedMsg(ms)
	}
}

func (m *tuiModel) startPull(name string) tea.Cmd {
	m.pulling, m.pullName, m.pullStatus = true, name, "starting…"
	ch := make(chan tea.Msg, 64)
	m.pullCh = ch
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelPull = cancel
	client := m.client
	go func() {
		err := client.Pull(ctx, name, func(p PullProgress) { ch <- pullEventMsg(p) })
		ch <- pullDoneMsg{name, err}
	}()
	return tea.Batch(m.spin.Tick, waitForEvent(ch))
}

func (m *tuiModel) updateModels(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case modelsLoadedMsg:
		m.loadingMdl = false
		m.setModelItems([]ModelInfo(msg))
		return m, nil
	case pullEventMsg:
		p := PullProgress(msg)
		if p.Total > 0 {
			pct := float64(p.Completed) / float64(p.Total) * 100
			m.pullStatus = fmt.Sprintf("%s  %.0f%%  %s/%s", p.Status, pct, humanBytes(p.Completed), humanBytes(p.Total))
		} else {
			m.pullStatus = p.Status
		}
		return m, waitForEvent(m.pullCh)
	case pullDoneMsg:
		m.pulling = false
		m.cancelPull = nil
		if msg.err != nil {
			return m, m.setFlash(theme.Bad.Render("✗ pull failed: " + msg.err.Error()))
		}
		m.loadingMdl = true
		return m, tea.Batch(m.setFlash(theme.OK.Render("✓ pulled "+msg.name)), m.cmdLoadModels(), m.spin.Tick)
	}

	// Pull-tag entry box.
	if m.addingPull {
		if k, ok := msg.(tea.KeyMsg); ok {
			switch k.String() {
			case "esc":
				m.addingPull = false
				return m, nil
			case "enter":
				name := strings.TrimSpace(m.pullInput.Value())
				if name == "" {
					return m, m.setFlash(theme.Warning.Render("enter a model tag"))
				}
				m.addingPull = false
				m.pullInput.Blur()
				return m, m.startPull(name)
			}
		}
		var cmd tea.Cmd
		m.pullInput, cmd = m.pullInput.Update(msg)
		return m, cmd
	}

	if k, ok := msg.(tea.KeyMsg); ok {
		if m.pulling {
			if k.String() == "esc" && m.cancelPull != nil {
				m.cancelPull()
				return m, m.setFlash(theme.Warning.Render("cancelling pull…"))
			}
			return m, nil
		}
		switch k.String() {
		case "esc":
			m.screen = m.prevScreen
			return m, nil
		case "p":
			m.addingPull = true
			m.pullInput.SetValue("")
			return m, m.pullInput.Focus()
		case "u", "enter":
			if it, ok := m.models.SelectedItem().(modelItem); ok {
				m.cfg.Model = it.name
				_ = m.cfg.Save()
				m.setModelItems(m.modelsSnapshot())
				return m, m.setFlash(theme.OK.Render("✓ active model: " + it.name))
			}
		case "d":
			if it, ok := m.models.SelectedItem().(modelItem); ok {
				return m, m.cmdDeleteModel(it.name)
			}
		case "r":
			m.loadingMdl = true
			return m, tea.Batch(m.cmdLoadModels(), m.spin.Tick)
		}
	}
	var cmd tea.Cmd
	m.models, cmd = m.models.Update(msg)
	return m, cmd
}

// modelsSnapshot reconstructs ModelInfo from the current list items so we can
// re-mark the active row without a network round-trip.
func (m *tuiModel) modelsSnapshot() []ModelInfo {
	var out []ModelInfo
	for _, it := range m.models.Items() {
		if mi, ok := it.(modelItem); ok {
			out = append(out, ModelInfo{Name: mi.name})
		}
	}
	return out
}

func (m *tuiModel) viewModels() (string, string) {
	if m.addingPull {
		title := theme.Title.Render("Pull a model") + theme.Dim.Render("  (e.g. qwen2.5-coder:14b, llama3.1:8b)")
		box := theme.FocusBox.Width(maxi(m.width-4, 20)).Render(m.pullInput.View())
		return title + "\n\n" + box, "enter pull · esc cancel"
	}
	if m.pulling {
		body := m.spin.View() + " " + theme.Title.Render("pulling ") + theme.Key.Render(m.pullName) +
			"\n\n  " + theme.Dim.Render(m.pullStatus)
		return body, "esc cancel"
	}
	if m.loadingMdl {
		return m.spin.View() + " " + theme.Dim.Render("loading models from "+m.cfg.OllamaHost), "esc back"
	}
	if len(m.models.Items()) == 0 {
		return theme.Dim.Render("No local models (or server offline at " + m.cfg.OllamaHost + ").\nPress p to pull one."),
			"p pull · r refresh · esc back"
	}
	return m.models.View(), "↑/↓ move · u use · p pull · d delete · r refresh · esc back"
}

// ── tools ─────────────────────────────────────────────────────────────────────

type toolItem struct {
	name, desc string
	on         bool
}

func (i toolItem) Title() string {
	if i.on {
		return theme.OK.Render("● ") + i.name
	}
	return theme.Dim.Render("○ ") + i.name
}
func (i toolItem) Description() string { return theme.Dim.Render(clip(i.desc, 70)) }
func (i toolItem) FilterValue() string { return i.name }

func (m *tuiModel) rebuildTools() {
	var items []list.Item
	for _, t := range m.cfg.Tools {
		items = append(items, toolItem{name: t.Name, desc: t.Description, on: t.Enabled})
	}
	m.tools.SetItems(items)
}

func (m *tuiModel) updateTools(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.form != nil {
		return m.updateToolForm(msg)
	}
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "esc":
			m.screen = scMenu
			return m, nil
		case "a":
			m.form = newToolForm(nil, -1)
			return m, m.form.focusCmd()
		case "e", "enter":
			if it, ok := m.tools.SelectedItem().(toolItem); ok {
				if t := m.cfg.FindTool(it.name); t != nil {
					m.form = newToolForm(t, m.toolIndex(it.name))
					return m, m.form.focusCmd()
				}
			}
		case " ", "t":
			if it, ok := m.tools.SelectedItem().(toolItem); ok {
				if t := m.cfg.FindTool(it.name); t != nil {
					t.Enabled = !t.Enabled
					_ = m.cfg.Save()
					m.rebuildTools()
					return m, m.setFlash(theme.OK.Render("✓ " + it.name + " " + onOff(t.Enabled)))
				}
			}
		case "d":
			if it, ok := m.tools.SelectedItem().(toolItem); ok {
				if idx := m.toolIndex(it.name); idx >= 0 {
					m.cfg.Tools = append(m.cfg.Tools[:idx], m.cfg.Tools[idx+1:]...)
					_ = m.cfg.Save()
					m.rebuildTools()
					return m, m.setFlash(theme.OK.Render("✓ removed " + it.name))
				}
			}
		}
	}
	var cmd tea.Cmd
	m.tools, cmd = m.tools.Update(msg)
	return m, cmd
}

func (m *tuiModel) toolIndex(name string) int {
	for i := range m.cfg.Tools {
		if strings.EqualFold(m.cfg.Tools[i].Name, name) {
			return i
		}
	}
	return -1
}

func (m *tuiModel) viewTools() (string, string) {
	if m.form != nil {
		return m.form.view(m.width), "tab/↑↓ field · ctrl+s save · esc cancel"
	}
	if len(m.cfg.Tools) == 0 {
		return theme.Dim.Render("No tools yet. Enabled tools are injected into the model's system prompt\n" +
			"as a curated TOOLBOX. Press a to add one."), "a add · esc back"
	}
	return m.tools.View(), "space toggle · a add · e edit · d delete · esc back"
}

func onOff(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

// ── tool editor form ──────────────────────────────────────────────────────────

type toolForm struct {
	inputs  []textinput.Model
	labels  []string
	focus   int
	editIdx int // -1 ⇒ adding a new tool
}

func newToolForm(t *Tool, idx int) *toolForm {
	labels := []string{"Name", "Description", "Command", "Channel (fs/process/service/exit_code)", "Assertion pattern"}
	ph := []string{"install-pkg", "install a package non-interactively", "pacman -S --noconfirm <pkg>", "exit_code", "0"}
	f := &toolForm{labels: labels, editIdx: idx}
	for i := range labels {
		in := textinput.New()
		in.Placeholder = ph[i]
		in.Width = 56
		f.inputs = append(f.inputs, in)
	}
	if t != nil {
		f.inputs[0].SetValue(t.Name)
		f.inputs[1].SetValue(t.Description)
		f.inputs[2].SetValue(t.Command)
		f.inputs[3].SetValue(t.Channel)
		f.inputs[4].SetValue(t.Assertion)
	}
	return f
}

func (f *toolForm) focusCmd() tea.Cmd {
	for i := range f.inputs {
		f.inputs[i].Blur()
	}
	f.inputs[f.focus].Focus()
	return textinput.Blink
}

func (f *toolForm) move(delta int) tea.Cmd {
	f.inputs[f.focus].Blur()
	f.focus = (f.focus + delta + len(f.inputs)) % len(f.inputs)
	return f.inputs[f.focus].Focus()
}

func (f *toolForm) toTool(prev *Tool) Tool {
	t := Tool{
		Name:        strings.TrimSpace(f.inputs[0].Value()),
		Description: strings.TrimSpace(f.inputs[1].Value()),
		Command:     strings.TrimSpace(f.inputs[2].Value()),
		Channel:     strings.TrimSpace(f.inputs[3].Value()),
		Assertion:   strings.TrimSpace(f.inputs[4].Value()),
		Enabled:     true,
	}
	if prev != nil {
		t.Enabled = prev.Enabled
	}
	return t
}

func (f *toolForm) view(width int) string {
	var b strings.Builder
	verb := "Add tool"
	if f.editIdx >= 0 {
		verb = "Edit tool"
	}
	b.WriteString(theme.Title.Render(verb) + "\n\n")
	for i := range f.inputs {
		label := theme.Dim.Render(f.labels[i])
		if i == f.focus {
			label = theme.Key.Render("▸ " + f.labels[i])
		}
		b.WriteString("  " + label + "\n  " + f.inputs[i].View() + "\n\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *tuiModel) updateToolForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "esc":
			m.form = nil
			return m, nil
		case "ctrl+s":
			return m.saveToolForm()
		case "tab", "down":
			return m, m.form.move(1)
		case "shift+tab", "up":
			return m, m.form.move(-1)
		case "enter":
			// Enter advances; on the last field it saves.
			if m.form.focus == len(m.form.inputs)-1 {
				return m.saveToolForm()
			}
			return m, m.form.move(1)
		}
	}
	var cmd tea.Cmd
	m.form.inputs[m.form.focus], cmd = m.form.inputs[m.form.focus].Update(msg)
	return m, cmd
}

func (m *tuiModel) saveToolForm() (tea.Model, tea.Cmd) {
	f := m.form
	if strings.TrimSpace(f.inputs[0].Value()) == "" {
		return m, m.setFlash(theme.Warning.Render("a tool needs a name"))
	}
	if f.editIdx >= 0 && f.editIdx < len(m.cfg.Tools) {
		prev := m.cfg.Tools[f.editIdx]
		m.cfg.Tools[f.editIdx] = f.toTool(&prev)
	} else {
		t := f.toTool(nil)
		if m.cfg.FindTool(t.Name) != nil {
			return m, m.setFlash(theme.Warning.Render("a tool named " + t.Name + " already exists"))
		}
		m.cfg.Tools = append(m.cfg.Tools, t)
	}
	if err := m.cfg.Save(); err != nil {
		return m, m.setFlash(theme.Bad.Render("✗ " + err.Error()))
	}
	name := f.inputs[0].Value()
	m.form = nil
	m.rebuildTools()
	return m, m.setFlash(theme.OK.Render("✓ saved tool " + name))
}
