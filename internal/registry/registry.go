// Package registry manages lazy downstream MCP sessions, one-minute tool caches,
// parallel searches and calls. An Engine belongs to one tap process and must be closed.
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
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fschrhunt/tap/internal/config"
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
	ctx           context.Context
	cancel        context.CancelFunc
	closeOnce     sync.Once
}
type entry struct {
	ready   chan struct{}
	session *mcp.ClientSession
	err     error
	mu      sync.Mutex
	tools   []wire.Object
	at      time.Time
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
	return &Engine{Path: path, Version: version, deadline: time.Duration(durationMS * float64(time.Millisecond)), deadlineMS: ms, entries: map[string]*entry{}, ctx: ctx, cancel: cancel}
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
	e.mu.Lock()
	ent := e.entries[name]
	if ent == nil {
		ent = &entry{ready: make(chan struct{})}
		e.entries[name] = ent
		go e.open(name, quiet, ent)
	}
	e.mu.Unlock()
	select {
	case <-ctx.Done():
		return ent, ctx.Err()
	case <-ent.ready:
		return ent, ent.err
	}
}

// open initializes one server within its own deadline and watches for disconnects.
func (e *Engine) open(name string, quiet bool, ent *entry) {
	ctx, cancel := context.WithTimeout(e.ctx, e.deadline)
	defer cancel()
	servers, err := config.Load(e.Path)
	var transport mcp.Transport
	if err == nil {
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
			err = fmt.Errorf("unknown server \"%s\" (configured: %s)", name, known)
		} else {
			transport, err = transportFor(name, def, quiet)
		}
	}
	if err == nil {
		client := mcp.NewClient(&mcp.Implementation{Name: "tap", Version: e.Version}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
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
	e.mu.Lock()
	if e.entries[name] == ent {
		delete(e.entries, name)
	}
	e.mu.Unlock()
	go func() {
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

// toolsFor shares a connect-and-list budget and caches successful lists for a minute.
func (e *Engine) toolsFor(ctx context.Context, name string, quiet bool) ([]wire.Object, error) {
	ctx, cancel := context.WithTimeout(ctx, e.deadline)
	defer cancel()
	ent, err := e.connect(ctx, name, quiet)
	if err != nil {
		e.discard(name, ent)
		return nil, e.deadlineError(ctx, err)
	}
	ent.mu.Lock()
	defer ent.mu.Unlock()
	if time.Since(ent.at) < time.Minute {
		return ent.tools, nil
	}
	cctx, raw := wire.Capture(ctx)
	_, err = ent.session.ListTools(cctx, nil)
	if err != nil {
		e.discard(name, ent)
		return nil, e.deadlineError(ctx, err)
	}
	v, err := wire.Decode(*raw)
	if err != nil {
		return nil, err
	}
	o, _ := v.(wire.Object)
	tools, _ := o.Get("tools").([]any)
	ent.tools = make([]wire.Object, 0, len(tools))
	for _, t := range tools {
		if tool, ok := t.(wire.Object); ok {
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
			ent.tools = append(ent.tools, tool)
		}
	}
	ent.at = time.Now()
	return ent.tools, nil
}

type serverTools struct {
	name  string
	tools []wire.Object
	err   string
}

// allTools queries configured servers in parallel while retaining registry order.
func (e *Engine) allTools(ctx context.Context, quiet bool) ([]serverTools, error) {
	servers, err := config.Load(e.Path)
	if err != nil {
		return nil, err
	}
	rows := make([]serverTools, len(servers))
	var wg sync.WaitGroup
	for i, f := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tools, err := e.toolsFor(ctx, f.Name, quiet)
			rows[i] = serverTools{name: f.Name, tools: tools}
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
		}
		integrations = append(integrations, row)
	}
	return wire.Object{{Name: "config", Value: e.Path}, {Name: "integrations", Value: integrations}}, nil
}

var camel = regexp.MustCompile(`([a-z0-9])([A-Z])`)
var punctuation = regexp.MustCompile(`[^a-z0-9]+`)

// tokenize makes camelCase, underscores and punctuation comparable for scoring.
func tokenize(s string) []string {
	s = strings.ToLower(camel.ReplaceAllString(s, "$1 $2"))
	out := []string{}
	for _, t := range punctuation.Split(s, -1) {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

type match struct {
	rank int
	tool wire.Object
}

// Search ranks tools matching every query term and applies JavaScript slice limits.
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
			continue
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
				for _, k := range []string{"title", "description", "inputSchema", "annotations"} {
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

// Call runs server.tool and returns the downstream JSON result without reshaping it.
func (e *Engine) Call(ctx context.Context, id string, args any, quiet bool) (wire.Object, error) {
	dot := strings.Index(id, ".")
	if dot < 1 {
		return nil, fmt.Errorf("invalid tool id \"%s\" (expected server.tool)", id)
	}
	name, tool := id[:dot], id[dot+1:]
	cctx, cancel := context.WithTimeout(ctx, e.deadline)
	ent, err := e.connect(cctx, name, quiet)
	err = e.deadlineError(cctx, err)
	cancel()
	if err != nil {
		return nil, err
	}
	cctx, raw := wire.Capture(ctx)
	_, err = ent.session.CallTool(cctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, err
	}
	v, err := wire.Decode(*raw)
	if err != nil {
		return nil, err
	}
	o, _ := v.(wire.Object)
	return ordered(o, "_meta", "content", "structuredContent", "isError"), nil
}

// Close tears down every downstream transport; safe after failed or partial connects.
func (e *Engine) Close() {
	e.closeOnce.Do(func() {
		e.cancel()
		e.mu.Lock()
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
