package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	ada "github.com/serainox420/assertion-driven-architecture/ada"
)

// Client is a configurable Ollama driver. Unlike ada.OllamaLLM (which hard-codes
// the package's system prompt and decoder options), this one reads every knob from
// Config — host, model, prompts, temperature, top_p, context, and the operator's
// tool registry — so the TUI and CLI can change behavior live. It satisfies both
// ada.LLM (GenerateTask) and ada.Planner (Plan).
type Client struct {
	cfg       *Config
	HTTP      *http.Client
	DebugHook func(raw string) // called with raw LLM response; wire to session.CaptureRaw
}

// NewClient binds a Client to the active config. It carries no global timeout —
// per-call deadlines (generation) and long-lived streams (pull) are handled with
// context, so a model download is never cut off at 2 minutes.
func NewClient(cfg *Config) *Client {
	return &Client{cfg: cfg, HTTP: &http.Client{}}
}

// systemPromptFor returns the worker system prompt: the configured override (or the
// ada package default) with the enabled TOOLBOX appended. Curating tools steers the
// model toward known-good commands without retraining or hand-editing the prompt.
func systemPromptFor(cfg *Config) string {
	base := cfg.SystemPrompt
	if strings.TrimSpace(base) == "" {
		base = ada.SystemPrompt
	}
	tools := cfg.EnabledTools()
	if len(tools) == 0 {
		return base
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\nTOOLBOX — operator-curated capabilities you SHOULD prefer when they fit the goal.\n")
	b.WriteString("Adapt the example command to the situation; still pair it with a checkable assertion.\n")
	for _, t := range tools {
		fmt.Fprintf(&b, "- %s: %s\n", t.Name, t.Description)
		if strings.TrimSpace(t.Command) != "" {
			fmt.Fprintf(&b, "    command: %s\n", t.Command)
		}
		if strings.TrimSpace(t.Channel) != "" || strings.TrimSpace(t.Assertion) != "" {
			fmt.Fprintf(&b, "    assert : channel=%s pattern=%s\n", t.Channel, t.Assertion)
		}
	}
	return b.String()
}

// plannerPromptFor returns the planner system prompt (override or default).
func plannerPromptFor(cfg *Config) string {
	if strings.TrimSpace(cfg.PlannerPrompt) != "" {
		return cfg.PlannerPrompt
	}
	return ada.PlannerPrompt
}

type ollamaRequest struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	System  string         `json:"system"`
	Format  any            `json:"format"`
	Stream  bool           `json:"stream"`
	Options map[string]any `json:"options"`
}

type ollamaResponse struct {
	Response string `json:"response"`
	Error    string `json:"error,omitempty"`
}

// complete issues one non-streaming, grammar-constrained completion.
func (c *Client) complete(ctx context.Context, model, system, prompt string, format any, opts map[string]any) (string, error) {
	buf, err := json.Marshal(ollamaRequest{
		Model: model, Prompt: prompt, System: system, Format: format, Stream: false, Options: opts,
	})
	if err != nil {
		return "", err
	}
	// A per-call ceiling so a wedged server can't hang a loop forever, while still
	// allowing slow local inference. The orchestrator's own ctx still cancels first.
	cctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodPost, c.cfg.OllamaHost+"/api/generate", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama request failed: %w", err)
	}
	defer resp.Body.Close()

	var out ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding ollama response: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("ollama error: %s", out.Error)
	}
	if c.DebugHook != nil {
		c.DebugHook(out.Response)
	}
	return out.Response, nil
}

// GenerateTask satisfies ada.LLM: snapshot → next Task.
func (c *Client) GenerateTask(ctx context.Context, snapshot ada.StateSnapshot, temperature float64) (ada.Task, error) {
	resp, err := c.complete(ctx, c.cfg.Model, systemPromptFor(c.cfg), ada.BuildPrompt(snapshot), ada.TaskSchema, map[string]any{
		"temperature": temperature,
		"num_predict": c.cfg.NumPredict,
		"top_p":       c.cfg.TopP,
		"num_ctx":     c.cfg.NumCtx,
	})
	if err != nil {
		return ada.Task{}, err
	}
	return ada.ParseTask([]byte(resp))
}

// Plan satisfies ada.Planner: objective + facts → next sub-goals / done.
func (c *Client) Plan(ctx context.Context, in ada.PlanInput, temperature float64) (ada.PlanDecision, error) {
	resp, err := c.complete(ctx, c.cfg.Planner(), plannerPromptFor(c.cfg), ada.BuildPlanPrompt(in), ada.PlanSchema, map[string]any{
		"temperature": temperature,
		"num_predict": 512,
		"num_ctx":     c.cfg.NumCtx,
	})
	if err != nil {
		return ada.PlanDecision{}, err
	}
	var dec ada.PlanDecision
	if err := json.Unmarshal([]byte(resp), &dec); err != nil {
		return ada.PlanDecision{}, fmt.Errorf("invalid PlanDecision JSON: %w", err)
	}
	return dec, nil
}

// ── model management (the Ollama REST surface) ────────────────────────────────

// ModelInfo is one entry from /api/tags.
type ModelInfo struct {
	Name       string    `json:"name"`
	Model      string    `json:"model"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	Details    struct {
		ParameterSize     string `json:"parameter_size"`
		QuantizationLevel string `json:"quantization_level"`
		Family            string `json:"family"`
	} `json:"details"`
}

// Ping checks that an Ollama server answers at the configured host.
func (c *Client) Ping(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, c.cfg.OllamaHost+"/api/tags", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Version returns the Ollama server version string.
func (c *Client) Version(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, c.cfg.OllamaHost+"/api/version", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// ListModels returns the locally available models, newest first.
func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, c.cfg.OllamaHost+"/api/tags", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

// ShowModel returns the raw /api/show payload for a model (modelfile, params,
// template, details) as a decoded map for flexible display.
func (c *Client) ShowModel(ctx context.Context, name string) (map[string]any, error) {
	body, _ := json.Marshal(map[string]string{"name": name})
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodPost, c.cfg.OllamaHost+"/api/show", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	if e, ok := m["error"].(string); ok && e != "" {
		return nil, fmt.Errorf("%s", e)
	}
	return m, nil
}

// DeleteModel removes a local model.
func (c *Client) DeleteModel(ctx context.Context, name string) error {
	body, _ := json.Marshal(map[string]string{"name": name})
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodDelete, c.cfg.OllamaHost+"/api/delete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("delete failed: %s", resp.Status)
	}
	return nil
}

// PullProgress is one streamed status update from a model pull.
type PullProgress struct {
	Status    string `json:"status"`
	Digest    string `json:"digest,omitempty"`
	Total     int64  `json:"total,omitempty"`
	Completed int64  `json:"completed,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Pull downloads a model, invoking onProgress for each streamed update. The pull
// honors ctx cancellation, so the TUI/CLI can abort a long download cleanly.
func (c *Client) Pull(ctx context.Context, name string, onProgress func(PullProgress)) error {
	body, _ := json.Marshal(map[string]any{"name": name, "stream": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.OllamaHost+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var p PullProgress
		if json.Unmarshal([]byte(line), &p) != nil {
			continue
		}
		if p.Error != "" {
			return fmt.Errorf("%s", p.Error)
		}
		if onProgress != nil {
			onProgress(p)
		}
	}
	return sc.Err()
}
