package registry

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/fschrhunt/tap/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// commandTransport gates complete downstream lines before the SDK decodes JSON.
// The SDK owns JSON-RPC dispatch and request cancellation; wire.Transport captures responses.
type commandTransport struct {
	command *exec.Cmd
	name    string
}

// Connect preserves spawn diagnostics and closes pipes on every failed setup path.
func (t *commandTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stdout, err := t.command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := t.command.StdinPipe()
	if err != nil {
		stdout.Close()
		return nil, err
	}
	// Bound the stderr-copy wait if descendants keep inherited pipes open.
	t.command.WaitDelay = 250 * time.Millisecond
	if err = t.command.Start(); err != nil {
		stdout.Close()
		stdin.Close()
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("spawn %s ENOENT", t.name)
		}
		if os.IsPermission(err) {
			return nil, fmt.Errorf("spawn %s EACCES", t.name)
		}
		return nil, err
	}
	child := &commandPipes{cmd: t.command, stdout: stdout, stdin: stdin}
	reader := &commandReader{child: child, reader: bufio.NewReaderSize(stdout, 4096)}
	// Tap enforces the byte limit before any JSON work. The SDK receives only
	// admitted complete frames and needs no separate streaming decoder budget.
	conn, err := (&mcp.IOTransport{Reader: reader, Writer: child, MaxLineLength: -1}).Connect(ctx)
	if err != nil {
		child.Close()
	}
	return conn, err
}

// commandReader holds at most one 16 MiB raw line, including CR/LF delimiters.
// It never exposes a partial line to the SDK, even if an oversized line starts with valid JSON.
type commandReader struct {
	child  *commandPipes
	reader *bufio.Reader
	frame  []byte
	offset int
}

// Read checks size before appending each fixed-size fragment, then validates only
// an accepted complete line. Validation prevents JSON from spanning multiple bounded lines.
func (r *commandReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.offset == len(r.frame) {
		r.frame = r.frame[:0]
		r.offset = 0
		for {
			part, err := r.reader.ReadSlice('\n')
			if len(part) > wire.MaxMessageBytes-len(r.frame) {
				r.frame = nil
				return 0, fmt.Errorf("downstream stdio frame exceeds 16 MiB")
			}
			if need := len(r.frame) + len(part); need > cap(r.frame) {
				grown := make([]byte, len(r.frame), min(wire.MaxMessageBytes, max(need, 2*cap(r.frame))))
				copy(grown, r.frame)
				r.frame = grown
			}
			r.frame = append(r.frame, part...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return 0, err
			}
			if len(r.frame) == 0 {
				return 0, io.EOF
			}
			if !json.Valid(r.frame) {
				return 0, fmt.Errorf("downstream stdio frame must contain one complete JSON value")
			}
			break
		}
	}
	n := copy(p, r.frame[r.offset:])
	r.offset += n
	return n, nil
}

// Close unblocks raw reads and shuts down the same child as the SDK's writer.
func (r *commandReader) Close() error { return r.child.Close() }

// commandPipes owns child shutdown once, shared by the SDK reader and writer.
type commandPipes struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stdin  io.WriteCloser
	once   sync.Once
	err    error
}

// Write sends SDK-framed requests to the child without changing cancellation messages.
func (p *commandPipes) Write(b []byte) (int, error) { return p.stdin.Write(b) }

// Close closes stdin, unblocks stdout reads, then waits 250 ms before SIGTERM and
// another 250 ms before SIGKILL. One Wait owns reaping; a final 250 ms timeout
// reports an unresponsive process without blocking connection shutdown.
func (p *commandPipes) Close() error {
	p.once.Do(func() {
		_ = p.stdin.Close()
		_ = p.stdout.Close()
		done := make(chan error, 1)
		go func() { done <- p.cmd.Wait() }()
		for _, signal := range []os.Signal{syscall.SIGTERM, syscall.SIGKILL} {
			timer := time.NewTimer(250 * time.Millisecond)
			select {
			case p.err = <-done:
				timer.Stop()
				return
			case <-timer.C:
				_ = p.cmd.Process.Signal(signal)
			}
		}
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case p.err = <-done:
		case <-timer.C:
			p.err = fmt.Errorf("unresponsive subprocess")
		}
	})
	return p.err
}
