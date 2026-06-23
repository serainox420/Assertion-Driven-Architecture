package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	ada "github.com/serainox420/assertion-driven-architecture/ada"
)

// Tool is a user-registered, reusable command capability. Enabled tools are
// surfaced to the worker model as a TOOLBOX appended to the system prompt, so the
// model prefers known-good commands and assertions instead of guessing. This is
// ADA's analogue of "tool use": the runtime still executes shell + checks an
// assertion, but the operator curates the palette the model draws from.
type Tool struct {
	Name        string `json:"name"`                // short, no spaces — referenced in the prompt
	Description string `json:"description"`         // what it does / when to use it
	Command     string `json:"command"`             // example or template command
	Channel     string `json:"channel,omitempty"`   // suggested assertion channel (fs/process/service/...)
	Assertion   string `json:"assertion,omitempty"` // suggested assertion pattern
	Enabled     bool   `json:"enabled"`             // only enabled tools reach the prompt
}

// Config is the entire, persistent state of the ada runtime — connection, model,
// every decoder/budget knob, prompt overrides, and the tool registry. It mirrors
// the Orchestrator/Coordinator fields so the TUI and CLI can drive them without
// touching code. Defaults match the ada package; env vars and flags overlay it.
type Config struct {
	// ── Connection / model ────────────────────────────────────────────────────
	OllamaHost   string `json:"ollama_host"`   // base URL, e.g. http://localhost:11434
	Model        string `json:"model"`         // worker model tag
	PlannerModel string `json:"planner_model"` // planner model (falls back to Model when empty)

	// ── Decoder ───────────────────────────────────────────────────────────────
	Temperature float64 `json:"temperature"` // normal THINK temperature (§9.3)
	ForkTemp    float64 `json:"fork_temp"`   // raised temperature on a Hard Context Fork
	PlanTemp    float64 `json:"plan_temp"`   // planner sampling temperature
	TopP        float64 `json:"top_p"`
	NumCtx      int     `json:"num_ctx"`     // context window tokens
	NumPredict  int     `json:"num_predict"` // max tokens per emission

	// ── Runtime budgets (Orchestrator / Coordinator) ──────────────────────────
	MaxSteps    int `json:"max_steps"`
	MaxRounds   int `json:"max_rounds"`
	GoalSteps   int `json:"goal_steps"`
	MaxEntropy  int `json:"max_entropy"`
	MaxFacts    int `json:"max_facts"`
	MaxStuck    int `json:"max_stuck"`
	MaxAttempts int `json:"max_attempts"`
	MaxRoutes   int `json:"max_routes"`
	Stall       int `json:"stall"`

	// ── Behavior ──────────────────────────────────────────────────────────────
	Memory     bool   `json:"memory"`      // persist & reuse re-validated durable facts
	MemoryFile string `json:"memory_file"` // empty ⇒ ada.DefaultMemoryPath()
	Meta       bool   `json:"meta"`        // enable the heuristic meta-controller
	Color      string `json:"color"`       // auto | always | never
	Markdown   bool   `json:"markdown"`    // render summaries/help as styled markdown

	// ── Prompt / template overrides (empty ⇒ ada package defaults) ────────────
	SystemPrompt  string `json:"system_prompt,omitempty"`
	PlannerPrompt string `json:"planner_prompt,omitempty"`

	// ── Debug log mode ────────────────────────────────────────────────────────
	DebugEnabled      bool   `json:"debug_enabled"`
	DebugDir          string `json:"debug_dir,omitempty"`
	DebugCombined     bool   `json:"debug_combined"`
	DebugLogMeta      bool   `json:"debug_log_meta"`
	DebugLogTasks     bool   `json:"debug_log_tasks"`
	DebugLogRawLLM    bool   `json:"debug_log_raw_llm"`
	DebugLogStdout    bool   `json:"debug_log_stdout"`
	DebugLogStderr    bool   `json:"debug_log_stderr"`
	DebugLogAnomalies bool   `json:"debug_log_anomalies"`
	DebugLogFacts     bool   `json:"debug_log_facts"`
	DebugLogPlanner   bool   `json:"debug_log_planner"`
	DebugLogSummary   bool   `json:"debug_log_summary"`

	// ── Tool registry ─────────────────────────────────────────────────────────
	Tools []Tool `json:"tools,omitempty"`

	// path is where this config was loaded from / will be saved (not serialized).
	path string `json:"-"`
}

// DefaultConfig returns a Config seeded with the same values the ada package and
// the shell scripts use, so a fresh install behaves identically to `make run`.
func DefaultConfig() *Config {
	return &Config{
		OllamaHost:   "http://localhost:11434",
		Model:        "qwen2.5-coder:14b",
		PlannerModel: "",
		Temperature:  0.0,
		ForkTemp:     0.8,
		PlanTemp:     0.4,
		TopP:         0.1,
		NumCtx:       8192,
		NumPredict:   1024,
		MaxSteps:     200,
		MaxRounds:    8,
		GoalSteps:    40,
		MaxEntropy:   6,
		MaxFacts:     15,
		MaxStuck:     10,
		MaxAttempts:  3,
		MaxRoutes:    3,
		Stall:        2,
		Memory:       true,
		MemoryFile:   "",
		Meta:         false,
		Color:        "auto",
		Markdown:     true,

		DebugEnabled:      false,
		DebugCombined:     true,
		DebugLogMeta:      true,
		DebugLogTasks:     true,
		DebugLogRawLLM:    true,
		DebugLogStdout:    true,
		DebugLogStderr:    true,
		DebugLogAnomalies: true,
		DebugLogFacts:     true,
		DebugLogPlanner:   true,
		DebugLogSummary:   true,
	}
}

// ConfigPath resolves the config file location: $ADA_CONFIG, else
// $XDG_CONFIG_HOME/ada/config.json, else ~/.config/ada/config.json.
func ConfigPath() string {
	if p := os.Getenv("ADA_CONFIG"); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "ada-config.json" // last resort: CWD
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "ada", "config.json")
}

// LoadConfig layers configuration: built-in defaults → JSON file (if present) →
// environment overrides. A missing or corrupt file is not fatal — defaults win —
// so the tool always starts. The resolved file path is remembered for Save.
func LoadConfig() *Config {
	c := DefaultConfig()
	c.path = ConfigPath()

	if data, err := os.ReadFile(c.path); err == nil {
		// Decode onto the defaults so unspecified keys keep their default value.
		_ = json.Unmarshal(data, c)
		c.path = ConfigPath() // path is not serialized; restore it
	}
	c.applyEnv()
	c.normalize()
	return c
}

// applyEnv overlays environment variables (ADA_* plus the Ollama/scripts vars), so
// the same knobs work from a shell, a Dockerfile, or CI without editing the file.
func (c *Config) applyEnv() {
	// Host: ADA_OLLAMA wins, then OLLAMA_HOST, then the scripts' OLLAMA_HOST_URL.
	for _, k := range []string{"ADA_OLLAMA", "OLLAMA_HOST", "OLLAMA_HOST_URL"} {
		if v := os.Getenv(k); v != "" {
			c.OllamaHost = v
			break
		}
	}
	envStr(&c.Model, "ADA_MODEL")
	envStr(&c.PlannerModel, "ADA_PLANNER_MODEL")
	envStr(&c.MemoryFile, "ADA_MEMORY_FILE")
	envStr(&c.Color, "ADA_COLOR")
	envStr(&c.SystemPrompt, "ADA_SYSTEM_PROMPT")
	envStr(&c.PlannerPrompt, "ADA_PLANNER_PROMPT")

	envFloat(&c.Temperature, "ADA_TEMPERATURE")
	envFloat(&c.ForkTemp, "ADA_FORK_TEMP")
	envFloat(&c.PlanTemp, "ADA_PLAN_TEMP")
	envFloat(&c.TopP, "ADA_TOP_P")
	envInt(&c.NumCtx, "ADA_NUM_CTX")
	envInt(&c.NumPredict, "ADA_NUM_PREDICT")

	envInt(&c.MaxSteps, "ADA_MAX_STEPS")
	envInt(&c.MaxRounds, "ADA_MAX_ROUNDS")
	envInt(&c.GoalSteps, "ADA_GOAL_STEPS")
	envInt(&c.MaxEntropy, "ADA_MAX_ENTROPY")
	envInt(&c.MaxFacts, "ADA_MAX_FACTS")
	envInt(&c.MaxStuck, "ADA_MAX_STUCK")
	envInt(&c.MaxAttempts, "ADA_MAX_ATTEMPTS")
	envInt(&c.MaxRoutes, "ADA_MAX_ROUTES")
	envInt(&c.Stall, "ADA_STALL")

	envBool(&c.Memory, "ADA_MEMORY_ENABLED")
	envBool(&c.Meta, "ADA_META")
	envBool(&c.Markdown, "ADA_MARKDOWN")
	envBool(&c.DebugEnabled, "ADA_DEBUG")
	envStr(&c.DebugDir, "ADA_DEBUG_DIR")
	envBool(&c.DebugCombined, "ADA_DEBUG_COMBINED")

	if os.Getenv("NO_COLOR") != "" {
		c.Color = "never"
	}
}

// normalize repairs an Ollama host into a usable base URL and clamps the color mode.
func (c *Config) normalize() {
	c.OllamaHost = normalizeHost(c.OllamaHost)
	switch c.Color {
	case "auto", "always", "never":
	default:
		c.Color = "auto"
	}
}

// normalizeHost turns "host:port" into "http://host:port" and trims trailing
// slashes, matching how ollama itself reads OLLAMA_HOST.
func normalizeHost(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return "http://localhost:11434"
	}
	if !strings.Contains(h, "://") {
		h = "http://" + h
	}
	return strings.TrimRight(h, "/")
}

// Planner returns the planner model, falling back to the worker model.
func (c *Config) Planner() string {
	if strings.TrimSpace(c.PlannerModel) != "" {
		return c.PlannerModel
	}
	return c.Model
}

// MemoryPath returns the effective memory file (configured override or default).
func (c *Config) MemoryPath() string {
	if strings.TrimSpace(c.MemoryFile) != "" {
		return c.MemoryFile
	}
	return ada.DefaultMemoryPath()
}

// Save writes the config as pretty JSON, creating parent directories as needed.
func (c *Config) Save() error {
	if c.path == "" {
		c.path = ConfigPath()
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, append(data, '\n'), 0o644)
}

// Path returns where the config lives on disk.
func (c *Config) Path() string { return c.path }

// EnabledTools returns only the tools the operator has switched on.
func (c *Config) EnabledTools() []Tool {
	var out []Tool
	for _, t := range c.Tools {
		if t.Enabled {
			out = append(out, t)
		}
	}
	return out
}

// FindTool returns a pointer to the named tool (case-insensitive) or nil.
func (c *Config) FindTool(name string) *Tool {
	for i := range c.Tools {
		if strings.EqualFold(c.Tools[i].Name, name) {
			return &c.Tools[i]
		}
	}
	return nil
}

// SetField sets a single config field by its JSON key, parsing the string value to
// the field's type. It powers `ada config set <key> <value>` and the TUI editor.
// Unknown keys and unparseable values return a descriptive error.
func (c *Config) SetField(key, value string) error {
	switch key {
	case "ollama_host":
		c.OllamaHost = normalizeHost(value)
	case "model":
		c.Model = value
	case "planner_model":
		c.PlannerModel = value
	case "memory_file":
		c.MemoryFile = value
	case "system_prompt":
		c.SystemPrompt = value
	case "planner_prompt":
		c.PlannerPrompt = value
	case "color":
		if value != "auto" && value != "always" && value != "never" {
			return fmt.Errorf("color must be auto|always|never")
		}
		c.Color = value
	case "temperature":
		return setFloat(&c.Temperature, value)
	case "fork_temp":
		return setFloat(&c.ForkTemp, value)
	case "plan_temp":
		return setFloat(&c.PlanTemp, value)
	case "top_p":
		return setFloat(&c.TopP, value)
	case "num_ctx":
		return setInt(&c.NumCtx, value)
	case "num_predict":
		return setInt(&c.NumPredict, value)
	case "max_steps":
		return setInt(&c.MaxSteps, value)
	case "max_rounds":
		return setInt(&c.MaxRounds, value)
	case "goal_steps":
		return setInt(&c.GoalSteps, value)
	case "max_entropy":
		return setInt(&c.MaxEntropy, value)
	case "max_facts":
		return setInt(&c.MaxFacts, value)
	case "max_stuck":
		return setInt(&c.MaxStuck, value)
	case "max_attempts":
		return setInt(&c.MaxAttempts, value)
	case "max_routes":
		return setInt(&c.MaxRoutes, value)
	case "stall":
		return setInt(&c.Stall, value)
	case "memory":
		return setBool(&c.Memory, value)
	case "meta":
		return setBool(&c.Meta, value)
	case "markdown":
		return setBool(&c.Markdown, value)
	case "debug_enabled":
		return setBool(&c.DebugEnabled, value)
	case "debug_dir":
		c.DebugDir = value
	case "debug_combined":
		return setBool(&c.DebugCombined, value)
	case "debug_log_meta":
		return setBool(&c.DebugLogMeta, value)
	case "debug_log_tasks":
		return setBool(&c.DebugLogTasks, value)
	case "debug_log_raw_llm":
		return setBool(&c.DebugLogRawLLM, value)
	case "debug_log_stdout":
		return setBool(&c.DebugLogStdout, value)
	case "debug_log_stderr":
		return setBool(&c.DebugLogStderr, value)
	case "debug_log_anomalies":
		return setBool(&c.DebugLogAnomalies, value)
	case "debug_log_facts":
		return setBool(&c.DebugLogFacts, value)
	case "debug_log_planner":
		return setBool(&c.DebugLogPlanner, value)
	case "debug_log_summary":
		return setBool(&c.DebugLogSummary, value)
	default:
		return fmt.Errorf("unknown config key %q (try `ada config list`)", key)
	}
	return nil
}

// Fields returns the editable scalar config keys with their current values, in a
// stable, display-friendly order. Used by `ada config list` and the TUI.
func (c *Config) Fields() []ConfigField {
	return []ConfigField{
		{"ollama_host", c.OllamaHost, "Ollama base URL"},
		{"model", c.Model, "Worker model tag"},
		{"planner_model", c.PlannerModel, "Planner model (blank ⇒ worker)"},
		{"temperature", ftoa(c.Temperature), "Normal decoder temperature"},
		{"fork_temp", ftoa(c.ForkTemp), "Raised temperature on a fork"},
		{"plan_temp", ftoa(c.PlanTemp), "Planner temperature"},
		{"top_p", ftoa(c.TopP), "Nucleus sampling top_p"},
		{"num_ctx", itoa(c.NumCtx), "Context window tokens"},
		{"num_predict", itoa(c.NumPredict), "Max tokens per emission"},
		{"max_steps", itoa(c.MaxSteps), "Global step budget"},
		{"max_rounds", itoa(c.MaxRounds), "Planning rounds"},
		{"goal_steps", itoa(c.GoalSteps), "Steps per sub-goal"},
		{"max_entropy", itoa(c.MaxEntropy), "Entropy ceiling → fork"},
		{"max_facts", itoa(c.MaxFacts), "Fact-folding cap"},
		{"max_stuck", itoa(c.MaxStuck), "Abandon after N no-progress steps"},
		{"max_attempts", itoa(c.MaxAttempts), "Same-proposition tries per route"},
		{"max_routes", itoa(c.MaxRoutes), "Strategies before FAILED"},
		{"stall", itoa(c.Stall), "No-progress successes before STABLE"},
		{"memory", btoa(c.Memory), "Persist re-validated facts"},
		{"meta", btoa(c.Meta), "Heuristic meta-controller"},
		{"markdown", btoa(c.Markdown), "Styled markdown rendering"},
		{"color", c.Color, "Color mode (auto|always|never)"},
	}
}

// DebugFields returns the editable debug-mode config keys with current values.
func (c *Config) DebugFields() []ConfigField {
	return []ConfigField{
		{"debug_enabled", btoa(c.DebugEnabled), "Record full run details for debugging"},
		{"debug_dir", c.DebugDir, "Override debug folder (blank → $XDG_STATE_HOME/ada/debug)"},
		{"debug_combined", btoa(c.DebugCombined), "Single combined report file per run (off → folder of files)"},
		{"debug_log_meta", btoa(c.DebugLogMeta), "meta — model params, limits, all settings"},
		{"debug_log_tasks", btoa(c.DebugLogTasks), "tasks — full task JSON + result per step"},
		{"debug_log_raw_llm", btoa(c.DebugLogRawLLM), "Include raw LLM response with each step"},
		{"debug_log_stdout", btoa(c.DebugLogStdout), "Include decoded stdout with each step"},
		{"debug_log_stderr", btoa(c.DebugLogStderr), "Include decoded stderr with each step"},
		{"debug_log_anomalies", btoa(c.DebugLogAnomalies), "anomalies — every anomaly payload"},
		{"debug_log_facts", btoa(c.DebugLogFacts), "facts — every fact as established"},
		{"debug_log_planner", btoa(c.DebugLogPlanner), "planner — every planner call (plan mode)"},
		{"debug_log_summary", btoa(c.DebugLogSummary), "summary — final outcome + all facts"},
	}
}

// ToDebugConfig converts the debug-related config fields into an ada.DebugConfig.
func (c *Config) ToDebugConfig() ada.DebugConfig {
	return ada.DebugConfig{
		Enabled:      c.DebugEnabled,
		Dir:          c.DebugDir,
		Combined:     c.DebugCombined,
		LogMeta:      c.DebugLogMeta,
		LogTasks:     c.DebugLogTasks,
		LogRawLLM:    c.DebugLogRawLLM,
		LogStdout:    c.DebugLogStdout,
		LogStderr:    c.DebugLogStderr,
		LogAnomalies: c.DebugLogAnomalies,
		LogFacts:     c.DebugLogFacts,
		LogPlanner:   c.DebugLogPlanner,
		LogSummary:   c.DebugLogSummary,
	}
}

// ConfigField is one editable scalar key shown in `config list` and the TUI.
type ConfigField struct {
	Key   string
	Value string
	Desc  string
}

// ── small typed-parse helpers ─────────────────────────────────────────────────

func envStr(dst *string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}
func envInt(dst *int, key string) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			*dst = n
		}
	}
}
func envFloat(dst *float64, key string) {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			*dst = f
		}
	}
}
func envBool(dst *bool, key string) {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			*dst = b
		}
	}
}

func setInt(dst *int, v string) error {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("expected an integer, got %q", v)
	}
	*dst = n
	return nil
}
func setFloat(dst *float64, v string) error {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return fmt.Errorf("expected a number, got %q", v)
	}
	*dst = f
	return nil
}
func setBool(dst *bool, v string) error {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("expected true/false, got %q", v)
	}
	*dst = b
	return nil
}

func itoa(n int) string     { return strconv.Itoa(n) }
func ftoa(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }
func btoa(b bool) string    { return strconv.FormatBool(b) }
