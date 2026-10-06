package tap_test

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigRejectsUnknownVerbWithoutChangingSettings(t *testing.T) {
	b := sandbox(t)
	b.write(map[string]any{"servers": map[string]any{}, "settings": map[string]any{"start": "search"}})
	r := b.run("config", "sett", "start", "start")
	equal(t, r.status, 2)
	contains(t, r.stderr, "needs set or unset")
	equal(t, b.read(), decode(t, `{"servers":{},"settings":{"start":"search"}}`))
}

func TestConfigSettingReadRejectsUnknownServer(t *testing.T) {
	b := sandbox(t)
	b.write(map[string]any{"servers": map[string]any{}})
	r := b.run("config", "start", "--server", "missing")
	equal(t, r.status, 1)
	contains(t, r.stderr, `there is no server named "missing"`)
}

func TestImportTableCanContainServerNamedServers(t *testing.T) {
	b := sandbox(t)
	source := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(source, []byte(`{"mcpServers":{"servers":{"command":"svc"},"files":{"command":"other"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	got := output(t, b.run("import", source, "--dry-run"))
	contains(t, got, "would add servers (stdio)")
	contains(t, got, "would add files (stdio)")
}

func TestImportUsesConfiguredAgentHome(t *testing.T) {
	b := sandbox(t)
	codexHome := filepath.Join(t.TempDir(), "codex")
	if err := os.MkdirAll(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("[mcp_servers.db]\ncommand = \"db\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b.env = append(b.env, "CODEX_HOME="+codexHome)
	contains(t, output(t, b.run("import", "codex", "--dry-run")), "would add db (stdio)")
}

func TestConnectDoesNotCallDisabledTapConnected(t *testing.T) {
	b := sandbox(t)
	configHome := filepath.Join(t.TempDir(), "xdg")
	openCode := filepath.Join(configHome, "opencode")
	if err := os.MkdirAll(openCode, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(openCode, "opencode.json"), []byte(`{"mcp":{"tap":{"type":"local","command":["tap"],"enabled":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	b.env = append(b.env, "XDG_CONFIG_HOME="+configHome)
	r := b.run("connect", "opencode")
	equal(t, r.status, 1)
	contains(t, r.stderr, "tap is turned off")
	if r.stdout != "" {
		t.Fatalf("unexpected success output: %s", r.stdout)
	}
}

func TestRemoveLocalNamesTheLocalRegistryWhenRemoteSelected(t *testing.T) {
	b := sandbox(t)
	b.write(map[string]any{
		"servers":        map[string]any{"files": map[string]any{"type": "stdio", "command": []string{"svc"}}},
		"remotes":        map[string]any{"work": map[string]any{"url": "https://example.invalid/mcp"}},
		"selectedRemote": "work",
	})
	equal(t, output(t, b.run("remove", "files", "--local")), "removed files")
}
