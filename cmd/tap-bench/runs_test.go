package main

import (
	"github.com/fschrhunt/tap/internal/discovery"
	"github.com/fschrhunt/tap/internal/wire"
	"testing"
)

// TestCostPerSuccess includes money spent on failures rather than hiding unsuccessful attempts.
func TestCostPerSuccess(t *testing.T) {
	yes, no := true, false
	tokens := int64(10)
	cost, latency := 0.1, 100.0
	r := taskRun{System: "tap", Model: "pinned-model", Settings: "fixed", Task: "task", Environment: "snapshot", Budget: "fixed", Verified: true, Success: &yes, InputTokens: &tokens, OutputTokens: &tokens, CostUSD: &cost, LatencyMS: &latency}
	failure := r
	failure.Trial = 1
	failure.Success = &no
	out, err := evaluateRuns([]taskRun{r, failure})
	if err != nil {
		t.Fatal(err)
	}
	report := out["reports"].([]any)[0].(map[string]any)
	if report["cost_per_success_usd"] != 0.2 {
		t.Fatalf("failed costs excluded: %v", report)
	}
}

// TestMatchedRuns rejects comparisons whose systems were tested under different budgets.
func TestMatchedRuns(t *testing.T) {
	yes := true
	tokens := int64(10)
	cost, latency := 0.1, 100.0
	r := taskRun{System: "tap", Model: "pinned", Settings: "fixed", Task: "task", Environment: "snapshot", Budget: "fixed", Verified: true, Success: &yes, InputTokens: &tokens, OutputTokens: &tokens, CostUSD: &cost, LatencyMS: &latency}
	other := r
	other.System = "competitor"
	other.Budget = "larger"
	if _, err := evaluateRuns([]taskRun{r, other}); err == nil {
		t.Fatal("unmatched budgets accepted")
	}
}

// TestLegacyMissingMetadata freezes absent title/description behavior rather than coercing null.
func TestLegacyMissingMetadata(t *testing.T) {
	tools := []discovery.Tool{{ID: "files.read", Server: "files", Definition: wire.Object{{Name: "name", Value: "read"}}}}
	if got := legacy(tools, "null"); len(got) != 0 {
		t.Fatalf("absent metadata became searchable: %v", got)
	}
}
