// Package registry manages bounded downstream sessions and resilient tool catalogs.
// An Engine belongs to one tap process and must be closed.
package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/fschrhunt/tap/internal/auth"
	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Engine holds resident sessions and caches for one fixed config path and version.
type Engine struct {
	Path, Version string
	// Grace is how long the revalidation of a catalog restored from the index waits. Those
	// tools already answer the search that asked for them, so a tap that serves an agent
	// sets it, and starting the servers follows that answer instead of competing with it. A
	// command, which waits for the fresh list, leaves it at zero.
	Grace       time.Duration
	deadline    time.Duration
	deadlineMS  float64
	mu          sync.Mutex
	entries     map[string]*entry
	ctx         context.Context
	cancel      context.CancelFunc
	closeOnce   sync.Once
	snapshotMu  sync.Mutex
	last        *configSnapshot
	indexMu     sync.Mutex
	indexDirty  chan struct{}
	indexDone   chan struct{}
	definitions map[string]string
	catalogs    map[string]*catalog
	indexLoaded bool
	slots       chan struct{}
	failTTL     time.Duration
	idleTTL     time.Duration
	workers     sync.WaitGroup
}

// entry is one connection generation; ready publishes session and err.
type entry struct {
	ready       chan struct{}
	session     *mcp.ClientSession
	err         error
	failedAt    time.Time
	openedAt    time.Time
	lastUsed    time.Time
	users       int
	fingerprint string
}

// New creates an engine without opening any downstream connections.
func New(path, version string) *Engine {
	ms := wire.Number(os.Getenv("TAP_DEADLINE_MS"))
	if math.IsNaN(ms) || ms == 0 {
		ms = 5000
	}
	durationMS := ms
	if ms < 1 || ms > 2147483647 {
		durationMS = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{Path: path, Version: version, deadline: time.Duration(durationMS * float64(time.Millisecond)), deadlineMS: ms, entries: map[string]*entry{}, ctx: ctx, cancel: cancel,
		indexDirty: make(chan struct{}, 1), indexDone: make(chan struct{}),
		definitions: map[string]string{}, catalogs: map[string]*catalog{}, slots: make(chan struct{}, 8),
		failTTL: envDuration("TAP_FAIL_TTL_MS", 5*time.Second), idleTTL: envDuration("TAP_IDLE_TTL_MS", 5*time.Minute)}
	e.workers.Add(1)
	go e.reap()
	go e.persistIndex()
	return e
}

// Message formats SDK errors without exposing credential-bearing endpoint URLs.
func Message(err error) string {
	var signIn *auth.Required
	if errors.As(err, &signIn) {
		return signIn.Error()
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, mcp.ErrConnectionClosed) {
		return "Connection closed"
	}
	var neterr *url.Error
	if errors.As(err, &neterr) {
		return "fetch failed"
	}
	var rpc *jsonrpc.Error
	if errors.As(err, &rpc) {
		return rpc.Message
	}
	return err.Error()
}

// deadlineError replaces SDK context errors with tap's per-server error.
func (e *Engine) deadlineError(ctx context.Context, err error) error {
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("server deadline exceeded (%s ms)", wire.String(e.deadlineMS))
	}
	return err
}

// connect shares initialization using the caller's already-loaded definition.
// Caller cancellation only stops waiting; initialization belongs to the engine.
func (e *Engine) connect(ctx context.Context, name string, def wire.Object, fingerprint string, quiet bool) (*entry, error) {
	e.mu.Lock()
	if e.ctx.Err() != nil {
		e.mu.Unlock()
		return nil, e.ctx.Err()
	}
	if e.definitions[name] != fingerprint {
		e.mu.Unlock()
		return nil, fmt.Errorf("server %q configuration changed", name)
	}
	ent := e.entries[name]
	if ent != nil {
		select {
		case <-ent.ready:
			if ent.err != nil && time.Since(ent.failedAt) >= e.failTTL {
				delete(e.entries, name)
				ent = nil
			}
		default:
		}
	}
	if ent == nil {
		ent = &entry{ready: make(chan struct{}), fingerprint: fingerprint, lastUsed: time.Now()}
		e.entries[name] = ent
		e.workers.Add(1)
		go e.open(name, def, quiet, ent)
	}
	ent.users++
	ent.lastUsed = time.Now()
	e.mu.Unlock()
	select {
	case <-ctx.Done():
		e.release(ent)
		return nil, ctx.Err()
	case <-ent.ready:
		if ent.err != nil {
			e.release(ent)
			return nil, ent.err
		}
		return ent, nil
	}
}

// release marks the end of a list or call so active sessions are never reaped.
func (e *Engine) release(ent *entry) {
	e.mu.Lock()
	ent.users--
	ent.lastUsed = time.Now()
	e.mu.Unlock()
}

// acquire bounds network and process work, without queuing cache lookups.
func (e *Engine) acquire(ctx context.Context) error {
	select {
	case e.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// open initializes within its own budget after admission and watches disconnects.
func (e *Engine) open(name string, def wire.Object, quiet bool, ent *entry) {
	defer e.workers.Done()
	err := e.acquire(e.ctx)
	if err == nil {
		defer func() { <-e.slots }()
		ent.openedAt = time.Now()
		ctx, cancel := context.WithTimeout(e.ctx, e.deadline)
		defer cancel()
		transport, terr := transportFor(e.Path, name, def, quiet)
		err = terr
		if err == nil {
			client := mcp.NewClient(&mcp.Implementation{Name: "tap", Version: e.Version}, &mcp.ClientOptions{
				Capabilities: &mcp.ClientCapabilities{},
				ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
					e.mu.Lock()
					if c := e.catalogs[name]; c != nil && c.fingerprint == ent.fingerprint {
						c.at = time.Time{}
						c.retryAt = time.Time{}
						c.revision++
					}
					e.mu.Unlock()
				},
			})
			// HTTP connections must retain SDK-private initialization hooks for
			// protocol headers and notifications. Only stdio needs raw capture.
			if _, http := transport.(*mcp.StreamableClientTransport); !http {
				transport = &wire.Transport{Base: transport}
			}
			ent.session, err = client.Connect(ctx, transport, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
		}
		err = e.deadlineError(ctx, err)
	}
	e.mu.Lock()
	ent.err = err
	ent.failedAt = time.Now()
	close(ent.ready)
	e.mu.Unlock()
	if err == nil {
		go func() { _ = ent.session.Wait(); e.discard(name, ent) }()
	}
}

// discard evicts only this generation and closes outside the engine lock.
func (e *Engine) discard(name string, ent *entry) {
	e.mu.Lock()
	if e.entries[name] == ent {
		delete(e.entries, name)
		e.closeEntry(ent)
	}
	e.mu.Unlock()
}

// closeEntry registers teardown under mu so Close also waits for detached sessions.
func (e *Engine) closeEntry(ent *entry) {
	e.workers.Add(1)
	go func() {
		defer e.workers.Done()
		<-ent.ready
		if ent.session != nil {
			_ = ent.session.Close()
		}
	}()
}

type headerTransport struct{ headers http.Header }

// RoundTrip adds expanded static headers and authentication to every HTTP request.
func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header = req.Header.Clone()
	for k, v := range t.headers {
		r.Header[k] = v
	}
	return http.DefaultTransport.RoundTrip(r)
}

// transportFor chooses the SDK transport and configures the child environment. An HTTP server
// with no credential of its own configured is lent the sign-in saved for it beside the config
// at path, if there is one.
func transportFor(path, name string, def wire.Object, quiet bool) (mcp.Transport, error) {
	typ, _ := def.Get("type").(string)
	endpoint, _ := def.Get("url").(string)
	if typ == "" {
		if endpoint != "" {
			typ = "http"
		} else {
			typ = "stdio"
		}
	}
	if typ == "stdio" {
		args, _ := def.Get("command").([]any)
		if len(args) == 0 || wire.String(args[0]) == "" {
			return nil, fmt.Errorf("server \"%s\" has no command", name)
		}
		argv := make([]string, len(args))
		for i, a := range args {
			argv[i] = wire.String(a)
		}
		cwd, _ := def.Get("cwd").(string)
		cwd = config.Home(cwd)
		env, _ := def.Get("env").(wire.Object)
		executable := config.Home(argv[0])
		if path, exists := env.Get("PATH"), env.Has("PATH"); exists && !strings.ContainsRune(executable, '/') {
			found := ""
			for _, dir := range filepath.SplitList(config.Expand(path)) {
				candidate := filepath.Join(dir, executable)
				if !filepath.IsAbs(candidate) {
					candidate = filepath.Join(cwd, candidate)
				}
				if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0111 != 0 {
					found, _ = filepath.Abs(candidate)
					break
				}
			}
			if found == "" {
				return nil, fmt.Errorf("spawn %s ENOENT", executable)
			}
			executable = found
		}
		cmd := exec.Command(executable, argv[1:]...)
		if cmd.Err != nil {
			return nil, fmt.Errorf("spawn %s ENOENT", config.Home(argv[0]))
		}
		cmd.Dir = cwd
		cmd.Env = os.Environ()
		for _, f := range env {
			cmd.Env = append(cmd.Env, f.Name+"="+config.Expand(f.Value))
		}
		if !quiet {
			cmd.Stderr = os.Stderr
		}
		return &commandTransport{CommandTransport: &mcp.CommandTransport{Command: cmd, TerminateDuration: 250 * time.Millisecond}, name: config.Home(argv[0])}, nil
	}
	if typ != "http" {
		return nil, fmt.Errorf("server \"%s\" has unsupported type \"%s\"", name, typ)
	}
	if endpoint == "" {
		return nil, fmt.Errorf("server \"%s\" has no url", name)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("Invalid URL")
	}
	headers := http.Header{}
	if h, ok := def.Get("headers").(wire.Object); ok {
		for _, f := range h {
			headers.Set(f.Name, config.Expand(f.Value))
		}
	}
	if token, ok := def.Get("bearerTokenEnv").(string); ok && os.Getenv(token) != "" {
		headers.Set("Authorization", "Bearer "+os.Getenv(token))
	}
	transport := &mcp.StreamableClientTransport{
		Endpoint: endpoint,
		HTTPClient: &http.Client{
			Transport:     headerTransport{headers},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		MaxRetries: -1,
	}
	if headers.Get("Authorization") == "" {
		transport.OAuthHandler = auth.Handler(path, name, endpoint)
	}
	return transport, nil
}

type serverTools struct {
	name  string
	tools []wire.Object
	err   string
	stale bool
}

// allTools queries configured servers in parallel while retaining registry order.
func (e *Engine) allTools(ctx context.Context, quiet bool) ([]serverTools, error) {
	snapshot, err := e.snapshot()
	if err != nil {
		return nil, err
	}
	servers := snapshot.servers
	rows := make([]serverTools, len(servers))
	var wg sync.WaitGroup
	for i, f := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			def, _ := f.Value.(wire.Object)
			tools, stale, err := e.toolsFor(ctx, f.Name, def, snapshot.fingerprints[f.Name], quiet)
			rows[i] = serverTools{name: f.Name, tools: tools, stale: stale}
			if err != nil {
				msg := Message(err)
				var neterr *url.Error
				if errors.As(err, &neterr) {
					msg = "fetch failed"
				}
				rows[i].err = msg
			}
		}()
	}
	wg.Wait()
	return rows, nil
}

// Listing returns catalog counts, marking stale rows and their last refresh errors.
func (e *Engine) Listing(ctx context.Context, quiet bool) (wire.Object, error) {
	rows, err := e.allTools(ctx, quiet)
	if err != nil {
		return nil, err
	}
	integrations := []any{}
	for _, r := range rows {
		if len(r.tools) == 0 && r.err == "" {
			continue
		}
		row := wire.Object{{Name: "server", Value: r.name}, {Name: "tools", Value: len(r.tools)}}
		if r.stale {
			row.Set("stale", true)
		}
		if r.err != "" {
			row.Set("error", r.err)
		}
		integrations = append(integrations, row)
	}
	return wire.Object{{Name: "config", Value: e.Path}, {Name: "integrations", Value: integrations}}, nil
}

// tokenize makes camelCase, underscores and punctuation comparable for scoring: lowercase
// runs of ASCII letters and digits, with a break where a capital follows a lowercase letter
// or a digit.
func tokenize(s string) []string {
	out := []string{}
	var word []byte
	flush := func() {
		if len(word) > 0 {
			out = append(out, string(word))
			word = word[:0]
		}
	}
	alnum := func(r rune) bool { return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' }
	var prev rune
	for _, r := range s {
		if alnum(prev) && r >= 'A' && r <= 'Z' {
			flush()
		}
		if lower := unicode.ToLower(r); alnum(lower) {
			word = append(word, byte(lower))
		} else {
			flush()
		}
		prev = r
	}
	flush()
	return out
}

type match struct {
	rank int
	tool wire.Object
}

// Search ranks tools matching every query term and applies JavaScript slice limits.
// Stale tools remain searchable alongside diagnostics for failed refreshes.
// CLI limits intentionally retain Node's unconstrained behavior; MCP validates 1..25.
func (e *Engine) Search(ctx context.Context, query string, limit float64, quiet bool) (wire.Object, error) {
	rows, err := e.allTools(ctx, quiet)
	if err != nil {
		return nil, err
	}
	tokens := tokenize(query)
	ranked := []match{}
	unavailable := []any{}
	for _, r := range rows {
		if r.err != "" {
			unavailable = append(unavailable, wire.Object{{Name: "server", Value: r.name}, {Name: "error", Value: r.err}})
		}
		for _, t := range r.tools {
			name, _ := t.Get("name").(string)
			title, _ := t.Get("title").(string)
			desc, _ := t.Get("description").(string)
			hay := strings.ToLower(name + " " + r.name + " " + title + " " + desc)
			names := tokenize(name)
			rank := 0
			for _, token := range tokens {
				if !strings.Contains(hay, token) {
					rank = 0
					break
				}
				rank++
				for _, n := range names {
					if n == token {
						rank += 2
						break
					}
				}
			}
			if rank > 0 {
				tool := wire.Object{{Name: "id", Value: r.name + "." + name}}
				if r.stale {
					tool.Set("stale", true)
				}
				for _, k := range []string{"title", "description", "inputSchema", "annotations", "outputSchema"} {
					if t.Has(k) {
						tool.Set(k, t.Get(k))
					}
				}
				ranked = append(ranked, match{rank, tool})
			}
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].rank > ranked[j].rank })
	n := len(ranked)
	end := n
	if limit < float64(n) {
		if limit <= -float64(n) {
			end = 0
		} else {
			end = int(limit)
			if end < 0 {
				end = n + end
			}
		}
	} else if limit != limit {
		end = 0
	}
	matches := []any{}
	for _, m := range ranked[:end] {
		matches = append(matches, m.tool)
	}
	out := wire.Object{{Name: "query", Value: query}, {Name: "total", Value: n}, {Name: "matches", Value: matches}, {Name: "unavailable", Value: unavailable}}
	if n == 0 && len(unavailable) == 0 {
		out.Set("hint", "No tool matched every term. Try fewer or broader terms, or omit the query to list the configured servers.")
	}
	return out, nil
}

// Call runs server.tool, retaining raw stdio results and SDK-supported HTTP result fields.
func (e *Engine) Call(ctx context.Context, id string, args any, quiet bool) (wire.Object, error) {
	return e.CallWithMeta(ctx, id, args, quiet, nil)
}

// CallWithMeta forwards application metadata, leaving protocol negotiation to each SDK session.
func (e *Engine) CallWithMeta(ctx context.Context, id string, args any, quiet bool, meta mcp.Meta) (wire.Object, error) {
	dot := strings.Index(id, ".")
	if dot < 1 {
		return nil, fmt.Errorf("invalid tool id \"%s\" (expected server.tool)", id)
	}
	name, tool := id[:dot], id[dot+1:]
	snapshot, err := e.snapshot()
	if err != nil {
		return nil, err
	}
	servers := snapshot.servers
	def, ok := servers.Get(name).(wire.Object)
	if !ok {
		names := make([]string, len(servers))
		for i, f := range servers {
			names[i] = f.Name
		}
		known := strings.Join(names, ", ")
		if known == "" {
			known = "none"
		}
		return nil, fmt.Errorf("unknown server %q (configured: %s)", name, known)
	}
	cctx, cancel := context.WithTimeout(ctx, e.deadline)
	ent, err := e.connect(cctx, name, def, snapshot.fingerprints[name], quiet)
	err = e.deadlineError(cctx, err)
	cancel()
	if err != nil {
		return nil, err
	}
	defer e.release(ent)
	cctx, raw := wire.Capture(ctx)
	result, err := ent.session.CallTool(cctx, &mcp.CallToolParams{Name: tool, Arguments: args, Meta: wire.ForwardMeta(meta)})
	if err != nil {
		return nil, err
	}
	if len(*raw) == 0 {
		*raw, err = wire.JSON(result, false)
		if err != nil {
			return nil, err
		}
	}
	v, err := wire.Decode(*raw)
	if err != nil {
		return nil, err
	}
	o, _ := v.(wire.Object)
	return ordered(o, "_meta", "content", "structuredContent", "isError"), nil
}

// Close waits for workers, flushes queued catalogs, and tears down every transport.
// It is safe after failed or partial connects.
func (e *Engine) Close() {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.cancel()
		entries := e.entries
		e.entries = map[string]*entry{}
		e.mu.Unlock()
		e.workers.Wait()
		close(e.indexDirty)
		<-e.indexDone
		var wg sync.WaitGroup
		for _, ent := range entries {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-ent.ready
				if ent.session != nil {
					_ = ent.session.Close()
				}
			}()
		}
		wg.Wait()

	})
}

// ordered mirrors the TypeScript SDK's known-field-first JSON decoding.
func ordered(o wire.Object, names ...string) wire.Object {
	out := wire.Object{}
	for _, name := range names {
		if o.Has(name) {
			out.Set(name, o.Get(name))
		}
	}
	for _, f := range o {
		if !out.Has(f.Name) {
			out.Set(f.Name, f.Value)
		}
	}
	return out
}

// commandTransport retains Node's spawn diagnostics around the SDK transport.
type commandTransport struct {
	*mcp.CommandTransport
	name string
}

// Connect reports child setup errors in the original CLI's form.
func (t *commandTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	c, err := t.CommandTransport.Connect(ctx)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("spawn %s ENOENT", t.name)
	}
	if os.IsPermission(err) {
		return nil, fmt.Errorf("spawn %s EACCES", t.name)
	}
	return c, err
}
