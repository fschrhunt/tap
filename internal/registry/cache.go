// Catalogs outlive sessions and retain the last successful list across failures.
package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fschrhunt/tap/internal/auth"
	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const catalogTTL = time.Minute
const maxCatalogBytes = 16 << 20

// staleRefreshes bounds how often an invalidation that lands mid-fetch makes the caller wait
// for one more fetch before the churn is reported.
const staleRefreshes = 2

// indexOpening is the first line of the saved index, and names its layout.
const indexOpening = `{"version":2,"servers":{`

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
	// digest names what the server last answered; a refresh that finds it again keeps tools.
	digest string
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

// configSnapshot keeps definitions and fingerprints together through fanout, with the state
// of the file they were read from.
type configSnapshot struct {
	servers      wire.Object
	fingerprints map[string]string
	file         os.FileInfo
	authDigest   string
}

// settled is how old a config file's modification time must be before an unchanged time and
// size are taken to mean unchanged content. It is longer than the coarsest timestamps a
// filesystem keeps, so an edit made within one tick of the last read is never missed.
const settled = 2 * time.Second

// snapshot runs once per operation and invalidates changed or removed generations. It reads
// the config again unless unchanged and settled, and checks owner-only sign-in state
// every time so removal invalidates sessions and persisted authority across processes.
func (e *Engine) snapshot() (*configSnapshot, error) {
	e.snapshotMu.Lock()
	defer e.snapshotMu.Unlock()
	if err := e.ctx.Err(); err != nil {
		return nil, err
	}
	authDigest, grants, err := auth.Fingerprints(e.Path)
	if err != nil {
		return nil, err
	}
	file, statErr := os.Stat(e.Path)
	if last := e.last; last != nil && last.authDigest == authDigest && statErr == nil && last.file != nil && os.SameFile(file, last.file) &&
		file.Size() == last.file.Size() && file.ModTime().Equal(last.file.ModTime()) && time.Since(file.ModTime()) > settled {
		return last, nil
	}
	e.last = nil
	servers, err := config.Load(e.Path)
	if err != nil {
		return nil, err
	}
	definitions := make(map[string]string, len(servers))
	for _, f := range servers {
		definitions[f.Name] = fmt.Sprintf("%x", sha256.Sum256([]byte(definitionFingerprint(f.Value)+"\x00"+grants[f.Name])))
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
	if statErr != nil {
		file = nil
	}
	e.last = &configSnapshot{servers: servers, fingerprints: definitions, file: file, authDigest: authDigest}
	return e.last, nil
}

// toolsFor returns tools, stale status and last refresh error together.
// Warm callers return immediately during one shared refresh; cold waits may cancel independently.
// A server started only by calls is not started to be asked again once tap holds its tools:
// they stay marked stale until it runs for a call or a refresh, and then it is asked.
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
	if c.good && e.start(def) == config.StartOnCall && !e.running(name) {
		tools, err := c.tools, c.err
		e.mu.Unlock()
		return tools, true, err
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
// An invalidation that lands mid-fetch marks the catalog stale, so the caller waits for one more
// fetch rather than being told the catalog changed.
func (e *Engine) liveTools(ctx context.Context, name string, def wire.Object, fingerprint string, quiet bool, ent *entry) ([]wire.Object, error) {
	for attempt := 0; ; attempt++ {
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
		if c.err != nil {
			err := c.err
			e.mu.Unlock()
			return nil, err
		}
		switch {
		case e.catalogs[name] != c || (ent != nil && c.session != ent):
			e.mu.Unlock()
			return nil, fmt.Errorf("catalog changed during refresh")
		case c.good && time.Since(c.at) < catalogTTL:
			tools := c.tools
			e.mu.Unlock()
			return tools, nil
		}
		e.mu.Unlock()
		if attempt >= staleRefreshes {
			return nil, fmt.Errorf("catalog changed during refresh")
		}
	}
}

// running reports whether a session with the server is open. The caller holds e.mu.
func (e *Engine) running(name string) bool {
	ent := e.entries[name]
	if ent == nil {
		return false
	}
	select {
	case <-ent.ready:
		return ent.err == nil
	default:
		return false
	}
}

// refresh publishes only complete lists; failures preserve prior tools and back off. A list
// with the digest of the last one keeps the tools already decoded and is not saved again.
func (e *Engine) refresh(name string, def wire.Object, quiet bool, c *catalog) {
	defer e.workers.Done()
	e.mu.Lock()
	revision, tools, known := c.revision, c.tools, c.digest
	e.mu.Unlock()
	began := time.Now()
	ent, err := e.connect(e.ctx, name, def, c.fingerprint, quiet)
	digest := ""
	if err == nil {
		defer e.release(ent)
		if ent.openedAt.After(began) {
			began = ent.openedAt
		}
		remaining := e.deadline - time.Since(began)
		err = e.acquire(e.ctx)
		if err == nil {
			ctx, cancel := context.WithTimeout(e.ctx, remaining)
			var listed []wire.Object
			listed, digest, err = listTools(ctx, ent.session, known)
			err = e.deadlineError(ctx, err)
			cancel()
			<-e.slots
			if err == nil && digest != known {
				tools = listed
			}
		}
	}
	e.mu.Lock()
	if err == nil {
		c.tools, c.good, c.at = tools, true, time.Now()
		c.session = ent
		c.instructions, c.digest = ent.session.InitializeResult().Instructions, digest
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
	if err == nil && digest != known && e.catalogs[name] == c {
		select {
		case e.indexDirty <- struct{}{}:
		default:
		}
	}
	e.mu.Unlock()
}

// listTools follows bounded cursors and returns the server's tools with the digest of what it
// answered: raw for stdio, and the SDK-supported fields for HTTP. A server that answers what
// known names returns no tools, since the caller already holds them; when that answer is a
// single page, it is not decoded at all.
func listTools(ctx context.Context, session *mcp.ClientSession, known string) ([]wire.Object, string, error) {
	instructions := session.InitializeResult().Instructions
	var pages [][]byte
	var lists [][]any
	cursor := ""
	seen := map[string]bool{}
	totalBytes := 0
	for pageNumber := 0; pageNumber < 100; pageNumber++ {
		cctx, raw := wire.Take(ctx)
		result, err := session.ListTools(cctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, "", err
		}
		if len(*raw) == 0 {
			b, err := wire.JSON(result, false)
			if err != nil {
				return nil, "", err
			}
			*raw = b
		}
		totalBytes += len(*raw)
		if totalBytes > maxCatalogBytes {
			return nil, "", fmt.Errorf("tools/list exceeds the 16 MiB catalog limit")
		}
		pages = append(pages, *raw)
		// A first page with the known digest is the whole list it named, so it has no cursor.
		if pageNumber == 0 && catalogDigest(instructions, pages) == known {
			return nil, known, nil
		}
		v, err := wire.DecodeExactOwned(*raw)
		if err != nil {
			return nil, "", err
		}
		o, ok := v.(wire.Object)
		if !ok {
			return nil, "", fmt.Errorf("invalid tools/list result")
		}
		list, ok := o.Get("tools").([]any)
		if !ok {
			return nil, "", fmt.Errorf("invalid tools/list tools")
		}
		lists = append(lists, list)
		cursor, _ = o.Get("nextCursor").(string)
		if cursor == "" {
			digest := catalogDigest(instructions, pages)
			if digest == known {
				return nil, known, nil
			}
			tools, err := shapeTools(lists)
			return tools, digest, err
		}
		if seen[cursor] {
			return nil, "", fmt.Errorf("repeated tools/list cursor")
		}
		seen[cursor] = true
	}
	return nil, "", fmt.Errorf("tools/list exceeded 100 pages")
}

// catalogDigest names a server's answer: its instructions and every page of its tool list.
// Two answers with one digest hold the same tools, so the second need not be decoded again.
func catalogDigest(instructions string, pages [][]byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d:%s", len(instructions), instructions)
	for _, page := range pages {
		h.Write(page)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// shapeTools turns listed pages into tool definitions, refusing empty or repeated names.
func shapeTools(lists [][]any) ([]wire.Object, error) {
	tools := []wire.Object{}
	names := map[string]bool{}
	for _, list := range lists {
		for _, t := range list {
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
	}
	return tools, nil
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

// loadIndex accepts only an owner-only regular file in saveIndex's layout, and from it only
// servers whose definitions match. Persisted tools are stale so the first lookup revalidates
// in the background.
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
	b := make([]byte, opened.Size())
	if _, err = io.ReadFull(f, b); err != nil {
		return
	}
	// The file is JSON laid out in lines: one that opens each server and one for each of its
	// tools. Lines are found without reading the JSON, and tools are decoded side by side,
	// since the first search waits for all of them. Any other shape is not this index.
	type saved struct {
		name  string
		head  wire.Object
		tools []wire.Object
		lines [][]byte
	}
	var servers []*saved
	var open *saved
	lines := bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
	if len(lines) < 2 || string(lines[0]) != indexOpening || string(lines[len(lines)-1]) != "}}" {
		return
	}
	total := 0
	for _, line := range lines[1 : len(lines)-1] {
		line = bytes.TrimSuffix(line, []byte(","))
		switch {
		case open == nil && bytes.HasSuffix(line, []byte(`"tools":[`)):
			v, err := wire.Decode(slices.Concat([]byte("{"), line, []byte("]}}")))
			row, _ := v.(wire.Object)
			if err != nil || len(row) != 1 {
				return
			}
			head, _ := row[0].Value.(wire.Object)
			open = &saved{name: row[0].Name, head: head}
		case open != nil && string(line) == "]}":
			if open.head.Get("fingerprint") == e.definitions[open.name] {
				open.tools = make([]wire.Object, len(open.lines))
				total += len(open.lines)
				servers = append(servers, open)
			}
			open = nil
		case open != nil && len(line) > 0 && line[0] == '{':
			open.lines = append(open.lines, line)
		default:
			return
		}
	}
	if open != nil {
		return
	}
	type job struct {
		to   *wire.Object
		line []byte
	}
	jobs := make([]job, 0, total)
	for _, s := range servers {
		for i, line := range s.lines {
			jobs = append(jobs, job{&s.tools[i], line})
		}
	}
	workers := min(runtime.GOMAXPROCS(0), 8, len(jobs))
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, j := range jobs[len(jobs)*w/workers : len(jobs)*(w+1)/workers] {
				if v, err := wire.DecodeExactOwned(j.line); err == nil {
					*j.to, _ = v.(wire.Object)
				}
			}
		}()
	}
	wg.Wait()
	for _, s := range servers {
		if slices.ContainsFunc(s.tools, func(t wire.Object) bool { return t == nil }) {
			continue
		}
		instructions, _ := s.head.Get("instructions").(string)
		digest, _ := s.head.Get("digest").(string)
		e.catalogs[s.name] = &catalog{fingerprint: e.definitions[s.name], tools: s.tools, good: true, instructions: instructions, digest: digest}
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
// Only fingerprints, digests and descriptors/guidance are stored, never connection definitions.
func (e *Engine) saveIndex() {
	if os.Getenv("TAP_CACHE_DIR") == "off" {
		return
	}
	e.indexMu.Lock()
	defer e.indexMu.Unlock()
	e.mu.Lock()
	type row struct {
		name  string
		head  wire.Object
		tools []wire.Object
	}
	var rows []row
	for name, c := range e.catalogs {
		if c.good && c.fingerprint == e.definitions[name] {
			rows = append(rows, row{name, wire.Object{{Name: "fingerprint", Value: c.fingerprint}, {Name: "digest", Value: c.digest}, {Name: "instructions", Value: c.instructions}}, c.tools})
		}
	}
	e.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	// One line opens each server and one holds each tool; loadIndex reads it by lines.
	b := []byte(indexOpening)
	for i, r := range rows {
		name, _ := wire.JSON(r.name, false)
		head, err := wire.JSON(r.head, false)
		if err != nil {
			return
		}
		if i > 0 {
			b = append(b, ',')
		}
		b = append(append(append(append(b, '\n'), name...), ':'), head[:len(head)-1]...)
		b = append(b, `,"tools":[`...)
		for j, tool := range r.tools {
			line, err := wire.JSON(tool, false)
			if err != nil {
				return
			}
			if j > 0 {
				b = append(b, ',')
			}
			b = append(append(b, '\n'), line...)
		}
		b = append(b, "\n]}"...)
	}
	b = append(b, "\n}}\n"...)
	if len(b) > maxCatalogBytes {
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
