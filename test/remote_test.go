package tap_test

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/fschrhunt/tap/internal/remote"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestRemoteStdioRelay verifies unchanged harness startup adopts connectors added through the remote CLI.
func TestRemoteStdioRelay(t *testing.T) {
	t.Setenv("TAP_REMOTE_TOKEN", "test-relay-token")
	t.Setenv("TAP_REMOTE_ADMIN_TOKEN", "")
	hostConfig := sandbox(t)
	handler, close, err := remote.Handler(hostConfig.config, "test", "test-relay-token", "")
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	host := httptest.NewServer(handler)
	defer host.Close()
	relay := sandbox(t)
	relay.write(map[string]any{"servers": map[string]any{"ignored": definition()}, "remote": map[string]any{"url": host.URL, "tokenEnv": "TAP_REMOTE_TOKEN"}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.Command(tapBin)
	cmd.Env = append(os.Environ(), "TAP_CONFIG="+relay.config)
	client := mcp.NewClient(&mcp.Implementation{Name: "unchanged-harness", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 2 {
		t.Fatalf("surface: %v %v", tools, err)
	}
	search := func(query string) map[string]any {
		t.Helper()
		args := map[string]any{}
		if query != "" {
			args["query"] = query
		}
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "plugin_search", Arguments: args})
		if err != nil || result.IsError {
			t.Fatalf("search: %v %v", result, err)
		}
		return decode(t, result.Content[0].(*mcp.TextContent).Text).(map[string]any)
	}
	equal(t, search("")["integrations"], []any{})
	output(t, relay.run("add", "fixture", "--", fixtureBin, "--serve"))
	equal(t, search("echo")["matches"].([]any)[0].(map[string]any)["id"], "fixture.echo")
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "plugin_call", Arguments: map[string]any{"tool": "fixture.echo", "arguments": map[string]any{"message": "through remote"}}})
	if err != nil || result.IsError {
		t.Fatalf("call: %v %v content=%+v", result, err, result.Content[0])
	}
	equal(t, result.Content[0].(*mcp.TextContent).Text, "through remote")
	output(t, relay.run("remove", "fixture"))
	equal(t, search("")["integrations"], []any{})
}
