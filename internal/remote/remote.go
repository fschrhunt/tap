// Package remote hosts a shared registry and relays tap's static MCP surface.
package remote

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fschrhunt/tap/internal/auth"
	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/human"
	"github.com/fschrhunt/tap/internal/registry"
	"github.com/fschrhunt/tap/internal/server"
	"github.com/fschrhunt/tap/internal/wire"
	"github.com/grandcat/zeroconf"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const bodyLimit = 4 << 20
const maxSignInFlows = 8

type signInFlow struct {
	page     chan string
	callback chan url.Values
	done     chan signInResult
	cancel   context.CancelFunc
}

type signInResult struct {
	result *auth.Result
	err    error
}

func validCallback(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "http" && config.Loopback(u.Hostname()) && u.Port() != "" && u.Path == "/callback" && u.RawQuery == "" && u.Fragment == "" && u.User == nil
}

// Options controls the listener address and optional TLS identity.
type Options struct {
	Addr, TLSCert, TLSKey string
}

func handler(path, version string, devices *deviceManager) (http.Handler, func(), error) {
	if devices == nil {
		return nil, nil, fmt.Errorf("a remote execution credential is required")
	}
	e := registry.New(path, version)
	s, err := server.New(hostedBackend{e}, version, e.Settings())
	if err != nil {
		e.Close()
		return nil, nil, err
	}
	authFlows := make(map[string]*signInFlow)
	var authMu sync.Mutex
	authCtx, stopAuth := context.WithCancel(context.Background())
	go e.Warm()
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{MaxRequestBodyBytes: bodyLimit, SessionTimeout: 30 * time.Minute, PropagateRequestCancellation: true})
	mux := http.NewServeMux()
	// Serialize admissions so simultaneous initializations cannot exceed the session cap.
	const maxSessions = 64
	var admission sync.Mutex
	admissions, stopAdmissions := context.WithCancel(context.Background())
	closed := false
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("Mcp-Session-Id") == "" {
			if !admission.TryLock() {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "another session is initializing; retry shortly", http.StatusTooManyRequests)
				return
			}
			defer admission.Unlock()
			// Shutdown cancels a headerless call before waiting for its admission lock.
			ctx, cancel := context.WithCancel(r.Context())
			stop := context.AfterFunc(admissions, cancel)
			defer stop()
			defer cancel()
			r = r.WithContext(ctx)
			if closed {
				http.Error(w, "server shutting down", http.StatusServiceUnavailable)
				return
			}
			n := 0
			for range s.Sessions() {
				n++
			}
			if n >= maxSessions {
				w.Header().Set("Retry-After", "60")
				http.Error(w, fmt.Sprintf("at most %d MCP sessions are allowed; close an unused one", maxSessions), http.StatusTooManyRequests)
				return
			}
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	mux.HandleFunc("/identity", func(w http.ResponseWriter, r *http.Request) {
		if devices == nil || r.TLS == nil || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(PairingInfo{ID: devices.id, Name: devices.name, Fingerprint: devices.fingerprint})
	})
	mux.HandleFunc("/pair", func(w http.ResponseWriter, r *http.Request) {
		if devices == nil || r.TLS == nil {
			http.Error(w, "pairing requires HTTPS", http.StatusUpgradeRequired)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var input struct{ Code, Name, Role string }
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			http.Error(w, "invalid pairing request", http.StatusBadRequest)
			return
		}
		id, credential, err := devices.enroll(input.Code, input.Name, input.Role)
		if err != nil {
			http.Error(w, "pairing failed", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(PairResult{ID: id, Token: credential, Role: input.Role})
	})
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
		removed, replaced := false, false
		if r.Method == http.MethodPost {
			def, ok := obj.Get("definition").(wire.Object)
			if !ok {
				http.Error(w, "invalid server definition", 400)
				return
			}
			replaced, err = config.Put(path, name, def)
		} else {
			if _, err = auth.Remove(path, name); err == nil {
				removed, err = config.Remove(path, name)
			}
		}
		if err != nil {
			http.Error(w, "cannot edit server registry", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"removed": removed, "replaced": replaced})
	})
	mux.HandleFunc("/signin/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var input struct {
			Name, RedirectURL, ClientID, ClientSecret string
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !validCallback(input.RedirectURL) {
			http.Error(w, "invalid sign-in request", http.StatusBadRequest)
			return
		}
		servers, err := config.Load(path)
		if err != nil {
			http.Error(w, "cannot load server registry", http.StatusInternalServerError)
			return
		}
		def, ok := servers.Get(input.Name).(wire.Object)
		if !ok {
			http.Error(w, "unknown server", http.StatusNotFound)
			return
		}
		endpoint, _ := def.Get("url").(string)
		if endpoint == "" {
			http.Error(w, "server does not use an HTTP address", http.StatusBadRequest)
			return
		}
		idBytes := make([]byte, 24)
		if _, err := rand.Read(idBytes); err != nil {
			http.Error(w, "cannot start sign-in", http.StatusInternalServerError)
			return
		}
		id := base64.RawURLEncoding.EncodeToString(idBytes)
		flowCtx, cancel := context.WithTimeout(authCtx, 10*time.Minute)
		flow := &signInFlow{page: make(chan string, 1), callback: make(chan url.Values, 1), done: make(chan signInResult, 1), cancel: cancel}
		authMu.Lock()
		if len(authFlows) >= maxSignInFlows {
			authMu.Unlock()
			cancel()
			http.Error(w, "too many sign-ins are in progress", http.StatusTooManyRequests)
			return
		}
		authFlows[id] = flow
		authMu.Unlock()
		headers := make(http.Header)
		if h, ok := def.Get("headers").(wire.Object); ok {
			for _, f := range h {
				headers.Set(f.Name, config.Expand(f.Value))
			}
		}
		go func() {
			result, err := auth.Authorize(flowCtx, auth.Options{ConfigPath: path, Name: input.Name, Endpoint: endpoint, Headers: headers,
				ClientID: input.ClientID, ClientSecret: input.ClientSecret, Version: version, RedirectURL: input.RedirectURL, AuthPage: flow.page, Callback: flow.callback})
			flow.done <- signInResult{result: result, err: err}
			close(flow.done)
			cancel()
			time.AfterFunc(time.Minute, func() {
				authMu.Lock()
				delete(authFlows, id)
				authMu.Unlock()
			})
		}()
		select {
		case page := <-flow.page:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "url": page})
		case result := <-flow.done:
			if result.err != nil {
				http.Error(w, "could not check sign-in", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"complete": true, "result": result.result})
		case <-r.Context().Done():
			cancel()
		}
	})
	mux.HandleFunc("/signin/remove", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var input struct{ Name string }
		if json.NewDecoder(r.Body).Decode(&input) != nil || input.Name == "" {
			http.Error(w, "invalid sign-out request", http.StatusBadRequest)
			return
		}
		removed, err := auth.Remove(path, input.Name)
		if err != nil {
			http.Error(w, "cannot update sign-in store", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"removed": removed})
	})
	mux.HandleFunc("/signin/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/signin/")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		authMu.Lock()
		flow := authFlows[id]
		authMu.Unlock()
		if flow == nil {
			http.Error(w, "sign-in expired", http.StatusGone)
			return
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("state") == "" {
			http.Error(w, "invalid callback", http.StatusBadRequest)
			return
		}
		select {
		case flow.callback <- r.Form:
		default:
			http.Error(w, "callback already received", http.StatusConflict)
			return
		}
		select {
		case result := <-flow.done:
			if result.err != nil {
				http.Error(w, "sign-in failed", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(result.result)
		case <-r.Context().Done():
		}
	})
	protected := http.NewCrossOriginProtection().Handler(mux)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publicPairing := r.URL.Path == "/identity" || r.URL.Path == "/pair"
		if publicPairing && r.TLS == nil {
			http.Error(w, "pairing requires HTTPS", http.StatusUpgradeRequired)
			return
		}
		if !publicPairing {
			authorization := r.Header.Get("Authorization")
			role := ""
			if devices != nil && strings.HasPrefix(authorization, "Bearer ") {
				role = devices.role(strings.TrimPrefix(authorization, "Bearer "))
			}
			adminRoute := r.URL.Path == "/servers" || strings.HasPrefix(r.URL.Path, "/signin/")
			if adminRoute && role != "admin" || !adminRoute && role == "" {
				http.Error(w, "unauthorized", 401)
				return
			}
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
		stopAuth()
		authMu.Lock()
		for _, flow := range authFlows {
			flow.cancel()
		}
		authFlows = make(map[string]*signInFlow)
		authMu.Unlock()
		stopAdmissions()
		admission.Lock()
		defer admission.Unlock()
		closed = true
		for session := range s.Sessions() {
			_ = session.Close()
		}
		e.Close()
	}, nil
}

// Serve exposes the local registry only over paired-device HTTPS until cancellation.
func Serve(ctx context.Context, path, version string, opts Options) error {
	if opts.Addr == "" {
		opts.Addr = "0.0.0.0:8443"
	}
	host, _, err := net.SplitHostPort(opts.Addr)
	if err != nil {
		return fmt.Errorf("invalid remote listen address")
	}
	if (opts.TLSCert == "") != (opts.TLSKey == "") {
		return fmt.Errorf("--tls-cert and --tls-key must be supplied together")
	}
	var devices *deviceManager
	var h http.Handler
	var cleanup func()
	devices, err = newDeviceManager(path, opts.TLSCert, opts.TLSKey)
	if err == nil {
		h, cleanup, err = handler(path, version, devices)
	}
	if err != nil {
		return err
	}
	defer cleanup()
	listener, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return fmt.Errorf("cannot listen on remote address")
	}
	defer listener.Close()
	var mdns *zeroconf.Server
	if !config.Loopback(host) {
		_, port, _ := net.SplitHostPort(listener.Addr().String())
		portNumber, _ := strconv.Atoi(port)
		mdns, err = zeroconf.Register(devices.name, pairingService, "local.", portNumber,
			[]string{"id=" + devices.id, "fingerprint=" + devices.fingerprint, "scheme=https"}, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "tap: local-network discovery is unavailable; pairing by address still works")
		}
	}
	out := human.Writer{W: os.Stderr}
	fmt.Fprintf(out, "tap remote serve: paired devices only on %s\n", listener.Addr())
	fmt.Fprintf(out, "Verify this TLS fingerprint on each client: %s\n", devices.fingerprint)
	fmt.Fprintf(out, "Admin pairing code (valid for 15 minutes): %s\n", devices.adminCode)
	fmt.Fprintf(out, "Execution-only pairing code (valid for 15 minutes): %s\n", devices.execCode)
	if mdns != nil {
		defer mdns.Shutdown()
	}
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
	err = srv.Serve(tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{devices.certificate()}}))
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

// New validates a paired relay and loads its owner-only device credential.
func New(cfg config.Remote, version string) (*Client, error) {
	if cfg.PeerID == "" {
		return nil, fmt.Errorf("legacy token remotes are no longer supported; pair this device with \"tap remote pair NAME HTTPS_URL\"")
	}
	endpoint, err := config.NormalizeURL(cfg.URL, false)
	if err != nil {
		return nil, err
	}
	cfg.URL = endpoint
	token := cfg.Token
	if token == "" {
		return nil, fmt.Errorf("paired remote credentials are missing; pair this device again")
	}
	lifetime, cancel := context.WithCancel(context.Background())
	base := http.RoundTripper(nil)
	if cfg.Fingerprint != "" {
		base = pinnedTransport(cfg.Fingerprint)
	}
	return &Client{ctx: lifetime, cancel: cancel, cfg: cfg, version: version, http: &http.Client{Transport: bearerTransport{token: token, base: base}, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("remote redirects are forbidden") }}}, nil
}

// pinnedTransport encrypts traffic and accepts only the server certificate paired out of band.
func pinnedTransport(fingerprint string) *http.Transport {
	want, _ := hex.DecodeString(fingerprint)
	return &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return fmt.Errorf("remote sent no certificate")
		}
		got := sha256.Sum256(state.PeerCertificates[0].Raw)
		if subtle.ConstantTimeCompare(got[:], want) != 1 {
			return fmt.Errorf("remote certificate changed; pair the device again after verifying its identity")
		}
		return nil
	}}}
}

// PairingInfo fetches the unauthenticated identity presented over TLS; the caller must verify
// its fingerprint out of band before sending a pairing code.
type PairingInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

// InspectPairing reads a candidate server identity without trusting it or sending credentials.
func InspectPairing(ctx context.Context, rawURL string) (*PairingInfo, error) {
	endpoint, err := config.NormalizeURL(rawURL, false)
	if err != nil || !strings.HasPrefix(endpoint, "https://") {
		return nil, fmt.Errorf("pairing requires an HTTPS address")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("redirects are forbidden") }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(endpoint, "/mcp")+"/identity", nil)
	if err != nil {
		return nil, fmt.Errorf("invalid pairing address")
	}
	resp, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("cannot reach pairing endpoint")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		return nil, fmt.Errorf("remote does not offer device pairing")
	}
	var info PairingInfo
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&info) != nil {
		return nil, fmt.Errorf("invalid remote identity")
	}
	fingerprint := sha256.Sum256(resp.TLS.PeerCertificates[0].Raw)
	if !strings.EqualFold(info.Fingerprint, hex.EncodeToString(fingerprint[:])) {
		return nil, fmt.Errorf("remote identity fingerprint does not match its TLS certificate")
	}
	return &info, nil
}

// PairResult is the per-device credential returned once over the pinned TLS connection.
type PairResult struct {
	ID    string `json:"id"`
	Token string `json:"token"`
	Role  string `json:"role"`
}

// Pair enrolls one client using a one-time host code after the TLS identity was checked out of band.
func Pair(ctx context.Context, rawURL, fingerprint, code, deviceName, role string) (*PairResult, error) {
	endpoint, err := config.NormalizeURL(rawURL, false)
	if err != nil || !strings.HasPrefix(endpoint, "https://") {
		return nil, fmt.Errorf("pairing requires an HTTPS address")
	}
	if role != "admin" && role != "execution" {
		return nil, fmt.Errorf("invalid pairing role")
	}
	body, err := json.Marshal(map[string]string{"code": code, "name": deviceName, "role": role})
	if err != nil {
		return nil, fmt.Errorf("invalid pairing request")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(endpoint, "/mcp")+"/pair", strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("invalid pairing address")
	}
	request.Header.Set("Content-Type", "application/json")
	transport := pinnedTransport(fingerprint)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("redirects are forbidden") }}
	resp, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("pairing failed; verify the remote fingerprint and pairing code")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pairing failed (HTTP %d)", resp.StatusCode)
	}
	var result PairResult
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result) != nil || result.ID == "" || result.Token == "" || result.Role != role {
		return nil, fmt.Errorf("invalid pairing response")
	}
	return &result, nil
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

// Health reports whether the selected remote is reachable with this client's credential.
func (c *Client) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	endpoint := strings.TrimSuffix(c.cfg.URL, "/mcp") + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("invalid remote health request")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("remote is not reachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote health check failed (HTTP %d)", resp.StatusCode)
	}
	return nil
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
	return wire.BoundResponse(base.RoundTrip(r))
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
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: c.cfg.URL, HTTPClient: c.http, MaxRetries: -1, MaxEventSize: wire.MaxMessageBytes, DisableStandaloneSSE: true}, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
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

// EditResult reports whether a remote definition was replaced or removed.
type EditResult struct {
	Replaced bool `json:"replaced"`
	Removed  bool `json:"removed"`
}

// Edit performs remote administration; an optional admin environment token overrides the relay token.
func (c *Client) Edit(ctx context.Context, name string, definition wire.Object) (EditResult, error) {
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
		return EditResult{}, fmt.Errorf("invalid server definition")
	}
	endpoint := strings.TrimSuffix(c.cfg.URL, "/mcp") + "/servers"
	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(string(b)))
	if err != nil {
		return EditResult{}, fmt.Errorf("invalid remote request")
	}
	req.Header.Set("Content-Type", "application/json")
	client := c.adminHTTP()
	resp, err := client.Do(req)
	if err != nil {
		return EditResult{}, fmt.Errorf("remote administration failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return EditResult{}, fmt.Errorf("remote administration failed (HTTP %d)", resp.StatusCode)
	}
	var out EditResult
	if json.NewDecoder(io.LimitReader(resp.Body, bodyLimit)).Decode(&out) != nil {
		return EditResult{}, fmt.Errorf("invalid remote response")
	}
	return out, nil
}

// adminHTTP uses this device's paired role for protected administration routes.
func (c *Client) adminHTTP() *http.Client { return c.http }

// Auth relays an OAuth callback through the laptop while keeping registration, exchange and
// stored credentials on the serving machine.
func (c *Client) Auth(ctx context.Context, name, clientID, clientSecret string, port int, open func(string) error, say io.Writer) (*auth.Result, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("cannot listen locally for the sign-in callback")
	}
	defer listener.Close()
	id := ""
	finished := make(chan signInResult, 1)
	callbackServer := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" || r.Method != http.MethodGet || r.URL.Query().Get("state") == "" {
			http.Error(w, "invalid sign-in callback", http.StatusBadRequest)
			return
		}
		endpoint := strings.TrimSuffix(c.cfg.URL, "/mcp") + "/signin/" + url.PathEscape(id)
		request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, strings.NewReader(r.URL.RawQuery))
		if err != nil {
			http.Error(w, "could not relay sign-in callback", http.StatusBadGateway)
			return
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := c.adminHTTP().Do(request)
		if err != nil {
			finished <- signInResult{err: fmt.Errorf("could not finish remote sign-in")}
			http.Error(w, "Tap could not finish sign-in with the remote. Return to the terminal.", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			finished <- signInResult{err: fmt.Errorf("remote sign-in failed (HTTP %d)", resp.StatusCode)}
			http.Error(w, "Tap could not finish sign-in with the remote. Return to the terminal.", http.StatusBadGateway)
			return
		}
		var result auth.Result
		if json.NewDecoder(io.LimitReader(resp.Body, bodyLimit)).Decode(&result) != nil {
			finished <- signInResult{err: fmt.Errorf("invalid remote sign-in response")}
			http.Error(w, "Tap received an invalid sign-in response. Return to the terminal.", http.StatusBadGateway)
			return
		}
		finished <- signInResult{result: &result}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "Tap is signed in. You can close this tab.\n")
	})}
	go func() { _ = callbackServer.Serve(listener) }()
	defer callbackServer.Close()
	redirect := "http://" + listener.Addr().String() + "/callback"
	startURL := strings.TrimSuffix(c.cfg.URL, "/mcp") + "/signin/start"
	var body struct {
		ID       string       `json:"id"`
		URL      string       `json:"url"`
		Complete bool         `json:"complete"`
		Result   *auth.Result `json:"result"`
	}
	if err := c.adminRequest(ctx, http.MethodPost, startURL, map[string]string{"name": name, "redirectURL": redirect, "clientID": clientID, "clientSecret": clientSecret}, &body); err != nil {
		return nil, err
	}
	if body.Complete && body.Result != nil {
		return body.Result, nil
	}
	if body.ID == "" || body.URL == "" {
		return nil, fmt.Errorf("invalid remote sign-in response")
	}
	id = body.ID
	if open != nil {
		if err := open(body.URL); err != nil && say != nil {
			fmt.Fprintf(say, "Open this page to sign in to %s:\n\n  %s\n\n", name, body.URL)
		}
	} else if say != nil {
		fmt.Fprintf(say, "Open this page to sign in to %s:\n\n  %s\n\n", name, body.URL)
	}
	select {
	case result := <-finished:
		return result.result, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, c.ctx.Err()
	}
}

// AuthRemove deletes a grant on the serving machine.
func (c *Client) AuthRemove(ctx context.Context, name string) (bool, error) {
	endpoint := strings.TrimSuffix(c.cfg.URL, "/mcp") + "/signin/remove"
	var result struct {
		Removed bool `json:"removed"`
	}
	if err := c.adminRequest(ctx, http.MethodPost, endpoint, map[string]string{"name": name}, &result); err != nil {
		return false, err
	}
	return result.Removed, nil
}

func (c *Client) adminRequest(ctx context.Context, method, endpoint string, input, output any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("invalid remote sign-in request")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("invalid remote request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.adminHTTP().Do(req)
	if err != nil {
		return fmt.Errorf("remote sign-in request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote sign-in failed (HTTP %d)", resp.StatusCode)
	}
	if json.NewDecoder(io.LimitReader(resp.Body, bodyLimit)).Decode(output) != nil {
		return fmt.Errorf("invalid remote sign-in response")
	}
	return nil
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
// sign-in instruction names only a server and can be acted on through the authenticated relay.
func safeCatalog(out wire.Object, field string) wire.Object {
	rows, _ := out.Get(field).([]any)
	for _, v := range rows {
		row, ok := v.(wire.Object)
		if !ok || !row.Has("error") {
			continue
		}
		if msg, _ := row.Get("error").(string); auth.IsRequiredMessage(msg) {
			row.Set("error", msg)
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
