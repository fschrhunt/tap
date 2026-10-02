package tap_test

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRemoteRelaysShareConnectors exercises the shipped CLI and two existing stdio agents end to end.
func TestRemoteRelaysShareConnectors(t *testing.T) {
	t.Setenv("TAP_REMOTE_TOKEN", "offline-relay-test-token")
	t.Setenv("TAP_REMOTE_ADMIN_TOKEN", "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	remote := sandbox(t)
	cmd := exec.Command(tapBin, "remote", "serve", "--addr", addr)
	cmd.Env = append(os.Environ(), "TAP_CONFIG="+remote.config, "TAP_DEADLINE_MS=500")
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("remote did not shut down")
		}
	})
	url := "http://" + addr
	client := &http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get(url + "/mcp")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusUnauthorized {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote did not start: %s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	local := sandbox(t)
	output(t, local.run("remote", "use", url))
	root := local.read().(map[string]any)
	a, b := startRoot(t, root), startRoot(t, root)
	a.initialize()
	b.initialize()
	equal(t, a.search(nil)["integrations"], []any{})
	equal(t, b.search(nil)["integrations"], []any{})
	trace := filepath.Join(t.TempDir(), "connector-trace")
	output(t, local.run("add", "shared", "--", fixtureBin, "--serve", "--trace", trace, "--result-meta"))
	for _, h := range []*harness{a, b} {
		equal(t, h.search(map[string]any{"query": "echo"})["total"], float64(1))
		r := h.request("tools/call", map[string]any{"name": "plugin_call", "arguments": map[string]any{"tool": "shared.data"}})
		equal(t, r["_meta"], map[string]any{"trace": "fixture-result"})
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, strings.Count(string(data), "initialize\n"), 1)
	output(t, local.run("remove", "shared"))
	r := a.request("tools/call", map[string]any{"name": "plugin_call", "arguments": map[string]any{"tool": "shared.data"}})
	equal(t, r["isError"], true)
}
