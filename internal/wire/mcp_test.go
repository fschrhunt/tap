package wire

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// capturePeer returns controlled IO outcomes without an SDK session.
type capturePeer struct {
	writeErr error
	response jsonrpc.Message
	readErr  error
}

func (p *capturePeer) Write(context.Context, jsonrpc.Message) error  { return p.writeErr }
func (p *capturePeer) Read(context.Context) (jsonrpc.Message, error) { return p.response, p.readErr }
func (p *capturePeer) Close() error                                  { return nil }
func (p *capturePeer) SessionID() string                             { return "" }

// TestCaptureFailedWriteReleasesSlot pins cleanup when the request never reaches its peer.
func TestCaptureFailedWriteReleasesSlot(t *testing.T) {
	base := &capturePeer{writeErr: errors.New("write failed")}
	c := &capturingConnection{Connection: base, pending: map[jsonrpc.ID]*pendingCapture{}}
	ctx, _ := Capture(context.Background())
	id, _ := jsonrpc.MakeID(float64(1))
	req := &jsonrpc.Request{ID: id, Method: "tools/list"}
	if err := c.Write(ctx, req); err == nil {
		t.Fatal("expected write failure")
	}
	if len(c.pending) != 0 {
		t.Fatal("failed write retained slot")
	}
}

// TestCaptureCancellationReleasesSlot pins cleanup when a peer never responds.
func TestCaptureCancellationReleasesSlot(t *testing.T) {
	c := &capturingConnection{Connection: &capturePeer{}, pending: map[jsonrpc.ID]*pendingCapture{}}
	ctx, cancel := context.WithCancel(context.Background())
	ctx, raw := Capture(ctx)
	id, _ := jsonrpc.MakeID(float64(1))
	req := &jsonrpc.Request{ID: id, Method: "tools/list"}
	if err := c.Write(ctx, req); err != nil {
		t.Fatal(err)
	}
	cancel()
	until := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		n := len(c.pending)
		c.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(until) {
			t.Fatal("cancelled call retained slot")
		}
		time.Sleep(time.Millisecond)
	}
	// A late response must not write into the abandoned slot.
	c.Connection = &capturePeer{response: &jsonrpc.Response{ID: req.ID, Result: []byte(`{"tools":[]}`)}}
	if _, err := c.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*raw) != 0 {
		t.Fatal("late response filled cancelled capture")
	}
}

// TestCaptureResponseAndShutdown pins normal capture and terminal cleanup.
func TestCaptureResponseAndShutdown(t *testing.T) {
	for _, ending := range []string{"response", "read error", "close"} {
		t.Run(ending, func(t *testing.T) {
			base := &capturePeer{}
			c := &capturingConnection{Connection: base, pending: map[jsonrpc.ID]*pendingCapture{}}
			ctx, raw := Capture(context.Background())
			id, _ := jsonrpc.MakeID(float64(1))
			req := &jsonrpc.Request{ID: id, Method: "tools/list"}
			if err := c.Write(ctx, req); err != nil {
				t.Fatal(err)
			}
			switch ending {
			case "response":
				base.response = &jsonrpc.Response{ID: req.ID, Result: []byte(`{"tools":[]}`)}
				if _, err := c.Read(ctx); err != nil {
					t.Fatal(err)
				}
				if string(*raw) != `{"tools":[]}` {
					t.Fatalf("raw=%s", *raw)
				}
			case "read error":
				base.readErr = errors.New("closed")
				if _, err := c.Read(ctx); err == nil {
					t.Fatal("expected read failure")
				}
			case "close":
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if len(c.pending) != 0 {
				t.Fatal("terminal event retained capture")
			}
		})
	}
}

// TestTakeLeavesTheSDKAnEmptyResult pins that a taken response reaches the slot whole while
// the SDK is given nothing to decode, and that an error response is passed on as it came.
func TestTakeLeavesTheSDKAnEmptyResult(t *testing.T) {
	base := &capturePeer{}
	c := &capturingConnection{Connection: base, pending: map[jsonrpc.ID]*pendingCapture{}}
	for i, tc := range []struct {
		response jsonrpc.Response
		raw, sdk string
	}{
		{jsonrpc.Response{Result: []byte(`{"tools":[{"name":"a"}]}`)}, `{"tools":[{"name":"a"}]}`, `{}`},
		{jsonrpc.Response{Error: errors.New("refused")}, ``, ``},
	} {
		ctx, raw := Take(context.Background())
		id, _ := jsonrpc.MakeID(float64(i))
		if err := c.Write(ctx, &jsonrpc.Request{ID: id, Method: "tools/list"}); err != nil {
			t.Fatal(err)
		}
		tc.response.ID = id
		base.response = &tc.response
		msg, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := msg.(*jsonrpc.Response); string(*raw) != tc.raw || string(got.Result) != tc.sdk || got.Error != tc.response.Error {
			t.Errorf("slot %q, SDK result %q, error %v; want %q and %q", *raw, got.Result, got.Error, tc.raw, tc.sdk)
		}
	}
}
