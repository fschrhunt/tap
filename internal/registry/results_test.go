package registry

import (
	"strings"
	"testing"
	"time"

	"github.com/fschrhunt/tap/internal/wire"
)

// TestRetainedNumbers preserves integers beyond float64 precision during inspection and transfer.
func TestRetainedNumbers(t *testing.T) {
	var s resultStore
	result := s.retain("source", wire.Object{}, []byte(`{"content":[],"structuredContent":{"id":9007199254740993}}`))
	preview := result.Get("structuredContent").(wire.Object)
	ref := preview.Get("reference").(string)
	stored, err := s.get(ref)
	if err != nil {
		t.Fatal(err)
	}
	v, err := decodeResult(stored.raw)
	if err != nil {
		t.Fatal(err)
	}
	value, err := selectPointer(v, "/structuredContent/id")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := wire.JSON(value, false)
	if string(raw) != "9007199254740993" {
		t.Fatalf("integer changed: %s", raw)
	}
}

// TestExpiredReference releases retained bytes without reexecuting the originating call.
func TestExpiredReference(t *testing.T) {
	s := resultStore{entries: map[string]storedResult{"old": {raw: []byte(`{}`), at: time.Now().Add(-resultTTL)}}, bytes: 2}
	if _, err := s.get("old"); err == nil {
		t.Fatal("expired reference returned")
	}
	if s.bytes != 0 || len(s.entries) != 0 {
		t.Fatal("expired payload not released")
	}
}

// TestResultRetentionBound returns inline without making an already-executed write a failure.
func TestResultRetentionBound(t *testing.T) {
	var s resultStore
	result := wire.Object{{Name: "isError", Value: false}, {Name: "content", Value: []any{wire.Object{{Name: "type", Value: "text"}, {Name: "text", Value: "successful write"}}}}}
	out := s.retain("source", result, []byte(`{"large":"`+strings.Repeat("x", maxResultBytes)+`"}`))
	if out.Get("isError") != false || out.Get("content") == nil || out.Get("_meta") == nil {
		t.Fatalf("overflow changed execution outcome: %v", out)
	}
}
