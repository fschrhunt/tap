package discovery

import (
	"fmt"
	"testing"

	"github.com/fschrhunt/tap/internal/wire"
)

// TestProviderRanking keeps exact provider identity ahead of identical competing operations.
func TestProviderRanking(t *testing.T) {
	tools := []Tool{{ID: "gmail.send_message", Server: "gmail", Definition: wire.Object{{Name: "name", Value: "send_message"}, {Name: "description", Value: "Send an email message"}}}, {ID: "slack.send_message", Server: "slack", Definition: wire.Object{{Name: "name", Value: "send_message"}, {Name: "description", Value: "Send a channel message"}}}}
	ranked := Rank(tools, "please send my Slack mesage")
	if len(ranked) == 0 || ranked[0].Tool.ID != "slack.send_message" {
		t.Fatalf("wrong provider: %v", ranked)
	}
}

// TestNoSubstringMatch prevents incidental character overlap from inventing capabilities.
func TestNoSubstringMatch(t *testing.T) {
	tools := []Tool{{ID: "files.delete_file", Server: "files", Definition: wire.Object{{Name: "description", Value: "Delete a file"}}}}
	if got := Rank(tools, "let"); len(got) > 0 {
		t.Fatalf("substring produced match: %v", got)
	}
}

// BenchmarkRank measures index construction plus ranking at realistic catalog sizes.
func BenchmarkRank(b *testing.B) {
	for _, size := range []int{50, 500, 3000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			tools := make([]Tool, size)
			for i := range tools {
				tools[i] = Tool{ID: fmt.Sprintf("server%d.list_records", i), Server: fmt.Sprintf("server%d", i), Definition: wire.Object{{Name: "name", Value: "list_records"}, {Name: "description", Value: "List customer records filtered by status"}}}
			}
			b.ReportAllocs()
			for b.Loop() {
				Rank(tools, "server42 customer records")
			}
		})
	}
}

// BenchmarkWarmSearch excludes index construction to measure repeated catalog queries.
func BenchmarkWarmSearch(b *testing.B) {
	for _, size := range []int{50, 500, 3000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			tools := make([]Tool, size)
			for i := range tools {
				tools[i] = Tool{ID: fmt.Sprintf("server%d.list_records", i), Server: fmt.Sprintf("server%d", i), Definition: wire.Object{{Name: "description", Value: "List customer records filtered by status"}}}
			}
			index := New(tools)
			b.ReportAllocs()
			for b.Loop() {
				index.Search("server42 customer records")
			}
		})
	}
}
