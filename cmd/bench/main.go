// Command bench evaluates retrieval on a frozen corpus without servers or models.
// Optional recorded rankings allow competitors to be compared on the identical queries.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/fschrhunt/tap/internal/discovery"
	"github.com/fschrhunt/tap/internal/wire"
)

type corpus struct {
	Tools []struct {
		ID, Server string
		Definition json.RawMessage
	}
	Queries []struct {
		Query  string
		Accept []string
	}
}

// run evaluates either tap, its original substring search, or recorded ID lists.
func run() error {
	input := flag.String("input", "bench/corpus.json", "frozen tools and labeled queries")
	method := flag.String("method", "tap", "tap or legacy")
	rankings := flag.String("rankings", "", "JSON mapping exact queries to ordered IDs from another retriever")
	runs := flag.String("runs", "", "verified agent task-run records; reports cost per success and latency")
	flag.Parse()
	if *runs != "" {
		return reportRuns(*runs)
	}
	b, err := os.ReadFile(*input)
	if err != nil {
		return err
	}
	var c corpus
	if err = json.Unmarshal(b, &c); err != nil {
		return err
	}
	if len(c.Tools) == 0 || len(c.Queries) == 0 {
		return fmt.Errorf("corpus must contain tools and queries")
	}
	tools := make([]discovery.Tool, 0, len(c.Tools))
	known := map[string]bool{}
	for _, t := range c.Tools {
		v, err := wire.DecodeExact(t.Definition)
		if err != nil {
			return err
		}
		o, ok := v.(wire.Object)
		if !ok || t.ID == "" || known[t.ID] {
			return fmt.Errorf("invalid or duplicate tool %q", t.ID)
		}
		tools = append(tools, discovery.Tool{ID: t.ID, Server: t.Server, Definition: o})
		known[t.ID] = true
	}
	recorded := map[string][]string{}
	if *rankings != "" {
		b, err := os.ReadFile(*rankings)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(b, &recorded); err != nil {
			return err
		}
	} else if *method != "tap" && *method != "legacy" {
		return fmt.Errorf("unknown method %q", *method)
	}
	hits := [3]int{}
	positive, absent, correctAbsent, bytesTotal := 0, 0, 0, 0
	reciprocal := 0.0
	times := []float64{}
	failures := []string{}
	for _, q := range c.Queries {
		for _, id := range q.Accept {
			if !known[id] {
				return fmt.Errorf("unknown expected tool %q", id)
			}
		}
		began := time.Now()
		ids := []string{}
		if *rankings != "" {
			var ok bool
			ids, ok = recorded[q.Query]
			if !ok {
				return fmt.Errorf("missing recorded query %q", q.Query)
			}
			seen := map[string]bool{}
			for _, id := range ids {
				if !known[id] || seen[id] {
					return fmt.Errorf("invalid recorded ID %q", id)
				}
				seen[id] = true
			}
		} else if *method == "legacy" {
			ids = legacy(tools, q.Query)
		} else {
			for _, m := range discovery.Rank(tools, q.Query) {
				ids = append(ids, m.Tool.ID)
			}
		}
		times = append(times, float64(time.Since(began).Microseconds()))
		if len(q.Accept) == 0 {
			absent++
			if len(ids) == 0 {
				correctAbsent++
			} else {
				failures = append(failures, q.Query)
			}
			continue
		}
		positive++
		rank := 0
		for i, id := range ids {
			for _, want := range q.Accept {
				if id == want && rank == 0 {
					rank = i + 1
				}
			}
		}
		if rank > 0 {
			reciprocal += 1 / float64(rank)
		}
		for i, k := range []int{1, 5, 8} {
			if rank > 0 && rank <= k {
				hits[i]++
			}
		}
		if rank == 0 || rank > 8 {
			failures = append(failures, q.Query)
		}
		for i, id := range ids {
			if i >= 8 {
				break
			}
			for _, t := range tools {
				if t.ID == id {
					b, _ := wire.JSON(t.Definition, false)
					bytesTotal += len(b)
				}
			}
		}
	}
	if positive == 0 {
		return fmt.Errorf("corpus has no positive queries")
	}
	sort.Float64s(times)
	name := *method
	if *rankings != "" {
		name = "recorded"
	}
	out := map[string]any{"method": name, "tools": len(tools), "positive_queries": positive, "absent_queries": absent, "correct_absent": correctAbsent, "recall_at_1": float64(hits[0]) / float64(positive), "recall_at_5": float64(hits[1]) / float64(positive), "recall_at_8": float64(hits[2]) / float64(positive), "mrr": reciprocal / float64(positive), "mean_top8_definition_bytes": bytesTotal / positive, "failures": failures, "note": "Retrieval only; bytes are not tokens. Corpus is a development set, not an independent leaderboard."}
	if *rankings == "" {
		out["p50_retrieval_us"] = times[len(times)/2]
		out["p95_retrieval_us"] = times[(len(times)-1)*95/100]
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// legacy reproduces tap's original all-terms substring ranker for a fixed baseline.
func legacy(tools []discovery.Tool, query string) []string {
	type hit struct {
		id    string
		score int
	}
	hits := []hit{}
	for _, t := range tools {
		name, _ := t.Definition.Get("name").(string)
		title, _ := t.Definition.Get("title").(string)
		description, _ := t.Definition.Get("description").(string)
		hay := strings.ToLower(name + " " + t.Server + " " + title + " " + description)
		score := 0
		for _, term := range legacyTokens(query) {
			if !strings.Contains(hay, term) {
				score = 0
				break
			}
			score++
			for _, n := range legacyTokens(name) {
				if n == term {
					score += 2
					break
				}
			}
		}
		if score > 0 {
			hits = append(hits, hit{t.ID, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	ids := []string{}
	for _, h := range hits {
		ids = append(ids, h.id)
	}
	return ids
}

var legacyCamel = regexp.MustCompile(`([a-z0-9])([A-Z])`)
var legacyPunctuation = regexp.MustCompile(`[^a-z0-9]+`)

// legacyTokens freezes the original ASCII tokenization, independent of future tap improvements.
func legacyTokens(text string) []string {
	parts := legacyPunctuation.Split(strings.ToLower(legacyCamel.ReplaceAllString(text, "$1 $2")), -1)
	out := []string{}
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// main reports invalid datasets and evaluation failures with a nonzero exit status.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}
