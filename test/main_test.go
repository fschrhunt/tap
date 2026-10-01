// Package tap_test exercises the built CLI and MCP surface with offline peers
// and temporary registries, without calling gateway implementation functions.
package tap_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	// Track implementation changes in Go's test cache; behavior is tested only via subprocesses.
	_ "github.com/fschrhunt/tap/internal/cli"
)

var tapBin, fixtureBin string

// TestMain builds the gateway and fixture once for the entire black-box suite.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tap-test-bins-")
	if err != nil {
		panic(err)
	}
	tapBin = filepath.Join(dir, "tap")
	fixtureBin = filepath.Join(dir, "fixture")
	for _, build := range [][2]string{{tapBin, "../cmd/tap"}, {fixtureBin, "./fixture"}} {
		cmd := exec.Command("go", "build", "-o", build[0], build[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build: %s\n%v\n", out, err)
			os.RemoveAll(dir)
			os.Exit(1)
		}
	}
	status := m.Run()
	os.RemoveAll(dir)
	os.Exit(status)
}

type result struct {
	status         int
	stdout, stderr string
}
type box struct {
	t      *testing.T
	config string
}

// sandbox creates a registry with no access to the caller's personal configuration.
func sandbox(t *testing.T) *box {
	t.Helper()
	return &box{t: t, config: filepath.Join(t.TempDir(), "servers.json")}
}

// run executes one CLI command with bounded runtime and separate output streams.
func (b *box) run(args ...string) result {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tapBin, args...)
	cmd.Env = append(os.Environ(), "TAP_CONFIG="+b.config, "TAP_DEADLINE_MS=500")
	var out, errout bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errout
	err := cmd.Run()
	status := 0
	if err != nil {
		var exit *exec.ExitError
		if e, ok := err.(*exec.ExitError); ok {
			exit = e
			status = exit.ExitCode()
		} else {
			b.t.Fatal(err)
		}
	}
	if ctx.Err() != nil {
		b.t.Fatal("CLI timed out")
	}
	return result{status, out.String(), errout.String()}
}

// output requires CLI success before inspecting its stdout.
func output(t *testing.T, r result) string {
	t.Helper()
	if r.status != 0 {
		t.Fatalf("exit %d: %s", r.status, r.stderr)
	}
	return strings.TrimSpace(r.stdout)
}

// decode converts JSON into plain values for semantic black-box assertions.
func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return v
}

// equal reports the complete compared values on a mismatch.
func equal(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

// contains asserts a useful substring without obscuring actual output on failure.
func contains(t *testing.T, s, part string) {
	t.Helper()
	if !strings.Contains(s, part) {
		t.Fatalf("%q does not contain %q", s, part)
	}
}

// write stores a test registry or intentionally malformed JSON.
func (b *box) write(value any) {
	b.t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		b.t.Fatal(err)
	}
	if err = os.WriteFile(b.config, data, 0600); err != nil {
		b.t.Fatal(err)
	}
}

// read decodes the config that tap wrote, including every stored key.
func (b *box) read() any {
	b.t.Helper()
	data, err := os.ReadFile(b.config)
	if err != nil {
		b.t.Fatal(err)
	}
	return decode(b.t, string(data))
}

// definition launches the fixture with the same stdio modes as the original suite.
func definition(args ...string) map[string]any {
	return map[string]any{"type": "stdio", "command": append([]string{fixtureBin, "--serve"}, args...)}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Write drains subprocess stderr safely while assertions read it concurrently.
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

// String snapshots captured stderr.
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

type harness struct {
	t       *testing.T
	config  string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	replies chan map[string]any
	done    chan error
	stderr  lockedBuffer
	id      int
}

// start negotiates an MCP session with a subprocess, retaining its stdin for EOF tests.
func start(t *testing.T, servers map[string]any) *harness {
	t.Helper()
	if servers == nil {
		servers = map[string]any{"fixture": definition()}
	}
	b := sandbox(t)
	b.write(map[string]any{"servers": servers})
	h := &harness{t: t, config: b.config, replies: make(chan map[string]any, 32), done: make(chan error, 1)}
	h.cmd = exec.Command(tapBin)
	h.cmd.Env = append(os.Environ(), "TAP_CONFIG="+h.config, "TAP_DEADLINE_MS=500")
	h.cmd.Stderr = &h.stderr
	var err error
	h.stdin, err = h.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := h.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 16*1024*1024)
		for scanner.Scan() {
			var msg map[string]any
			if json.Unmarshal(scanner.Bytes(), &msg) == nil && msg["id"] != nil {
				h.replies <- msg
			}
		}
		close(h.replies)
	}()
	go func() { h.done <- h.cmd.Wait(); close(h.done) }()
	t.Cleanup(func() {
		h.stdin.Close()
		select {
		case <-h.done:
		case <-time.After(3 * time.Second):
			h.cmd.Process.Kill()
			<-h.done
			t.Error("tap had to be killed instead of exiting")
		}
	})
	return h
}

// request correlates JSON-RPC responses and bounds each request like the Node harness.
func (h *harness) request(method string, params any) map[string]any {
	h.t.Helper()
	h.id++
	if params == nil {
		params = map[string]any{}
	}
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": h.id, "method": method, "params": params})
	if _, err := fmt.Fprintln(h.stdin, string(data)); err != nil {
		h.t.Fatal(err)
	}
	select {
	case msg, ok := <-h.replies:
		if !ok {
			h.t.Fatalf("tap exited: %s", h.stderr.String())
		}
		equal(h.t, msg["id"], float64(h.id))
		if msg["error"] != nil {
			h.t.Fatalf("%s: %v", method, msg["error"])
		}
		return msg["result"].(map[string]any)
	case <-time.After(12 * time.Second):
		h.t.Fatalf("%s timed out: %s", method, h.stderr.String())
		return nil
	}
}

// initialize performs protocol negotiation before tools calls.
func (h *harness) initialize() map[string]any {
	h.t.Helper()
	init := h.request("initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}})
	fmt.Fprintln(h.stdin, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return init
}

// search decodes tap's JSON text result and rejects tool failures.
func (h *harness) search(args map[string]any) map[string]any {
	h.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	r := h.request("tools/call", map[string]any{"name": "plugin_search", "arguments": args})
	if r["isError"] == true {
		h.t.Fatalf("search failed: %v", r)
	}
	return decode(h.t, r["content"].([]any)[0].(map[string]any)["text"].(string)).(map[string]any)
}

// call invokes a fixture tool through the public plugin_call surface.
func (h *harness) call(tool string, args map[string]any) map[string]any {
	h.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	return h.request("tools/call", map[string]any{"name": "plugin_call", "arguments": map[string]any{"tool": "fixture." + tool, "arguments": args}})
}
