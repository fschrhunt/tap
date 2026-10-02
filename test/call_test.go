package tap_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// invokeCall exercises all plugin_call operations without prefixing fixture IDs.
func invokeCall(h *harness, args map[string]any) map[string]any {
	h.t.Helper()
	return h.request("tools/call", map[string]any{"name": "plugin_call", "arguments": args})
}

// failureCode asserts an actionable gateway failure, not a downstream tool error.
func failureCode(t *testing.T, r map[string]any, want string) {
	t.Helper()
	equal(t, r["isError"], true)
	equal(t, r["structuredContent"].(map[string]any)["code"], want)
}

// TestPreCallValidation refuses invalid input before sending tools/call, without echoing secrets.
func TestPreCallValidation(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	h := start(t, map[string]any{"fixture": definition("--trace", trace)})
	h.initialize()
	r := h.call("echo", map[string]any{"message": []string{"argument-secret"}})
	failureCode(t, r, "invalid_arguments")
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	contains(t, text, "/message: expected string")
	if strings.Contains(text, "argument-secret") {
		t.Fatal("invalid value leaked")
	}
	data, _ := os.ReadFile(trace)
	if strings.Contains(string(data), "tools/call") {
		t.Fatal("invalid call reached backend")
	}
}

// TestRemoteSchemaRefusal never contacts an external schema URL or sends the operation.
func TestRemoteSchemaRefusal(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	h := start(t, map[string]any{"fixture": definition("--remote-schema", "--trace", trace)})
	h.initialize()
	failureCode(t, h.call("echo", map[string]any{"message": "hi"}), "schema_unavailable")
	data, _ := os.ReadFile(trace)
	if strings.Contains(string(data), "tools/call") {
		t.Fatal("unvalidated call reached backend")
	}
}

// TestPolicyDenial enforces deny-over-allow without even starting the target process.
func TestPolicyDenial(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	def := definition("--trace", trace)
	def["policy"] = map[string]any{"allow": []string{"*"}, "deny": []string{"echo"}}
	h := start(t, map[string]any{"fixture": def})
	h.initialize()
	failureCode(t, h.call("echo", map[string]any{"message": "hi"}), "permission_denied")
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatal("denied call started the backend")
	}
}

// TestMalformedPolicy fails closed instead of treating a mistaken allow rule as unrestricted.
func TestMalformedPolicy(t *testing.T) {
	def := definition()
	def["policy"] = map[string]any{"allow": "echo"}
	h := start(t, map[string]any{"fixture": def})
	h.initialize()
	failureCode(t, h.call("echo", map[string]any{"message": "hi"}), "invalid_policy")
}

// TestResultInspection pages full retained data, distinguishes null, then releases the reference.
func TestResultInspection(t *testing.T) {
	h := start(t, map[string]any{"fixture": definition("--large-result")})
	h.initialize()
	r := invokeCall(h, map[string]any{"tool": "fixture.data", "resultMode": "reference"})
	ref := r["structuredContent"].(map[string]any)["reference"].(string)
	read := func(args map[string]any) map[string]any {
		r := invokeCall(h, args)
		if r["isError"] == true {
			t.Fatalf("inspect: %v", r)
		}
		return decode(t, r["content"].([]any)[0].(map[string]any)["text"].(string)).(map[string]any)
	}
	page := read(map[string]any{"operation": "inspect", "reference": ref, "pointer": "/structuredContent/rows", "offset": 10, "limit": 2})
	equal(t, page["total"], float64(1000))
	equal(t, page["nextOffset"], float64(12))
	equal(t, page["value"].([]any)[0].(map[string]any)["index"], float64(10))
	page = read(map[string]any{"operation": "inspect", "reference": ref, "pointer": "/structuredContent/a~1b~0c"})
	value, present := page["value"]
	if !present || value != nil {
		t.Fatal("JSON null was lost")
	}
	read(map[string]any{"operation": "drop", "reference": ref})
	failureCode(t, invokeCall(h, map[string]any{"operation": "inspect", "reference": ref}), "reference_unavailable")
}

// TestExplicitReferenceCopy forwards a selected value without requiring model recitation.
func TestExplicitReferenceCopy(t *testing.T) {
	h := start(t, map[string]any{"fixture": definition("--large-result")})
	h.initialize()
	r := invokeCall(h, map[string]any{"tool": "fixture.data", "resultMode": "reference"})
	ref := r["structuredContent"].(map[string]any)["reference"].(string)
	r = invokeCall(h, map[string]any{"tool": "fixture.echo", "argumentRefs": []any{map[string]any{"target": "/message", "reference": ref, "pointer": "/structuredContent/text"}}})
	equal(t, r["content"].([]any)[0].(map[string]any)["text"], "copied without model recitation")
}

// TestReferenceFlow requires a source-side grant before transferring data between servers.
func TestReferenceFlow(t *testing.T) {
	h := start(t, map[string]any{"fixture": definition("--large-result"), "target": definition()})
	h.initialize()
	r := invokeCall(h, map[string]any{"tool": "fixture.data", "resultMode": "reference"})
	ref := r["structuredContent"].(map[string]any)["reference"].(string)
	args := map[string]any{"tool": "target.echo", "argumentRefs": []any{map[string]any{"target": "/message", "reference": ref, "pointer": "/structuredContent/text"}}}
	failureCode(t, invokeCall(h, args), "reference_flow_denied")
	def := definition("--large-result")
	def["policy"] = map[string]any{"referenceTo": []string{"target"}}
	b := &box{t: t, config: h.config}
	b.write(map[string]any{"servers": map[string]any{"fixture": def, "target": definition()}})
	r = invokeCall(h, args)
	equal(t, r["content"].([]any)[0].(map[string]any)["text"], "copied without model recitation")
}

// TestReferencedError preserves the error flag and makes original failure content retrievable.
func TestReferencedError(t *testing.T) {
	h := start(t, nil)
	h.initialize()
	r := invokeCall(h, map[string]any{"tool": "fixture.fail", "resultMode": "reference"})
	equal(t, r["isError"], true)
	ref := r["structuredContent"].(map[string]any)["reference"].(string)
	r = invokeCall(h, map[string]any{"operation": "inspect", "reference": ref, "pointer": "/content/0/text"})
	data := decode(t, r["content"].([]any)[0].(map[string]any)["text"].(string)).(map[string]any)
	equal(t, data["value"], "fixture failure")
}

// TestUnknownOutcome never retries an operation that failed after downstream execution.
func TestUnknownOutcome(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	h := start(t, map[string]any{"fixture": definition("--protocol-error", "--trace", trace)})
	h.initialize()
	failureCode(t, h.call("data", nil), "call_outcome_unknown")
	raw, _ := os.ReadFile(trace)
	equal(t, strings.Count(string(raw), "tools/call\n"), 1)
}

// TestConcurrentCalls protects request correlation and shared initialization during a burst.
func TestConcurrentCalls(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	h := start(t, map[string]any{"fixture": definition("--trace", trace)})
	h.initialize()
	const count = 16
	for i := 0; i < count; i++ {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 100 + i, "method": "tools/call", "params": map[string]any{"name": "plugin_call", "arguments": map[string]any{"tool": "fixture.echo", "arguments": map[string]any{"message": fmt.Sprint(i)}}}})
		if _, err := fmt.Fprintln(h.stdin, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[int]bool{}
	timer := time.NewTimer(12 * time.Second)
	defer timer.Stop()
	for i := 0; i < count; i++ {
		select {
		case msg := <-h.replies:
			id := int(msg["id"].(float64)) - 100
			if id < 0 || id >= count || seen[id] {
				t.Fatalf("bad correlation %v", msg)
			}
			seen[id] = true
			if msg["error"] != nil {
				t.Fatalf("RPC error: %v", msg)
			}
			r := msg["result"].(map[string]any)
			if r["isError"] == true {
				t.Fatalf("concurrent call: %v", r)
			}
			equal(t, r["content"].([]any)[0].(map[string]any)["text"], fmt.Sprint(id))
		case <-timer.C:
			t.Fatal("concurrent calls timed out")
		}
	}
	raw, _ := os.ReadFile(trace)
	equal(t, strings.Count(string(raw), "initialize\n"), 1)
}

// TestNumericRoundTrip preserves large IDs through inline results, inspection and reference copies.
func TestNumericRoundTrip(t *testing.T) {
	h := start(t, map[string]any{"fixture": definition("--numeric")})
	h.initialize()
	id := json.Number("9007199254740993")
	r := invokeCall(h, map[string]any{"tool": "fixture.data", "arguments": map[string]any{"id": id}, "resultMode": "reference"})
	ref := r["structuredContent"].(map[string]any)["reference"].(string)
	r = invokeCall(h, map[string]any{"operation": "inspect", "reference": ref, "pointer": "/structuredContent/id"})
	contains(t, r["content"].([]any)[0].(map[string]any)["text"].(string), "9007199254740993")
	r = invokeCall(h, map[string]any{"tool": "fixture.data", "argumentRefs": []any{map[string]any{"target": "/id", "reference": ref, "pointer": "/structuredContent/id"}}, "resultMode": "reference"})
	next := r["structuredContent"].(map[string]any)["reference"].(string)
	r = invokeCall(h, map[string]any{"operation": "inspect", "reference": next, "pointer": "/structuredContent/id"})
	contains(t, r["content"].([]any)[0].(map[string]any)["text"].(string), "9007199254740993")
	// The harness decodes structured JSON to float64, so pin inline precision with raw CLI output.
	b := sandbox(t)
	b.write(map[string]any{"servers": map[string]any{"fixture": definition("--numeric")}})
	contains(t, output(t, b.run("call", "fixture.data", "--args", `{"id":9007199254740993}`, "--json")), "9007199254740993")
}
