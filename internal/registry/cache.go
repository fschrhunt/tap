package registry

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fschrhunt/tap/internal/wire"
)

const catalogTTL = time.Minute
const maxCatalogBytes = 16 << 20

// catalog stores metadata only, never tool results or expanded credentials.
type catalog struct {
	key          string
	tools        []wire.Object
	instructions string
	at           time.Time
	cached       bool
	generation   uint64
}

// cacheKey scopes metadata to config, working directory, version and inherited environment.
// Only the digest is stored; changing credentials or launch conditions invalidates the catalog.
func (e *Engine) cacheKey(name string, def wire.Object) string {
	b, _ := wire.JSON(def, false)
	var canonical any
	_ = json.Unmarshal(b, &canonical)
	b, _ = json.Marshal(canonical)
	env := os.Environ()
	sort.Strings(env)
	cwd, _ := os.Getwd()
	path, _ := filepath.Abs(e.Path)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(path+"\x00"+name+"\x00"+e.Version+"\x00"+cwd+"\x00"+string(b)+"\x00"+strings.Join(env, "\x00"))))
}

// cacheDirectory returns a private metadata location, or disables persistence with "off".
func cacheDirectory() string {
	if p := os.Getenv("TAP_CACHE_DIR"); p != "" {
		if p == "off" {
			return ""
		}
		return p
	}
	p, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(p, "tap")
}

// readCatalog accepts bounded, fresh, versioned cache files and ignores corrupt/stale entries.
func (e *Engine) readCatalog(key string) (catalog, bool) {
	if e.cacheDir == "" {
		return catalog{}, false
	}
	f, err := os.Open(filepath.Join(e.cacheDir, key+".json"))
	if err != nil {
		return catalog{}, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > maxCatalogBytes {
		return catalog{}, false
	}
	var saved struct {
		Version      int
		At           time.Time
		Tools        []json.RawMessage
		Instructions string
	}
	if json.NewDecoder(f).Decode(&saved) != nil || saved.Version != 1 || time.Since(saved.At) < 0 || time.Since(saved.At) >= catalogTTL {
		return catalog{}, false
	}
	c := catalog{key: key, at: saved.At, instructions: saved.Instructions, cached: true, tools: []wire.Object{}}
	for _, raw := range saved.Tools {
		v, err := wire.DecodeExact(raw)
		o, ok := v.(wire.Object)
		if err != nil || !ok {
			return catalog{}, false
		}
		c.tools = append(c.tools, o)
	}
	return c, true
}

// writeCatalog atomically replaces owner-only metadata; cache failure never blocks tool use.
func (e *Engine) writeCatalog(c catalog) {
	if e.cacheDir == "" {
		return
	}
	b, err := wire.JSON(struct {
		Version      int
		At           time.Time
		Tools        []wire.Object
		Instructions string
	}{1, c.at, c.tools, c.instructions}, false)
	if err != nil || len(b) > maxCatalogBytes {
		return
	}
	if os.MkdirAll(e.cacheDir, 0700) != nil {
		return
	}
	f, err := os.CreateTemp(e.cacheDir, ".catalog-*")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return
	}
	if f.Close() != nil {
		return
	}
	_ = os.Rename(f.Name(), filepath.Join(e.cacheDir, c.key+".json"))
}

// invalidateCatalog evicts only a matching generation after tools/list_changed.
func (e *Engine) invalidateCatalog(name string, ent *entry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.entries[name] != ent {
		return
	}
	ent.changed.Add(1)
	delete(e.catalogs, name)
	if e.cacheDir != "" {
		_ = os.Remove(filepath.Join(e.cacheDir, ent.key+".json"))
	}
}
