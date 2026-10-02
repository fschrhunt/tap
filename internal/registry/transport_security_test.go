package registry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fschrhunt/tap/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestHTTPRedirectDoesNotForwardCredentials prevents transport-injected headers leaking on redirects.
func TestHTTPRedirectDoesNotForwardCredentials(t *testing.T) {
	var contacted atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	transport, err := transportFor("redirect", wire.Object{
		{Name: "url", Value: source.URL},
		{Name: "headers", Value: wire.Object{{Name: "X-Api-Key", Value: "synthetic-test-secret"}}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.(*mcp.StreamableClientTransport).HTTPClient.Get(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect || contacted.Load() {
		t.Fatal("followed an MCP redirect with configured credentials")
	}
}

// TestHTTPProtocolHeaders preserves SDK-private HTTP session initialization hooks.
func TestHTTPProtocolHeaders(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "strict-peer", Version: "test"}, nil)
	addTool(server, "echo")
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	var requests atomic.Int32
	var missing atomic.Bool
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) > 1 && r.Header.Get("MCP-Protocol-Version") != "2025-11-25" {
			missing.Store(true)
			http.Error(w, "protocol version header required", http.StatusBadRequest)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(host.Close)
	e := testEngine(t, definitions(host.URL))
	result := listing(t, e)
	row := result.Get("integrations").([]any)[0].(wire.Object)
	if row.Get("tools") != 1 {
		t.Fatalf("strict HTTP discovery failed: %v", row)
	}
	if _, err := e.Call(context.Background(), "test.echo", map[string]any{}, true); err != nil {
		t.Fatal(err)
	}
	e.Close()
	if missing.Load() {
		t.Fatal("SDK protocol headers were lost")
	}
}

// TestHTTPListChangedRefreshesCatalog verifies idle HTTP notifications invalidate cached schemas.
func TestHTTPListChangedRefreshesCatalog(t *testing.T) {
	p := newPeer(t, nil)
	e := testEngine(t, definitions(p.url))
	listing(t, e)
	addTool(p.server, "second")
	waitFor(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.catalogs["test"].at.IsZero()
	})
	listing(t, e)
	waitRefresh(t, e)
	result := listing(t, e)
	if result.Get("integrations").([]any)[0].(wire.Object).Get("tools") != 2 {
		t.Fatal("HTTP tool-change notification did not refresh discovery")
	}
}

// TestNetworkDiagnosticsHideURLs keeps credential-bearing endpoint URLs out of user-facing errors.
func TestNetworkDiagnosticsHideURLs(t *testing.T) {
	err := &url.Error{Op: "Post", URL: "https://example.invalid/mcp?token=synthetic-secret", Err: errors.New("connection refused")}
	if message := Message(err); strings.Contains(message, "synthetic-secret") || strings.Contains(message, "example.invalid") {
		t.Fatalf("endpoint leaked in diagnostic: %q", message)
	}
}
