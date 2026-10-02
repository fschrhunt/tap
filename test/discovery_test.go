package tap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestScopedDiscovery never starts an unrelated backend, even on a cold catalog.
func TestScopedDiscovery(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	h := start(t, map[string]any{"fixture": definition(), "unrelated": definition("--trace", trace, "--hang-connect")})
	h.initialize()
	r := h.search(map[string]any{"query": "please echo my message", "server": "fixture"})
	equal(t, r["matches"].([]any)[0].(map[string]any)["id"], "fixture.echo")
	h.search(map[string]any{"query": "fixture echo"})
	h.search(map[string]any{"ids": []string{"fixture.data"}, "detail": "full"})
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatal("unrelated backend started")
	}
}

// TestDiscoveryPaging allows browsing without knowing a tool's search vocabulary.
func TestDiscoveryPaging(t *testing.T) {
	h := start(t, nil)
	h.initialize()
	first := h.search(map[string]any{"server": "fixture", "detail": "summary", "limit": 1})
	equal(t, first["total"], float64(3))
	equal(t, first["nextOffset"], float64(1))
	tool := first["matches"].([]any)[0].(map[string]any)
	equal(t, tool["schemaLoaded"], false)
	if tool["inputSchema"] != nil {
		t.Fatal("summary contains a schema")
	}
	second := h.search(map[string]any{"server": "fixture", "offset": first["nextOffset"], "limit": 2})
	equal(t, len(second["matches"].([]any)), 2)
	if second["nextOffset"] != nil {
		t.Fatal("unexpected next page")
	}
}

// TestAdaptiveDisclosure omits oversized schemas explicitly and can retrieve them intact.
func TestAdaptiveDisclosure(t *testing.T) {
	h := start(t, map[string]any{"fixture": definition("--large-schema")})
	h.initialize()
	r := h.search(map[string]any{"query": "echo", "maxBytes": 2048})
	tool := r["matches"].([]any)[0].(map[string]any)
	equal(t, tool["schemaLoaded"], false)
	if tool["inputSchema"] != nil {
		t.Fatal("partial schema returned")
	}
	r = h.search(map[string]any{"ids": []string{"fixture.echo"}, "detail": "full", "maxBytes": 100000})
	tool = r["matches"].([]any)[0].(map[string]any)
	equal(t, tool["schemaLoaded"], true)
	equal(t, tool["inputSchema"].(map[string]any)["description"], strings.Repeat("schema documentation ", 3000))
}

// TestUsageContract preserves output schemas and labels server guidance as untrusted.
func TestUsageContract(t *testing.T) {
	h := start(t, map[string]any{"fixture": definition("--output-schema")})
	h.initialize()
	tool := h.search(map[string]any{"ids": []string{"fixture.data"}, "detail": "full"})["matches"].([]any)[0].(map[string]any)
	equal(t, tool["outputSchema"].(map[string]any)["type"], "object")
	equal(t, tool["serverGuidance"].(map[string]any)["trust"], "untrusted_server_content")
}

// TestMetadataPersistence restores searchable descriptors without claiming live availability.
func TestMetadataPersistence(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	b := sandbox(t)
	b.write(map[string]any{"servers": map[string]any{"fixture": definition("--trace", trace)}})
	output(t, b.run("search", "echo", "--json"))
	r := decode(t, output(t, b.run("search", "echo", "--json"))).(map[string]any)
	cat := r["catalogs"].([]any)[0].(map[string]any)
	equal(t, cat["source"], "cache")
	equal(t, cat["availability"], "not_checked")
	equal(t, r["matches"].([]any)[0].(map[string]any)["id"], "fixture.echo")
	live := decode(t, output(t, b.run("refresh", "fixture"))).(map[string]any)
	equal(t, live["integrations"].([]any)[0].(map[string]any)["availability"], "reachable")
}

// TestMetadataCredentialScope invalidates metadata when inherited credentials change.
func TestMetadataCredentialScope(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	b := sandbox(t)
	b.write(map[string]any{"servers": map[string]any{"fixture": definition("--trace", trace)}})
	t.Setenv("TAP_TEST_SECRET", "first-secret")
	output(t, b.run("search", "echo", "--json"))
	t.Setenv("TAP_TEST_SECRET", "second-secret")
	output(t, b.run("search", "echo", "--json"))
	data, _ := os.ReadFile(trace)
	equal(t, strings.Count(string(data), "initialize\n"), 2)
	st, err := os.Stat(b.config + ".tools.json")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, st.Mode().Perm(), os.FileMode(0600))
	raw, err := os.ReadFile(b.config + ".tools.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "first-secret") || strings.Contains(string(raw), "second-secret") {
		t.Fatal("credentials written to metadata cache")
	}
}

// TestConfigReplacement retires a connected generation when the launch definition changes.
func TestConfigReplacement(t *testing.T) {
	h := start(t, nil)
	h.initialize()
	h.search(nil)
	b := &box{t: t, config: h.config}
	b.write(map[string]any{"servers": map[string]any{"fixture": definition("--output-schema")}})
	r := h.search(map[string]any{"ids": []string{"fixture.data"}, "detail": "full"})
	if r["matches"].([]any)[0].(map[string]any)["outputSchema"] == nil {
		t.Fatal("old connection reused after config replacement")
	}
}

// TestAllCatalogPages discovers tools beyond the first tools/list response.
func TestAllCatalogPages(t *testing.T) {
	h := start(t, map[string]any{"fixture": definition("--paged")})
	h.initialize()
	r := h.search(map[string]any{"server": "fixture"})
	equal(t, r["total"], float64(3))
	equal(t, h.call("data", nil)["structuredContent"], map[string]any{"count": float64(3)})
}

// TestIdleShutdown restarts opt-in stdio sessions without destroying the metadata catalog.
func TestIdleShutdown(t *testing.T) {
	dir := t.TempDir()
	trace := filepath.Join(dir, "trace")
	closed := filepath.Join(dir, "closed")
	def := definition("--trace", trace, "--exit-file", closed)
	def["idleTimeoutMs"] = 100
	h := start(t, map[string]any{"fixture": def})
	h.initialize()
	h.call("data", nil)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(closed); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle backend never stopped")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.search(map[string]any{"server": "fixture"})
	data, _ := os.ReadFile(trace)
	equal(t, strings.Count(string(data), "initialize\n"), 1)
	h.call("data", nil)
	data, _ = os.ReadFile(trace)
	equal(t, strings.Count(string(data), "initialize\n"), 2)
}

// TestCatalogNotification invalidates memory and disk metadata after a real SDK notification.
func TestCatalogNotification(t *testing.T) {
	h := start(t, map[string]any{"fixture": definition("--notify")})
	h.initialize()
	h.search(map[string]any{"query": "echo"})
	h.call("data", nil)
	deadline := time.Now().Add(3 * time.Second)
	for {
		r := h.search(map[string]any{"ids": []string{"fixture.echo"}, "detail": "full"})
		tool := r["matches"].([]any)[0].(map[string]any)
		if tool["description"] == "Updated echo contract after tools/list_changed." {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("notification did not invalidate catalog")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStaleMetadata marks restored descriptors stale during background revalidation.
func TestStaleMetadata(t *testing.T) {
	b := sandbox(t)
	b.write(map[string]any{"servers": map[string]any{"fixture": definition()}})
	output(t, b.run("search", "echo", "--json"))
	r := decode(t, output(t, b.run("search", "echo", "--json"))).(map[string]any)
	equal(t, r["matches"].([]any)[0].(map[string]any)["stale"], true)
	equal(t, r["catalogs"].([]any)[0].(map[string]any)["availability"], "not_checked")
}
