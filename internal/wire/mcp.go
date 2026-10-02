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
	return &capturingConnection{Connection: c, pending: map[jsonrpc.ID]*pendingCapture{}}, nil
}

type capturingConnection struct {
	mcp.Connection
	mu      sync.Mutex
	pending map[jsonrpc.ID]*pendingCapture
}

// pendingCapture owns a response slot and its cancellation cleanup.
type pendingCapture struct {
	raw  *json.RawMessage
	stop func() bool
}

// Write associates outgoing SDK requests with their response slots and removes
// them on failed writes or request cancellation, even if no response arrives.
func (c *capturingConnection) Write(ctx context.Context, msg jsonrpc.Message) error {
	var pending *pendingCapture
	var id jsonrpc.ID
	if req, ok := msg.(*jsonrpc.Request); ok && req.IsCall() {
		if raw, ok := ctx.Value(captureKey{}).(*json.RawMessage); ok {
			id = req.ID
			pending = &pendingCapture{raw: raw}
			c.mu.Lock()
			c.pending[id] = pending
			pending.stop = context.AfterFunc(ctx, func() {
				c.mu.Lock()
				if c.pending[id] == pending {
					delete(c.pending, id)
				}
				c.mu.Unlock()
			})
			c.mu.Unlock()
		}
	}
	err := c.Connection.Write(ctx, msg)
	if err != nil && pending != nil {
		c.mu.Lock()
		if c.pending[id] == pending {
			delete(c.pending, id)
		}
		pending.stop()
		c.mu.Unlock()
	}
	return err
}

// Read retains the response before the SDK decodes typed content and schemas.
func (c *capturingConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	msg, err := c.Connection.Read(ctx)
	if res, ok := msg.(*jsonrpc.Response); ok {
		c.mu.Lock()
		if slot := c.pending[res.ID]; slot != nil {
			*slot.raw = append(json.RawMessage(nil), res.Result...)
			slot.stop()
			delete(c.pending, res.ID)
		}
		c.mu.Unlock()
	}
	if err != nil {
		c.clearPending()
	}
	return msg, err
}

// clearPending releases response slots when the connection ends.
func (c *capturingConnection) clearPending() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, slot := range c.pending {
		slot.stop()
		delete(c.pending, id)
	}
}

// Close releases captures even when the peer never completes outstanding calls.
func (c *capturingConnection) Close() error {
	c.clearPending()
	return c.Connection.Close()
}
