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
			equal(t, r.status, 2)
			equal(t, r.stdout, "")
			contains(t, r.stderr, `there is no "`+alias+`" command`)
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

// TestToolErrorExitCode pins that a tool's own error fails the command, with what it said on stdout.
func TestToolErrorExitCode(t *testing.T) {
	b := sandbox(t)
	b.write(map[string]any{"servers": map[string]any{"fixture": definition()}})
	equal(t, b.run("call", "fixture.fail"), result{1, "fixture failure\n", "tap: fixture.fail reported an error\n"})
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
			r := b.run("call", "fixture.echo", "--args", c.input)
			equal(t, r.status, 2)
			contains(t, r.stderr, "tap: --args is not valid JSON: "+c.message+"\n")
		})
	}
}

// TestCLISearchLimit pins that --limit caps the matches and refuses what is not a count.
func TestCLISearchLimit(t *testing.T) {
	b := sandbox(t)
	b.write(map[string]any{"servers": map[string]any{"fixture": definition()}})
	r := decode(t, output(t, b.run("search", "fixture", "--limit", "2", "--json"))).(map[string]any)
	equal(t, r["total"], float64(3))
	equal(t, len(r["matches"].([]any)), 2)
	for _, limit := range []string{"0", "-1", "nonsense", "1.5"} {
		refused := b.run("search", "fixture", "--limit", limit)
		equal(t, refused.status, 2)
		contains(t, refused.stderr, "--limit takes a whole number of at least 1")
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

// TestMisuseSaysWhatToTypeInstead pins how a wrongly called command answers: the mistake,
// the command's usage and where its examples are, on stderr, with exit code 2.
func TestMisuseSaysWhatToTypeInstead(t *testing.T) {
	b := sandbox(t)
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"serach", "x"}, `there is no "serach" command. Did you mean "search"?`},
		{[]string{"list", "--jsno"}, "list has no flag --jsno. Did you mean --json?"},
		{[]string{"add", "docs", "https://example.invalid/mcp", "--bearer-token-env"}, "--bearer-token-env needs a value"},
		{[]string{"add", "docs", "example.invalid"}, `"example.invalid" is not an http or https address`},
		{[]string{"search"}, "search needs something to look for\n\nUsage: tap search QUERY... [--limit N] [--json]\nRun \"tap search --help\" for examples."},
		{[]string{"call", "echo"}, `"echo" is not a tool id`},
	} {
		r := b.run(c.args...)
		equal(t, r.status, 2)
		equal(t, r.stdout, "")
		contains(t, r.stderr, c.says)
	}
	if _, err := os.Stat(b.config); err == nil {
		t.Fatal("a refused command wrote the config")
	}
}

// TestHelpFlagShowsACommandsHelpWithoutRunningIt pins that -h and --help anywhere mean help.
func TestHelpFlagShowsACommandsHelpWithoutRunningIt(t *testing.T) {
	b := sandbox(t)
	for _, args := range [][]string{{"add", "docs", "https://example.invalid/mcp", "--help"}, {"add", "-h"}, {"help", "add"}} {
		r := b.run(args...)
		equal(t, r.status, 0)
		contains(t, r.stdout, "tap add files -- npx -y @modelcontextprotocol/server-filesystem ~/notes")
	}
	if _, err := os.Stat(b.config); err == nil {
		t.Fatal("asking for help added a server")
	}
}

// TestStateChangesAreSaidAndMissingOnesFail pins what add and remove report.
func TestStateChangesAreSaidAndMissingOnesFail(t *testing.T) {
	b := sandbox(t)
	equal(t, output(t, b.run("add", "docs", "https://example.invalid/mcp")), "added docs (http)")
	equal(t, output(t, b.run("add", "docs", "--", "some-server")), "replaced docs (stdio)")
	equal(t, output(t, b.run("remove", "docs")), "removed docs")
	r := b.run("remove", "docs")
	equal(t, r.status, 1)
	contains(t, r.stderr, `there is no server named "docs"`)
}

// TestListExplainsUnavailableServers pins the reasons a person reads, without the layers
// an error passed through.
func TestListExplainsUnavailableServers(t *testing.T) {
	b := sandbox(t)
	b.write(map[string]any{"servers": map[string]any{"fixture": definition(), "missing": map[string]any{"type": "stdio", "command": []string{"/nonexistent/tap-test-server"}}}})
	equal(t, output(t, b.run("list")), "fixture  3 tools\nmissing  unavailable: its command was not found: /nonexistent/tap-test-server")
}

// TestImport pins that an agent's servers are added once, what is left behind is explained,
// and a dry run changes nothing.
func TestImport(t *testing.T) {
	b := sandbox(t)
	source := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(source, []byte(`{"mcpServers":{
		"files":{"command":"`+fixtureBin+`","args":["--serve"]},
		"docs":{"type":"http","url":"https://docs.example.invalid/mcp"},
		"old":{"type":"sse","url":"https://old.example.invalid/sse"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	dry := output(t, b.run("import", source, "--dry-run"))
	contains(t, dry, "would add files (stdio)")
	if _, err := os.Stat(b.config); err == nil {
		t.Fatal("a dry run wrote the config")
	}
	added := output(t, b.run("import", source))
	contains(t, added, "added files (stdio)")
	contains(t, added, "added docs (http)")
	contains(t, added, "skipped old: it uses the sse transport")
	equal(t, b.read(), decode(t, `{"servers":{"files":{"type":"stdio","command":["`+fixtureBin+`","--serve"]},"docs":{"type":"http","url":"https://docs.example.invalid/mcp"}}}`))
	contains(t, output(t, b.run("import", source)), "has files already")
	output(t, b.run("add", "docs", "https://other.example.invalid/mcp"))
	contains(t, output(t, b.run("import", source)), "skipped docs: tap has another server by that name; --force replaces it")
	contains(t, output(t, b.run("import", source, "--force")), "added docs (http)")
}
