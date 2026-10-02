package tap_test

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCLI ports each original CLI behavior without weakening its assertions.
func TestCLI(t *testing.T) {
	t.Run("version reads binary version", func(t *testing.T) { equal(t, output(t, sandbox(t).run("version")), "dev") })
	t.Run("path honors TAP_CONFIG", func(t *testing.T) { b := sandbox(t); equal(t, output(t, b.run("path")), b.config) })
	t.Run("add stores an HTTP server with headers and bearerTokenEnv", func(t *testing.T) {
		b := sandbox(t)
		output(t, b.run("add", "remote", "https://example.invalid/mcp", "--header", "X-Trace=on", "--bearer-token-env", "TEST_TOKEN"))
		equal(t, b.read(), decode(t, `{"servers":{"remote":{"type":"http","url":"https://example.invalid/mcp","headers":{"X-Trace":"on"},"bearerTokenEnv":"TEST_TOKEN"}}}`))
	})
	t.Run("add stores a stdio command, environment, and cwd", func(t *testing.T) {
		b := sandbox(t)
		output(t, b.run("add", "fixture", "--env", "MODE=test", "--cwd", ".", "--", fixtureBin, "--serve"))
		equal(t, b.read(), decode(t, `{"servers":{"fixture":{"type":"stdio","command":["`+fixtureBin+`","--serve"],"env":{"MODE":"test"},"cwd":"."}}}`))
	})
	t.Run("add rejects dots because tool ids use server.tool", func(t *testing.T) {
		r := sandbox(t).run("add", "bad.name", "https://example.invalid/mcp")
		equal(t, r.status, 1)
		contains(t, r.stderr, "cannot contain a dot: tool ids use server.tool")
	})
	t.Run("saved configs are owner-only", func(t *testing.T) {
		b := sandbox(t)
		output(t, b.run("add", "remote", "https://example.invalid/mcp"))
		st, err := os.Stat(b.config)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, st.Mode().Perm(), os.FileMode(0600))
	})
	t.Run("remove deletes the named server", func(t *testing.T) {
		b := sandbox(t)
		output(t, b.run("add", "remote", "https://example.invalid/mcp"))
		equal(t, output(t, b.run("remove", "remote")), "removed remote")
		equal(t, b.read(), decode(t, `{"servers":{}}`))
	})
	t.Run("list reports fixture tools", func(t *testing.T) {
		b := sandbox(t)
		output(t, b.run("add", "fixture", "--", fixtureBin, "--serve"))
		equal(t, decode(t, output(t, b.run("list", "--json"))), map[string]any{"config": b.config, "integrations": []any{map[string]any{"server": "fixture", "tools": float64(3)}}})
	})
	for _, command := range []string{"list", "search", "call"} {
		t.Run(command+" drains server stderr", func(t *testing.T) {
			b := sandbox(t)
			output(t, b.run("add", "fixture", "--", fixtureBin, "--serve", "--noisy"))
			args := []string{command}
			if command == "search" {
				args = append(args, "echo")
			}
			if command == "call" {
				args = append(args, "fixture.echo", "message=hello")
			}
			r := b.run(append(args, "--json")...)
			body := decode(t, output(t, r)).(map[string]any)
			equal(t, r.stderr, "")
			if command == "list" {
				equal(t, body["integrations"].([]any)[0].(map[string]any)["tools"], float64(3))
			}
			if command == "search" {
				equal(t, body["matches"].([]any)[0].(map[string]any)["id"], "fixture.echo")
				equal(t, body["unavailable"], []any{})
			}
		})
	}
	for _, alias := range []string{"serve", "mcp"} {
		t.Run(alias+" is rejected as an unknown command", func(t *testing.T) {
			r := sandbox(t).run(alias)
			equal(t, r.status, 1)
			contains(t, r.stdout, "tap list")
		})
	}
	t.Run("local server type is rejected", func(t *testing.T) {
		b := sandbox(t)
		def := definition()
		def["type"] = "local"
		b.write(map[string]any{"servers": map[string]any{"fixture": def}})
		r := decode(t, output(t, b.run("list", "--json"))).(map[string]any)
		contains(t, r["integrations"].([]any)[0].(map[string]any)["error"].(string), `unsupported type "local"`)
	})
	t.Run("corrupt config errors name the file", func(t *testing.T) {
		b := sandbox(t)
		if err := os.WriteFile(b.config, []byte("{ invalid"), 0600); err != nil {
			t.Fatal(err)
		}
		r := b.run("list")
		equal(t, r.status, 1)
		contains(t, r.stderr, b.config)
	})
	t.Run("add keeps everything after -- as the server command", func(t *testing.T) {
		b := sandbox(t)
		output(t, b.run("add", "fs", "--", "some-server", "--cwd", "/srv", "--json"))
		equal(t, b.read(), decode(t, `{"servers":{"fs":{"type":"stdio","command":["some-server","--cwd","/srv","--json"]}}}`))
	})
}

// TestConfigSymlink protects write-through and exact owner-only config serialization.
func TestConfigSymlink(t *testing.T) {
	b := sandbox(t)
	target := filepath.Join(t.TempDir(), "real.json")
	if err := os.WriteFile(target, []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, b.config); err != nil {
		t.Fatal(err)
	}
	output(t, b.run("add", "remote", "https://example.invalid/mcp"))
	st, err := os.Lstat(b.config)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("config symlink was replaced")
	}
	data, _ := os.ReadFile(target)
	equal(t, string(data), "{\n  \"servers\": {\n    \"remote\": {\n      \"type\": \"http\",\n      \"url\": \"https://example.invalid/mcp\"\n    }\n  }\n}\n")
	output(t, b.run("remove", "remote"))
	data, _ = os.ReadFile(target)
	equal(t, string(data), "{\n  \"servers\": {}\n}\n")
}

// TestCLIArguments protects merging JSON input with string overrides.
func TestCLIArguments(t *testing.T) {
	b := sandbox(t)
	b.write(map[string]any{"servers": map[string]any{"fixture": definition()}})
	equal(t, output(t, b.run("call", "fixture.echo", "--args", `{"message":"json"}`, "message=override")), "override")
}

// TestToolErrorExitCode preserves the original tool-error exit code and diagnostic.
func TestToolErrorExitCode(t *testing.T) {
	b := sandbox(t)
	b.write(map[string]any{"servers": map[string]any{"fixture": definition()}})
	equal(t, b.run("call", "fixture.fail"), result{0, "fixture failure\n", "tap: fixture.fail reported an error\n"})
}

// TestJSONDiagnostics pins the original diagnostics for malformed config and arguments.
func TestJSONDiagnostics(t *testing.T) {
	cases := []struct{ input, message string }{
		{"{ invalid", "Expected property name or '}' in JSON at position 2 (line 1 column 3)"},
		{`{"a":1,}`, "Expected double-quoted property name in JSON at position 7 (line 1 column 8)"},
		{`{"a" 1}`, "Expected ':' after property name in JSON at position 5 (line 1 column 6)"},
		{"[1", "Expected ',' or ']' after array element in JSON at position 2 (line 1 column 3)"},
		{`{"a":true`, "Expected ',' or '}' after property value in JSON at position 9 (line 1 column 10)"},
		{"1e", "Exponent part is missing a number in JSON at position 2 (line 1 column 3)"},
		{`{"a":"unterminated}`, "Unterminated string in JSON at position 19 (line 1 column 20)"},
		{"undefined", `"undefined" is not valid JSON`},
		{"{}x", "Unexpected non-whitespace character after JSON at position 2 (line 1 column 3)"},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			b := sandbox(t)
			if err := os.WriteFile(b.config, []byte(c.input), 0600); err != nil {
				t.Fatal(err)
			}
			equal(t, b.run("list"), result{1, "", "tap: " + b.config + " is not valid JSON: " + c.message + "\n"})
			equal(t, b.run("call", "fixture.echo", "--args", c.input), result{1, "", "tap: " + c.message + "\n"})
		})
	}
}

// TestCLIUnboundedLimit preserves JavaScript slice behavior for shell search limits.
func TestCLIUnboundedLimit(t *testing.T) {
	b := sandbox(t)
	b.write(map[string]any{"servers": map[string]any{"fixture": definition()}})
	for _, c := range []struct {
		limit string
		count int
	}{{"-1", 2}, {"0", 0}, {"nonsense", 0}, {"0x2", 2}} {
		t.Run(c.limit, func(t *testing.T) {
			r := decode(t, output(t, b.run("search", "fixture", "--limit", c.limit, "--json"))).(map[string]any)
			equal(t, r["total"], float64(3))
			equal(t, len(r["matches"].([]any)), c.count)
		})
	}
}

// TestLiteralUnicodeEscape protects JSON serialization of text containing literal escape syntax.
func TestLiteralUnicodeEscape(t *testing.T) {
	b := sandbox(t)
	b.write(map[string]any{"servers": map[string]any{"fixture": definition()}})
	r := decode(t, output(t, b.run("call", "fixture.echo", `message=\u2028`, "--json"))).(map[string]any)
	equal(t, r["content"], []any{map[string]any{"type": "text", "text": `\u2028`}})
}

// TestHelp keeps the command and flag reference in sync with CLI help.
func TestHelp(t *testing.T) {
	data, err := os.ReadFile("testdata/help.txt")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, sandbox(t).run("help"), result{0, string(data), ""})
}

// TestDisconnectedServer reports a peer that exits during initialization as unavailable.
func TestDisconnectedServer(t *testing.T) {
	b := sandbox(t)
	b.write(map[string]any{"servers": map[string]any{"fixture": definition("--exit")}})
	r := decode(t, output(t, b.run("list", "--json"))).(map[string]any)
	equal(t, r["integrations"], []any{map[string]any{"server": "fixture", "tools": float64(0), "error": "Connection closed"}})
}
