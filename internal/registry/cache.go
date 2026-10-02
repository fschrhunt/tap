// Catalogs outlive sessions and retain the last successful list across failures.
package registry

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// catalog is guarded by Engine.mu; published tool objects are immutable.
type catalog struct {
	fingerprint string
	tools       []wire.Object
	at, retryAt time.Time
	good        bool
	refreshing  chan struct{}
	err         error
	revision    uint64
}

// envDuration accepts zero for deterministic retry tests.
func envDuration(name string, fallback time.Duration) time.Duration {
	if ms, err := strconv.ParseInt(os.Getenv(name), 10, 64); err == nil && ms >= 0 && ms <= 2147483647 {
		return time.Duration(ms) * time.Millisecond
	}
	return fallback
}

// definitionFingerprint hashes configuration and expanded connection inputs only;
// credentials never appear in the persisted index.
func definitionFingerprint(def any) string {
	b, _ := wire.JSON(def, false)
	token := ""
	if o, ok := def.(wire.Object); ok {
		if name, ok := o.Get("bearerTokenEnv").(string); ok {
			token = os.Getenv(name)
		}
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(config.Expand(string(b))+"\x00"+token+"\x00"+os.Getenv("HOME")+"\x00"+os.Getenv("PATH"))))
}

// configSnapshot keeps definitions and their fingerprints together through fanout.
type configSnapshot struct {
	servers      wire.Object
	fingerprints map[string]string
}

// snapshot reads once per operation and invalidates changed or removed generations.
func (e *Engine) snapshot() (*configSnapshot, error) {
	e.snapshotMu.Lock()
	defer e.snapshotMu.Unlock()
	servers, err := config.Load(e.Path)
	if err != nil {
		return nil, err
	}
	definitions := make(map[string]string, len(servers))
	for _, f := range servers {
		definitions[f.Name] = definitionFingerprint(f.Value)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for name, ent := range e.entries {
		if definitions[name] != ent.fingerprint {
			delete(e.entries, name)
			e.closeEntry(ent)
		}
	}
	for name, c := range e.catalogs {
		if definitions[name] != c.fingerprint {
			delete(e.catalogs, name)
		}
	}
	e.definitions = definitions
	if !e.indexLoaded {
		e.indexLoaded = true
		e.loadIndex()
	}
	return &configSnapshot{servers: servers, fingerprints: definitions}, nil
}

// toolsFor returns tools, their stale status, and the last refresh error together.
// Successful catalogs return immediately while sharing one refresh.
// A cold caller may cancel its wait without cancelling shared work.
func (e *Engine) toolsFor(ctx context.Context, name string, def wire.Object, fingerprint string, quiet bool) ([]wire.Object, bool, error) {
	e.mu.Lock()
	if e.ctx.Err() != nil {
		e.mu.Unlock()
		return nil, false, e.ctx.Err()
	}
	if e.definitions[name] != fingerprint {
		e.mu.Unlock()
		return nil, false, fmt.Errorf("server %q configuration changed", name)
	}
	c := e.catalogs[name]
	if c == nil {
		c = &catalog{fingerprint: fingerprint}
		e.catalogs[name] = c
	}
	if c.good && time.Since(c.at) < time.Minute {
		tools := c.tools
		e.mu.Unlock()
		return tools, false, nil
	}
	if c.refreshing == nil && !time.Now().Before(c.retryAt) {
		c.refreshing = make(chan struct{})
		e.workers.Add(1)
		go e.refresh(name, def, quiet, c)
	}
	if c.good {
		tools, err := c.tools, c.err
		e.mu.Unlock()
		return tools, true, err
	}
	done := c.refreshing
	if done == nil {
		err := c.err
		e.mu.Unlock()
		return nil, false, err
	}
	e.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, false, ctx.Err()
	case <-done:
		e.mu.Lock()
		tools, err := c.tools, c.err
		stale := c.good && time.Since(c.at) >= time.Minute
		e.mu.Unlock()
		return tools, stale, err
	}
}

// refresh publishes only a complete paginated list; failure preserves prior tools.
func (e *Engine) refresh(name string, def wire.Object, quiet bool, c *catalog) {
	defer e.workers.Done()
	e.mu.Lock()
	revision := c.revision
	e.mu.Unlock()
	began := time.Now()
	ent, err := e.connect(e.ctx, name, def, c.fingerprint, quiet)
	var tools []wire.Object
	if err == nil {
		defer e.release(ent)
		// Admission wait does not spend the server budget. Initialization and listing
		// still share the budget, including a connect that was already in flight.
		if ent.openedAt.After(began) {
			began = ent.openedAt
		}
		remaining := e.deadline - time.Since(began)
		err = e.acquire(e.ctx)
		if err == nil {
			ctx, cancel := context.WithTimeout(e.ctx, remaining)
			tools, err = listTools(ctx, ent.session)
			err = e.deadlineError(ctx, err)
			cancel()
			<-e.slots
		}
	}
	e.mu.Lock()
	if err == nil {
		c.tools, c.good, c.at = tools, true, time.Now()
		if c.revision != revision {
			c.at = time.Time{}
		}
		c.retryAt = time.Time{}
	} else {
		c.retryAt = time.Now().Add(e.failTTL)
	}
	c.err = err
	close(c.refreshing)
	c.refreshing = nil
	if err == nil && e.catalogs[name] == c {
		select {
		case e.indexDirty <- struct{}{}:
		default:
		}
	}
	e.mu.Unlock()
}

// listTools follows cursors while retaining the raw wire shape and annotations.
func listTools(ctx context.Context, session *mcp.ClientSession) ([]wire.Object, error) {
	tools := []wire.Object{}
	cursor := ""
	seen := map[string]bool{}
	for {
		cctx, raw := wire.Capture(ctx)
		result, err := session.ListTools(cctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		// The SDK may serve a typed cached page for newer negotiated protocols.
		if len(*raw) == 0 {
			b, err := wire.JSON(result, false)
			if err != nil {
				return nil, err
			}
			*raw = b
		}
		v, err := wire.Decode(*raw)
		if err != nil {
			return nil, err
		}
		o, ok := v.(wire.Object)
		if !ok {
			return nil, fmt.Errorf("invalid tools/list result")
		}
		page, ok := o.Get("tools").([]any)
		if !ok {
			return nil, fmt.Errorf("invalid tools/list tools")
		}
		for _, t := range page {
			tool, ok := t.(wire.Object)
			if !ok {
				return nil, fmt.Errorf("invalid tool definition")
			}
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
			tools = append(tools, tool)
		}
		cursor = result.NextCursor
		if cursor == "" {
			return tools, nil
		}
		if seen[cursor] {
			return nil, fmt.Errorf("repeated tools/list cursor")
		}
		seen[cursor] = true
	}
}

// reap closes idle, unused sessions while keeping their catalogs.
func (e *Engine) reap() {
	defer e.workers.Done()
	interval := e.idleTTL / 2
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case now := <-ticker.C:
			e.reapIdle(now)
		}
	}
}

// reapIdle detaches expired generations; close never holds the engine lock.
func (e *Engine) reapIdle(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for name, ent := range e.entries {
		select {
		case <-ent.ready:
			if ent.err == nil && ent.users == 0 && now.Sub(ent.lastUsed) >= e.idleTTL {
				delete(e.entries, name)
				e.closeEntry(ent)
			}
		default:
		}
	}
}

// loadIndex accepts only an owner-only regular file and matching definitions.
// Persisted tools are stale so the first lookup revalidates in the background.
func (e *Engine) loadIndex() {
	path := e.Path + ".tools.json"
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 {
		return
	}
	b, err := io.ReadAll(io.LimitReader(f, 16<<20))
	if err != nil {
		return
	}
	v, err := wire.Decode(b)
	if err != nil {
		return
	}
	index, ok := v.(wire.Object)
	if !ok || index.Get("version") != float64(1) {
		return
	}
	rows, _ := index.Get("servers").(wire.Object)
	for _, f := range rows {
		row, ok := f.Value.(wire.Object)
		if !ok || row.Get("fingerprint") != e.definitions[f.Name] || e.definitions[f.Name] == "" {
			continue
		}
		raw, ok := row.Get("tools").([]any)
		if !ok {
			continue
		}
		tools := make([]wire.Object, 0, len(raw))
		for _, t := range raw {
			tool, ok := t.(wire.Object)
			if !ok {
				tools = nil
				break
			}
			tools = append(tools, tool)
		}
		if tools != nil {
			e.catalogs[f.Name] = &catalog{fingerprint: e.definitions[f.Name], tools: tools, good: true}
		}
	}
}

// persistIndex coalesces successful refreshes; Close ends it after refresh workers
// finish so the final queued write includes every successful catalog.
func (e *Engine) persistIndex() {
	defer close(e.indexDone)
	for range e.indexDirty {
		timer := time.NewTimer(20 * time.Millisecond)
	debounce:
		for {
			select {
			case _, ok := <-e.indexDirty:
				if !ok {
					timer.Stop()
					e.saveIndex()
					return
				}
			case <-timer.C:
				break debounce
			}
		}
		e.saveIndex()
	}
}

// saveIndex atomically replaces the index with mode 0600, never following its symlink.
// Only fingerprints and tool descriptors are stored, never connection definitions.
func (e *Engine) saveIndex() {
	e.indexMu.Lock()
	defer e.indexMu.Unlock()
	e.mu.Lock()
	rows := wire.Object{}
	for name, c := range e.catalogs {
		if c.good && c.fingerprint == e.definitions[name] {
			tools := make([]any, len(c.tools))
			for i, t := range c.tools {
				tools[i] = t
			}
			rows.Set(name, wire.Object{{Name: "fingerprint", Value: c.fingerprint}, {Name: "tools", Value: tools}})
		}
	}
	e.mu.Unlock()
	b, err := wire.JSON(wire.Object{{Name: "version", Value: 1}, {Name: "servers", Value: rows}}, false)
	if err != nil {
		return
	}
	// The config directory already exists; persistence is optional if it is unwritable.
	path := e.Path + ".tools.json"
	f, err := os.CreateTemp(filepath.Dir(path), ".tap-tools-*")
	if err != nil {
		return
	}
	temp := f.Name()
	defer os.Remove(temp)
	_, err = f.Write(b)
	closeErr := f.Close()
	if err == nil && closeErr == nil {
		_ = os.Rename(temp, path)
	}
}
