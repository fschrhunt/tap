package tap_test

import "testing"

// TestRemovedServerRejectsResidentCalls prevents a removed connector remaining callable.
func TestRemovedServerRejectsResidentCalls(t *testing.T) {
	h := start(t, nil)
	h.initialize()
	h.call("echo", map[string]any{"message": "open the resident session"})
	(&box{t: t, config: h.config}).write(map[string]any{"servers": map[string]any{}})
	r := h.call("echo", map[string]any{"message": "must not reach the old session"})
	equal(t, r["isError"], true)
	contains(t, r["content"].([]any)[0].(map[string]any)["text"].(string), "unknown server")
}

// TestChangedServerUsesNewDefinition checks direct calls, without a search-triggered reload.
func TestChangedServerUsesNewDefinition(t *testing.T) {
	h := start(t, nil)
	h.initialize()
	h.call("echo", map[string]any{"message": "open the original session"})
	def := definition("--inspect")
	def["env"] = map[string]any{"MODE": "replacement"}
	(&box{t: t, config: h.config}).write(map[string]any{"servers": map[string]any{"fixture": def}})
	r := h.call("echo", map[string]any{"message": "inspect"})
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	equal(t, decode(t, text).(map[string]any)["mode"], "replacement")
}
