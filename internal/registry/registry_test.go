package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// peer provides offline SDK sessions with controllable list and initialization.
type peer struct {
	server          *mcp.Server
	url             string
	connects, lists atomic.Int32
}

// newPeer runs a local SDK peer; middleware can reject or delay individual methods.
func newPeer(t *testing.T, intercept func(context.Context, string, mcp.Request) (mcp.Result, error, bool)) *peer {
	t.Helper()
	p := &peer{server: mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ServerOptions{PageSize: 1})}
	p.server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "initialize" {
				p.connects.Add(1)
			}
			if method == "tools/list" {
				p.lists.Add(1)
			}
			if intercept != nil {
				if result, err, handled := intercept(ctx, method, req); handled {
					return result, err
				}
			}
			return next(ctx, method, req)
		}
	})
	addTool(p.server, "echo")
	h := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return p.server }, nil))
	p.url = h.URL
	t.Cleanup(h.Close)
	return p
}

// addTool exposes an empty successful call using the SDK's normal tool handler.
func addTool(s *mcp.Server, name string) {
	s.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
}

// testEngine isolates all config and index IO in a temporary TAP_CONFIG.
func testEngine(t *testing.T, servers wire.Object) *Engine {
	t.Helper()
	path := filepath.Join(t.TempDir(), "servers.json")
	t.Setenv("TAP_CONFIG", path)
	if err := config.Save(path, servers); err != nil {
		t.Fatal(err)
	}
	e := New(path, "test")
	t.Cleanup(e.Close)
	return e
}

// definitions constructs one HTTP server definition.
func definitions(url string) wire.Object {
	return wire.Object{{Name: "test", Value: wire.Object{{Name: "url", Value: url}}}}
}

// listing requires a successful top-level registry query.
func listing(t *testing.T, e *Engine) wire.Object {
	t.Helper()
	result, err := e.Listing(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// waitFor gives asynchronous SDK notification and refresh work a bounded wait.
func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(until) {
			t.Fatal("timed out waiting for registry work")
		}
		time.Sleep(time.Millisecond)
	}
}

// waitRefresh waits for the one catalog's refresh to finish.
func waitRefresh(t *testing.T, e *Engine) {
	t.Helper()
	waitFor(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		c := e.catalogs["test"]
		return c != nil && c.refreshing == nil
	})
}

// TestFailedConnectBackoff pins retries, the zero override, and definition invalidation.
func TestFailedConnectBackoff(t *testing.T) {
	for _, ttl := range []string{"60000", "0"} {
		t.Run(ttl, func(t *testing.T) {
			t.Setenv("TAP_FAIL_TTL_MS", ttl)
			p := newPeer(t, func(_ context.Context, method string, _ mcp.Request) (mcp.Result, error, bool) {
				if method == "initialize" {
					return nil, errors.New("offline"), true
				}
				return nil, nil, false
			})
			e := testEngine(t, definitions(p.url))
			listing(t, e)
			listing(t, e)
			want := int32(1)
			if ttl == "0" {
				want = 2
			}
			if got := p.connects.Load(); got != want {
				t.Fatalf("connects=%d want %d", got, want)
			}
			def := wire.Object{{Name: "url", Value: p.url}, {Name: "marker", Value: "changed"}}
			if err := config.Save(e.Path, wire.Object{{Name: "test", Value: def}}); err != nil {
				t.Fatal(err)
			}
			listing(t, e)
			if got := p.connects.Load(); got != want+1 {
				t.Fatalf("definition change kept failed generation: %d", got)
			}
		})
	}
}

// TestStaleRefreshPreservesTools ensures list failure keeps a searchable last good list.
func TestStaleRefreshPreservesTools(t *testing.T) {
	t.Setenv("TAP_FAIL_TTL_MS", "60000")
	var fail atomic.Bool
	entered, release := make(chan struct{}, 1), make(chan struct{})
	p := newPeer(t, func(ctx context.Context, method string, _ mcp.Request) (mcp.Result, error, bool) {
		if method == "tools/list" && fail.Load() {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil, errors.New("list failed"), true
		}
		return nil, nil, false
	})
	e := testEngine(t, definitions(p.url))
	fresh := listing(t, e).Get("integrations").([]any)[0].(wire.Object)
	if fresh.Has("stale") || fresh.Has("error") {
		t.Fatalf("fresh listing: %v", fresh)
	}
	found, err := e.Search(context.Background(), "echo", 10, true)
	if err != nil || found.Get("matches").([]any)[0].(wire.Object).Has("stale") {
		t.Fatalf("fresh search: %v %v", found, err)
	}
	e.mu.Lock()
	e.catalogs["test"].at = time.Time{}
	e.mu.Unlock()
	fail.Store(true)
	result := listing(t, e)
	row := result.Get("integrations").([]any)[0].(wire.Object)
	if row.Get("tools") != 1 || row.Get("stale") != true {
		t.Fatalf("stale listing: %v", row)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh")
	}
	close(release)
	waitRefresh(t, e)
	found, err = e.Search(context.Background(), "echo", 10, true)
	if err != nil || found.Get("total") != 1 || len(found.Get("unavailable").([]any)) != 1 {
		t.Fatalf("stale search: %v %v", found, err)
	}
	match := found.Get("matches").([]any)[0].(wire.Object)
	diagnostic := found.Get("unavailable").([]any)[0].(wire.Object)
	row = listing(t, e).Get("integrations").([]any)[0].(wire.Object)
	if match.Get("id") != "test.echo" || match.Get("stale") != true || diagnostic.Get("server") != "test" || diagnostic.Get("error") != "list failed" || row.Get("error") != "list failed" || row.Get("stale") != true {
		t.Fatalf("stale diagnostics and matches: %v %v", found, row)
	}
	if p.lists.Load() != 2 || p.connects.Load() != 1 {
		t.Fatalf("unexpected retries or eviction: lists=%d connects=%d", p.lists.Load(), p.connects.Load())
	}
}

// TestCancelledWaitKeepsSharedSession pins caller-independent initialization and refresh.
func TestCancelledWaitKeepsSharedSession(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	p := newPeer(t, func(ctx context.Context, method string, _ mcp.Request) (mcp.Result, error, bool) {
		if method == "initialize" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err(), true
			}
		}
		return nil, nil, false
	})
	e := testEngine(t, definitions(p.url))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := e.Listing(ctx, true); done <- err }()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop waiting")
	}
	close(release)
	listing(t, e)
	if p.connects.Load() != 1 || p.lists.Load() != 1 {
		t.Fatalf("shared work discarded: connects=%d lists=%d", p.connects.Load(), p.lists.Load())
	}
}

// TestPaginationAndListChanged pins complete pagination and notification invalidation.
func TestPaginationAndListChanged(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, wire.Object{{Name: "test", Value: wire.Object{
		{Name: "command", Value: []any{executable, "-test.run=^TestRegistryChild$"}},
		{Name: "env", Value: wire.Object{{Name: "TAP_REGISTRY_CHILD", Value: "1"}}},
	}}})
	result := listing(t, e)
	if n := result.Get("integrations").([]any)[0].(wire.Object).Get("tools"); n != 2 {
		t.Fatalf("pagination tools=%v", n)
	}
	if _, err := e.Call(context.Background(), "test.echo", map[string]any{}, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.catalogs["test"].at.IsZero() })
	listing(t, e)
	waitRefresh(t, e)
	result = listing(t, e)
	if n := result.Get("integrations").([]any)[0].(wire.Object).Get("tools"); n != 3 {
		t.Fatalf("invalidated tools=%v", n)
	}
}

// TestRegistryChild serves a hermetic stdio peer only when re-executed by the test.
func TestRegistryChild(t *testing.T) {
	if os.Getenv("TAP_REGISTRY_CHILD") != "1" {
		return
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "child", Version: "1"}, &mcp.ServerOptions{PageSize: 1})
	addTool(s, "second")
	s.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		addTool(s, "third")
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// TestIndexPrivacyAndFingerprint pins owner-only persistence and changed-config rejection.
func TestIndexPrivacyAndFingerprint(t *testing.T) {
	p := newPeer(t, nil)
	servers := definitions(p.url)
	def := servers.Get("test").(wire.Object)
	def.Set("headers", wire.Object{{Name: "X-Secret", Value: "private-test-credential"}})
	servers.Set("test", def)
	e := testEngine(t, servers)
	listing(t, e)
	waitFor(t, func() bool { _, err := os.Stat(e.Path + ".tools.json"); return err == nil })
	path := e.Path + ".tools.json"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "private-test-credential") || strings.Contains(string(b), p.url) {
		t.Fatal("connection credentials persisted")
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("index permissions: %v %v", st, err)
	}
	e2 := New(e.Path, "test")
	defer e2.Close()
	if _, err := e2.snapshot(); err != nil {
		t.Fatal(err)
	}
	e2.mu.Lock()
	c := e2.catalogs["test"]
	e2.mu.Unlock()
	if c == nil || !c.good || len(c.tools) != 1 {
		t.Fatal("persisted tools not restored")
	}
	def.Set("headers", wire.Object{{Name: "X-Secret", Value: "changed"}})
	if err := config.Save(e.Path, wire.Object{{Name: "test", Value: def}}); err != nil {
		t.Fatal(err)
	}
	e3 := New(e.Path, "test")
	defer e3.Close()
	if _, err := e3.snapshot(); err != nil {
		t.Fatal(err)
	}
	if len(e3.catalogs) != 0 {
		t.Fatal("mismatched index restored")
	}
}

// TestDiscoveryDoesNotWaitForIndex ensures optional IO cannot block cold discovery
// and Close flushes the latest successful catalog after coalesced refreshes.
func TestDiscoveryDoesNotWaitForIndex(t *testing.T) {
	var latest atomic.Bool
	p := newPeer(t, func(_ context.Context, method string, _ mcp.Request) (mcp.Result, error, bool) {
		if method == "tools/list" {
			name := "first"
			if latest.Load() {
				name = "latest"
			}
			return &mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: name, InputSchema: map[string]any{"type": "object"}}}}, nil, true
		}
		return nil, nil, false
	})
	e := testEngine(t, definitions(p.url))
	e.indexMu.Lock()
	locked := true
	defer func() {
		if locked {
			e.indexMu.Unlock()
		}
	}()
	type result struct {
		listing wire.Object
		err     error
	}
	discovered := make(chan result, 1)
	go func() {
		rows, err := e.Listing(context.Background(), true)
		discovered <- result{rows, err}
	}()
	select {
	case got := <-discovered:
		if got.err != nil || got.listing.Get("integrations").([]any)[0].(wire.Object).Get("tools") != 1 {
			t.Fatalf("cold discovery: %v %v", got.listing, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cold discovery waited for index IO")
	}
	latest.Store(true)
	e.mu.Lock()
	e.catalogs["test"].at = time.Time{}
	e.mu.Unlock()
	listing(t, e)
	waitRefresh(t, e)
	if _, err := os.Stat(e.Path + ".tools.json"); !os.IsNotExist(err) {
		t.Fatalf("index written despite blocked IO: %v", err)
	}
	closed := make(chan struct{})
	go func() { e.Close(); close(closed) }()
	waitFor(t, func() bool { return e.ctx.Err() != nil })
	select {
	case <-closed:
		t.Fatal("Close did not wait for queued index IO")
	default:
	}
	e.indexMu.Unlock()
	locked = false
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not flush queued index")
	}
	next := New(e.Path, "test")
	defer next.Close()
	if _, err := next.snapshot(); err != nil {
		t.Fatal(err)
	}
	c := next.catalogs["test"]
	if c == nil || len(c.tools) != 1 || c.tools[0].Get("name") != "latest" {
		t.Fatalf("Close did not persist latest catalog: %v", c)
	}
}

// TestIndexSymlink pins safe atomic replacement without changing the symlink target.
func TestIndexSymlink(t *testing.T) {
	p := newPeer(t, nil)
	e := testEngine(t, definitions(p.url))
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, e.Path+".tools.json"); err != nil {
		t.Fatal(err)
	}
	listing(t, e)
	waitFor(t, func() bool { st, err := os.Lstat(e.Path + ".tools.json"); return err == nil && st.Mode().IsRegular() })
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "untouched" {
		t.Fatal("index followed symlink")
	}
	st, err := os.Lstat(e.Path + ".tools.json")
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		t.Fatal("index not safely replaced")
	}
}

// TestRemovedServerAndIdleReaping pins direct-call removal and catalog/session separation.
func TestRemovedServerAndIdleReaping(t *testing.T) {
	p := newPeer(t, nil)
	e := testEngine(t, definitions(p.url))
	listing(t, e)
	e.mu.Lock()
	ent := e.entries["test"]
	ent.users++
	ent.lastUsed = time.Now().Add(-2 * e.idleTTL)
	e.mu.Unlock()
	e.reapIdle(time.Now())
	e.mu.Lock()
	kept := e.entries["test"] == ent
	ent.users--
	e.mu.Unlock()
	if !kept {
		t.Fatal("active session reaped")
	}
	e.reapIdle(time.Now())
	e.mu.Lock()
	reaped := e.entries["test"] == nil
	cached := e.catalogs["test"].good
	e.mu.Unlock()
	if !reaped || !cached {
		t.Fatal("idle session/catalog lifecycle incorrect")
	}
	listing(t, e)
	if p.connects.Load() != 1 {
		t.Fatal("warm catalog reconnected idle server")
	}
	if _, err := e.Call(context.Background(), "test.echo", map[string]any{}, true); err != nil {
		t.Fatal(err)
	}
	if p.connects.Load() != 2 {
		t.Fatal("call did not reconnect reaped session")
	}
	if err := config.Save(e.Path, wire.Object{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Call(context.Background(), "test.echo", map[string]any{}, true); err == nil || !strings.Contains(err.Error(), "unknown server") {
		t.Fatalf("removed server called: %v", err)
	}
}

// TestBoundedFanoutKeepsWarmCacheFast pins admission without queuing cached tools.
func TestBoundedFanoutKeepsWarmCacheFast(t *testing.T) {
	entered, release := make(chan struct{}, 32), make(chan struct{})
	var active, peak atomic.Int32
	p := newPeer(t, func(ctx context.Context, method string, _ mcp.Request) (mcp.Result, error, bool) {
		if method == "initialize" {
			n := active.Add(1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			active.Add(-1)
		}
		return nil, nil, false
	})
	servers := wire.Object{}
	for i := 0; i < 12; i++ {
		servers.Set(fmt.Sprintf("s%d", i), wire.Object{{Name: "url", Value: p.url}})
	}
	e := testEngine(t, servers)
	if _, err := e.snapshot(); err != nil {
		t.Fatal(err)
	}
	def := servers.Get("s0").(wire.Object)
	fingerprint := definitionFingerprint(def)
	e.mu.Lock()
	e.catalogs["s0"] = &catalog{fingerprint: fingerprint, good: true, at: time.Now(), tools: []wire.Object{{{Name: "name", Value: "cached"}}}}
	e.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := e.Listing(context.Background(), true); done <- err }()
	for i := 0; i < cap(e.slots); i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("fanout did not fill admission")
		}
	}
	// Once all admitted connects are blocked, cache reads still complete.
	warm := make(chan error, 1)
	go func() { _, _, err := e.toolsFor(context.Background(), "s0", def, fingerprint, true); warm <- err }()
	select {
	case err := <-warm:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("warm cache queued")
	}
	if got := peak.Load(); got > int32(cap(e.slots)) {
		t.Fatalf("peak=%d", got)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fanout stuck")
	}
	if got := peak.Load(); got > int32(cap(e.slots)) {
		t.Fatalf("peak=%d", got)
	}
}

// TestCancelledCallKeepsSession ensures an interrupted call does not evict a healthy session.
func TestCancelledCallKeepsSession(t *testing.T) {
	entered := make(chan struct{})
	var once atomic.Bool
	p := newPeer(t, func(ctx context.Context, method string, _ mcp.Request) (mcp.Result, error, bool) {
		if method == "tools/call" && once.CompareAndSwap(false, true) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err(), true
		}
		return nil, nil, false
	})
	e := testEngine(t, definitions(p.url))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := e.Call(ctx, "test.echo", map[string]any{}, true); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("call never reached peer")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled call error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled call stuck")
	}
	if _, err := e.Call(context.Background(), "test.echo", map[string]any{}, true); err != nil {
		t.Fatal(err)
	}
	if p.connects.Load() != 1 {
		t.Fatal("cancelled call discarded healthy session")
	}
}

// TestIndexRejectsUnsafeOrInvalidFiles makes optional persistence fail closed.
func TestIndexRejectsUnsafeOrInvalidFiles(t *testing.T) {
	p := newPeer(t, nil)
	e := testEngine(t, definitions(p.url))
	listing(t, e)
	waitFor(t, func() bool { _, err := os.Stat(e.Path + ".tools.json"); return err == nil })
	path := e.Path + ".tools.json"
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		mode os.FileMode
		data []byte
	}{
		{"public", 0644, valid},
		{"malformed", 0600, []byte("{")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			next := New(e.Path, "test")
			defer next.Close()
			if _, err := next.snapshot(); err != nil {
				t.Fatal(err)
			}
			if len(next.catalogs) != 0 {
				t.Fatal("unsafe index restored")
			}
		})
	}
}

// TestFanoutUsesConfigSnapshot ensures queued connects do not reread a changed file.
func TestFanoutUsesConfigSnapshot(t *testing.T) {
	var changed atomic.Bool
	var path string
	p := newPeer(t, func(_ context.Context, method string, _ mcp.Request) (mcp.Result, error, bool) {
		if method == "initialize" && changed.CompareAndSwap(false, true) {
			if err := config.Save(path, wire.Object{}); err != nil {
				return nil, err, true
			}
		}
		return nil, nil, false
	})
	servers := definitions(p.url)
	servers.Set("other", wire.Object{{Name: "url", Value: p.url}})
	e := testEngine(t, servers)
	path = e.Path
	// Force the second connect to start after the first peer has removed the config.
	e.slots = make(chan struct{}, 1)
	result := listing(t, e)
	rows := result.Get("integrations").([]any)
	if len(rows) != 2 {
		t.Fatalf("snapshot rows=%v", rows)
	}
	for _, raw := range rows {
		row := raw.(wire.Object)
		if row.Has("error") || row.Get("tools") != 1 {
			t.Fatalf("queued connect reread config: %v", row)
		}
	}
	if p.connects.Load() != 2 {
		t.Fatalf("snapshot connects=%d", p.connects.Load())
	}
	// The next operation sees the removal, including a resident direct-call session.
	if _, err := e.Call(context.Background(), "test.echo", map[string]any{}, true); err == nil {
		t.Fatal("next call did not see config removal")
	}
}

// TestSettledConfigIsNotReadAgain pins when a snapshot may be reused: only while the config
// file is unchanged and its modification time is old enough to trust.
func TestSettledConfigIsNotReadAgain(t *testing.T) {
	e := testEngine(t, wire.Object{})
	first, err := e.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := e.snapshot(); again == first {
		t.Fatal("a config written a moment ago was not read again")
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(e.Path, old, old); err != nil {
		t.Fatal(err)
	}
	settledOnce, _ := e.snapshot()
	if again, _ := e.snapshot(); again != settledOnce {
		t.Fatal("a settled, unchanged config was read again")
	}
	if err := os.WriteFile(e.Path, []byte(`{"servers":{"added":{"type":"http","url":"http://127.0.0.1:1/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(e.Path, old, old); err != nil {
		t.Fatal(err)
	}
	changed, err := e.snapshot()
	if err != nil || !changed.servers.Has("added") {
		t.Fatalf("a config of another size with the same time was not read again: %v, %v", changed, err)
	}
}

// TestServerThatWantsASignInSaysHowToGiveIt pins what a search reports for an OAuth server
// tap has no sign-in for.
func TestServerThatWantsASignInSaysHowToGiveIt(t *testing.T) {
	guarded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="http://`+r.Host+`/.well-known/oauth-protected-resource"`)
		http.Error(w, "sign in first", http.StatusUnauthorized)
	}))
	t.Cleanup(guarded.Close)
	row := listing(t, testEngine(t, definitions(guarded.URL))).Get("integrations").([]any)[0].(wire.Object)
	if got := wire.String(row.Get("error")); got != `needs you to sign in: run "tap auth test"` {
		t.Fatalf("error = %q", got)
	}
}

// TestUnchangedListKeepsCatalog pins the digest: a refresh that finds the list a server last
// answered keeps the tools already decoded, and one that finds another list replaces them.
func TestUnchangedListKeepsCatalog(t *testing.T) {
	p := newPeer(t, nil)
	e := testEngine(t, definitions(p.url))
	listing(t, e)
	snapshot, err := e.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	def := snapshot.servers.Get("test").(wire.Object)
	refresh := func() []wire.Object {
		t.Helper()
		tools, err := e.liveTools(context.Background(), "test", def, snapshot.fingerprints["test"], true, nil)
		if err != nil {
			t.Fatal(err)
		}
		return tools
	}
	first, lists := refresh(), p.lists.Load()
	again := refresh()
	if p.lists.Load() == lists {
		t.Fatal("the second refresh did not ask the server")
	}
	if &again[0] != &first[0] {
		t.Fatal("an unchanged list was decoded again")
	}
	addTool(p.server, "second")
	if changed := refresh(); len(changed) != 2 {
		t.Fatalf("a changed list was not taken: %d tools", len(changed))
	}
}

// TestStartOnCallLeavesServersAlone pins the default start setting: once tap holds a server's
// tools, a search answers from them without starting it, and a call starts it and checks them.
func TestStartOnCallLeavesServersAlone(t *testing.T) {
	p := newPeer(t, nil)
	e := testEngine(t, definitions(p.url))
	listing(t, e)
	e.Close()
	connects := p.connects.Load()
	next := New(e.Path, "test")
	defer next.Close()
	found, err := next.Discover(context.Background(), SearchOptions{Query: "echo", Limit: 8}, true)
	if err != nil {
		t.Fatal(err)
	}
	if match := found.Get("matches").([]any)[0].(wire.Object); match.Get("id") != "test.echo" || match.Get("stale") != true {
		t.Fatalf("search did not answer from the saved tools: %v", match)
	}
	next.mu.Lock()
	started := next.entries["test"] != nil || next.catalogs["test"].refreshing != nil
	next.mu.Unlock()
	if started || p.connects.Load() != connects {
		t.Fatal("a search started the server")
	}
	if _, err := next.CallWithOptions(context.Background(), "test.echo", wire.Object{}, true, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	if p.connects.Load() != connects+1 {
		t.Fatal("the call did not start the server")
	}
}

// TestStartWithTapKeepsServerRunning pins the eager start setting: warming up starts the
// server, and it is not stopped for being unused.
func TestStartWithTapKeepsServerRunning(t *testing.T) {
	t.Setenv("TAP_IDLE_TTL_MS", "0")
	p := newPeer(t, nil)
	e := testEngine(t, definitions(p.url))
	if err := config.Set(e.Path, "test", "start", config.StartWithTap); err != nil {
		t.Fatal(err)
	}
	e.Warm()
	waitFor(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.running("test") })
	time.Sleep(50 * time.Millisecond)
	e.mu.Lock()
	running := e.running("test")
	e.mu.Unlock()
	if !running || p.connects.Load() != 1 {
		t.Fatalf("running %v after %d connections", running, p.connects.Load())
	}
}

// TestListChangedDuringRefreshAnswers pins the retry: a list_changed notification that lands
// while the refresh is fetching marks the catalog stale, so the waiting caller fetches again
// instead of being told the catalog changed. The peer holds its list response until tap's
// handler has taken the invalidation, which makes the race land every time.
func TestListChangedDuringRefreshAnswers(t *testing.T) {
	var e *Engine
	var server *mcp.Server
	var lists atomic.Int32
	p := newPeer(t, func(_ context.Context, method string, _ mcp.Request) (mcp.Result, error, bool) {
		if method == "tools/list" && lists.Add(1) == 2 {
			addTool(server, "second")
			until := time.Now().Add(2 * time.Second)
			for time.Now().Before(until) {
				e.mu.Lock()
				raced := e.catalogs["test"] != nil && e.catalogs["test"].revision > 0
				e.mu.Unlock()
				if raced {
					break
				}
				time.Sleep(time.Millisecond)
			}
		}
		return nil, nil, false
	})
	server = p.server
	e = testEngine(t, definitions(p.url))
	listing(t, e)
	snapshot, err := e.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	def := snapshot.servers.Get("test").(wire.Object)
	tools, err := e.liveTools(context.Background(), "test", def, snapshot.fingerprints["test"], true, nil)
	if err != nil {
		t.Fatalf("refresh with a racing invalidation: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("a changed list was not taken: %d tools", len(tools))
	}
}
