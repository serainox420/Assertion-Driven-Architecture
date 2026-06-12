package main

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// runTUI launches the full-screen interactive interface. Without a terminal (piped
// or redirected) it prints help instead, so `ada | cat` never hangs on input.
func runTUI(cfg *Config) error {
	if !isTTY(os.Stdout) || !isTTY(os.Stdin) {
		printHelp()
		return nil
	}
	p := tea.NewProgram(newTUIModel(cfg), tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// ── screens ───────────────────────────────────────────────────────────────────

type screen int

const (
	scMenu screen = iota
	scRun
	scSettings
	scModels
	scTools
	scEnv
	scHelp
)

func (s screen) crumb() string {
	return map[screen]string{
		scMenu: "Home", scRun: "Run", scSettings: "Settings",
		scModels: "Models", scTools: "Toolbox", scEnv: "Environment", scHelp: "Help",
	}[s]
}

// run sub-states.
type runState int

const (
	rsInput runState = iota // collecting the objective
	rsActive
	rsDone
)

// ── model ─────────────────────────────────────────────────────────────────────

type tuiModel struct {
	cfg    *Config
	client *Client
	screen screen
	width  int
	height int

	statusOK   bool
	statusText string
	flash      string // transient status message (saved X, error Y)

	spin spinner.Model

	// menu
	menu list.Model

	// run
	runInput  textinput.Model
	runMode   string // "flat" | "plan" | "demo" | "plandemo"
	runState  runState
	runVP     viewport.Model
	runLog    []string
	runResult *RunResult
	events    chan tea.Msg
	cancelRun context.CancelFunc

	// settings
	settings  list.Model
	editing   bool
	editKey   string
	editInput textinput.Model

	// models
	models     list.Model
	loadingMdl bool
	pulling    bool
	pullName   string
	pullStatus string
	pullCh     chan tea.Msg
	cancelPull context.CancelFunc
	addingPull bool
	pullInput  textinput.Model

	// tools
	tools list.Model
	form  *toolForm

	// env / help
	envVP  viewport.Model
	helpVP viewport.Model
}

// menuItem implements list.DefaultItem for the home menu.
type menuItem struct{ t, d, id string }

func (i menuItem) Title() string       { return i.t }
func (i menuItem) Description() string { return i.d }
func (i menuItem) FilterValue() string { return i.t }

func newTUIModel(cfg *Config) *tuiModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(theme.Primary)

	m := &tuiModel{
		cfg:        cfg,
		client:     NewClient(cfg),
		screen:     scMenu,
		width:      80,
		height:     24,
		spin:       sp,
		statusText: "checking…",
	}

	m.menu = newList([]list.Item{
		menuItem{"Run an objective", "Drive one goal through the flat ADA loop", "run"},
		menuItem{"Planning run", "Decompose an open-ended objective and execute", "plan"},
		menuItem{"Offline demo", "Walk the loop with no model server", "demo"},
		menuItem{"Planning demo", "Offline decomposition demo", "plandemo"},
		menuItem{"Models", "List, pull, switch and remove Ollama models", "models"},
		menuItem{"Settings", "Host, model, decoder, budgets, prompts", "settings"},
		menuItem{"Toolbox", "Curate the commands offered to the model", "tools"},
		menuItem{"Environment", "Inspect the ADA_* knobs", "env"},
		menuItem{"Help", "Commands & key bindings", "help"},
		menuItem{"Quit", "Leave ada", "quit"},
	})

	m.runInput = textinput.New()
	m.runInput.Placeholder = "create /tmp/app/ready and prove the file exists"
	m.runInput.Width = 60

	m.editInput = textinput.New()
	m.editInput.Width = 50

	m.pullInput = textinput.New()
	m.pullInput.Placeholder = "qwen2.5-coder:14b"
	m.pullInput.Width = 40

	m.settings = newList(nil)
	m.models = newList(nil)
	m.tools = newList(nil)
	m.runVP = viewport.New(78, 18)
	m.envVP = viewport.New(78, 18)
	m.helpVP = viewport.New(78, 18)
	m.rebuildSettings()
	m.rebuildTools()
	return m
}

// newList builds a chrome-less list (we draw our own header/footer).
func newList(items []list.Item) list.Model {
	d := list.NewDefaultDelegate()
	d.Styles.SelectedTitle = d.Styles.SelectedTitle.Foreground(theme.Primary).BorderForeground(theme.Primary)
	d.Styles.SelectedDesc = d.Styles.SelectedDesc.Foreground(theme.Accent).BorderForeground(theme.Primary)
	l := list.New(items, d, 78, 18)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetFilteringEnabled(false)
	return l
}

func (m *tuiModel) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.cmdStatus(), m.spin.Tick)
}

// ── messages & background commands ────────────────────────────────────────────

type logLineMsg string
type runDoneMsg RunResult
type statusMsg struct {
	ok   bool
	text string
}
type modelsLoadedMsg []ModelInfo
type errMsg string
type pullEventMsg PullProgress
type pullDoneMsg struct {
	name string
	err  error
}
type flashClearMsg struct{}

func waitForEvent(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func (m *tuiModel) cmdStatus() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if err := client.Ping(ctx); err != nil {
			return statusMsg{false, "offline"}
		}
		v, _ := client.Version(ctx)
		t := "online"
		if v != "" {
			t = "v" + v
		}
		return statusMsg{true, t}
	}
}

func (m *tuiModel) cmdLoadModels() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ms, err := client.ListModels(ctx)
		if err != nil {
			return errMsg(err.Error())
		}
		return modelsLoadedMsg(ms)
	}
}

func flashCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return flashClearMsg{} })
}

func (m *tuiModel) setFlash(s string) tea.Cmd {
	m.flash = s
	return flashCmd()
}

// ── update ────────────────────────────────────────────────────────────────────

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil

	case spinner.TickMsg:
		if m.runState == rsActive || m.pulling || m.loadingMdl {
			var cmd tea.Cmd
			m.spin, cmd = m.spin.Update(msg)
			return m, cmd
		}
		return m, nil

	case statusMsg:
		m.statusOK, m.statusText = msg.ok, msg.text
		return m, nil

	case flashClearMsg:
		m.flash = ""
		return m, nil

	case errMsg:
		m.loadingMdl = false
		return m, m.setFlash(theme.Bad.Render("✗ " + string(msg)))

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			if m.cancelRun != nil {
				m.cancelRun()
			}
			return m, tea.Quit
		}
		if m.screen == scMenu && msg.String() == "q" {
			return m, tea.Quit
		}
	}

	switch m.screen {
	case scMenu:
		return m.updateMenu(msg)
	case scRun:
		return m.updateRun(msg)
	case scSettings:
		return m.updateSettings(msg)
	case scModels:
		return m.updateModels(msg)
	case scTools:
		return m.updateTools(msg)
	case scEnv:
		return m.updateViewport(msg, &m.envVP)
	case scHelp:
		return m.updateViewport(msg, &m.helpVP)
	}
	return m, nil
}

func (m *tuiModel) resize(w, h int) {
	m.width, m.height = w, h
	bodyH := h - 4
	if bodyH < 4 {
		bodyH = 4
	}
	m.menu.SetSize(w-2, bodyH)
	m.settings.SetSize(w-2, bodyH)
	m.models.SetSize(w-2, bodyH)
	m.tools.SetSize(w-2, bodyH)
	m.runVP.Width, m.runVP.Height = w-2, bodyH-2
	m.envVP.Width, m.envVP.Height = w-2, bodyH
	m.helpVP.Width, m.helpVP.Height = w-2, bodyH
	m.runInput.Width = w - 20
}

func (m *tuiModel) updateMenu(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok && k.String() == "enter" {
		if it, ok := m.menu.SelectedItem().(menuItem); ok {
			return m.dispatchMenu(it.id)
		}
	}
	var cmd tea.Cmd
	m.menu, cmd = m.menu.Update(msg)
	return m, cmd
}

func (m *tuiModel) dispatchMenu(id string) (tea.Model, tea.Cmd) {
	switch id {
	case "quit":
		return m, tea.Quit
	case "run":
		m.screen, m.runMode, m.runState = scRun, "flat", rsInput
		m.runInput.SetValue("")
		return m, m.runInput.Focus()
	case "plan":
		m.screen, m.runMode, m.runState = scRun, "plan", rsInput
		m.runInput.SetValue("")
		return m, m.runInput.Focus()
	case "demo":
		m.screen, m.runMode = scRun, "demo"
		return m, m.startRun("")
	case "plandemo":
		m.screen, m.runMode = scRun, "plandemo"
		return m, m.startRun("")
	case "models":
		m.screen, m.loadingMdl = scModels, true
		return m, tea.Batch(m.cmdLoadModels(), m.spin.Tick)
	case "settings":
		m.screen = scSettings
		m.rebuildSettings()
		return m, nil
	case "tools":
		m.screen = scTools
		m.rebuildTools()
		return m, nil
	case "env":
		m.screen = scEnv
		m.envVP.SetContent(m.envContent())
		m.envVP.GotoTop()
		return m, nil
	case "help":
		m.screen = scHelp
		m.helpVP.SetContent(renderMarkdown(helpMarkdown))
		m.helpVP.GotoTop()
		return m, nil
	}
	return m, nil
}

func (m *tuiModel) updateViewport(msg tea.Msg, vp *viewport.Model) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok && (k.String() == "esc" || k.String() == "q") {
		m.screen = scMenu
		return m, nil
	}
	var cmd tea.Cmd
	*vp, cmd = vp.Update(msg)
	return m, cmd
}

// ── view ──────────────────────────────────────────────────────────────────────

func (m *tuiModel) View() string {
	var body, footer string
	switch m.screen {
	case scMenu:
		body = m.menu.View()
		footer = "↑/↓ move · enter select · q quit"
	case scRun:
		body, footer = m.viewRun()
	case scSettings:
		body, footer = m.viewSettings()
	case scModels:
		body, footer = m.viewModels()
	case scTools:
		body, footer = m.viewTools()
	case scEnv:
		body = m.envVP.View()
		footer = "↑/↓ scroll · esc back"
	case scHelp:
		body = m.helpVP.View()
		footer = "↑/↓ scroll · esc back"
	}

	foot := theme.Help.Render(footer)
	if m.flash != "" {
		foot = m.flash + theme.Dim.Render("  ·  ") + foot
	}
	return m.renderHeader() + "\n" + body + "\n" + foot
}

func (m *tuiModel) renderHeader() string {
	left := theme.SelPill.Render(" ADA ") + theme.Crumb.Render(" ▸ "+m.screen.crumb())
	dot := theme.Bad.Render("●")
	if m.statusOK {
		dot = theme.OK.Render("●")
	}
	right := theme.Dim.Render(m.cfg.Model+"  ") + dot + theme.Dim.Render(" "+m.statusText)
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	bar := left + strings.Repeat(" ", gap) + right
	return bar + "\n" + theme.Dim.Render(strings.Repeat("─", maxi(m.width, 1)))
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ── env content ───────────────────────────────────────────────────────────────

func (m *tuiModel) envContent() string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("Environment knobs") + theme.Dim.Render("  — every setting is also an ADA_* variable") + "\n\n")
	pairs := [][2]string{
		{"ADA_OLLAMA", m.cfg.OllamaHost}, {"ADA_MODEL", m.cfg.Model},
		{"ADA_PLANNER_MODEL", m.cfg.PlannerModel}, {"ADA_TEMPERATURE", ftoa(m.cfg.Temperature)},
		{"ADA_TOP_P", ftoa(m.cfg.TopP)}, {"ADA_NUM_CTX", itoa(m.cfg.NumCtx)},
		{"ADA_MAX_STEPS", itoa(m.cfg.MaxSteps)}, {"ADA_MAX_ROUNDS", itoa(m.cfg.MaxRounds)},
		{"ADA_GOAL_STEPS", itoa(m.cfg.GoalSteps)}, {"ADA_MAX_ENTROPY", itoa(m.cfg.MaxEntropy)},
		{"ADA_MAX_FACTS", itoa(m.cfg.MaxFacts)}, {"ADA_MAX_STUCK", itoa(m.cfg.MaxStuck)},
		{"ADA_MAX_ATTEMPTS", itoa(m.cfg.MaxAttempts)}, {"ADA_MAX_ROUTES", itoa(m.cfg.MaxRoutes)},
		{"ADA_STALL", itoa(m.cfg.Stall)}, {"ADA_MEMORY_ENABLED", btoa(m.cfg.Memory)},
		{"ADA_MEMORY_FILE", m.cfg.MemoryPath()}, {"ADA_META", btoa(m.cfg.Meta)},
		{"ADA_COLOR", m.cfg.Color}, {"ADA_CONFIG", m.cfg.Path()},
	}
	for _, p := range pairs {
		b.WriteString("  " + theme.Key.Render(padRight(p[0], 20)) + " " + theme.Val.Render(p[1]) + "\n")
	}
	return b.String()
}
