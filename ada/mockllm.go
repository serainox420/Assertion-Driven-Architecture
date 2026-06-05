package ada

import "context"

// MockLLM is a deterministic, offline stand-in for a real model. It lets the
// runtime and orchestrator be exercised — and the loop be demoed — without an
// inference server. Respond is the policy: snapshot in, one Task out.
type MockLLM struct {
	Respond func(s StateSnapshot) (Task, error)
}

// GenerateTask ignores temperature (a deterministic mock has no sampler) and
// defers entirely to Respond.
func (m *MockLLM) GenerateTask(_ context.Context, s StateSnapshot, _ float64) (Task, error) {
	return m.Respond(s)
}

// ScriptedLLM returns a MockLLM that emits the given tasks in order, one per
// turn, regardless of state — useful for golden-path tests.
func ScriptedLLM(tasks ...Task) *MockLLM {
	i := 0
	return &MockLLM{Respond: func(StateSnapshot) (Task, error) {
		if i >= len(tasks) {
			// Nothing left to do; emit a trivially-passing no-op.
			return Task{
				ID:         "noop",
				Command:    "true",
				Mode:       ModeBlocking,
				TimeoutSec: 5,
				Assertion:  Assertion{Type: "exit", Pattern: "0", Channel: ChannelExitCode},
			}, nil
		}
		t := tasks[i]
		i++
		return t, nil
	}}
}
