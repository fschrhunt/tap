package registry

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/discovery"
	"github.com/fschrhunt/tap/internal/wire"
)

// SearchOptions chooses search, exact inspection or server browsing within one tool.
// Auto disclosure returns complete schemas when they fit, otherwise labeled summaries.
type SearchOptions struct {
	Query, Server, Detail string
	IDs                   []string
	Limit                 float64
	Offset, MaxBytes      int
	Refresh               bool
}

// Overview lists a bounded set of integration names without starting any server.
func (e *Engine) Overview() string {
	servers, err := config.Load(e.Path)
	if err != nil {
		return ""
	}
	names := []string{}
	size := 0
	for _, s := range servers {
		if len(names) >= 16 || size+len(s.Name) > 1024 {
			break
		}
		names = append(names, s.Name)
		size += len(s.Name)
	}
	if len(names) == 0 {
		return ""
	}
	b, _ := wire.JSON(names, false)
	if more := len(servers) - len(names); more > 0 {
		return fmt.Sprintf(" Servers: %s and %d more; plugin_search with no arguments lists them.", b, more)
	}
	return fmt.Sprintf(" Servers: %s.", b)
}

// Search keeps the shell's full-schema search and original numeric limit semantics.
func (e *Engine) Search(ctx context.Context, query string, limit float64, quiet bool) (wire.Object, error) {
	return e.Discover(ctx, SearchOptions{Query: query, Limit: limit, Detail: "full", MaxBytes: maxCatalogBytes}, quiet)
}

// Discover searches or browses scoped catalogs and discloses only the requested detail.
func (e *Engine) Discover(ctx context.Context, opt SearchOptions, quiet bool) (wire.Object, error) {
	if len(opt.Query) > discovery.MaxQueryBytes {
		return nil, &Failure{"invalid_query", "search query exceeds 4096 bytes", "Use a shorter query."}
	}
	if opt.Detail == "" {
		opt.Detail = "auto"
	}
	if opt.MaxBytes == 0 {
		opt.MaxBytes = e.settings.SearchMaxBytes
	}
	snapshot, err := e.snapshot()
	if err != nil {
		return nil, err
	}
	servers := snapshot.servers
	scope := opt.Server
	if len(opt.IDs) > 0 {
		wanted := map[string]bool{}
		for _, id := range opt.IDs {
			name, _, ok := strings.Cut(id, ".")
			if !ok || name == "" {
				return nil, fmt.Errorf("invalid tool id %q (expected server.tool)", id)
			}
			if scope != "" && name != scope {
				return nil, fmt.Errorf("tool %q is outside server %q", id, scope)
			}
			if def, ok := servers.Get(name).(wire.Object); ok {
				_, tool, _ := strings.Cut(id, ".")
				if err := checkPolicy(def, tool); err != nil {
					return nil, err
				}
			}
			wanted[name] = true
		}
		selected := wire.Object{}
		for _, s := range servers {
			if wanted[s.Name] {
				selected = append(selected, s)
				delete(wanted, s.Name)
			}
		}
		if len(wanted) > 0 {
			return nil, fmt.Errorf("unknown server in requested tool ids")
		}
		servers = selected
	} else if scope == "" {
		// A provider explicitly named in a query is a hard scope only when unambiguous.
		for _, token := range discovery.Tokens(opt.Query) {
			if servers.Has(token) {
				if scope != "" && scope != token {
					scope = ""
					break
				}
				scope = token
			}
		}
	}
	if scope != "" {
		def, ok := servers.Get(scope).(wire.Object)
		if !ok {
			return nil, fmt.Errorf("unknown server %q", scope)
		}
		servers = wire.Object{{Name: scope, Value: def}}
	}
	rows, err := e.rowsFor(ctx, servers, snapshot.fingerprints, quiet, opt.Refresh)
	if err != nil {
		return nil, err
	}
	tools := []discovery.Tool{}
	unavailable := []any{}
	catalogs := []any{}
	metadata := map[string]catalog{}
	staleServers := map[string]bool{}
	for _, r := range rows {
		def, _ := servers.Get(r.name).(wire.Object)
		if _, err := policy(def); err != nil {
			r.err = err.Error()
		}
		if r.err != "" {
			unavailable = append(unavailable, wire.Object{{Name: "server", Value: r.name}, {Name: "error", Value: r.err}})
		}
		staleServers[r.name] = r.stale
		metadata[r.name] = r.catalog
		availability := "not_checked"
		if !r.cached && r.err == "" {
			availability = "reachable"
		}
		row := wire.Object{{Name: "server", Value: r.name}, {Name: "tools", Value: len(r.tools)}, {Name: "source", Value: map[bool]string{true: "cache", false: "live"}[r.cached]}, {Name: "observedAt", Value: r.catalog.at.UTC().Format(time.RFC3339Nano)}, {Name: "availability", Value: availability}, {Name: "stale", Value: r.stale}}
		if r.err != "" {
			row.Set("error", r.err)
		}
		catalogs = append(catalogs, row)
		for _, t := range r.tools {
			name, _ := t.Get("name").(string)
			if name == "" || checkPolicy(def, name) != nil {
				continue
			}
			if len(tools) >= discovery.MaxRankTools {
				return nil, &Failure{"catalog_too_large", "search exceeds 65536 candidate tools", "Scope the search to one server."}
			}
			tools = append(tools, discovery.Tool{ID: r.name + "." + name, Server: r.name, Definition: t})
		}
	}
	ranked := []discovery.Match{}
	if len(opt.IDs) > 0 {
		for _, id := range opt.IDs {
			found := false
			for _, t := range tools {
				if t.ID == id {
					ranked = append(ranked, discovery.Match{Tool: t})
					found = true
					break
				}
			}
			if !found && len(unavailable) == 0 {
				return nil, fmt.Errorf("unknown tool %q; refresh its server catalog", id)
			}
		}
	} else if opt.Query == "" && (opt.Server != "" || opt.Refresh) {
		for _, t := range tools {
			ranked = append(ranked, discovery.Match{Tool: t})
		}
	} else if opt.Query != "" {
		ranked = discovery.Rank(tools, opt.Query)
	}
	n := len(ranked)
	start := min(max(opt.Offset, 0), n)
	end := n
	if opt.Limit != opt.Limit {
		end = start
	} else if opt.Limit < float64(n-start) {
		if opt.Limit <= -float64(n-start) {
			end = start
		} else {
			end = start + int(opt.Limit)
			if end < start {
				end = n + int(opt.Limit)
			}
		}
	}
	matches := []any{}
	out := wire.Object{{Name: "query", Value: opt.Query}, {Name: "total", Value: n}, {Name: "matches", Value: matches}, {Name: "unavailable", Value: unavailable}, {Name: "catalogs", Value: catalogs}}
	for _, m := range ranked[start:end] {
		full := toolView(m.Tool, true)
		summary := toolView(m.Tool, false)
		if staleServers[m.Tool.Server] {
			full.Set("stale", true)
			summary.Set("stale", true)
		}
		if c := metadata[m.Tool.Server]; c.instructions != "" {
			full.Set("serverGuidance", wire.Object{{Name: "trust", Value: "untrusted_server_content"}, {Name: "text", Value: c.instructions}})
		}
		view := full
		if opt.Detail == "summary" {
			view = summary
		}
		trial := append(append([]any{}, matches...), view)
		out.Set("matches", trial)
		out.Set("nextOffset", start+len(trial))
		b, _ := wire.JSON(out, true)
		if len(b) > opt.MaxBytes && opt.Detail == "auto" {
			view = summary
			trial = append(append([]any{}, matches...), view)
			out.Set("matches", trial)
			b, _ = wire.JSON(out, true)
		}
		if len(b) > opt.MaxBytes {
			out.Set("matches", matches)
			if len(matches) == 0 {
				return nil, fmt.Errorf("tool metadata exceeds maxBytes %d; request detail summary or increase maxBytes (maximum %d)", opt.MaxBytes, maxCatalogBytes)
			}
			break
		}
		matches = trial
	}
	out.Set("matches", matches)
	for _, m := range matches {
		if m.(wire.Object).Get("schemaLoaded") == false {
			out.Set("note", "A match with schemaLoaded false has no input schema here. Look its id up with ids and detail full before calling it.")
			break
		}
	}
	if start+len(matches) < n {
		out.Set("nextOffset", start+len(matches))
	} else {
		out.Delete("nextOffset")
	}
	if n == 0 && len(unavailable) == 0 {
		out.Set("hint", "No matching capability. Try fewer or broader terms, or browse with server and no query. Lexical search cannot infer every paraphrase.")
	}
	if b, _ := wire.JSON(out, true); len(b) > opt.MaxBytes {
		return nil, fmt.Errorf("catalog metadata exceeds maxBytes %d; restrict discovery with server or increase maxBytes", opt.MaxBytes)
	}
	return out, nil
}

// Warm reads the saved tool lists, so the first search does not wait for them, and starts
// the servers whose start setting is start. It starts no other server.
func (e *Engine) Warm() {
	snapshot, err := e.snapshot()
	if err != nil {
		return
	}
	for _, f := range snapshot.servers {
		if def, _ := f.Value.(wire.Object); e.start(def) == config.StartWithTap {
			go func() { _, _ = e.liveTools(e.ctx, f.Name, def, snapshot.fingerprints[f.Name], true, nil) }()
		}
	}
}

// toolView never clips schemas; summaries explicitly identify missing call contracts.
func toolView(t discovery.Tool, full bool) wire.Object {
	out := wire.Object{{Name: "id", Value: t.ID}}
	for _, k := range []string{"title", "description", "annotations", "inputSchema", "outputSchema"} {
		if !full && (k == "inputSchema" || k == "outputSchema") {
			continue
		}
		if t.Definition.Has(k) {
			value := t.Definition.Get(k)
			if !full && (k == "description" || k == "title") {
				if s, ok := value.(string); ok && len([]rune(s)) > 512 {
					value = string([]rune(s)[:512])
					out.Set("textTruncated", true)
				}
			}
			out.Set(k, value)
		}
	}
	out.Set("schemaLoaded", full)
	return out
}
