package discovery

import (
	"fmt"
	"strings"
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

// TestWholeQueryBeforeAnyWord pins that tools holding every word come back alone, that any
// word is enough only when none holds them all, and that a tool's own name finds that tool
// even when its words are filler.
func TestWholeQueryBeforeAnyWord(t *testing.T) {
	tool := func(id, description string) Tool {
		server, _, _ := strings.Cut(id, ".")
		return Tool{ID: id, Server: server, Definition: wire.Object{{Name: "description", Value: description}}}
	}
	tools := []Tool{tool("github.get_me", "Details of the authenticated user"), tool("github.get_teams", "Teams of a user"),
		tool("browser.browser_click", "Click an element"), tool("browser.browser_hover", "Hover over an element in the browser")}
	ids := func(query string) string {
		out := []string{}
		for _, m := range Rank(tools, query) {
			out = append(out, m.Tool.ID)
		}
		return strings.Join(out, " ")
	}
	for query, want := range map[string]string{
		"browser click":     "browser.browser_click",
		"get me":            "github.get_me",
		"github.get_teams":  "github.get_teams",
		"click the sidebar": "browser.browser_click",
		"teams":             "github.get_teams",
	} {
		if got := ids(query); got != want {
			t.Errorf("Rank(%q) = %q, want %q", query, got, want)
		}
	}
	if got := ids("browser element"); got != "browser.browser_hover browser.browser_click" {
		t.Errorf("Rank(%q) = %q, want both browser tools, the one holding both words in more fields first", "browser element", got)
	}
}

// TestWordsAndTerms pins how text becomes the words that are indexed and asked for.
func TestWordsAndTerms(t *testing.T) {
	for input, want := range map[string]string{
		"getFileInfo":            "get file info",
		"list_allowed-dirs v2":   "list allowed dirs v2",
		"HTTPServer2Go API-post": "httpserver2 go api post",
		"  Créer  ÉTÉ":           "créer été",
		"":                       "",
	} {
		if got := strings.Join(Tokens(input), " "); got != want {
			t.Errorf("Tokens(%q) = %q, want %q", input, got, want)
		}
	}
	if got := strings.Join(terms("Please list my Repositories and the boss for issues"), " "); got != "list repository boss issue" {
		t.Errorf("terms = %q", got)
	}
}

// BenchmarkRank measures one query at realistic catalog sizes.
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
