// Package remote hosts a shared registry and relays tap's static MCP surface.
package remote

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fschrhunt/tap/internal/auth"
	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/registry"
	"github.com/fschrhunt/tap/internal/server"
	"github.com/fschrhunt/tap/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const bodyLimit = 4 << 20

// Options controls the listener; tokens always come from the environment.
type Options struct {
	Addr, TLSCert, TLSKey string
	AllowInsecure         bool
}

// Handler exposes authenticated MCP and write-only server administration.
func Handler(path, version, token, adminToken string) (http.Handler, func(), error) {
	if token == "" {
		return nil, nil, fmt.Errorf("TAP_REMOTE_TOKEN must be set")
	}
	if adminToken == "" {
		adminToken = token
	}
	e := registry.New(path, version)
	s, err := server.New(hostedBackend{e}, version, e.Settings())
	if err != nil {
		e.Close()
		return nil, nil, err
	}
	go e.Warm()
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{MaxRequestBodyBytes: bodyLimit, SessionTimeout: 30 * time.Minute, PropagateRequestCancellation: true})
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)
	mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			status := http.StatusBadRequest
			var oversized *http.MaxBytesError
			if errors.As(err, &oversized) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "invalid or oversized request", status)
			return
		}
		v, err := wire.Decode(b)
		if err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		obj, ok := v.(wire.Object)
		if !ok {
			http.Error(w, "invalid request", 400)
			return
		}
		name, ok := obj.Get("name").(string)
		if !ok || name == "" || strings.Contains(name, ".") {
			http.Error(w, "invalid server name", 400)
			return
		}
		removed := false
		if r.Method == http.MethodPost {
			def, ok := obj.Get("definition").(wire.Object)
			if !ok {
				http.Error(w, "invalid server definition", 400)
				return
			}
			err = config.Add(path, name, def)
		} else {
			removed, err = config.Remove(path, name)
		}
		if err != nil {
			http.Error(w, "cannot edit server registry", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"removed": removed})
	})
	protected := http.NewCrossOriginProtection().Handler(mux)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := token
		if r.URL.Path == "/servers" {
			expected = adminToken
		}
		got := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		want := sha256.Sum256([]byte("Bearer " + expected))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		// All browser origins must match the endpoint, including safe GET methods.
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			if err != nil || u.Scheme != scheme || u.Host != r.Host || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
				http.Error(w, "forbidden origin", 403)
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)
		protected.ServeHTTP(w, r)
	})
	return h, func() {
		for session := range s.Sessions() {
			_ = session.Close()
		}
		e.Close()
	}, nil
}

// Serve listens until cancellation, requiring TLS or explicit consent off loopback.
func Serve(ctx context.Context, path, version string, opts Options) error {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:7777"
	}
	host, _, err := net.SplitHostPort(opts.Addr)
	if err != nil {
		return fmt.Errorf("invalid remote listen address")
	}
	if (opts.TLSCert == "") != (opts.TLSKey == "") {
		return fmt.Errorf("--tls-cert and --tls-key must be supplied together")
	}
	if !config.Loopback(host) && opts.TLSCert == "" && !opts.AllowInsecure {
		return fmt.Errorf("nonloopback remote serve requires TLS or --allow-insecure")
	}
	h, cleanup, err := Handler(path, version, os.Getenv("TAP_REMOTE_TOKEN"), os.Getenv("TAP_REMOTE_ADMIN_TOKEN"))
	if err != nil {
		return err
	}
	defer cleanup()
	listener, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return fmt.Errorf("cannot listen on remote address")
	}
	defer listener.Close()
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: time.Minute}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = srv.Close()
		case <-done:
		}
	}()
	if opts.TLSCert != "" {
		err = srv.ServeTLS(listener, opts.TLSCert, opts.TLSKey)
	} else {
		err = srv.Serve(listener)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remote server failed")
	}
	return nil
}

// Client shares one remote MCP session and reconnects after it closes.
type Client struct {
	cfg     config.Remote
	version string
	http    *http.Client
	mu      sync.Mutex
	session *mcp.ClientSession
	opening *initialization
	workers sync.WaitGroup
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
}

// initialization publishes one shared connection attempt to independently cancellable callers.
type initialization struct {
	ready   chan struct{}
	session *mcp.ClientSession
	err     error
}

// New validates relay settings and requires a nonempty environment token.
func New(cfg config.Remote, version string) (*Client, error) {
	endpoint, err := config.NormalizeURL(cfg.URL, cfg.AllowInsecure)
	if err != nil {
		return nil, err
	}
	cfg.URL = endpoint
	if cfg.TokenEnv == "" {
		cfg.TokenEnv = "TAP_REMOTE_TOKEN"
	}
	token := os.Getenv(cfg.TokenEnv)
	if token == "" {
		return nil, fmt.Errorf("remote token environment variable is not set")
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &Client{ctx: lifetime, cancel: cancel, cfg: cfg, version: version, http: &http.Client{Transport: bearerTransport{token: token}, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("remote redirects are forbidden") }}}, nil
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

// RoundTrip attaches credentials only to requests issued by the redirect-free client.
func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+t.token)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// connect shares initialization without blocking cancellation or holding a lock during network work.
func (c *Client) connect(ctx context.Context) (*mcp.ClientSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("remote client is closed")
	}
	if c.session != nil {
		session := c.session
		c.mu.Unlock()
		return session, nil
	}
	if c.opening == nil {
		c.opening = &initialization{ready: make(chan struct{})}
		c.workers.Add(1)
		go c.open(c.opening)
	}
	pending := c.opening
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, c.ctx.Err()
	case <-pending.ready:
		return pending.session, pending.err
	}
}

// open owns the initialization deadline independently of the callers waiting for it.
func (c *Client) open(pending *initialization) {
	defer c.workers.Done()
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "tap", Version: c.version}, nil)
	// The remote's two definitions never change; discovery happens through calls,
	// so a relay needs no extra persistent notification stream.
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: c.cfg.URL, HTTPClient: c.http, MaxRetries: -1, DisableStandaloneSSE: true}, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		err = fmt.Errorf("cannot connect to remote")
	}
	c.mu.Lock()
	if c.closed {
		err = context.Canceled
	}
	published := err == nil
	if published {
		c.session = session
		pending.session = session
	}
	pending.err = err
	c.opening = nil
	close(pending.ready)
	c.mu.Unlock()
	if !published {
		if session != nil {
			_ = session.Close()
		}
		return
	}
	go func() {
		_ = session.Wait()
		c.mu.Lock()
		if c.session == session {
			c.session = nil
		}
		c.mu.Unlock()
	}()
}

// invoke uses SDK HTTP results and forwards cancellation without replaying tool calls.
func (c *Client) invoke(ctx context.Context, name string, args any, meta mcp.Meta) (wire.Object, error) {
	session, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args, Meta: meta})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c.mu.Lock()
		if c.session == session {
			c.session = nil
		}
		c.mu.Unlock()
		go session.Close()
		return nil, fmt.Errorf("remote request failed")
	}
	b, err := wire.JSON(result, false)
	if err != nil {
		return nil, fmt.Errorf("invalid remote result")
	}
	v, err := wire.DecodeExact(b)
	if err != nil {
		return nil, fmt.Errorf("invalid remote result")
	}
	obj, ok := v.(wire.Object)
	if !ok {
		return nil, fmt.Errorf("invalid remote result")
	}
	return obj, nil
}

// catalog unwraps the text JSON returned by plugin_search.
func (c *Client) catalog(ctx context.Context, args any) (wire.Object, error) {
	result, err := c.invoke(ctx, "plugin_search", args, nil)
	if err != nil {
		return nil, err
	}
	if result.Get("isError") == true {
		return nil, fmt.Errorf("remote search failed")
	}
	content, _ := result.Get("content").([]any)
	if len(content) != 1 {
		return nil, fmt.Errorf("invalid remote catalog")
	}
	part, ok := content[0].(wire.Object)
	if !ok {
		return nil, fmt.Errorf("invalid remote catalog")
	}
	text, _ := part.Get("text").(string)
	v, err := wire.DecodeExact([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("invalid remote catalog")
	}
	obj, ok := v.(wire.Object)
	if !ok {
		return nil, fmt.Errorf("invalid remote catalog")
	}
	return obj, nil
}

// Listing returns the remote registry catalog without reading local servers.
func (c *Client) Listing(ctx context.Context, quiet bool) (wire.Object, error) {
	return c.catalog(ctx, wire.Object{})
}

// Search searches the remote registry using tap's static tool.
func (c *Client) Search(ctx context.Context, query string, limit float64, quiet bool) (wire.Object, error) {
	if query == "" {
		catalog, err := c.Listing(ctx, quiet)
		if err != nil {
			return nil, err
		}
		unavailable := []any{}
		rows, _ := catalog.Get("integrations").([]any)
		for _, v := range rows {
			if row, ok := v.(wire.Object); ok && row.Has("error") {
				unavailable = append(unavailable, wire.Object{{Name: "server", Value: row.Get("server")}, {Name: "error", Value: row.Get("error")}})
			}
		}
		out := wire.Object{{Name: "query", Value: query}, {Name: "total", Value: 0}, {Name: "matches", Value: []any{}}, {Name: "unavailable", Value: unavailable}}
		if len(unavailable) == 0 {
			out.Set("hint", "No tool matched every term. Try fewer or broader terms, or omit the query to list the configured servers.")
		}
		return out, nil
	}
	return c.catalog(ctx, wire.Object{{Name: "query", Value: query}, {Name: "limit", Value: limit}})
}

// Call preserves all remote result fields, including _meta.
func (c *Client) Call(ctx context.Context, id string, args any, quiet bool) (wire.Object, error) {
	return c.CallWithMeta(ctx, id, args, quiet, nil)
}

// Edit performs remote administration; an optional admin environment token overrides the relay token.
func (c *Client) Edit(ctx context.Context, name string, definition wire.Object) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	method := http.MethodPost
	obj := wire.Object{{Name: "name", Value: name}}
	if definition == nil {
		method = http.MethodDelete
	} else {
		obj.Set("definition", definition)
	}
	b, err := wire.JSON(obj, false)
	if err != nil {
		return false, fmt.Errorf("invalid server definition")
	}
	endpoint := strings.TrimSuffix(c.cfg.URL, "/mcp") + "/servers"
	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(string(b)))
	if err != nil {
		return false, fmt.Errorf("invalid remote request")
	}
	req.Header.Set("Content-Type", "application/json")
	client := c.http
	if token := os.Getenv("TAP_REMOTE_ADMIN_TOKEN"); token != "" {
		client = &http.Client{Transport: bearerTransport{token: token}, CheckRedirect: c.http.CheckRedirect}
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("remote administration failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("remote administration failed (HTTP %d)", resp.StatusCode)
	}
	var out struct {
		Removed bool `json:"removed"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, bodyLimit)).Decode(&out) != nil {
		return false, fmt.Errorf("invalid remote response")
	}
	return out.Removed, nil
}

// Close shuts down the relay session and prevents reconnects.
func (c *Client) Close() {
	c.cancel()
	c.mu.Lock()
	c.closed = true
	session := c.session
	c.session = nil
	c.mu.Unlock()
	c.workers.Wait()
	if session != nil {
		_ = session.Close()
	}
}

// CallWithMeta preserves request metadata across the stdio relay hop.
func (c *Client) CallWithMeta(ctx context.Context, id string, args any, quiet bool, meta mcp.Meta) (wire.Object, error) {
	return c.CallWithOptions(ctx, id, args, quiet, false, nil, meta)
}

// Discover forwards the complete discovery request without opening local connectors.
func (c *Client) Discover(ctx context.Context, opt registry.SearchOptions, quiet bool) (wire.Object, error) {
	if opt.Query == "" && opt.Server == "" && len(opt.IDs) == 0 && !opt.Refresh {
		return c.Search(ctx, "", opt.Limit, quiet)
	}
	args := wire.Object{{Name: "limit", Value: opt.Limit}, {Name: "offset", Value: opt.Offset}, {Name: "maxBytes", Value: opt.MaxBytes}, {Name: "refresh", Value: opt.Refresh}}
	if opt.Query != "" {
		args.Set("query", opt.Query)
	}
	if opt.Server != "" {
		args.Set("server", opt.Server)
	}
	if opt.Detail != "" {
		args.Set("detail", opt.Detail)
	}
	if len(opt.IDs) > 0 {
		args.Set("ids", opt.IDs)
	}
	return c.catalog(ctx, args)
}

// Refresh performs a blocking live refresh on the host and returns its discovery catalog.
func (c *Client) Refresh(ctx context.Context, name string, quiet bool) (wire.Object, error) {
	out, err := c.catalog(ctx, wire.Object{{Name: "server", Value: name}, {Name: "refresh", Value: true}, {Name: "detail", Value: "summary"}})
	if err != nil {
		return nil, err
	}
	rows, _ := out.Get("catalogs").([]any)
	return wire.Object{{Name: "integrations", Value: rows}}, nil
}

// CallWithOptions forwards validated call controls and explicit references over one relay session.
func (c *Client) CallWithOptions(ctx context.Context, id string, args any, quiet, retain bool, refs []registry.ArgumentReference, meta mcp.Meta) (wire.Object, error) {
	request := wire.Object{{Name: "tool", Value: id}, {Name: "arguments", Value: args}}
	if retain {
		request.Set("resultMode", "reference")
	}
	if len(refs) > 0 {
		values := []any{}
		for _, ref := range refs {
			values = append(values, wire.Object{{Name: "target", Value: ref.Target}, {Name: "reference", Value: ref.Reference}, {Name: "pointer", Value: ref.Pointer}})
		}
		request.Set("argumentRefs", values)
	}
	return c.invoke(ctx, "plugin_call", request, wire.ForwardMeta(meta))
}

// Inspect reads a reference created on this relay's remote MCP session.
func (c *Client) Inspect(ctx context.Context, id, pointer string, offset, limit, maxBytes int) (wire.Object, error) {
	return c.referenceResult(ctx, wire.Object{{Name: "operation", Value: "inspect"}, {Name: "reference", Value: id}, {Name: "pointer", Value: pointer}, {Name: "offset", Value: offset}, {Name: "limit", Value: limit}, {Name: "maxBytes", Value: maxBytes}})
}

// Drop releases a reference on this relay's remote MCP session.
func (c *Client) Drop(ctx context.Context, id string) (wire.Object, error) {
	return c.referenceResult(ctx, wire.Object{{Name: "operation", Value: "drop"}, {Name: "reference", Value: id}})
}

// referenceResult unwraps inspection text while preserving machine-readable refusal codes.
func (c *Client) referenceResult(ctx context.Context, args wire.Object) (wire.Object, error) {
	r, err := c.invoke(ctx, "plugin_call", args, nil)
	if err != nil {
		return nil, err
	}
	if r.Get("isError") == true {
		if o, ok := r.Get("structuredContent").(wire.Object); ok {
			code, _ := o.Get("code").(string)
			message, _ := o.Get("message").(string)
			recovery, _ := o.Get("recovery").(string)
			return nil, &registry.Failure{Code: code, Message: message, Recovery: recovery}
		}
		return nil, fmt.Errorf("remote reference operation failed")
	}
	content, _ := r.Get("content").([]any)
	if len(content) != 1 {
		return nil, fmt.Errorf("invalid remote reference response")
	}
	part, _ := content[0].(wire.Object)
	text, _ := part.Get("text").(string)
	v, err := wire.DecodeExact([]byte(text))
	out, ok := v.(wire.Object)
	if err != nil || !ok {
		return nil, fmt.Errorf("invalid remote reference response")
	}
	return out, nil
}

// hostedBackend suppresses child stderr and config-derived connection errors on the network.
type hostedBackend struct{ *registry.Engine }

// ReferenceSessionRequired prevents stateless HTTP clients from sharing an empty reference scope.
func (e hostedBackend) ReferenceSessionRequired() bool { return true }

// safeCatalog hides connector diagnostics that may contain credentials or commands. Tap's own
// sign-in instruction names only a server, so it crosses the network unchanged apart from
// pointing at where it can be acted on: the machine running tap remote serve.
func safeCatalog(out wire.Object, field string) wire.Object {
	rows, _ := out.Get(field).([]any)
	for _, v := range rows {
		row, ok := v.(wire.Object)
		if !ok || !row.Has("error") {
			continue
		}
		if msg, _ := row.Get("error").(string); auth.IsRequiredMessage(msg) {
			row.Set("error", msg+" on the machine running tap remote serve")
			continue
		}
		row.Set("error", "server unavailable")
	}
	return out
}

// Listing returns counts with generic connection failures and never logs child stderr.
func (e hostedBackend) Listing(ctx context.Context, quiet bool) (wire.Object, error) {
	out, err := e.Engine.Listing(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("cannot read remote registry")
	}
	// A remote client needs the catalog, not the host's local filesystem path.
	out.Delete("config")
	return safeCatalog(out, "integrations"), nil
}

// Search exposes tool definitions while hiding connection diagnostics.
func (e hostedBackend) Search(ctx context.Context, query string, limit float64, quiet bool) (wire.Object, error) {
	out, err := e.Engine.Search(ctx, query, limit, true)
	if err != nil {
		return nil, fmt.Errorf("cannot search remote registry")
	}
	return safeCatalog(out, "unavailable"), nil
}

// Call returns connector results but never exposes transport configuration on failure.
func (e hostedBackend) Call(ctx context.Context, id string, args any, quiet bool) (wire.Object, error) {
	out, err := e.Engine.Call(ctx, id, args, true)
	if err != nil {
		return nil, fmt.Errorf("connector call failed")
	}
	return out, nil
}

// CallWithMeta forwards application metadata while keeping host diagnostics private.
func (e hostedBackend) CallWithMeta(ctx context.Context, id string, args any, quiet bool, meta mcp.Meta) (wire.Object, error) {
	return e.CallWithOptions(ctx, id, args, quiet, false, nil, meta)
}

// CallWithOptions keeps precise value-free gateway errors but hides transport/config diagnostics.
func (e hostedBackend) CallWithOptions(ctx context.Context, id string, args any, quiet, retain bool, refs []registry.ArgumentReference, meta mcp.Meta) (wire.Object, error) {
	out, err := e.Engine.CallWithOptions(ctx, id, args, true, retain, refs, meta)
	if err != nil {
		var failure *registry.Failure
		if errors.As(err, &failure) {
			return nil, err
		}
		return nil, fmt.Errorf("connector call failed")
	}
	return out, nil
}

// Discover exposes capabilities without host filesystem paths or connector diagnostics.
func (e hostedBackend) Discover(ctx context.Context, opt registry.SearchOptions, quiet bool) (wire.Object, error) {
	out, err := e.Engine.Discover(ctx, opt, true)
	if err != nil {
		return nil, fmt.Errorf("cannot search remote registry")
	}
	safeCatalog(out, "catalogs")
	return safeCatalog(out, "unavailable"), nil
}

// Refresh returns live counts with generic errors and no host config path.
func (e hostedBackend) Refresh(ctx context.Context, name string, quiet bool) (wire.Object, error) {
	out, err := e.Engine.Refresh(ctx, name, true)
	if err != nil {
		return nil, fmt.Errorf("cannot refresh remote registry")
	}
	out.Delete("config")
	return safeCatalog(out, "integrations"), nil
}
