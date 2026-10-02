// Catalogs outlive sessions and retain the last successful list across failures.
package registry

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const catalogTTL = time.Minute
const maxCatalogBytes = 16 << 20

// catalog is guarded by Engine.mu; published tool objects are immutable.
type catalog struct {
	fingerprint  string
	tools        []wire.Object
	at, retryAt  time.Time
	good         bool
	refreshing   chan struct{}
	err          error
	revision     uint64
	instructions string
	session      *entry
}

// envDuration accepts zero for deterministic retry tests.
func envDuration(name string, fallback time.Duration) time.Duration {
	if ms, err := strconv.ParseInt(os.Getenv(name), 10, 64); err == nil && ms >= 0 && ms <= 2147483647 {
		return time.Duration(ms) * time.Millisecond
	}
	return fallback
}

// definitionFingerprint hashes configured/expanded inputs and inherited child credentials.
// Only a digest, never connection definitions or credentials, enters the persisted index.
func definitionFingerprint(def any) string {
	b, _ := wire.JSON(def, false)
	token, inherited := "", ""
	if o, ok := def.(wire.Object); ok {
		if name, ok := o.Get("bearerTokenEnv").(string); ok {
			token = os.Getenv(name)
		}
		if endpointIsStdio(o) {
			env := os.Environ()
			sort.Strings(env)
			cwd, _ := os.Getwd()
			inherited = cwd + "\x00" + strings.Join(env, "\x00")
		}
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(config.Expand(string(b))+"\x00"+token+"\x00"+os.Getenv("HOME")+"\x00"+os.Getenv("PATH")+"\x00"+inherited)))
}

// configSnapshot keeps definitions and fingerprints together through fanout.
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
	if e.ctx.Err() != nil {
		return nil, e.ctx.Err()
	}
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

// toolsFor returns tools, stale status and last refresh error together.
// Warm callers return immediately during one shared refresh; cold waits may cancel independently.
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
	if c.good && time.Since(c.at) < catalogTTL {
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
		stale := c.good && time.Since(c.at) >= catalogTTL
		e.mu.Unlock()
		return tools, stale, err
	}
}

// liveTools waits for a successful catalog belonging to the calling session.
// A nil session requests an explicit refresh; stale descriptors are never used to validate calls.
func (e *Engine) liveTools(ctx context.Context, name string, def wire.Object, fingerprint string, quiet bool, ent *entry) ([]wire.Object, error) {
	e.mu.Lock()
	if e.ctx.Err() != nil {
		e.mu.Unlock()
		return nil, e.ctx.Err()
	}
	if e.definitions[name] != fingerprint {
		e.mu.Unlock()
		return nil, fmt.Errorf("server configuration changed")
	}
	c := e.catalogs[name]
	if c == nil {
		c = &catalog{fingerprint: fingerprint}
		e.catalogs[name] = c
	}
	if ent != nil && c.good && c.session == ent && time.Since(c.at) < catalogTTL && c.err == nil {
		tools := c.tools
		e.mu.Unlock()
		return tools, nil
	}
	if c.refreshing == nil {
		c.at = time.Time{}
		c.retryAt = time.Time{}
		c.refreshing = make(chan struct{})
		e.workers.Add(1)
		go e.refresh(name, def, quiet, c)
	}
	done := c.refreshing
	e.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if e.catalogs[name] != c || !c.good || time.Since(c.at) >= catalogTTL || (ent != nil && c.session != ent) {
		return nil, fmt.Errorf("catalog changed during refresh")
	}
	return c.tools, nil
}

// refresh publishes only complete lists; failures preserve prior tools and back off.
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
		c.session = ent
		c.instructions = ent.session.InitializeResult().Instructions
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

// listTools follows bounded cursors, retaining raw stdio and SDK-supported HTTP fields.
func listTools(ctx context.Context, session *mcp.ClientSession) ([]wire.Object, error) {
	tools := []wire.Object{}
	cursor := ""
	seen := map[string]bool{}
	names := map[string]bool{}
	totalBytes := 0
	for pageNumber := 0; pageNumber < 100; pageNumber++ {
		cctx, raw := wire.Capture(ctx)
		result, err := session.ListTools(cctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		if len(*raw) == 0 {
			b, err := wire.JSON(result, false)
			if err != nil {
				return nil, err
			}
			*raw = b
		}
		totalBytes += len(*raw)
		if totalBytes > maxCatalogBytes {
			return nil, fmt.Errorf("tools/list exceeds the 16 MiB catalog limit")
		}
		v, err := wire.DecodeExact(*raw)
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
			name, _ := tool.Get("name").(string)
			if name == "" || names[name] {
				return nil, fmt.Errorf("tools/list contains an empty or duplicate tool name")
			}
			names[name] = true
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
	return nil, fmt.Errorf("tools/list exceeded 100 pages")
}

// reap closes idle unused sessions, preserving their catalogs and active operations.
func (e *Engine) reap() {
	defer e.workers.Done()
	interval := min(e.idleTTL/2, 100*time.Millisecond)
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
			if ent.err == nil && ent.users == 0 && now.Sub(ent.lastUsed) >= ent.idle {
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
	if os.Getenv("TAP_CACHE_DIR") == "off" {
		return
	}
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
	if err != nil || !os.SameFile(st, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 || opened.Size() > maxCatalogBytes {
		return
	}
	b, err := io.ReadAll(io.LimitReader(f, maxCatalogBytes+1))
	if err != nil || len(b) > maxCatalogBytes {
		return
	}
	v, err := wire.DecodeExact(b)
	if err != nil {
		return
	}
	index, ok := v.(wire.Object)
	if !ok || wire.String(index.Get("version")) != "1" {
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
			instructions, _ := row.Get("instructions").(string)
			e.catalogs[f.Name] = &catalog{fingerprint: e.definitions[f.Name], tools: tools, good: true, instructions: instructions}
		}
	}
}

// persistIndex coalesces successful refreshes; Close flushes the final queued write.
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

// saveIndex atomically replaces the owner-only index without following its symlink.
// Only fingerprints and descriptors/guidance are stored, never connection definitions.
func (e *Engine) saveIndex() {
	if os.Getenv("TAP_CACHE_DIR") == "off" {
		return
	}
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
			rows.Set(name, wire.Object{{Name: "fingerprint", Value: c.fingerprint}, {Name: "tools", Value: tools}, {Name: "instructions", Value: c.instructions}})
		}
	}
	e.mu.Unlock()
	b, err := wire.JSON(wire.Object{{Name: "version", Value: 1}, {Name: "servers", Value: rows}}, false)
	if err != nil || len(b) > maxCatalogBytes {
		return
	}
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
