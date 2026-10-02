// Package registry manages bounded downstream sessions, resilient catalogs and validated calls.
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
	"strings"
	"sync"
	"time"

	"github.com/fschrhunt/tap/internal/auth"
	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Engine holds resident sessions and caches for one fixed config path and version.
type Engine struct {
	Path, Version         string
	deadline              time.Duration
	deadlineMS            float64
	mu                    sync.Mutex
	entries               map[string]*entry
	ctx                   context.Context
	cancel                context.CancelFunc
	closeOnce             sync.Once
	snapshotMu, indexMu   sync.Mutex
	last                  *configSnapshot
	indexDirty, indexDone chan struct{}
	definitions           map[string]string
	catalogs              map[string]*catalog
	indexLoaded           bool
	slots                 chan struct{}
	failTTL, idleTTL      time.Duration
	workers               sync.WaitGroup
	results               resultStore
}

// entry is one connection generation; ready publishes session and err.
type entry struct {
	ready                        chan struct{}
	session                      *mcp.ClientSession
	err                          error
	failedAt, openedAt, lastUsed time.Time
	users                        int
	fingerprint                  string
	idle                         time.Duration
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
	e := &Engine{Path: path, Version: version, deadline: time.Duration(durationMS * float64(time.Millisecond)), deadlineMS: ms, entries: map[string]*entry{}, ctx: ctx, cancel: cancel, indexDirty: make(chan struct{}, 1), indexDone: make(chan struct{}), definitions: map[string]string{}, catalogs: map[string]*catalog{}, slots: make(chan struct{}, 8), failTTL: envDuration("TAP_FAIL_TTL_MS", 5*time.Second), idleTTL: envDuration("TAP_IDLE_TTL_MS", 5*time.Minute)}
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
		ent = &entry{ready: make(chan struct{}), fingerprint: fingerprint, lastUsed: time.Now(), idle: e.idleTTL}
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
		if def.Has("idleTimeoutMs") {
			ms, ok := def.Get("idleTimeoutMs").(float64)
			if !ok || ms <= 0 || ms > 86400000 || ms != math.Trunc(ms) || !endpointIsStdio(def) {
				err = fmt.Errorf("idleTimeoutMs must be an integer 1..86400000 for a stdio server")
			} else {
				ent.idle = time.Duration(ms) * time.Millisecond
			}
		}
		if err == nil {
			client := mcp.NewClient(&mcp.Implementation{Name: "tap", Version: e.Version}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}, ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				e.mu.Lock()
				if c := e.catalogs[name]; c != nil && c.fingerprint == ent.fingerprint {
					c.at = time.Time{}
					c.retryAt = time.Time{}
					c.revision++
					if e.ctx.Err() == nil {
						select {
						case e.indexDirty <- struct{}{}:
						default:
						}
					}
				}
				e.mu.Unlock()
			}})
			// HTTP must retain SDK-private initialization hooks; only stdio needs raw capture.
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

// endpointIsStdio identifies local processes eligible for a per-server idle timeout.
func endpointIsStdio(def wire.Object) bool {
	return def.Get("type") == "stdio" || (def.Get("type") == nil && def.Get("url") == nil)
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
			return nil, fmt.Errorf("server %q has no command", name)
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
		return nil, fmt.Errorf("server %q has unsupported type %q", name, typ)
	}
	if endpoint == "" {
		return nil, fmt.Errorf("server %q has no url", name)
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
	transport := &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: headerTransport{headers}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, MaxRetries: -1}
	if headers.Get("Authorization") == "" {
		transport.OAuthHandler = auth.Handler(path, name, endpoint)
	}
	return transport, nil
}

type serverTools struct {
	name    string
	tools   []wire.Object
	err     string
	stale   bool
	catalog catalog
	cached  bool
}

// allTools queries configured servers in parallel while retaining registry order.
func (e *Engine) allTools(ctx context.Context, quiet bool) ([]serverTools, error) {
	return e.selectedTools(ctx, "", quiet, false)
}

// selectedTools uses one snapshot for scoped or full discovery.
func (e *Engine) selectedTools(ctx context.Context, server string, quiet, refresh bool) ([]serverTools, error) {
	snapshot, err := e.snapshot()
	if err != nil {
		return nil, err
	}
	servers := snapshot.servers
	if server != "" {
		def, ok := servers.Get(server).(wire.Object)
		if !ok {
			return nil, fmt.Errorf("unknown server %q", server)
		}
		servers = wire.Object{{Name: server, Value: def}}
	}
	return e.rowsFor(ctx, servers, snapshot.fingerprints, quiet, refresh)
}

// rowsFor shares refreshes while reporting cached metadata separately from live availability.
func (e *Engine) rowsFor(ctx context.Context, servers wire.Object, fingerprints map[string]string, quiet, force bool) ([]serverTools, error) {
	rows := make([]serverTools, len(servers))
	var wg sync.WaitGroup
	for i, f := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			def, _ := f.Value.(wire.Object)
			var tools []wire.Object
			var stale bool
			var err error
			e.mu.Lock()
			c := e.catalogs[f.Name]
			cached := c != nil && c.good
			e.mu.Unlock()
			if force {
				tools, err = e.liveTools(ctx, f.Name, def, fingerprints[f.Name], quiet, nil)
				stale = err != nil
			} else {
				tools, stale, err = e.toolsFor(ctx, f.Name, def, fingerprints[f.Name], quiet)
			}
			e.mu.Lock()
			var view catalog
			if c := e.catalogs[f.Name]; c != nil {
				view = catalog{fingerprint: c.fingerprint, tools: tools, at: c.at, instructions: c.instructions, revision: c.revision}
			}
			e.mu.Unlock()
			rows[i] = serverTools{name: f.Name, tools: tools, stale: stale, catalog: view, cached: cached && !force}
			if err != nil {
				rows[i].err = Message(err)
			}
		}()
	}
	wg.Wait()
	return rows, nil
}

// Listing returns catalog counts, marking stale rows and last refresh errors.
func (e *Engine) Listing(ctx context.Context, quiet bool) (wire.Object, error) {
	rows, err := e.allTools(ctx, quiet)
	return e.listingRows(rows, err)
}

// Refresh waits for live discovery of the selected server or all integrations.
func (e *Engine) Refresh(ctx context.Context, server string, quiet bool) (wire.Object, error) {
	rows, err := e.selectedTools(ctx, server, quiet, true)
	return e.listingRows(rows, err)
}

// listingRows distinguishes cached catalogs from live availability checks.
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
		if r.stale {
			row.Set("stale", true)
		}
		if r.err != "" {
			row.Set("error", r.err)
		}
		row.Set("source", map[bool]string{true: "cache", false: "live"}[r.cached])
		row.Set("observedAt", r.catalog.at.UTC().Format(time.RFC3339Nano))
		availability := "not_checked"
		if !r.cached && r.err == "" {
			availability = "reachable"
		}
		row.Set("availability", availability)
		integrations = append(integrations, row)
	}
	return wire.Object{{Name: "config", Value: e.Path}, {Name: "integrations", Value: integrations}}, nil
}

// Call validates and runs server.tool with inline results.
func (e *Engine) Call(ctx context.Context, id string, args any, quiet bool) (wire.Object, error) {
	return e.CallWithMeta(ctx, id, args, quiet, nil)
}

// CallWithMeta forwards application metadata without forwarding protocol negotiation fields.
func (e *Engine) CallWithMeta(ctx context.Context, id string, args any, quiet bool, meta mcp.Meta) (wire.Object, error) {
	return e.CallWithOptions(ctx, id, args, quiet, false, nil, meta)
}

// CallWith enables explicit result retention and reference copies without application metadata.
func (e *Engine) CallWith(ctx context.Context, id string, args any, quiet, retain bool, refs []ArgumentReference) (wire.Object, error) {
	return e.CallWithOptions(ctx, id, args, quiet, retain, refs, nil)
}

// CallWithOptions enforces policy and a live contract before execution; calls are never retried.
func (e *Engine) CallWithOptions(ctx context.Context, id string, args any, quiet, retain bool, refs []ArgumentReference, meta mcp.Meta) (wire.Object, error) {
	dot := strings.Index(id, ".")
	if dot < 1 || dot == len(id)-1 {
		return nil, fmt.Errorf("invalid tool id %q (expected server.tool)", id)
	}
	name, tool := id[:dot], id[dot+1:]
	snapshot, err := e.snapshot()
	if err != nil {
		return nil, err
	}
	def, ok := snapshot.servers.Get(name).(wire.Object)
	if !ok {
		return nil, &Failure{"unknown_server", "unknown server", "List configured servers with plugin_search."}
	}
	if err = checkPolicy(def, tool); err != nil {
		return nil, err
	}
	if len(refs) > 0 {
		args, err = e.resolveReferences(ctx, name, args, refs, snapshot.servers)
		if err != nil {
			return nil, err
		}
	}
	cctx, cancel := context.WithTimeout(ctx, e.deadline)
	ent, err := e.connect(cctx, name, def, snapshot.fingerprints[name], quiet)
	cancel()
	if err != nil {
		return nil, &causedFailure{&Failure{"server_unavailable", "server could not be connected", "Check configuration or authentication. No tools/call was sent."}, err}
	}
	defer e.release(ent)
	tools, err := e.liveTools(ctx, name, def, snapshot.fingerprints[name], quiet, ent)
	if err != nil {
		return nil, &causedFailure{&Failure{"catalog_unavailable", "live tool catalog could not be read", "Check server availability. No tools/call was sent."}, err}
	}
	var schema any
	for _, t := range tools {
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
	current, err := e.snapshot()
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	c := e.catalogs[name]
	same := e.entries[name] == ent && current.fingerprints[name] == ent.fingerprint && c != nil && c.session == ent && time.Since(c.at) < catalogTTL
	if same {
		same = false
		validated, _ := wire.JSON(schema, false)
		for _, t := range c.tools {
			if t.Get("name") == tool {
				advertised, _ := wire.JSON(t.Get("inputSchema"), false)
				same = string(validated) == string(advertised)
				break
			}
		}
	}
	e.mu.Unlock()
	if !same {
		return nil, &Failure{"stale_schema", "configuration or tool catalog changed before the call", "Refresh and inspect the tool. No tools/call was sent."}
	}
	cctx, raw := wire.Capture(ctx)
	result, err := ent.session.CallTool(cctx, &mcp.CallToolParams{Name: tool, Arguments: args, Meta: wire.ForwardMeta(meta)})
	if err != nil {
		return nil, &causedFailure{&Failure{"call_outcome_unknown", "downstream tool call failed at the protocol or transport layer", "The operation may have run. Verify external state before retrying a write; tap never retries calls automatically."}, err}
	}
	if len(*raw) == 0 {
		*raw, err = wire.JSON(result, false)
		if err != nil {
			return nil, err
		}
	}
	v, err := wire.DecodeExact(*raw)
	if err != nil {
		return nil, err
	}
	o, _ := v.(wire.Object)
	o = ordered(o, "_meta", "content", "structuredContent", "isError")
	if retain {
		return e.results.retainScoped(ctx, name, o, *raw), nil
	}
	return o, nil
}

// Close waits for workers, flushes queued catalogs, and tears down every transport.
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

// ordered retains known-field-first JSON ordering without dropping extension fields.
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

// commandTransport retains familiar spawn diagnostics around the SDK transport.
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
