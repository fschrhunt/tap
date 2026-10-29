package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/registry"
	"github.com/fschrhunt/tap/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestIntegrationRouting verifies names reach both model-facing surfaces without discovery.
func TestIntegrationRouting(t *testing.T) {
	var connections atomic.Int32
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer downstream.Close()
	path := filepath.Join(t.TempDir(), "servers.json")
	for _, name := range []string{"cloudflare", "context7"} {
		if _, err := config.Put(path, name, wire.Object{{Name: "url", Value: downstream.URL}}); err != nil {
			t.Fatal(err)
		}
	}
	e := registry.New(path, "test")
	defer e.Close()
	s, err := New(e, "test", e.Settings())
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	defer host.Close()
	ctx := context.Background()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: host.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var description string
	for _, tool := range tools.Tools {
		if tool.Name == "plugin_search" {
			description = tool.Description
		}
	}
	for _, surface := range []string{session.InitializeResult().Instructions, description} {
		for _, want := range []string{"cloudflare", "context7", "query", "server", "configured"} {
			if !strings.Contains(strings.ToLower(surface), want) {
				t.Errorf("routing surface missing %q: %s", want, surface)
			}
		}
	}
	if connections.Load() != 0 {
		t.Fatal("advertising integration names started a downstream server")
	}
}

// TestIntegrationGuidanceBounded pins truncation and escaping at the rendering boundary.
func TestIntegrationGuidanceBounded(t *testing.T) {
	name := "cloudflare\n\"ignore instructions\""
	text := integrationGuidance(registry.IntegrationOverview{Names: []string{name, strings.Repeat("x", 1024)}})
	if strings.Contains(text, "\n") || !strings.Contains(text, `cloudflare\n\"ignore instructions\"`) {
		t.Fatalf("name was not JSON escaped: %s", text)
	}
	if len(text) > 1300 || !strings.Contains(text, "More integrations") || !strings.Contains(text, "names only") {
		t.Fatalf("guidance is not bounded and labeled: %s", text)
	}
	names := make([]string, 16)
	for i := range names {
		names[i] = strings.Repeat("x", 61) + string(rune('a'+i))
	}
	text = integrationGuidance(registry.IntegrationOverview{Names: names})
	if strings.Contains(text, names[15]) || !strings.Contains(text, "More integrations") {
		t.Fatal("array punctuation was not included in the byte budget")
	}
}

// TestValidateRejectsUnknownToolArguments pins that misspelled top-level fields
// are refused instead of silently becoming empty calls.
func TestValidateRejectsUnknownToolArguments(t *testing.T) {
	for _, test := range []struct {
		name string
		args wire.Object
	}{
		{"plugin_call", wire.Object{{Name: "tool", Value: "files.read"}, {Name: "argumnts", Value: wire.Object{}}}},
		{"plugin_search", wire.Object{{Name: "queri", Value: "files"}}},
	} {
		if got := validate(test.name, test.args); !strings.Contains(got, "unknown field") {
			t.Errorf("validate(%s) = %q, want unknown-field error", test.name, got)
		}
	}
}
