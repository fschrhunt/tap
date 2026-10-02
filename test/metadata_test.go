package tap_test

import "testing"

// TestResultMetadata protects connector metadata through tap's SDK result wrapper.
func TestResultMetadata(t *testing.T) {
	h := start(t, map[string]any{"fixture": definition("--result-meta")})
	h.initialize()
	equal(t, h.call("data", nil)["_meta"], map[string]any{"trace": "fixture-result"})
}

// TestSearchOutputSchema keeps the result contract available alongside input schemas.
func TestSearchOutputSchema(t *testing.T) {
	h := start(t, map[string]any{"fixture": definition("--output-schema")})
	h.initialize()
	r := h.search(map[string]any{"query": "data"})
	equal(t, r["matches"].([]any)[0].(map[string]any)["outputSchema"], map[string]any{"type": "object"})
}

// TestResultIdentityBelongsToTap rejects connector identity metadata while preserving application data.
func TestResultIdentityBelongsToTap(t *testing.T) {
	h := start(t, map[string]any{"fixture": definition("--spoof-identity")})
	r := h.request("tools/call", map[string]any{
		"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}},
		"name":  "plugin_call", "arguments": map[string]any{"tool": "fixture.data"},
	})
	meta := r["_meta"].(map[string]any)
	equal(t, meta["trace"], "fixture-result")
	equal(t, meta["io.modelcontextprotocol/serverInfo"].(map[string]any)["name"], "tap")
}

// TestCallMetadataUsesIndependentProtocolHops forwards application metadata but not negotiation or progress.
func TestCallMetadataUsesIndependentProtocolHops(t *testing.T) {
	h := start(t, map[string]any{"fixture": definition("--request-meta")})
	r := h.request("tools/call", map[string]any{
		"_meta": map[string]any{
			"trace": "client-request", "progressToken": "upstream-progress",
			"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{},
		},
		"name": "plugin_call", "arguments": map[string]any{"tool": "fixture.data"},
	})
	equal(t, r["structuredContent"], map[string]any{"trace": "client-request"})
}
