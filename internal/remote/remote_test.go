package remote

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// host starts an isolated authenticated remote registry, with optional TLS.
func host(t *testing.T, tls bool) (*httptest.Server, *Client) {
	t.Helper()
	t.Setenv("TAP_REMOTE_TOKEN", "relay-token")
	t.Setenv("TAP_REMOTE_ADMIN_TOKEN", "")
	h, close, err := Handler(filepath.Join(t.TempDir(), "servers.json"), "test", "relay-token", "")
	if err != nil {
		t.Fatal(err)
	}
	var httpServer *httptest.Server
	if tls {
		httpServer = httptest.NewTLSServer(h)
	} else {
		httpServer = httptest.NewServer(h)
	}
	t.Cleanup(func() { httpServer.Close(); close() })
	c, err := New(config.Remote{URL: httpServer.URL, TokenEnv: "TAP_REMOTE_TOKEN"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return httpServer, c
}

// TestAuthenticationAndOrigins protects every endpoint and keeps server definitions write-only.
func TestAuthenticationAndOrigins(t *testing.T) {
	h, close, err := Handler(filepath.Join(t.TempDir(), "config.json"), "test", "relay", "admin")
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	for _, tc := range []struct {
		path, token, origin, method string
		status                      int
	}{
		{"/mcp", "", "", "GET", 401}, {"/servers", "relay", "", "POST", 401}, {"/servers", "admin", "", "GET", 405}, {"/missing", "", "", "GET", 401}, {"/mcp", "relay", "https://evil.example", "GET", 403}, {"/servers", "admin", "http://example.com", "DELETE", 400},
	} {
		r := httptest.NewRequest(tc.method, "http://example.com"+tc.path, nil)
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%+v: %d", tc, w.Code)
		}
	}
	if _, _, err := Handler("unused", "test", "", ""); err == nil {
		t.Fatal("allowed no token")
	}
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
	if _, err := c.Edit(ctx, "fixture", wire.Object{{Name: "url", Value: downstream.URL}}); err != nil {
		t.Fatal(err)
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
	if err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	result, err = c.Listing(ctx, true)
	if err != nil || len(result.Get("integrations").([]any)) != 0 {
		t.Fatalf("removal: %v %v", result, err)
	}
}

// TestTLSRelay requires normal certificate verification and accepts a trusted native TLS endpoint.
func TestTLSRelay(t *testing.T) {
	s, c := host(t, true)
	if _, err := c.Listing(context.Background(), true); err == nil {
		t.Fatal("accepted untrusted TLS certificate")
	}
	c.http.Transport = bearerTransport{token: "relay-token", base: s.Client().Transport}
	if _, err := c.Listing(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}

// TestRedirectsNeverForwardToken ensures even a same-origin redirect is refused.
func TestRedirectsNeverForwardToken(t *testing.T) {
	t.Setenv("TAP_REMOTE_TOKEN", "relay-token")
	targetHit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHit = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	c, err := New(config.Remote{URL: redirect.URL}, "test")
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
	s, _ := host(t, false)
	req, _ := http.NewRequest("POST", s.URL+"/servers", strings.NewReader(strings.Repeat("x", bodyLimit+1)))
	req.Header.Set("Authorization", "Bearer relay-token")
	resp, err := http.DefaultClient.Do(req)
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

// TestServeExposure requires deliberate insecure consent and complete TLS options.
func TestServeExposure(t *testing.T) {
	t.Setenv("TAP_REMOTE_TOKEN", "test-token")
	for _, opts := range []Options{{Addr: "0.0.0.0:0"}, {Addr: "127.0.0.1:0", TLSCert: "cert-only"}} {
		if err := Serve(context.Background(), filepath.Join(t.TempDir(), "servers.json"), "test", opts); err == nil {
			t.Fatalf("accepted unsafe options: %+v", opts)
		}
	}
}

// TestNativeTLSServe verifies supplied certificates and cancellation of the actual listener.
func TestNativeTLSServe(t *testing.T) {
	t.Setenv("TAP_REMOTE_TOKEN", "relay-token")
	t.Setenv("TAP_REMOTE_ADMIN_TOKEN", "")
	reference := httptest.NewTLSServer(http.NotFoundHandler())
	address := reference.Listener.Addr().String()
	certificate := reference.TLS.Certificates[0]
	trustedTransport := reference.Client().Transport
	reference.Close()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	private, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, filepath.Join(dir, "servers.json"), "test", Options{Addr: address, TLSCert: certPath, TLSKey: keyPath})
	}()
	c, err := New(config.Remote{URL: "https://" + address}, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.http.Transport = bearerTransport{token: "relay-token", base: trustedTransport}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err = c.Listing(ctx, true); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	c.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("TLS listener did not stop")
	}
}

// TestAdminToken lets relay users search while requiring the separate token for edits.
func TestAdminToken(t *testing.T) {
	t.Setenv("TAP_REMOTE_TOKEN", "relay")
	t.Setenv("TAP_REMOTE_ADMIN_TOKEN", "")
	h, cleanup, err := Handler(filepath.Join(t.TempDir(), "servers.json"), "test", "relay", "admin")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	s := httptest.NewServer(h)
	defer s.Close()
	c, err := New(config.Remote{URL: s.URL}, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Listing(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	def := wire.Object{{Name: "url", Value: "http://localhost:1"}}
	if _, err := c.Edit(context.Background(), "fixture", def); err == nil {
		t.Fatal("relay token edited registry")
	}
	t.Setenv("TAP_REMOTE_ADMIN_TOKEN", "admin")
	if _, err := c.Edit(context.Background(), "fixture", def); err != nil {
		t.Fatal(err)
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
