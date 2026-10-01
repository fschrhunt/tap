package tap_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMCP ports each original protocol behavior and its deadline assertions.
func TestMCP(t *testing.T) {
	t.Run("initialize identifies tap and its tools capability", func(t *testing.T) {
		h := start(t, nil)
		init := h.initialize()
		equal(t, init["serverInfo"].(map[string]any)["name"], "tap")
		if init["capabilities"].(map[string]any)["tools"] == nil {
			t.Fatal("missing tools capability")
		}
		equal(t, init["protocolVersion"], "2025-11-25")
	})
	t.Run("MCP 2026-07-28 requests get the results that revision requires; older sessions do not", func(t *testing.T) {
		h := start(t, nil)
		meta := map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
		list := h.request("tools/list", map[string]any{"_meta": meta})
		equal(t, list["resultType"], "complete")
		equal(t, list["cacheScope"], "public")
		if _, ok := list["ttlMs"].(float64); !ok {
			t.Fatalf("no ttlMs: %v", list)
		}
		equal(t, h.request("tools/call", map[string]any{"_meta": meta, "name": "plugin_search", "arguments": map[string]any{}})["resultType"], "complete")
		old := start(t, nil)
		old.initialize()
		if _, ok := old.request("tools/list", nil)["resultType"]; ok {
			t.Fatal("resultType sent in a 2025-11-25 session")
		}
	})
	t.Run("tap exposes exactly plugin_search and plugin_call", func(t *testing.T) {
		h := start(t, nil)
		h.initialize()
		r := h.request("tools/list", nil)
		tools := r["tools"].([]any)
		names := map[string]bool{}
		for _, tool := range tools {
			names[tool.(map[string]any)["name"].(string)] = true
		}
		equal(t, names, map[string]bool{"plugin_search": true, "plugin_call": true})
	})
	t.Run("plugin_search with a query returns matching ids and input schemas", func(t *testing.T) {
		h := start(t, nil)
		h.initialize()
		r := h.search(map[string]any{"query": "echo message"})
		equal(t, r["total"], float64(1))
		tool := r["matches"].([]any)[0].(map[string]any)
		equal(t, tool["id"], "fixture.echo")
		schema := tool["inputSchema"].(map[string]any)
		equal(t, schema["properties"].(map[string]any)["message"].(map[string]any)["type"], "string")
		equal(t, schema["required"], []any{"message"})
		equal(t, r["unavailable"], []any{})
	})
	t.Run("plugin_search without a query returns the server catalog", func(t *testing.T) {
		h := start(t, nil)
		h.initialize()
		equal(t, h.search(nil), map[string]any{"config": h.config, "integrations": []any{map[string]any{"server": "fixture", "tools": float64(3)}}})
	})
	t.Run("plugin_search with nonsense returns no matches and a recovery hint", func(t *testing.T) {
		h := start(t, nil)
		h.initialize()
		r := h.search(map[string]any{"query": "zzzznonexistent"})
		equal(t, r["total"], float64(0))
		equal(t, r["matches"], []any{})
		equal(t, r["unavailable"], []any{})
		contains(t, r["hint"].(string), "fewer or broader terms")
	})
	t.Run("plugin_call passes through content", func(t *testing.T) {
		h := start(t, nil)
		h.initialize()
		r := h.call("echo", map[string]any{"message": "hello from the harness"})
		equal(t, r["content"], []any{map[string]any{"type": "text", "text": "hello from the harness"}})
	})
	t.Run("plugin_call passes through isError", func(t *testing.T) {
		h := start(t, nil)
		h.initialize()
		r := h.call("fail", nil)
		equal(t, r["isError"], true)
		equal(t, r["content"], []any{map[string]any{"type": "text", "text": "fixture failure"}})
	})
	t.Run("plugin_call passes through structuredContent", func(t *testing.T) {
		h := start(t, nil)
		h.initialize()
		equal(t, h.call("data", nil)["structuredContent"], map[string]any{"count": float64(3)})
	})
	for _, mode := range []string{"connect", "list"} {
		t.Run("a hung "+mode+" becomes unavailable within the per-server deadline", func(t *testing.T) {
			h := start(t, map[string]any{"fixture": definition(), "hung": definition("--hang-" + mode)})
			h.initialize()
			began := time.Now()
			r := h.search(map[string]any{"query": "echo"})
			elapsed := time.Since(began)
			if elapsed < 450*time.Millisecond || elapsed >= 3*time.Second {
				t.Fatalf("deadline took %v", elapsed)
			}
			equal(t, r["matches"].([]any)[0].(map[string]any)["id"], "fixture.echo")
			equal(t, r["unavailable"], []any{map[string]any{"server": "hung", "error": "server deadline exceeded (500 ms)"}})
		})
	}
	t.Run("a timed-out server is retried on a later search", func(t *testing.T) {
		h := start(t, map[string]any{"fixture": definition("--hang-connect")})
		h.initialize()
		equal(t, len(h.search(map[string]any{"query": "echo"})["unavailable"].([]any)), 1)
		b := &box{t: t, config: h.config}
		b.write(map[string]any{"servers": map[string]any{"fixture": definition()}})
		r := h.search(map[string]any{"query": "echo"})
		equal(t, r["unavailable"], []any{})
		equal(t, r["matches"].([]any)[0].(map[string]any)["id"], "fixture.echo")
	})
	t.Run("MCP mode inherits downstream stderr", func(t *testing.T) {
		h := start(t, map[string]any{"fixture": definition("--noisy")})
		h.initialize()
		h.search(nil)
		contains(t, h.stderr.String(), "fixture stderr")
	})
	t.Run("tap exits on its own when the harness closes stdin", func(t *testing.T) {
		h := start(t, nil)
		h.initialize()
		h.call("echo", map[string]any{"message": "connect the stdio server first"})
		h.stdin.Close()
		select {
		case err := <-h.done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("tap had to be killed instead of exiting")
		}
	})
}

// TestSurface pins every visible tool attribute against the original Node snapshot.
func TestSurface(t *testing.T) {
	h := start(t, nil)
	h.initialize()
	b, err := os.ReadFile("testdata/surface.json")
	if err != nil {
		t.Fatal(err)
	}
	// Compared by name: the order of tools carries no meaning, and the SDK adds fields such as ttlMs
	// that tap does not decide.
	byName := func(list map[string]any) map[string]any {
		out := map[string]any{}
		for _, tool := range list["tools"].([]any) {
			out[tool.(map[string]any)["name"].(string)] = tool
		}
		return out
	}
	equal(t, byName(h.request("tools/list", nil)), byName(decode(t, string(b)).(map[string]any)))
}

// TestSearchLimit protects total-before-limit and the requested shortlist size.
func TestSearchLimit(t *testing.T) {
	h := start(t, nil)
	h.initialize()
	r := h.search(map[string]any{"query": "fixture", "limit": 1})
	equal(t, r["total"], float64(3))
	equal(t, len(r["matches"].([]any)), 1)
}

// TestSearchDefaultLimit pins the default of eight against nine matching tools.
func TestSearchDefaultLimit(t *testing.T) {
	h := start(t, map[string]any{"fixture1": definition(), "fixture2": definition(), "fixture3": definition()})
	h.initialize()
	r := h.search(map[string]any{"query": "fixture"})
	equal(t, r["total"], float64(9))
	equal(t, len(r["matches"].([]any)), 8)
}

// TestSearchValidation pins the exact input bounds and ordered validation messages.
func TestSearchValidation(t *testing.T) {
	h := start(t, nil)
	h.initialize()
	for _, c := range []struct {
		limit   any
		message string
	}{
		{0, "limit: Too small: expected number to be >=1"},
		{26, "limit: Too big: expected number to be <=25"},
		{1.5, "limit: Invalid input: expected int, received number"},
		{"2", "limit: Invalid input: expected number, received string"},
	} {
		t.Run(fmt.Sprint(c.limit), func(t *testing.T) {
			r := h.request("tools/call", map[string]any{"name": "plugin_search", "arguments": map[string]any{"limit": c.limit}})
			equal(t, r["isError"], true)
			equal(t, r["content"].([]any)[0].(map[string]any)["text"], "Input validation error: Invalid arguments for tool plugin_search: "+c.message)
		})
	}
}

// TestCallErrors protects unknown IDs and downstream validation messages.
func TestCallErrors(t *testing.T) {
	h := start(t, nil)
	h.initialize()
	for _, tool := range []string{"unknown", "echo"} {
		r := h.call(tool, nil)
		equal(t, r["isError"], true)
		text := r["content"].([]any)[0].(map[string]any)["text"].(string)
		if tool == "unknown" {
			equal(t, text, "fixture.unknown failed: Tool unknown not found. If the tool id or arguments are wrong, run plugin_search to look them up.")
		} else {
			equal(t, text, "Input validation error: Invalid arguments for tool echo: message: Invalid input: expected string, received undefined")
		}
	}
}

// TestLazyCache checks startup laziness and reuse of the one-minute tool list.
func TestLazyCache(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	h := start(t, map[string]any{"fixture": definition("--trace", trace)})
	h.initialize()
	h.request("tools/list", nil)
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatal("downstream was opened before search")
	}
	h.search(nil)
	h.search(map[string]any{"query": "echo"})
	b, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, strings.Count(string(b), "initialize\n"), 1)
	equal(t, strings.Count(string(b), "tools/list\n"), 1)
}

// TestSharedDeadline ensures initialization and listing share one budget.
func TestSharedDeadline(t *testing.T) {
	h := start(t, map[string]any{"fixture": definition("--delay-connect", "--delay-list")})
	h.initialize()
	began := time.Now()
	r := h.search(map[string]any{"query": "echo"})
	if elapsed := time.Since(began); elapsed < 450*time.Millisecond || elapsed >= 3*time.Second {
		t.Fatalf("shared deadline took %v", elapsed)
	}
	equal(t, r["unavailable"], []any{map[string]any{"server": "fixture", "error": "server deadline exceeded (500 ms)"}})
}

// TestEnvironmentAndHome checks config expansion at the child-process boundary.
func TestEnvironmentAndHome(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "work")
	if err := os.Mkdir(cwd, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fixtureBin)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, "fixture"), data, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("TAP_TEST_MODE", "expanded")
	def := map[string]any{"command": []string{"~/fixture", "--serve", "--inspect"}, "cwd": "~/work", "env": map[string]any{"MODE": "${TAP_TEST_MODE}:${UNSET_TAP_TEST}"}}
	h := start(t, map[string]any{"fixture": def})
	h.initialize()
	r := h.call("echo", map[string]any{"message": "inspect"})
	s := r["content"].([]any)[0].(map[string]any)["text"].(string)
	realCWD, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, decode(t, s), map[string]any{"cwd": realCWD, "mode": "expanded:"})
}

// TestEmptyConfigEnvironment checks that an empty TAP_CONFIG uses the home default.
func TestEmptyConfigEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cmd := exec.Command(tapBin, "path")
	cmd.Env = append(os.Environ(), "TAP_CONFIG=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	equal(t, string(out), filepath.Join(home, ".tap", "servers.json")+"\n")
}

// TestRichContent preserves content metadata; an explicit null structuredContent, which the spec does
// not allow, is dropped.
func TestRichContent(t *testing.T) {
	h := start(t, map[string]any{"fixture": definition("--rich")})
	h.initialize()
	r := h.call("data", nil)
	equal(t, r["content"], decode(t, `[{"type":"image","data":"AA==","mimeType":"image/png","annotations":{"audience":["user"]},"_meta":{"extra":true}}]`))
	if _, ok := r["structuredContent"]; ok {
		t.Fatalf("null structuredContent passed on: %v", r)
	}
	if r["isError"] == true {
		t.Fatalf("a successful call reported as an error: %v", r)
	}
}

// TestStreamableHTTP covers the HTTP transport with expanded headers and a bearer token.
func TestStreamableHTTP(t *testing.T) {
	t.Setenv("TAP_TEST_HEADER", "expanded")
	t.Setenv("TAP_TEST_TOKEN", "test-token")
	cmd := exec.Command(fixtureBin, "--serve", "--http", "--expect-header", "X-Test=expanded:", "--expect-header", "Authorization=Bearer test-token")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Wait() })
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("fixture did not announce its URL")
	}
	h := start(t, map[string]any{"fixture": map[string]any{"url": scanner.Text(), "headers": map[string]any{"X-Test": "${TAP_TEST_HEADER}:${UNSET_TAP_TEST}"}, "bearerTokenEnv": "TAP_TEST_TOKEN"}})
	h.initialize()
	equal(t, h.search(nil)["integrations"], []any{map[string]any{"server": "fixture", "tools": float64(3)}})
	equal(t, h.call("echo", map[string]any{"message": "over HTTP"})["content"], []any{map[string]any{"type": "text", "text": "over HTTP"}})
}

// TestEmptyServerCatalog preserves the original omission of servers with no tools.
func TestEmptyServerCatalog(t *testing.T) {
	h := start(t, map[string]any{"fixture": definition("--empty")})
	h.initialize()
	equal(t, h.search(nil)["integrations"], []any{})
}

// TestConfigOrder protects server insertion order when scores are tied.
func TestConfigOrder(t *testing.T) {
	h := start(t, map[string]any{})
	command, _ := json.Marshal([]string{fixtureBin, "--serve"})
	data := `{"servers":{"zeta":{"command":` + string(command) + `},"alpha":{"command":` + string(command) + `}}}`
	if err := os.WriteFile(h.config, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	h.initialize()
	matches := h.search(map[string]any{"query": "echo"})["matches"].([]any)
	equal(t, matches[0].(map[string]any)["id"], "zeta.echo")
	equal(t, matches[1].(map[string]any)["id"], "alpha.echo")
}

// TestShutdownPendingCall ensures EOF tears down every child even with an active call.
func TestShutdownPendingCall(t *testing.T) {
	dir := t.TempDir()
	trace := filepath.Join(dir, "trace")
	closed := filepath.Join(dir, "closed")
	h := start(t, map[string]any{"fixture": definition("--hang-call", "--trace", trace, "--exit-file", closed)})
	h.initialize()
	h.search(nil)
	fmt.Fprintln(h.stdin, `{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"plugin_call","arguments":{"tool":"fixture.echo","arguments":{"message":"hang"}}}}`)
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, _ := os.ReadFile(trace)
		if strings.Contains(string(b), "tools/call\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("downstream call did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.stdin.Close()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("EOF did not stop active call")
	}
	b, err := os.ReadFile(closed)
	if err != nil {
		t.Fatal("downstream was not closed:", err)
	}
	equal(t, string(b), "closed\n")
}

// TestCommandEnvironmentPath resolves a bare command using its configured environment.
func TestCommandEnvironmentPath(t *testing.T) {
	dir := t.TempDir()
	data, err := os.ReadFile(fixtureBin)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "tap-env-fixture"), data, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAP_FIXTURE_PATH", dir)
	h := start(t, map[string]any{"fixture": map[string]any{"command": []string{"tap-env-fixture", "--serve"}, "env": map[string]any{"PATH": "${TAP_FIXTURE_PATH}"}}})
	h.initialize()
	equal(t, h.call("echo", map[string]any{"message": "custom PATH"})["content"], []any{map[string]any{"type": "text", "text": "custom PATH"}})
}
