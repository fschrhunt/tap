// Package registry manages lazy downstream MCP sessions, scoped metadata caches,
// discovery and calls. An Engine belongs to one tap process and must be closed.
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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/discovery"
	"github.com/fschrhunt/tap/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Engine holds resident sessions and caches for one fixed config path and version.
type Engine struct {
	Path, Version string
	deadline      time.Duration
	deadlineMS    float64
	mu            sync.Mutex
	entries       map[string]*entry
	catalogs      map[string]catalog
	cacheDir      string
	results       resultStore
	indexMu       sync.Mutex
	index         *discovery.Index
	indexKey      string
	ctx           context.Context
	cancel        context.CancelFunc
	closeOnce     sync.Once
	closing       bool
	cleanup       sync.WaitGroup
}
type entry struct {
	ready    chan struct{}
	session  *mcp.ClientSession
	err      error
	mu       sync.Mutex
	tools    []wire.Object
	at       time.Time
	key      string
	changed  atomic.Uint64
	listed   uint64
	uses     int
	lastUsed time.Time
	idle     time.Duration
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
	e := &Engine{Path: path, Version: version, deadline: time.Duration(durationMS * float64(time.Millisecond)), deadlineMS: ms, entries: map[string]*entry{}, catalogs: map[string]catalog{}, cacheDir: cacheDirectory(), ctx: ctx, cancel: cancel}
	go e.stopIdle()
	return e
}

// Message formats SDK errors in the same form as the TypeScript client.
func Message(err error) string {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, mcp.ErrConnectionClosed) {
		return "Connection closed"
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

// connect shares an in-flight initialization and retries discarded sessions.
func (e *Engine) connect(ctx context.Context, name string, quiet bool) (*entry, error) {
	servers, err := config.Load(e.Path)
	if err != nil {
		return nil, err
	}
	def, ok := servers.Get(name).(wire.Object)
	if !ok {
		return nil, fmt.Errorf("unknown server %q; run plugin_search without a query to list configured servers", name)
	}
	key := e.cacheKey(name, def)
	e.mu.Lock()
	if e.ctx.Err() != nil {
		e.mu.Unlock()
		return nil, e.ctx.Err()
	}
	ent := e.entries[name]
	if ent != nil && ent.key != key {
		delete(e.entries, name)
		e.retireLocked(ent)
		ent = nil
		delete(e.catalogs, name)
	}
	if ent == nil {
		ent = &entry{ready: make(chan struct{}), key: key}
		e.entries[name] = ent
		go e.open(name, def, quiet, ent)
	}
	e.mu.Unlock()
	select {
	case <-ctx.Done():
		return ent, ctx.Err()
	case <-ent.ready:
		if ent.err != nil {
			return ent, ent.err
		}
		e.mu.Lock()
		if e.entries[name] != ent {
			e.mu.Unlock()
			return e.connect(ctx, name, quiet)
		}
		ent.uses++
		e.mu.Unlock()
		return ent, nil
	}
}

// open initializes one server within its own deadline and watches for disconnects.
func (e *Engine) open(name string, def wire.Object, quiet bool, ent *entry) {
	ctx, cancel := context.WithTimeout(e.ctx, e.deadline)
	defer cancel()
	var err error
	var transport mcp.Transport
	transport, err = transportFor(name, def, quiet)
	if def.Has("idleTimeoutMs") {
		ms, ok := def.Get("idleTimeoutMs").(float64)
		if !ok || ms <= 0 || ms > 86400000 || ms != math.Trunc(ms) || !endpointIsStdio(def) {
			err = fmt.Errorf("idleTimeoutMs must be an integer 1..86400000 for a stdio server")
		} else {
			ent.idle = time.Duration(ms) * time.Millisecond
		}
	}
	if err == nil {
		client := mcp.NewClient(&mcp.Implementation{Name: "tap", Version: e.Version}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}, ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { e.invalidateCatalog(name, ent) }})
		ent.session, err = client.Connect(ctx, &wire.Transport{Base: transport}, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	}
	ent.err = e.deadlineError(ctx, err)
	close(ent.ready)
	if ent.err != nil {
		e.discard(name, ent)
		return
	}
	go func() { _ = ent.session.Wait(); e.discard(name, ent) }()
}

// discard evicts only the failed generation and closes it without delaying search.
func (e *Engine) discard(name string, ent *entry) {
	if ent == nil {
		return
	}
	e.mu.Lock()
	if e.entries[name] == ent {
		delete(e.entries, name)
		if !e.closing {
			e.retireLocked(ent)
		}
	}
	e.mu.Unlock()
}

// retireLocked tracks asynchronous closes so EOF also waits for previously retired children.
// The engine lock must be held; no new retirement may start after closing is set.
func (e *Engine) retireLocked(ent *entry) {
	e.cleanup.Add(1)
	go func() { defer e.cleanup.Done(); closeEntry(ent) }()
}

// reconcile retires changed/removed sessions and drops metadata for removed integrations.
func (e *Engine) reconcile(servers wire.Object) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return
	}
	for name, ent := range e.entries {
		def, ok := servers.Get(name).(wire.Object)
		if !ok || e.cacheKey(name, def) != ent.key {
			delete(e.entries, name)
			delete(e.catalogs, name)
			e.retireLocked(ent)
		}
	}
	for name := range e.catalogs {
		if !servers.Has(name) {
			delete(e.catalogs, name)
		}
	}
}

// closeEntry waits for initialization before closing a retired connection.
func closeEntry(ent *entry) {
	<-ent.ready
	if ent.session != nil {
		_ = ent.session.Close()
	}
}

// release marks a completed operation idle; active operations are never idle-stopped.
func (e *Engine) release(ent *entry) {
	e.mu.Lock()
	ent.uses--
	ent.lastUsed = time.Now()
	e.mu.Unlock()
}

// endpointIsStdio distinguishes restartable local processes from remote sessions.
func endpointIsStdio(def wire.Object) bool {
	return def.Get("type") == "stdio" || (def.Get("type") == nil && def.Get("url") == nil)
}

// stopIdle honors opt-in timeouts only for initialized, unused local backends.
func (e *Engine) stopIdle() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-t.C:
			e.mu.Lock()
			for name, ent := range e.entries {
				select {
				case <-ent.ready:
					if ent.err == nil && ent.idle > 0 && ent.uses == 0 && !ent.lastUsed.IsZero() && time.Since(ent.lastUsed) >= ent.idle {
						delete(e.entries, name)
						if !e.closing {
							e.retireLocked(ent)
						}
					}
				default:
				}
			}
			e.mu.Unlock()
		}
	}
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

// transportFor chooses the SDK transport and configures the child environment.
func transportFor(name string, def wire.Object, quiet bool) (mcp.Transport, error) {
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
	return &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: headerTransport{headers}}, MaxRetries: -1}, nil
}

// catalogFor serves fresh metadata without connecting, or lists all pages within one budget.
func (e *Engine) catalogFor(ctx context.Context, name string, quiet, refresh bool) (catalog, error) {
	requestedAt := time.Now()
	servers, err := config.Load(e.Path)
	if err != nil {
		return catalog{}, err
	}
	def, ok := servers.Get(name).(wire.Object)
	if !ok {
		return catalog{}, fmt.Errorf("unknown server %q", name)
	}
	key := e.cacheKey(name, def)
	if !refresh {
		e.mu.Lock()
		c, found := e.catalogs[name]
		if !found || c.key != key {
			c, found = e.readCatalog(key)
			if found {
				e.catalogs[name] = c
			}
		}
		e.mu.Unlock()
		if found && time.Since(c.at) >= 0 && time.Since(c.at) < catalogTTL {
			c.cached = true
			return c, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, e.deadline)
	defer cancel()
	ent, err := e.connect(ctx, name, quiet)
	if err != nil {
		return catalog{}, e.deadlineError(ctx, err)
	}
	defer e.release(ent)
	ent.mu.Lock()
	defer ent.mu.Unlock()
	if ctx.Err() != nil {
		return catalog{}, e.deadlineError(ctx, ctx.Err())
	}
	if (!refresh || ent.at.After(requestedAt)) && time.Since(ent.at) < catalogTTL && ent.listed == ent.changed.Load() {
		return catalog{key: key, tools: ent.tools, at: ent.at, instructions: ent.session.InitializeResult().Instructions, cached: true, generation: ent.listed}, nil
	}
	version := ent.changed.Load()
	collected := []wire.Object{}
	totalBytes := 0
	toolNames := map[string]bool{}
	cursor := ""
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page >= 100 {
			return catalog{}, fmt.Errorf("tools/list exceeded 100 pages")
		}
		cctx, raw := wire.Capture(ctx)
		_, err = ent.session.ListTools(cctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			e.discard(name, ent)
			return catalog{}, e.deadlineError(ctx, err)
		}
		totalBytes += len(*raw)
		if totalBytes > maxCatalogBytes {
			return catalog{}, fmt.Errorf("tools/list exceeds the 16 MiB catalog limit")
		}
		v, err := wire.DecodeExact(*raw)
		if err != nil {
			return catalog{}, err
		}
		o, _ := v.(wire.Object)
		tools, _ := o.Get("tools").([]any)
		for _, t := range tools {
			if tool, ok := t.(wire.Object); ok {
				name, _ := tool.Get("name").(string)
				if name == "" || toolNames[name] {
					return catalog{}, fmt.Errorf("tools/list contains an empty or duplicate tool name")
				}
				toolNames[name] = true
				if schema, ok := tool.Get("inputSchema").(wire.Object); ok {
					tool.Set("inputSchema", ordered(schema, "type", "properties", "required"))
				}
				if hints, ok := tool.Get("annotations").(wire.Object); ok {
					filtered := wire.Object{}
					for _, key := range []string{"title", "readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
						if hints.Has(key) {
							filtered.Set(key, hints.Get(key))
						}
					}
					tool.Set("annotations", filtered)
				}
				collected = append(collected, tool)
			}
		}
		cursor, _ = o.Get("nextCursor").(string)
		if cursor == "" {
			break
		}
		if seen[cursor] {
			return catalog{}, fmt.Errorf("tools/list repeated a pagination cursor")
		}
		seen[cursor] = true
	}
	ent.tools = collected
	ent.listed = version
	ent.at = time.Now()
	c := catalog{key: key, tools: ent.tools, at: ent.at, instructions: ent.session.InitializeResult().Instructions, generation: version}
	e.mu.Lock()
	if ent.changed.Load() == version && e.entries[name] == ent {
		e.catalogs[name] = c
		e.writeCatalog(c)
	}
	e.mu.Unlock()
	return c, nil
}

type serverTools struct {
	name    string
	tools   []wire.Object
	err     string
	catalog catalog
}

// allTools queries configured servers in parallel while retaining registry order.
func (e *Engine) allTools(ctx context.Context, quiet bool) ([]serverTools, error) {
	return e.selectedTools(ctx, "", quiet, false)
}

// selectedTools discovers only the requested server, or all servers for unscoped discovery.
func (e *Engine) selectedTools(ctx context.Context, server string, quiet, refresh bool) ([]serverTools, error) {
	servers, err := config.Load(e.Path)
	if err != nil {
		return nil, err
	}
	e.reconcile(servers)
	if server != "" {
		def, ok := servers.Get(server).(wire.Object)
		if !ok {
			return nil, fmt.Errorf("unknown server %q", server)
		}
		servers = wire.Object{{Name: server, Value: def}}
	}
	return e.rowsFor(ctx, servers, quiet, refresh)
}

// rowsFor fetches scoped metadata concurrently, retaining configuration order.
func (e *Engine) rowsFor(ctx context.Context, servers wire.Object, quiet, refresh bool) ([]serverTools, error) {
	rows := make([]serverTools, len(servers))
	var wg sync.WaitGroup
	for i, f := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := e.catalogFor(ctx, f.Name, quiet, refresh)
			rows[i] = serverTools{name: f.Name, tools: c.tools, catalog: c}
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

// Listing returns the catalog shared by CLI list and MCP search without a query.
func (e *Engine) Listing(ctx context.Context, quiet bool) (wire.Object, error) {
	rows, err := e.allTools(ctx, quiet)
	return e.listingRows(rows, err)
}

// Refresh explicitly refreshes selected metadata and checks observed availability.
func (e *Engine) Refresh(ctx context.Context, server string, quiet bool) (wire.Object, error) {
	rows, err := e.selectedTools(ctx, server, quiet, true)
	return e.listingRows(rows, err)
}

// listingRows distinguishes cached catalogs from a live availability check.
func (e *Engine) listingRows(rows []serverTools, err error) (wire.Object, error) {
	if err != nil {
		return nil, err
	}
	integrations := []any{}
	for _, r := range rows {
		if len(r.tools) == 0 && r.err == "" {
			continue
		}
		row := wire.Object{{Name: "server", Value: r.name}, {Name: "tools", Value: len(r.tools)}}
		if r.err != "" {
			row.Set("error", r.err)
		} else {
			row.Set("source", map[bool]string{true: "cache", false: "live"}[r.catalog.cached])
			row.Set("observedAt", r.catalog.at.UTC().Format(time.RFC3339Nano))
			row.Set("availability", map[bool]string{true: "not_checked", false: "reachable"}[r.catalog.cached])
		}
		integrations = append(integrations, row)
	}
	return wire.Object{{Name: "config", Value: e.Path}, {Name: "integrations", Value: integrations}}, nil
}

// Call validates and runs server.tool, returning the downstream result without reshaping it.
func (e *Engine) Call(ctx context.Context, id string, args any, quiet bool) (wire.Object, error) {
	return e.CallWith(ctx, id, args, quiet, false, nil)
}

// CallWith enforces policy and a live schema, copies explicit references, and never retries a call.
func (e *Engine) CallWith(ctx context.Context, id string, args any, quiet, retain bool, refs []ArgumentReference) (wire.Object, error) {
	dot := strings.Index(id, ".")
	if dot < 1 || dot == len(id)-1 {
		return nil, fmt.Errorf("invalid tool id \"%s\" (expected server.tool)", id)
	}
	name, tool := id[:dot], id[dot+1:]
	servers, err := config.Load(e.Path)
	if err != nil {
		return nil, err
	}
	e.reconcile(servers)
	def, ok := servers.Get(name).(wire.Object)
	if !ok {
		return nil, &Failure{"unknown_server", "unknown server", "List configured servers with plugin_search."}
	}
	if err = checkPolicy(def, tool); err != nil {
		return nil, err
	}
	if len(refs) > 0 {
		args, err = e.resolveReferences(name, args, refs, servers)
		if err != nil {
			return nil, err
		}
	}
	cctx, cancel := context.WithTimeout(ctx, e.deadline)
	ent, err := e.connect(cctx, name, quiet)
	err = e.deadlineError(cctx, err)
	cancel()
	if err != nil {
		return nil, &Failure{"server_unavailable", "server could not be connected", "Check server configuration or authentication. No tools/call was sent."}
	}
	defer e.release(ent)
	ent.mu.Lock()
	fresh := time.Since(ent.at) < catalogTTL && ent.listed == ent.changed.Load()
	ent.mu.Unlock()
	c, err := e.catalogFor(ctx, name, quiet, !fresh)
	if err != nil {
		return nil, &Failure{"catalog_unavailable", "live tool catalog could not be read", "Check server availability. No tools/call was sent."}
	}
	var schema any
	for _, t := range c.tools {
		if t.Get("name") == tool {
			schema = t.Get("inputSchema")
			break
		}
	}
	if schema == nil {
		return nil, &Failure{"unknown_tool", "tool is absent from the live server catalog", "Browse this server with plugin_search server and refresh true."}
	}
	if err = validateArguments(schema, args); err != nil {
		return nil, err
	}
	current, err := config.Load(e.Path)
	if err != nil {
		return nil, err
	}
	currentDef, ok := current.Get(name).(wire.Object)
	if !ok || e.cacheKey(name, currentDef) != ent.key {
		return nil, &Failure{"configuration_changed", "server configuration changed before the call", "Discover the tool again. No tools/call was sent."}
	}
	e.mu.Lock()
	same := e.entries[name] == ent
	e.mu.Unlock()
	ent.mu.Lock()
	unchanged := ent.changed.Load() == c.generation
	ent.mu.Unlock()
	if !same || !unchanged {
		return nil, &Failure{"stale_schema", "tool catalog changed before the call", "Refresh and inspect the tool. No tools/call was sent."}
	}
	cctx, raw := wire.Capture(ctx)
	_, err = ent.session.CallTool(cctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, &Failure{"call_outcome_unknown", "downstream tool call failed at the protocol or transport layer", "The operation may have run. Verify external state before retrying a write; tap never retries calls automatically."}
	}
	v, err := wire.DecodeExact(*raw)
	if err != nil {
		return nil, err
	}
	o, _ := v.(wire.Object)
	o = ordered(o, "_meta", "content", "structuredContent", "isError")
	if retain {
		return e.results.retain(name, o, *raw), nil
	}
	return o, nil
}

// Close tears down every downstream transport; safe after failed or partial connects.
func (e *Engine) Close() {
	e.closeOnce.Do(func() {
		e.cancel()
		e.mu.Lock()
		e.closing = true
		entries := e.entries
		e.entries = map[string]*entry{}
		e.mu.Unlock()
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
		e.cleanup.Wait()

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
