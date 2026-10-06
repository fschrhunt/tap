package registry

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/wire"
)

// TestOverviewBounds pins count and encoded-byte budgets before names leave the registry.
func TestOverviewBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		names []string
		want  int
	}{
		{"count", func() []string {
			names := make([]string, 17)
			for i := range names {
				names[i] = fmt.Sprintf("server%d", i)
			}
			return names
		}(), 16},
		{"escaped bytes", []string{strings.Repeat("<", 171), "cloudflare"}, 0},
		{"array punctuation", func() []string {
			names := make([]string, 16)
			for i := range names {
				names[i] = fmt.Sprintf("%02d%s", i, strings.Repeat("x", 60))
			}
			return names
		}(), 15},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "servers.json")
			for _, name := range tc.names {
				if _, err := config.Put(path, name, wire.Object{{Name: "command", Value: "never-start-this"}}); err != nil {
					t.Fatal(err)
				}
			}
			e := New(path, "test")
			defer e.Close()
			overview, err := e.Overview()
			if err != nil {
				t.Fatal(err)
			}
			if len(overview.Names) != tc.want || overview.More != len(tc.names)-tc.want {
				t.Fatalf("incorrect bounded overview: %+v", overview)
			}
		})
	}
}
