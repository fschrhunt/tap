package wire

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Result carries an already-shaped JSON result through SDK middleware unchanged.
type Result struct {
	mcp.ResultBase
	Value any
}

// MarshalJSON preserves the result's field order and explicit false/null values.
func (r *Result) MarshalJSON() ([]byte, error) { return JSON(r.Value, false) }

type captureKey struct{}

// Capture gives one SDK request a slot for its unmodified response JSON.
func Capture(ctx context.Context) (context.Context, *json.RawMessage) {
	raw := new(json.RawMessage)
	return context.WithValue(ctx, captureKey{}, raw), raw
}

// Transport decorates an SDK transport to retain raw responses alongside decoding.
// It leaves framing, initialization, cancellation and shutdown to the SDK.
type Transport struct{ Base mcp.Transport }

// Connect opens the underlying transport and wraps its connection.
func (t *Transport) Connect(ctx context.Context) (mcp.Connection, error) {
	c, err := t.Base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &capturingConnection{Connection: c, pending: map[jsonrpc.ID]*json.RawMessage{}}, nil
}

type capturingConnection struct {
	mcp.Connection
	mu      sync.Mutex
	pending map[jsonrpc.ID]*json.RawMessage
}

// Write associates outgoing SDK requests with their response slots.
func (c *capturingConnection) Write(ctx context.Context, msg jsonrpc.Message) error {
	if req, ok := msg.(*jsonrpc.Request); ok && req.IsCall() {
		if slot, ok := ctx.Value(captureKey{}).(*json.RawMessage); ok {
			c.mu.Lock()
			c.pending[req.ID] = slot
			c.mu.Unlock()
		}
	}
	return c.Connection.Write(ctx, msg)
}

// Read retains the response before the SDK decodes typed content and schemas.
func (c *capturingConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	msg, err := c.Connection.Read(ctx)
	if res, ok := msg.(*jsonrpc.Response); ok {
		c.mu.Lock()
		if slot := c.pending[res.ID]; slot != nil {
			*slot = append(json.RawMessage(nil), res.Result...)
			delete(c.pending, res.ID)
		}
		c.mu.Unlock()
	}
	return msg, err
}
