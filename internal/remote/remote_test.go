package remote

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fschrhunt/tap/internal/auth"
	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/registry"
	"github.com/fschrhunt/tap/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// testHost starts a paired HTTPS registry with credentials scoped to the requested role.
func testHandler(t *testing.T, path string) (http.Handler, *deviceManager, func()) {
	t.Helper()
	manager, err := newDeviceManager(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	h, cleanup, err := handler(path, "test", manager)
	if err != nil {
		t.Fatal(err)
	}
	return h, manager, cleanup
}

func testHost(t *testing.T, path string) (*httptest.Server, *deviceManager) {
	t.Helper()
	h, manager, cleanup := testHandler(t, path)
	server := httptest.NewUnstartedServer(h)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{manager.certificate()}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(func() { server.Close(); cleanup() })
	return server, manager
}

func pairedTestClient(t *testing.T, server *httptest.Server, manager *deviceManager, role string) *Client {
	t.Helper()
	code := manager.execCode
	if role == "admin" {
		code = manager.adminCode
	}
	paired, err := Pair(context.Background(), server.URL, manager.fingerprint, code, "test client", role)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(config.Remote{URL: server.URL, PeerID: paired.ID, Fingerprint: manager.fingerprint, Token: paired.Token}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// host starts an isolated paired registry with an administrator client.
func host(t *testing.T, _ bool) (*httptest.Server, *Client) {
	t.Helper()
	server, manager := testHost(t, filepath.Join(t.TempDir(), "servers.json"))
	return server, pairedTestClient(t, server, manager, "admin")
}

// oauthProvider is an offline MCP OAuth service used to verify the complete remote callback relay.
func oauthProvider(t *testing.T) *httptest.Server {
	t.Helper()
	var provider *httptest.Server
	tools := mcp.NewServer(&mcp.Implementation{Name: "oauth-fixture", Version: "test"}, nil)
	mcp.AddTool(tools, &mcp.Tool{Name: "whoami"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	stream := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return tools }, nil)
	mux := http.NewServeMux()
	provider = httptest.NewServer(mux)
	t.Cleanup(provider.Close)
	jsonReply := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer remote-oauth-token" {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+provider.URL+`/.well-known/oauth-protected-resource"`)
			http.Error(w, "sign in first", http.StatusUnauthorized)
			return
		}
		stream.ServeHTTP(w, r)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, map[string]any{"resource": provider.URL + "/mcp", "authorization_servers": []string{provider.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, map[string]any{"issuer": provider.URL, "authorization_endpoint": provider.URL + "/authorize", "token_endpoint": provider.URL + "/token",
			"registration_endpoint": provider.URL + "/register", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		jsonReply(w, map[string]string{"client_id": "test-client"})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=test-code&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, map[string]any{"access_token": "remote-oauth-token", "token_type": "Bearer", "refresh_token": "refresh", "expires_in": 3600})
	})
	return provider
}

// TestRemoteAuthRelaysBrowserCallback verifies that the browser stays on the client while the
// remote performs OAuth and writes its own private grant store.
func TestRemoteAuthRelaysBrowserCallback(t *testing.T) {
	provider := oauthProvider(t)
	path := filepath.Join(t.TempDir(), "servers.json")
	if err := config.Add(path, "oauth", wire.Object{{Name: "type", Value: "http"}, {Name: "url", Value: provider.URL + "/mcp"}}); err != nil {
		t.Fatal(err)
	}
	hosted, manager := testHost(t, path)
	c := pairedTestClient(t, hosted, manager, "admin")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := c.Auth(ctx, "oauth", "", "", 0, func(page string) error {
		resp, err := http.Get(page)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.Body.Close()
	}, io.Discard)
	if err != nil || result == nil || !result.SignedIn || !result.HadGrant || result.Tools != 1 {
		t.Fatalf("remote Auth = %+v, %v", result, err)
	}
	if !auth.Has(path, "oauth", provider.URL+"/mcp") {
		t.Fatal("remote grant was not saved on the serving machine")
	}
	result, err = c.Auth(ctx, "oauth", "", "", 0, func(string) error {
		return errors.New("already-authorized flow unexpectedly opened a browser")
	}, io.Discard)
	if err != nil || result == nil || result.SignedIn || !result.HadGrant || result.Tools != 1 {
		t.Fatalf("repeat remote Auth = %+v, %v; want completed without a browser", result, err)
	}
}

// TestRemoteAuthWithoutOAuthReturnsAnImmediateResult pins providers that do not challenge.
func TestRemoteAuthWithoutOAuthReturnsAnImmediateResult(t *testing.T) {
	tools := mcp.NewServer(&mcp.Implementation{Name: "plain", Version: "test"}, nil)
	mcp.AddTool(tools, &mcp.Tool{Name: "ping"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	downstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return tools }, nil))
	t.Cleanup(downstream.Close)
	path := filepath.Join(t.TempDir(), "servers.json")
	if err := config.Add(path, "plain", wire.Object{{Name: "type", Value: "http"}, {Name: "url", Value: downstream.URL}}); err != nil {
		t.Fatal(err)
	}
	hosted, manager := testHost(t, path)
	c := pairedTestClient(t, hosted, manager, "admin")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := c.Auth(ctx, "plain", "", "", 0, nil, io.Discard)
	if err != nil || result == nil || result.SignedIn || result.HadGrant || result.Tools != 1 {
		t.Fatalf("remote Auth without OAuth = %+v, %v", result, err)
	}
}

// TestPairingPinsTLSIssuesScopedCredentialsAndRevokesThem exercises enrollment end to end.
func TestPairingPinsTLSIssuesScopedCredentialsAndRevokesThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	manager, err := newDeviceManager(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	h, cleanup, err := handler(path, "test", manager)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{manager.certificate()}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	identity, err := InspectPairing(ctx, srv.URL)
	if err != nil || identity.Fingerprint != manager.fingerprint {
		t.Fatalf("InspectPairing = %+v, %v", identity, err)
	}
	paired, err := Pair(ctx, srv.URL, identity.Fingerprint, manager.execCode, "test client", "execution")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Pair(ctx, srv.URL, identity.Fingerprint, manager.execCode, "second client", "execution"); err == nil {
		t.Fatal("one-time execution code was reused")
	}
	c, err := New(config.Remote{URL: srv.URL, PeerID: paired.ID, Fingerprint: identity.Fingerprint, Token: paired.Token}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Health(ctx); err != nil {
		t.Fatalf("paired execution credential health check: %v", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/servers", nil)
	req.Header.Set("Authorization", "Bearer "+paired.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("execution credential accessed admin route: HTTP %d", resp.StatusCode)
	}
	adminPair, err := Pair(ctx, srv.URL, identity.Fingerprint, manager.adminCode, "admin client", "admin")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := New(config.Remote{URL: srv.URL, PeerID: adminPair.ID, Fingerprint: identity.Fingerprint, Token: adminPair.Token}, "test")
	if err != nil {
		t.Fatal(err)
	}
	adminReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/servers", nil)
	adminResp, err := admin.adminHTTP().Do(adminReq)
	if err != nil {
		t.Fatalf("paired admin request: %v", err)
	}
	adminResp.Body.Close()
	if adminResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("paired admin role did not authorize admin route: HTTP %d", adminResp.StatusCode)
	}
	admin.Close()
	if revoked, err := RevokeDevice(path, paired.ID); err != nil || !revoked {
		t.Fatalf("RevokeDevice = %v, %v", revoked, err)
	}
	if err := c.Health(ctx); err == nil {
		t.Fatal("revoked credential still passed health check")
	}
	c.Close()
}

// TestExistingSessionAdoptsServers verifies add/remove and result metadata through a live relay.
func TestExistingSessionAdoptsServers(t *testing.T) {
	fixture := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "test"}, nil)
	fixture.AddTool(&mcp.Tool{Name: "echo", Description: "echo", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Meta: mcp.Meta{"marker": "kept"}, Content: []mcp.Content{&mcp.TextContent{Text: "hello"}}}, nil
	})
	downstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fixture }, nil))
	t.Cleanup(downstream.Close)
	_, c := host(t, false)
	ctx := context.Background()
	if result, err := c.Listing(ctx, true); err != nil || len(result.Get("integrations").([]any)) != 0 {
		t.Fatalf("initial listing: %v %v", result, err)
	}
	if result, err := c.Listing(ctx, true); err != nil || result.Has("config") {
		t.Fatalf("remote listing exposed host config path: %v %v", result, err)
	}
	added, err := c.Edit(ctx, "fixture", wire.Object{{Name: "url", Value: downstream.URL}})
	if err != nil || added.Replaced {
		t.Fatalf("add: %+v %v", added, err)
	}
	replaced, err := c.Edit(ctx, "fixture", wire.Object{{Name: "url", Value: downstream.URL}})
	if err != nil || !replaced.Replaced {
		t.Fatalf("replace: %+v %v", replaced, err)
	}
	result, err := c.Search(ctx, "echo", 8, true)
	if err != nil || wire.String(result.Get("total")) != "1" {
		t.Fatalf("adoption: %v %v", result, err)
	}
	result, err = c.Call(ctx, "fixture.echo", wire.Object{}, true)
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := result.Get("_meta").(wire.Object)
	if !ok || meta.Get("marker") != "kept" {
		t.Fatalf("metadata lost: %v", result)
	}
	removed, err := c.Edit(ctx, "fixture", nil)
	if err != nil || !removed.Removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	result, err = c.Listing(ctx, true)
	if err != nil || len(result.Get("integrations").([]any)) != 0 {
		t.Fatalf("removal: %v %v", result, err)
	}
}

// TestReferenceSessionIsolation forbids inspect/drop/copy by a different authenticated session.
func TestReferenceSessionIsolation(t *testing.T) {
	t.Setenv("TAP_REFERENCES", "on")
	fixture := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "test"}, nil)
	fixture.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "private result"}}}, nil
	})
	downstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fixture }, nil))
	t.Cleanup(downstream.Close)
	h, c := host(t, false)
	other, err := New(config.Remote{URL: h.URL, PeerID: c.cfg.PeerID, Fingerprint: c.cfg.Fingerprint, Token: c.cfg.Token}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	ctx := context.Background()
	if _, err = c.Edit(ctx, "fixture", wire.Object{{Name: "url", Value: downstream.URL}}); err != nil {
		t.Fatal(err)
	}
	r, err := c.CallWithOptions(ctx, "fixture.echo", wire.Object{}, true, true, nil, nil)
	if err != nil || r.Get("isError") == true {
		t.Fatalf("retention: %v %v", r, err)
	}
	ref := r.Get("structuredContent").(wire.Object).Get("reference").(string)
	if _, err = other.Inspect(ctx, ref, "/content/0/text", 0, 100, 32768); err == nil {
		t.Fatal("other session inspected reference")
	}
	if _, err = other.Drop(ctx, ref); err == nil {
		t.Fatal("other session dropped reference")
	}
	r, err = other.CallWithOptions(ctx, "fixture.echo", wire.Object{}, true, false, []registry.ArgumentReference{{Target: "/message", Reference: ref, Pointer: "/content/0/text"}}, nil)
	if err != nil || r.Get("isError") != true || r.Get("structuredContent").(wire.Object).Get("code") != "reference_unavailable" {
		t.Fatalf("cross-session copy: %v %v", r, err)
	}
	inspected, err := c.Inspect(ctx, ref, "/content/0/text", 0, 100, 32768)
	if err != nil || inspected.Get("value") != "private result" {
		t.Fatalf("owner inspection: %v %v", inspected, err)
	}
	if _, err = c.Drop(ctx, ref); err != nil {
		t.Fatal(err)
	}
}

// TestScopedRemoteDiscovery routes exact inspection and refresh without touching unrelated providers.
func TestScopedRemoteDiscovery(t *testing.T) {
	fixture := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "test"}, nil)
	fixture.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	downstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fixture }, nil))
	t.Cleanup(downstream.Close)
	var unrelatedHit atomic.Bool
	unrelated := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { unrelatedHit.Store(true) }))
	t.Cleanup(unrelated.Close)
	_, c := host(t, false)
	ctx := context.Background()
	for name, url := range map[string]string{"fixture": downstream.URL, "unrelated": unrelated.URL} {
		if _, err := c.Edit(ctx, name, wire.Object{{Name: "url", Value: url}}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := c.Discover(ctx, registry.SearchOptions{IDs: []string{"fixture.echo"}, Detail: "full", Limit: 8, MaxBytes: 32768}, true)
	if err != nil || r.Get("matches").([]any)[0].(wire.Object).Get("schemaLoaded") != true {
		t.Fatalf("inspection: %v %v", r, err)
	}
	r, err = c.Refresh(ctx, "fixture", true)
	if err != nil || len(r.Get("integrations").([]any)) != 1 || r.Has("config") {
		t.Fatalf("refresh: %v %v", r, err)
	}
	if unrelatedHit.Load() {
		t.Fatal("scoped discovery contacted unrelated provider")
	}
}

// TestTLSRelay requires normal certificate verification and accepts a trusted native TLS endpoint.
func TestTLSRelay(t *testing.T) {
	s, c := host(t, true)
	if _, err := c.Listing(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.URL, "https://") {
		t.Fatalf("paired relay is not HTTPS: %s", s.URL)
	}
}

// TestRedirectsNeverForwardToken ensures even a same-origin redirect is refused.
func TestRedirectsNeverForwardToken(t *testing.T) {
	targetHit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHit = true }))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	cert := redirect.TLS.Certificates[0]
	fingerprint := sha256.Sum256(cert.Certificate[0])
	c, err := New(config.Remote{URL: redirect.URL, PeerID: "redirect-test", Fingerprint: hex.EncodeToString(fingerprint[:]), Token: "paired-test-token"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Listing(context.Background(), true); err == nil {
		t.Fatal("followed redirect")
	}
	if targetHit {
		t.Fatal("forwarded credentials")
	}
}

// TestCancellationReachesConnector ensures cancelled relay calls release the downstream handler.
func TestCancellationReachesConnector(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	fixture := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "test"}, nil)
	fixture.AddTool(&mcp.Tool{Name: "wait", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	downstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fixture }, nil))
	t.Cleanup(downstream.Close)
	_, c := host(t, false)
	if _, err := c.Edit(context.Background(), "fixture", wire.Object{{Name: "url", Value: downstream.URL}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Call(ctx, "fixture.wait", wire.Object{}, true); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("call never started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay stuck")
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("downstream was not cancelled")
	}
}

// TestBodyCap rejects oversized admin requests without exposing submitted secrets.
func TestBodyCap(t *testing.T) {
	s, c := host(t, false)
	req, _ := http.NewRequest("POST", s.URL+"/servers", strings.NewReader(strings.Repeat("x", bodyLimit+1)))
	resp, err := c.adminHTTP().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || strings.Contains(string(b), "xxxx") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

// TestReconnect opens a fresh SDK session after the transport session closes.
func TestReconnect(t *testing.T) {
	_, c := host(t, false)
	ctx := context.Background()
	if _, err := c.Listing(ctx, true); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		c.mu.Lock()
		cleared := c.session == nil
		c.mu.Unlock()
		if cleared {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed session remained cached")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := c.Listing(ctx, true); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	same := c.session == session
	c.mu.Unlock()
	if same {
		t.Fatal("reused closed session")
	}
}

// TestServeRequiresCompleteTLSOptions rejects incomplete explicit identity configuration.
func TestServeRequiresCompleteTLSOptions(t *testing.T) {
	if err := Serve(context.Background(), filepath.Join(t.TempDir(), "servers.json"), "test", Options{Addr: "127.0.0.1:0", TLSCert: "cert-only"}); err == nil {
		t.Fatal("accepted incomplete TLS identity")
	}
}

// TestConnectionErrorsHideSecrets keeps configuration-derived diagnostics off the remote surface.
func TestConnectionErrorsHideSecrets(t *testing.T) {
	_, c := host(t, false)
	if _, err := c.Edit(context.Background(), "fixture", wire.Object{{Name: "command", Value: []string{"/tmp/credential-do-not-echo"}}}); err != nil {
		t.Fatal(err)
	}
	result, err := c.Listing(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := wire.JSON(result, false)
	if strings.Contains(string(data), "credential-do-not-echo") {
		t.Fatalf("leaked config: %s", data)
	}
	result, err = c.Call(context.Background(), "fixture.echo", wire.Object{}, true)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = wire.JSON(result, false)
	if strings.Contains(string(data), "credential-do-not-echo") {
		t.Fatalf("leaked config: %s", data)
	}
}

// TestSafeCatalogKeepsSignInInstruction pins what crosses the network in a catalog row: tap's
// own sign-in instruction with where to run it, while connector diagnostics stay masked and
// anything padded around the instruction does not pass as tap's own.
func TestSafeCatalogKeepsSignInInstruction(t *testing.T) {
	rows := []any{
		wire.Object{{Name: "server", Value: "cloudflare"}, {Name: "error", Value: `needs you to sign in: run "tap auth cloudflare"`}},
		wire.Object{{Name: "server", Value: "files"}, {Name: "error", Value: `exec: "npx": executable file not found in $PATH`}},
		wire.Object{{Name: "server", Value: "odd"}, {Name: "error", Value: `needs you to sign in: run "tap auth odd" (as root)`}},
	}
	out := safeCatalog(wire.Object{{Name: "integrations", Value: rows}}, "integrations")
	got, _ := out.Get("integrations").([]any)
	want := []string{
		`needs you to sign in: run "tap auth cloudflare"`,
		"server unavailable",
		"server unavailable",
	}
	for i, w := range want {
		row, ok := got[i].(wire.Object)
		if !ok {
			t.Fatalf("row %d is not an object", i)
		}
		if e := wire.String(row.Get("error")); e != w {
			t.Errorf("row %d error = %q, want %q", i, e, w)
		}
	}
}
