package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// drive feeds a message into the model and returns it re-typed, failing on a View
// panic. It deliberately discards returned Cmds so no network/goroutine work runs.
func drive(t *testing.T, m *tuiModel, msg tea.Msg) *tuiModel {
	t.Helper()
	res, _ := m.Update(msg)
	tm, ok := res.(*tuiModel)
	if !ok {
		t.Fatalf("Update returned %T, want *tuiModel", res)
	}
	_ = tm.View() // must never panic
	return tm
}

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func typeText(t *testing.T, m *tuiModel, s string) *tuiModel {
	for _, r := range s {
		m = drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

// selectMenu moves the cursor to the menu item with the given id and presses enter.
func selectMenu(t *testing.T, m *tuiModel, id string) *tuiModel {
	t.Helper()
	for i, it := range m.menu.Items() {
		if mi, ok := it.(menuItem); ok && mi.id == id {
			m.menu.Select(i)
			return drive(t, m, key("enter"))
		}
	}
	t.Fatalf("menu item %q not found", id)
	return m
}

func newTestModel(t *testing.T) *tuiModel {
	t.Helper()
	t.Setenv("ADA_CONFIG", t.TempDir()+"/config.json")
	m := newTUIModel(DefaultConfig())
	return drive(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
}

// selectSettingsItem navigates to Settings then selects the sub-menu item with
// the given id.
func selectSettingsItem(t *testing.T, m *tuiModel, id string) *tuiModel {
	t.Helper()
	m = selectMenu(t, m, "settings")
	for i, it := range m.settingsMenu.Items() {
		if mi, ok := it.(menuItem); ok && mi.id == id {
			m.settingsMenu.Select(i)
			return drive(t, m, key("enter"))
		}
	}
	t.Fatalf("settings menu item %q not found", id)
	return m
}

func TestTUINavigatesAllScreens(t *testing.T) {
	// Top-level menu items that return directly to scMenu on esc.
	for _, id := range []string{"settings", "tools", "help"} {
		m := newTestModel(t)
		m = selectMenu(t, m, id)
		m = drive(t, m, key("esc"))
		if m.screen != scMenu {
			t.Fatalf("after esc from %q, screen=%v want menu", id, m.screen)
		}
	}
	// Settings sub-items: esc from env/models returns to scSettingsMenu.
	for _, id := range []string{"env", "models"} {
		m := newTestModel(t)
		m = selectSettingsItem(t, m, id)
		m = drive(t, m, key("esc"))
		if m.screen != scSettingsMenu {
			t.Fatalf("after esc from settings/%q, screen=%v want scSettingsMenu", id, m.screen)
		}
	}
}

func TestTUISettingsEdit(t *testing.T) {
	m := newTestModel(t)
	m = selectSettingsItem(t, m, "general")
	// Select the "model" row, edit it, type a value, save.
	for i, it := range m.settings.Items() {
		if kv, ok := it.(kvItem); ok && kv.key == "model" {
			m.settings.Select(i)
		}
	}
	m = drive(t, m, key("enter")) // begin editing
	if !m.editing {
		t.Fatal("expected editing mode")
	}
	// Clear and type a new value.
	m.editInput.SetValue("")
	m = typeText(t, m, "llama3.1:8b")
	m = drive(t, m, key("enter")) // save
	if m.editing {
		t.Fatal("still editing after save")
	}
	if m.cfg.Model != "llama3.1:8b" {
		t.Fatalf("model=%q want llama3.1:8b", m.cfg.Model)
	}
}

func TestTUIToolFormAddsTool(t *testing.T) {
	m := newTestModel(t)
	m = selectMenu(t, m, "tools")
	m = drive(t, m, key("a")) // open add form
	if m.form == nil {
		t.Fatal("expected a tool form")
	}
	m = typeText(t, m, "mytool")       // Name (focus starts on field 0)
	m = drive(t, m, key("tab"))        // → Description
	m = typeText(t, m, "does a thing") // Description
	m = drive(t, m, key("ctrl+s"))     // save
	if m.form != nil {
		t.Fatal("form should close after save")
	}
	if tl := m.cfg.FindTool("mytool"); tl == nil || tl.Description != "does a thing" {
		t.Fatalf("tool not saved correctly: %+v", tl)
	}
	// Toggle it off with space.
	m.tools.Select(0)
	m = drive(t, m, key("space"))
	if m.cfg.FindTool("mytool").Enabled {
		t.Fatal("space should have toggled the tool off")
	}
}

func TestTUIRunInputValidation(t *testing.T) {
	m := newTestModel(t)
	m = selectMenu(t, m, "run")
	if m.screen != scRun || m.runState != rsTypeSelect {
		t.Fatalf("expected run type-select screen, got screen=%v state=%v", m.screen, m.runState)
	}
	// Select "flat" run type to reach objective input.
	m = drive(t, m, key("enter"))
	if m.runState != rsInput {
		t.Fatalf("after selecting run type, expected rsInput, got state=%v", m.runState)
	}
	// Enter with an empty objective must not start a run (stays in input).
	m = drive(t, m, key("enter"))
	if m.runState != rsInput {
		t.Fatal("empty objective should not start a run")
	}
}

func TestTUISurvivesTinyTerminal(t *testing.T) {
	m := newTestModel(t)
	for _, sz := range []tea.WindowSizeMsg{{Width: 10, Height: 5}, {Width: 1, Height: 1}, {Width: 200, Height: 60}} {
		m = drive(t, m, sz)
		for _, id := range []string{"settings", "tools", "help"} {
			mm := selectMenu(t, m, id)
			if !strings.Contains(mm.View(), "ADA") {
				t.Fatalf("header missing at size %dx%d", sz.Width, sz.Height)
			}
			m = drive(t, mm, key("esc"))
		}
	}
}
