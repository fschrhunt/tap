package tap_test

import (
	"fmt"
	"os"
	"testing"
)

// BenchmarkDiscoveryCall measures the public stdio path, including both MCP hops.
// Cold sessions start tap and its backend; warm sessions reuse both connections.
func BenchmarkDiscoveryCall(b *testing.B) {
	b.Setenv("TAP_REFERENCES", "off")
	for _, cold := range []bool{true, false} {
		name := "warm"
		if cold {
			name = "cold"
		}
		b.Run(name, func(b *testing.B) {
			box := sandbox(b)
			box.write(map[string]any{"servers": map[string]any{"fixture": definition()}})
			var h *harness
			// Retain only the current session, rather than every cold sample's
			// buffers and channels until benchmark cleanup.
			b.Cleanup(func() {
				if h != nil {
					h.close()
				}
			})
			exercise := func(message string) {
				matches := h.search(map[string]any{"server": "fixture", "query": "echo"})["matches"].([]any)
				if len(matches) == 0 {
					b.Fatal("echo was not discovered")
				}
				id := matches[0].(map[string]any)["id"]
				equal(b, id, "fixture.echo")
				r := h.request("tools/call", map[string]any{"name": "plugin_call", "arguments": map[string]any{
					"tool": id, "arguments": map[string]any{"message": message},
				}})
				if r["isError"] == true {
					b.Fatalf("call failed: %v", r)
				}
				equal(b, r["content"].([]any)[0].(map[string]any)["text"], message)
			}
			if !cold {
				h = startBox(b, box)
				h.initialize()
				exercise("prime")
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if cold {
					b.StopTimer()
					if err := os.Remove(box.config + ".tools.json"); err != nil && !os.IsNotExist(err) {
						b.Fatal(err)
					}
					b.StartTimer()
					h = startBox(b, box)
					h.initialize()
				}
				exercise(fmt.Sprintf("sample-%d", i))
				if cold {
					b.StopTimer()
					h.close()
					b.StartTimer()
				}
			}
		})
	}
}
