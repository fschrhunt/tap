package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fschrhunt/tap/internal/wire"
)

// TestSettingsPrecedence pins where a value comes from: the default, then the config, then
// the setting's environment variable, which is ignored when it holds what the setting does
// not take.
func TestSettingsPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	if got := LoadValues(path); got.Start != StartOnCall || got.DeadlineMs != 5000 || got.References {
		t.Fatalf("defaults: %+v", got)
	}
	for name, value := range map[string]any{"start": "search", "deadlineMs": float64(2500), "references": true} {
		if err := Set(path, "", name, value); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TAP_DEADLINE_MS", "900")
	t.Setenv("TAP_REFERENCES", "maybe")
	got := LoadValues(path)
	if got.Start != StartOnSearch || got.DeadlineMs != 900 || !got.References {
		t.Fatalf("config and environment: %+v", got)
	}
	current, err := Current(path)
	if err != nil {
		t.Fatal(err)
	}
	from := map[string]string{}
	for _, v := range current {
		from[v.Name] = v.From
	}
	if from["start"] != "config" || from["deadlineMs"] != "TAP_DEADLINE_MS" || from["references"] != "config" || from["searchLimit"] != "default" {
		t.Fatalf("sources: %v", from)
	}
}

// TestMistypedSettingIsRefused pins that a config holding a setting tap would not take is
// refused as a whole, rather than read with a guess, and that a server's own start is checked.
func TestMistypedSettingIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	for config, want := range map[string]string{
		`{"servers":{},"settings":{"deadlineMs":"5000"}}`:                    "deadlineMs takes a whole number from 1 to 2147483647",
		`{"servers":{},"settings":{"lazy":true}}`:                            `no setting "lazy"`,
		`{"servers":{"a":{"command":["x"],"start":"later"}}}`:                "server a: config setting start takes call, search or start",
		`{"servers":{"a":{"command":["x"]}},"settings":{"references":"on"}}`: "references takes on or off",
	} {
		if err := os.WriteFile(path, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", config, err, want)
		}
	}
}

func TestServerIdleTimeoutMustFitStdioContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	if err := os.WriteFile(path, []byte(`{"servers":{"stdio":{"type":"stdio","command":["svc"]},"http":{"type":"http","url":"https://example.invalid/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, server := range []string{"stdio", "http"} {
		if err := Set(path, server, "idleTimeoutMs", float64(0)); err == nil {
			t.Errorf("Set idleTimeoutMs=0 for %s succeeded", server)
		}
	}
	if err := Set(path, "http", "idleTimeoutMs", float64(1000)); err == nil {
		t.Error("Set idleTimeoutMs for HTTP server succeeded")
	}
	if err := Set(path, "stdio", "idleTimeoutMs", float64(1000)); err != nil {
		t.Fatalf("Set valid stdio override: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load valid override: %v", err)
	}
}

func TestLegacyInvalidServerIdleTimeoutCanBeUnset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	if err := os.WriteFile(path, []byte(`{"servers":{"stdio":{"type":"stdio","command":["svc"],"idleTimeoutMs":0}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Set(path, "stdio", "idleTimeoutMs", nil); err != nil {
		t.Fatalf("unset legacy invalid override: %v", err)
	}
	servers, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if def := servers.Get("stdio").(wire.Object); def.Has("idleTimeoutMs") {
		t.Fatal("invalid override remains after unset")
	}
}
