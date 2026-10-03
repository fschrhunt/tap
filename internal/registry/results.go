package registry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fschrhunt/tap/internal/wire"
)

const resultTTL = 10 * time.Minute
const maxResultBytes = 8 << 20
const maxStoredBytes = 32 << 20

// resultStore keeps bounded, session-only results; opaque references are never persisted.
type resultStore struct {
	mu      sync.Mutex
	entries map[string]storedResult
	bytes   int
}
type storedResult struct {
	raw    []byte
	server string
	at     time.Time
	scope  string
}

type resultScopeKey struct{}

// WithResultScope binds references to the actual MCP session, never client-supplied metadata.
func WithResultScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, resultScopeKey{}, scope)
}

// resultScope reads the trusted server-side session identity (empty for standalone CLI calls).
func resultScope(ctx context.Context) string {
	scope, _ := ctx.Value(resultScopeKey{}).(string)
	return scope
}

// scopedResult refuses cross-session reference reads before revealing retained content.
func (s *resultStore) scopedResult(ctx context.Context, id string) (storedResult, error) {
	v, err := s.get(id)
	if err != nil {
		return v, err
	}
	if v.scope != resultScope(ctx) {
		return storedResult{}, &Failure{"reference_unavailable", "result reference is unavailable in this session", "Use only references created by this MCP session."}
	}
	return v, nil
}

// ArgumentReference explicitly copies one retained JSON value into an existing argument object.
// No marker in ordinary tool arguments is interpreted as a reference.
type ArgumentReference struct{ Target, Reference, Pointer string }

// retain stores a complete result or leaves it inline if retention cannot be fulfilled.
func (s *resultStore) retain(server string, result wire.Object, b []byte) wire.Object {
	return s.retainScoped(context.Background(), server, result, b)
}

// retainScoped stores a complete raw result under the caller's trusted session scope.
func (s *resultStore) retainScoped(ctx context.Context, server string, result wire.Object, b []byte) wire.Object {
	if !json.Valid(b) || len(b) > maxResultBytes {
		meta, _ := result.Get("_meta").(wire.Object)
		meta.Set("tap", wire.Object{{Name: "referenceUnavailable", Value: "result exceeds the 8 MiB retention limit; returned inline, do not repeat the call"}})
		result.Set("_meta", meta)
		return result
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return result
	}
	id := hex.EncodeToString(idBytes)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]storedResult{}
	}
	s.expire()
	for len(s.entries) >= 128 || s.bytes+len(b) > maxStoredBytes {
		oldest := ""
		var at time.Time
		for k, v := range s.entries {
			if oldest == "" || v.at.Before(at) {
				oldest = k
				at = v.at
			}
		}
		s.remove(oldest)
	}
	s.entries[id] = storedResult{raw: append([]byte(nil), b...), server: server, at: time.Now(), scope: resultScope(ctx)}
	s.bytes += len(b)
	kinds := []any{}
	seenKinds := map[string]bool{}
	if content, ok := result.Get("content").([]any); ok {
		for _, part := range content {
			if o, ok := part.(wire.Object); ok {
				if kind, ok := o.Get("type").(string); ok && !seenKinds[kind] {
					kinds = append(kinds, kind)
					seenKinds[kind] = true
				}
			}
		}
	}
	preview := wire.Object{{Name: "reference", Value: id}, {Name: "bytes", Value: len(b)}, {Name: "expiresAt", Value: time.Now().Add(resultTTL).UTC().Format(time.RFC3339Nano)}, {Name: "contentTypes", Value: kinds}, {Name: "isError", Value: result.Get("isError") == true}, {Name: "hint", Value: "Inspect this session-only reference with plugin_call operation inspect. Full content, including images and errors, is retained; this is not the original result."}}
	text, _ := wire.JSON(preview, true)
	return wire.Object{{Name: "content", Value: []any{wire.Object{{Name: "type", Value: "text"}, {Name: "text", Value: string(text)}}}}, {Name: "structuredContent", Value: preview}, {Name: "isError", Value: result.Get("isError") == true}}
}

// get lends immutable raw bytes; deletion cannot invalidate a caller's slice.
// Callers must decode before mutation and never write into the retained bytes.
func (s *resultStore) get(id string) (storedResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	v, ok := s.entries[id]
	if !ok {
		return storedResult{}, &Failure{"reference_unavailable", "result reference is unknown, expired or evicted", "References are session-only. Do not repeat a write merely to recreate its result."}
	}
	return v, nil
}

// expire removes expired entries under the store lock.
func (s *resultStore) expire() {
	for id, v := range s.entries {
		if time.Since(v.at) >= resultTTL {
			s.remove(id)
		}
	}
}

// remove releases one retained payload under the store lock.
func (s *resultStore) remove(id string) { s.bytes -= len(s.entries[id].raw); delete(s.entries, id) }

// Drop explicitly releases one reference without executing any backend operation.
func (e *Engine) Drop(ctx context.Context, id string) (wire.Object, error) {
	s := &e.results
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	if v, ok := s.entries[id]; !ok || v.scope != resultScope(ctx) {
		return nil, &Failure{"reference_unavailable", "result reference is unknown, expired or evicted", "No backend operation was performed."}
	}
	s.remove(id)
	return wire.Object{{Name: "dropped", Value: true}}, nil
}

// Inspect returns a selected value or deterministic array/object page, bounded by serialized bytes.
func (e *Engine) Inspect(ctx context.Context, id, pointer string, offset, limit, maxBytes int) (wire.Object, error) {
	v, err := e.results.scopedResult(ctx, id)
	if err != nil {
		return nil, err
	}
	value, err := decodeResult(v.raw)
	if err != nil {
		return nil, err
	}
	value, err = selectPointer(value, pointer)
	if err != nil {
		return nil, err
	}
	out := wire.Object{{Name: "reference", Value: id}, {Name: "pointer", Value: pointer}, {Name: "value", Value: value}}
	switch x := value.(type) {
	case []any:
		start := min(offset, len(x))
		end := min(start+limit, len(x))
		out.Set("value", x[start:end])
		out.Set("total", len(x))
		if end < len(x) {
			out.Set("nextOffset", end)
		}
	case map[string]any:
		keys := sortedKeys(x)
		start := min(offset, len(keys))
		end := min(start+limit, len(keys))
		page := map[string]any{}
		for _, k := range keys[start:end] {
			page[k] = x[k]
		}
		out.Set("value", page)
		out.Set("total", len(keys))
		if end < len(keys) {
			out.Set("nextOffset", end)
		}
	}
	b, _ := wire.JSON(out, true)
	if len(b) > maxBytes {
		return nil, &Failure{"result_too_large", "selected result exceeds maxBytes", "Select a deeper JSON Pointer, reduce limit, or increase maxBytes. The retained result is unchanged."}
	}
	return out, nil
}

// sortedKeys makes object paging and validation diagnostics deterministic.
func sortedKeys(o map[string]any) []string {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pointerParts validates RFC 6901 syntax including escape sequences and array indices.
func pointerParts(p string) ([]string, error) {
	if p == "" {
		return nil, nil
	}
	if !strings.HasPrefix(p, "/") {
		return nil, &Failure{"invalid_pointer", "JSON Pointer must be empty or begin with /", "Use RFC 6901 pointers."}
	}
	parts := strings.Split(p[1:], "/")
	for i, s := range parts {
		for j := 0; j < len(s); j++ {
			if s[j] == '~' {
				if j+1 >= len(s) || (s[j+1] != '0' && s[j+1] != '1') {
					return nil, &Failure{"invalid_pointer", "invalid JSON Pointer escape", "Use ~0 for ~ and ~1 for /."}
				}
				j++
			}
		}
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
	}
	return parts, nil
}

// selectPointer selects without coercion; missing properties are distinct from JSON null.
func selectPointer(value any, p string) (any, error) {
	parts, err := pointerParts(p)
	if err != nil {
		return nil, err
	}
	for _, part := range parts {
		switch x := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = x[part]
			if !ok {
				return nil, &Failure{"invalid_pointer", "JSON Pointer property does not exist", "Inspect the parent value first."}
			}
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(x) || strconv.Itoa(i) != part {
				return nil, &Failure{"invalid_pointer", "JSON Pointer array index is invalid", "Use an existing zero-based index."}
			}
			value = x[i]
		default:
			return nil, &Failure{"invalid_pointer", "JSON Pointer traverses a scalar", "Inspect the parent value first."}
		}
	}
	return value, nil
}

// resolveReferences decodes each immutable result once and caps cumulative expanded
// argument bytes at 8 MiB before attaching copies and final schema validation.
func (e *Engine) resolveReferences(ctx context.Context, target string, args any, refs []ArgumentReference, servers wire.Object) (any, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	if len(refs) > 128 || len(b) > maxResultBytes {
		return nil, &Failure{"arguments_too_large", "argument expansion exceeds 128 references or 8 MiB", "Use fewer references or smaller arguments."}
	}
	expanded := len(b)
	decoded := map[string]any{}
	decodedBytes := 0
	plain, err := decodeResult(b)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments")
	}
	paths := []string{}
	for _, ref := range refs {
		parts, err := pointerParts(ref.Target)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			return nil, &Failure{"invalid_pointer", "reference target cannot replace the whole arguments object", "Use a property pointer such as /message."}
		}
		for _, p := range paths {
			if p == ref.Target || strings.HasPrefix(p, ref.Target+"/") || strings.HasPrefix(ref.Target, p+"/") {
				return nil, &Failure{"invalid_pointer", "reference targets overlap", "Use nonoverlapping target pointers."}
			}
		}
		paths = append(paths, ref.Target)
		stored, err := e.results.scopedResult(ctx, ref.Reference)
		if err != nil {
			return nil, err
		}
		def, ok := servers.Get(stored.server).(wire.Object)
		if !ok {
			return nil, &Failure{"reference_flow_denied", "source server is no longer configured", "Restore the source policy outside the agent session."}
		}
		if err = checkReferenceFlow(def, stored.server, target); err != nil {
			return nil, err
		}
		value, loaded := decoded[ref.Reference]
		if !loaded {
			if len(stored.raw) > maxStoredBytes-decodedBytes {
				return nil, &Failure{"arguments_too_large", "referenced source data exceeds 32 MiB", "Use fewer retained results."}
			}
			value, err = decodeResult(stored.raw)
			if err != nil {
				return nil, err
			}
			decoded[ref.Reference] = value
			decodedBytes += len(stored.raw)
		}
		value, err = selectPointer(value, ref.Pointer)
		if err != nil {
			return nil, err
		}
		selected, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		targetBytes, err := json.Marshal(ref.Target)
		if err != nil {
			return nil, err
		}
		if len(selected)+len(targetBytes)+2 > maxResultBytes-expanded {
			return nil, &Failure{"arguments_too_large", "expanded arguments exceed 8 MiB", "Select smaller values or use fewer references."}
		}
		expanded += len(selected) + len(targetBytes) + 2
		// Detach the selected value: later target assignments must not mutate the decoded source.
		value, err = decodeResult(selected)
		if err != nil {
			return nil, err
		}
		parent := plain
		if len(parts) > 1 {
			parent, err = selectPointer(plain, ref.Target[:strings.LastIndex(ref.Target, "/")])
			if err != nil {
				return nil, err
			}
		}
		key := parts[len(parts)-1]
		switch x := parent.(type) {
		case map[string]any:
			x[key] = value
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(x) || strconv.Itoa(i) != key {
				return nil, &Failure{"invalid_pointer", "reference target array index is invalid", "Use an existing index."}
			}
			x[i] = value
		default:
			return nil, &Failure{"invalid_pointer", "reference target parent is not an object or array", "Provide the target parent in arguments."}
		}
	}
	return plain, nil
}

// decodeResult preserves JSON numbers exactly when inspecting or forwarding retained data.
func decodeResult(raw []byte) (any, error) {
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	err := d.Decode(&v)
	return v, err
}
