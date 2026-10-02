package cli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/remote"
)

// TestRemoteRouting verifies relay-only administration, explicit local edits and config preservation.
func TestRemoteRouting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.json")
	hostPath := filepath.Join(t.TempDir(), "host.json")
	t.Setenv("TAP_CONFIG", path)
	t.Setenv("TAP_REMOTE_TOKEN", "secret")
	t.Setenv("TAP_REMOTE_ADMIN_TOKEN", "")
	h, close, err := remote.Handler(hostPath, "test", "secret", "")
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	host := httptest.NewServer(h)
	defer host.Close()
	run := func(args ...string) string {
		t.Helper()
		var out, stderr bytes.Buffer
		if code := Run(context.Background(), args, "test", &out, &stderr); code != 0 {
			t.Fatalf("%v: %s", args, stderr.String())
		}
		return out.String()
	}
	run("add", "local", "http://localhost:1")
	run("remote", "use", host.URL)
	run("add", "host", "http://localhost:2")
	run("add", "extra", "http://localhost:3", "--local")
	local, err := config.Load(path)
	if err != nil || !local.Has("local") || !local.Has("extra") || local.Has("host") {
		t.Fatalf("local config: %v %v", local, err)
	}
	hosted, err := config.Load(hostPath)
	if err != nil || !hosted.Has("host") || hosted.Has("local") {
		t.Fatalf("host config: %v %v", hosted, err)
	}
	run("search")
	run("remove", "host")
	hosted, _ = config.Load(hostPath)
	if len(hosted) != 0 {
		t.Fatal("remove did not route remote")
	}
	if output := run("list", "--json"); strings.Contains(output, "local") || strings.Contains(output, "extra") {
		t.Fatalf("local merged: %s", output)
	}
	run("remote", "off")
	cfg, err := config.LoadRemote(path)
	if err != nil || cfg != nil {
		t.Fatal("remote off failed")
	}
	local, _ = config.Load(path)
	if len(local) != 2 {
		t.Fatal("off discarded local servers")
	}
}

// TestRemoteMissingTokenFailsClosed ensures configured relays cannot silently use local servers.
func TestRemoteMissingTokenFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.json")
	t.Setenv("TAP_CONFIG", path)
	t.Setenv("TAP_REMOTE_TOKEN", "")
	if err := config.SetRemote(path, &config.Remote{URL: "http://localhost:7777", TokenEnv: "TAP_REMOTE_TOKEN"}); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"list"}, "test", &out, &stderr); code == 0 || out.Len() != 0 {
		t.Fatalf("fell back: %d %s", code, out.String())
	}
}
