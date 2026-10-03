package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fschrhunt/tap/internal/config"
)

// TestLegacyRemoteFailsClosed requires users to pair devices instead of reusing shared tokens.
func TestLegacyRemoteFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.json")
	t.Setenv("TAP_CONFIG", path)
	t.Setenv("TAP_REMOTE_TOKEN", "legacy-secret")
	if err := config.SetRemote(path, &config.Remote{URL: "http://localhost:7777", TokenEnv: "TAP_REMOTE_TOKEN"}); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"list"}, "test", &out, &stderr); code == 0 || out.Len() != 0 || !strings.Contains(stderr.String(), "legacy token remotes are no longer supported") {
		t.Fatalf("fell back: %d %s", code, out.String())
	}
}
