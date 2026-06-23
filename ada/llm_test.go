package ada

import "testing"

// extractJSONObject must peel the wrappers small models add despite grammar-
// constrained decoding — code fences, a prose preamble, trailing chatter — and
// return the balanced object, without being fooled by braces inside string values.
func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", `{"a":1}`, `{"a":1}`},
		{"fenced", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"preamble", `Here is the task: {"a":1}`, `{"a":1}`},
		{"trailing prose", `{"a":1}  -- done`, `{"a":1}`},
		{"nested", `{"a":{"b":2},"c":3}`, `{"a":{"b":2},"c":3}`},
		{"brace in string", `{"cmd":"echo ${HOME} }{"}`, `{"cmd":"echo ${HOME} }{"}`},
		{"no object", `not json at all`, `not json at all`},
	}
	for _, c := range cases {
		if got := extractJSONObject(c.in); got != c.want {
			t.Errorf("%s: extractJSONObject(%q) = %q; want %q", c.name, c.in, got, c.want)
		}
	}
}

// ParseTask must succeed on a fenced / prose-wrapped Task the way a small model
// often emits it — these used to cost a THINK_FAILED retry — while still rejecting
// a genuinely incomplete Task.
func TestParseTaskToleratesWrappers(t *testing.T) {
	raw := "```json\n" +
		`{"id":"t","command":"true","mode":"blocking","timeout_sec":5,` +
		`"assertion":{"type":"exit","pattern":"0","channel":"exit_code"}}` +
		"\n```"
	task, err := ParseTask([]byte(raw))
	if err != nil {
		t.Fatalf("fenced Task should parse, got error: %v", err)
	}
	if task.ID != "t" || task.Command != "true" || task.Assertion.Channel != ChannelExitCode {
		t.Errorf("parsed Task missing fields: %+v", task)
	}

	// A response with no usable object still errors (not a silent empty Task).
	if _, err := ParseTask([]byte("sorry, I cannot help")); err == nil {
		t.Error("a response with no JSON object must return an error")
	}
	// A well-formed object that is missing required fields is still rejected.
	if _, err := ParseTask([]byte(`{"id":"x"}`)); err == nil {
		t.Error("an incomplete Task (no command/assertion) must return an error")
	}
}
