package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fschrhunt/tap/internal/wire"
)

// TestConcurrentEdits ensures relay settings and all parallel server additions survive.
func TestConcurrentEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := SetRemote(path, &Remote{URL: "http://localhost:7777/mcp", TokenEnv: "TOKEN"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Add(path, fmt.Sprint(i), wire.Object{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	r, err := LoadRemote(path)
	if err != nil || r == nil || r.TokenEnv != "TOKEN" {
		t.Fatalf("remote lost: %v %v", r, err)
	}
	if err := SetRemote(path, nil); err != nil {
		t.Fatal(err)
	}
	servers, err := Load(path)
	if err != nil || len(servers) != 20 {
		t.Fatalf("servers lost: %d %v", len(servers), err)
	}
}

// TestNamedRemoteProfilesSelectWithoutDiscardingOthers pins named-target lifecycle.
func TestNamedRemoteProfilesSelectWithoutDiscardingOthers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for name, host := range map[string]string{"home": "home.example", "work": "work.example"} {
		if err := SaveRemoteProfile(path, name, Remote{URL: "https://" + host, TokenEnv: "REMOTE_TOKEN"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := SelectRemoteProfile(path, "work"); err != nil {
		t.Fatal(err)
	}
	selected, err := LoadRemote(path)
	if err != nil || selected == nil || selected.URL != "https://work.example/mcp" {
		t.Fatalf("selected remote = %+v, %v", selected, err)
	}
	profiles, err := LoadRemoteProfiles(path)
	if err != nil || profiles.Selected != "work" || len(profiles.Profiles) != 2 {
		t.Fatalf("profiles = %+v, %v", profiles, err)
	}
	if removed, err := RemoveRemoteProfile(path, "work"); err != nil || !removed {
		t.Fatalf("remove selected = %v, %v", removed, err)
	}
	selected, err = LoadRemote(path)
	if err != nil || selected != nil {
		t.Fatalf("selected after removing active profile = %+v, %v", selected, err)
	}
	profiles, err = LoadRemoteProfiles(path)
	if err != nil || len(profiles.Profiles) != 1 || profiles.Profiles["home"].URL != "https://home.example/mcp" {
		t.Fatalf("remaining profiles = %+v, %v", profiles, err)
	}
}

// TestRemoteCredentialSidecarKeepsPairedTokensPrivate pins the paired-secret storage contract.
func TestRemoteCredentialSidecarKeepsPairedTokensPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	if err := SaveRemoteSecret(path, "device-1", "secret-token"); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRemoteSecret(path, "device-1")
	if err != nil || got != "secret-token" {
		t.Fatalf("LoadRemoteSecret = %q, %v", got, err)
	}
	info, err := os.Stat(remoteSecretsPath(path))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("secret sidecar mode = %v, %v", info, err)
	}
	config, err := os.ReadFile(path)
	if err == nil || strings.Contains(string(config), "secret-token") {
		t.Fatalf("token leaked to server config: %s (%v)", config, err)
	}
}

// TestInvalidConfigFailsClosed protects malformed roots from destructive edits.
func TestInvalidConfigFailsClosed(t *testing.T) {
	for _, input := range []string{`[]`, `{"servers":null}`, `{"servers":{"x":null}}`, `{"remote":null}`, `{"remote":{"url":"https://example.com","allowInsecure":"yes"}}`, `{"remote":{"url":"http://example.com"}}`} {
		t.Run(input, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadRemote(path); err == nil {
				t.Fatal("accepted malformed config")
			}
			if err := Add(path, "x", wire.Object{}); err == nil {
				t.Fatal("edited malformed config")
			}
			data, _ := os.ReadFile(path)
			if string(data) != input {
				t.Fatal("modified malformed config")
			}
		})
	}
}

// TestURLSecurity pins credential-free normalization and explicit insecure consent.
func TestURLSecurity(t *testing.T) {
	for _, raw := range []string{"ftp://localhost", "https://u:p@example.com", "https://example.com/mcp?q=secret", "https://example.com/#secret", "https://example.com/other", "http://example.com"} {
		if _, err := NormalizeURL(raw, false); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:7777", "http://[::1]:7777/", "https://example.com/mcp/"} {
		if _, err := NormalizeURL(raw, false); err != nil {
			t.Error(err)
		}
	}
	if got, err := NormalizeURL("http://example.com", true); err != nil || got != "http://example.com/mcp" {
		t.Fatalf("%s %v", got, err)
	}
}

// TestMalformedConfigHidesCredentials prevents JSON diagnostic snippets from exposing values.
func TestMalformedConfigHidesCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	if err := os.WriteFile(path, []byte(`{"headers": credential-do-not-echo}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("accepted invalid JSON")
	}
	if strings.Contains(err.Error(), "credential-do-not-echo") {
		t.Fatal("config error exposed value")
	}
}
