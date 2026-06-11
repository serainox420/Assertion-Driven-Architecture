package ada

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// LLM is the stochastic hypothesis generator, reduced to a stateless function:
// StateSnapshot → Task. The temperature is chosen by the orchestrator (low for
// normal THINK steps, raised on a fork to force novelty — §9.3).
type LLM interface {
	GenerateTask(ctx context.Context, snapshot StateSnapshot, temperature float64) (Task, error)
}

// ParseTask decodes (and lightly validates) a Task emitted by the model. A parse
// failure is returned as an error so the orchestrator can turn it into a virtual
// assertion failure rather than crashing the loop (§9.2).
func ParseTask(raw []byte) (Task, error) {
	var t Task
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&t); err != nil {
		return Task{}, fmt.Errorf("invalid Task JSON: %w", err)
	}
	if t.ID == "" || t.Command == "" || t.Assertion.Channel == "" {
		return Task{}, fmt.Errorf("incomplete Task: id/command/assertion.channel are required")
	}
	if t.Mode == "" {
		t.Mode = ModeBlocking
	}
	return t, nil
}

// OllamaLLM drives a local Ollama / llama.cpp server via grammar-constrained
// decoding (§9.1). It is model-agnostic; point Model at any tool-less coder
// model — the schema does the structuring, not the model's goodwill.
type OllamaLLM struct {
	BaseURL string // e.g. http://localhost:11434
	Model   string // e.g. qwen2.5-coder:14b
	NumCtx  int    // compressed context fits comfortably (§9.1)
	HTTP    *http.Client
}

// NewOllamaLLM returns a client with sensible defaults.
func NewOllamaLLM(baseURL, model string) *OllamaLLM {
	return &OllamaLLM{
		BaseURL: baseURL,
		Model:   model,
		NumCtx:  8192,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

type ollamaRequest struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	System  string         `json:"system"`
	Format  any            `json:"format"` // engine-enforced schema; not a suggestion
	Stream  bool           `json:"stream"`
	Options map[string]any `json:"options"`
}

type ollamaResponse struct {
	Response string `json:"response"`
	Error    string `json:"error,omitempty"`
}

// complete issues one grammar-constrained completion and returns the raw response
// string. Shared by GenerateTask (execution) and Plan (planning).
func (o *OllamaLLM) complete(ctx context.Context, system, prompt string, format any, opts map[string]any) (string, error) {
	buf, err := json.Marshal(ollamaRequest{
		Model: o.Model, Prompt: prompt, System: system, Format: format, Stream: false, Options: opts,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/api/generate", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.HTTP.Do(req)
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
	return out.Response, nil
}

// GenerateTask issues one grammar-constrained completion and parses the result.
func (o *OllamaLLM) GenerateTask(ctx context.Context, snapshot StateSnapshot, temperature float64) (Task, error) {
	resp, err := o.complete(ctx, SystemPrompt, BuildPrompt(snapshot), TaskSchema, map[string]any{
		"temperature": temperature, // 0.0 normal; raised on fork (§9.3)
		"num_predict": 512,         // a Task JSON is never long
		"top_p":       0.1,
		"num_ctx":     o.NumCtx,
	})
	if err != nil {
		return Task{}, err
	}
	return ParseTask([]byte(resp))
}

// Plan issues one grammar-constrained planning completion (planning mode, §5.6).
// OllamaLLM therefore implements both LLM and Planner.
func (o *OllamaLLM) Plan(ctx context.Context, in PlanInput, temperature float64) (PlanDecision, error) {
	resp, err := o.complete(ctx, PlannerPrompt, BuildPlanPrompt(in), PlanSchema, map[string]any{
		"temperature": temperature,
		"num_predict": 512,
		"num_ctx":     o.NumCtx,
	})
	if err != nil {
		return PlanDecision{}, err
	}
	var dec PlanDecision
	if err := json.Unmarshal([]byte(resp), &dec); err != nil {
		return PlanDecision{}, fmt.Errorf("invalid PlanDecision JSON: %w", err)
	}
	return dec, nil
}
