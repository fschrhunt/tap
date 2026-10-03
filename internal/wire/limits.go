package wire

import (
	"fmt"
	"io"
	"mime"
	"net/http"
)

// MaxMessageBytes bounds downstream JSON bodies, stdio frames and SSE events before decoding.
const MaxMessageBytes = 16 << 20

// BoundResponse limits bodies before SDK buffering, including error responses that
// claim to be SSE. Only HTTP 200 SSE streams use the SDK's per-event MaxEventSize.
func BoundResponse(resp *http.Response, err error) (*http.Response, error) {
	if err != nil {
		return resp, err
	}
	typ, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if typ != "text/event-stream" || resp.StatusCode != http.StatusOK {
		if resp.ContentLength > MaxMessageBytes {
			resp.Body.Close()
			return nil, fmt.Errorf("downstream response exceeds 16 MiB")
		}
		resp.Body = &boundedBody{ReadCloser: resp.Body, remaining: MaxMessageBytes}
	}
	return resp, nil
}

// boundedBody returns an error rather than a successful truncated JSON document.
type boundedBody struct {
	io.ReadCloser
	remaining int
}

// Read lends at most the remaining byte budget and probes overflow without buffering it.
func (b *boundedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.remaining == 0 {
		var probe [1]byte
		n, err := b.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("downstream response exceeds 16 MiB")
		}
		return 0, err
	}
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= n
	return n, err
}
