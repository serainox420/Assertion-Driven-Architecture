package ada

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// LLM is the stochastic hypothesis generator, reduced to a stateless function:
// StateSnapshot → Task. The temperature is chosen by the orchestrator (low for
// normal THINK steps, raised on a fork to force novelty — §9.3).
type LLM interface {
	GenerateTask(ctx context.Context, snapshot StateSnapshot, temperature float64) (Task, error)
}

// extractJSONObject pulls the first complete top-level {...} object out of a model
// response, tolerating the wrappers small models still add despite grammar-
// constrained decoding: ```json code fences, a "Here is the task:" preamble, or
// trailing prose. It scans from the first '{' through its MATCHING '}', honoring
// quoted strings and escapes so a brace inside a string value does not end it
// early. If no balanced object is found it returns the input unchanged, so the JSON
// decoder still reports the original error. This turns a class of avoidable
// THINK_FAILED retries into clean parses.
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return s
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// inside a string literal: ignore structural bytes
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s
}

// ParseTask decodes (and lightly validates) a Task emitted by the model. A parse
// failure is returned as an error so the orchestrator can turn it into a virtual
// assertion failure rather than crashing the loop (§9.2).
func ParseTask(raw []byte) (Task, error) {
	var t Task
	dec := json.NewDecoder(strings.NewReader(extractJSONObject(string(raw))))
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

	// DebugHook, when set, is called with the raw response string after every
	// completion — before JSON parsing — so a DebugSession can capture it for
	// tasks.jsonl. Wire via session.CaptureRaw. Nil is a no-op.
	DebugHook func(raw string)
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
	if o.DebugHook != nil {
		o.DebugHook(out.Response)
	}
	return out.Response, nil
}

// NucleusForTemp widens the nucleus (top_p) whenever the orchestrator samples at an
// exploratory temperature. A Hard Context Fork — or a resend perturbation — raises the
// temperature precisely to force a DIFFERENT command, but a hotter temperature explores
// NOTHING if top_p stays pinned low: a small top_p clamps the nucleus to the few
// highest-probability tokens, so the model re-emits the IDENTICAL command no matter how
// high the temperature. That is exactly the "forcing a different strategy did nothing"
// failure — the fork raised temperature while top_p stayed at 0.1. When the temperature
// is exploratory, ensure the nucleus is wide enough to admit genuine alternatives;
// otherwise leave the caller's deterministic top_p untouched (low temp ⇒ reliability).
func NucleusForTemp(temperature, baseTopP float64) float64 {
	const exploratory = 0.5
	if temperature >= exploratory && baseTopP < exploreTopP {
		return exploreTopP
	}
	return baseTopP
}

// exploreTopP is the nucleus used while sampling hot — wide enough that a raised
// temperature can actually reach a different command.
const exploreTopP = 0.9

// GenerateTask issues one grammar-constrained completion and parses the result.
func (o *OllamaLLM) GenerateTask(ctx context.Context, snapshot StateSnapshot, temperature float64) (Task, error) {
	resp, err := o.complete(ctx, SystemPrompt, BuildPrompt(snapshot), TaskSchema, map[string]any{
		"temperature": temperature,                      // 0.0 normal; raised on fork (§9.3)
		"num_predict": 1024,                             // headroom so a longer Task JSON isn't truncated mid-object
		"top_p":       NucleusForTemp(temperature, 0.1), // widen the nucleus when sampling hot, or a fork explores nothing
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
	if err := json.Unmarshal([]byte(extractJSONObject(resp)), &dec); err != nil {
		return PlanDecision{}, fmt.Errorf("invalid PlanDecision JSON: %w", err)
	}
	return dec, nil
}
