package agents

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fschrhunt/tap/internal/wire"
)

// read writes one agent's config and returns what Read makes of it, as JSON.
func read(t *testing.T, name, body string) (servers string, skipped []Skipped) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Read(Place{ID: "test", Agent: "Agent", Path: path}, "/work/app")
	if err != nil {
		t.Fatal(err)
	}
	all := wire.Object{}
	for _, s := range c.Servers {
		all.Set(s.Name, s.Def)
	}
	b, _ := wire.JSON(all, false)
	return string(b), c.Skipped
}

// TestReadTakesEachAgentsServers pins how every agent's own shape becomes tap's.
func TestReadTakesEachAgentsServers(t *testing.T) {
	for _, c := range []struct{ name, file, body, want string }{
		{"Claude Code, with a project's servers", ".claude.json",
			`{"mcpServers":{"files":{"command":"npx","args":["-y","server-filesystem","~/notes"],"env":{"MODE":"read"}}},
			  "projects":{"/work/app":{"mcpServers":{"docs":{"type":"http","url":"https://docs.example.com/mcp","headers":{"X-Client":"claude"}}}},
			              "/other":{"mcpServers":{"elsewhere":{"command":"nope"}}}}}`,
			`{"files":{"type":"stdio","command":["npx","-y","server-filesystem","~/notes"],"env":{"MODE":"read"}},"docs":{"type":"http","url":"https://docs.example.com/mcp","headers":{"X-Client":"claude"}}}`},
		{"Codex", "config.toml",
			"model = \"gpt\"\n[mcp_servers.db]\ncommand = \"node\"\nargs = [\"server.mjs\"]\ncwd = \"~/code/app\"\n[mcp_servers.db.env]\nDATABASE_URL = \"${DATABASE_URL}\"\n" +
				"[mcp_servers.issues]\nurl = \"https://issues.example.com/mcp\"\nbearer_token_env_var = \"ISSUES_TOKEN\"\n[mcp_servers.issues.env_http_headers]\nX-Org = \"ORG_ID\"\n",
			`{"db":{"type":"stdio","command":["node","server.mjs"],"env":{"DATABASE_URL":"${DATABASE_URL}"},"cwd":"~/code/app"},"issues":{"type":"http","url":"https://issues.example.com/mcp","headers":{"X-Org":"${ORG_ID}"},"bearerTokenEnv":"ISSUES_TOKEN"}}`},
		{"OpenCode, with comments and a trailing comma", "opencode.jsonc",
			"{\n  // servers\n  \"mcp\": {\n    \"browser\": { \"type\": \"local\", \"command\": [\"npx\", \"@playwright/mcp\"], \"environment\": { \"HEADLESS\": \"1\" } },\n" +
				"    \"search\": { \"type\": \"remote\", \"url\": \"https://search.example.com/mcp\" }, /* last */\n  },\n}\n",
			`{"browser":{"type":"stdio","command":["npx","@playwright/mcp"],"env":{"HEADLESS":"1"}},"search":{"type":"http","url":"https://search.example.com/mcp"}}`},
		{"VS Code", "mcp.json", `{"servers":{"my.files":{"type":"stdio","command":"srv"}}}`, `{"my-files":{"type":"stdio","command":["srv"]}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, skipped := read(t, c.file, c.body); got != c.want || len(skipped) != 0 {
				t.Errorf("servers = %s, skipped %v\nwant      %s", got, skipped, c.want)
			}
		})
	}
}

// TestReadSkipsWhatTapCannotTakeOver pins the servers left behind, each with its reason.
func TestReadSkipsWhatTapCannotTakeOver(t *testing.T) {
	servers, skipped := read(t, "mcp.json", `{"mcpServers":{
		"tap":{"command":"/usr/local/bin/tap"},
		"old":{"type":"sse","url":"https://old.example.com/sse"},
		"off":{"command":"srv","disabled":true},
		"empty":{}}}`)
	want := []Skipped{{"tap", "it is tap itself"}, {"old", "it uses the sse transport, and tap speaks Streamable HTTP"}, {"off", "it is turned off in Agent"}, {"empty", "it has neither a command nor a url"}}
	if servers != "{}" || len(skipped) != len(want) {
		t.Fatalf("servers = %s, skipped = %v; want none taken and %v", servers, skipped, want)
	}
	for i := range want {
		if skipped[i] != want[i] {
			t.Errorf("skipped[%d] = %v, want %v", i, skipped[i], want[i])
		}
	}
}
