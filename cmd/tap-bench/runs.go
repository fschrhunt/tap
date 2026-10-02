package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
)

// taskRun records measured agent outcomes; the external harness supplies a deterministic verifier.
// Costs must be actual billed/estimated provider costs including failed attempts and caching.
type taskRun struct {
	System              string   `json:"system"`
	Model               string   `json:"model"`
	Settings            string   `json:"model_settings"`
	Task                string   `json:"task"`
	Trial               int      `json:"trial"`
	Environment         string   `json:"environment"`
	Budget              string   `json:"budget"`
	Verified            bool     `json:"verified"`
	Success             *bool    `json:"success"`
	InputTokens         *int64   `json:"input_tokens"`
	OutputTokens        *int64   `json:"output_tokens"`
	CostUSD             *float64 `json:"cost_usd"`
	LatencyMS           *float64 `json:"latency_ms"`
	ToolCalls           int      `json:"tool_calls"`
	Searches            int      `json:"searches"`
	Retries             int      `json:"retries"`
	BackendStarts       int      `json:"backend_starts"`
	ProcessTreeRSSBytes int64    `json:"process_tree_rss_bytes"`
}

// reportRuns consumes recorded runs only; it never invokes a model or spends API credits.
func reportRuns(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var records []taskRun
	if err = json.Unmarshal(b, &records); err != nil {
		return err
	}
	out, err := evaluateRuns(records)
	if err != nil {
		return err
	}
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(out)
}

// evaluateRuns rejects unmatched tasks/settings and aggregates failed costs into cost per success.
func evaluateRuns(records []taskRun) (map[string]any, error) {
	if len(records) == 0 {
		return nil, fmt.Errorf("no task runs")
	}
	groups := map[string][]taskRun{}
	sets := map[string]map[string]bool{}
	models := map[string][]string{}
	for _, r := range records {
		if r.System == "" || r.Model == "" || r.Settings == "" || r.Task == "" || r.Environment == "" || r.Budget == "" || r.Trial < 0 || !r.Verified || r.Success == nil || r.InputTokens == nil || r.OutputTokens == nil || r.CostUSD == nil || r.LatencyMS == nil {
			return nil, fmt.Errorf("run must identify system/model/settings/task/trial/environment/budget, verified outcome and measured usage")
		}
		if *r.InputTokens < 0 || *r.OutputTokens < 0 || *r.CostUSD < 0 || *r.LatencyMS < 0 || math.IsNaN(*r.CostUSD) || math.IsInf(*r.CostUSD, 0) || math.IsNaN(*r.LatencyMS) || math.IsInf(*r.LatencyMS, 0) || r.ToolCalls < 0 || r.Searches < 0 || r.Retries < 0 || r.BackendStarts < 0 || r.ProcessTreeRSSBytes < 0 {
			return nil, fmt.Errorf("run contains invalid measurements")
		}
		group := r.Model + "\x00" + r.System
		keyBytes, _ := json.Marshal([]any{r.Task, r.Trial, r.Environment, r.Budget, r.Settings})
		key := string(keyBytes)
		if sets[group] == nil {
			sets[group] = map[string]bool{}
			models[r.Model] = append(models[r.Model], group)
		}
		if sets[group][key] {
			return nil, fmt.Errorf("duplicate task/trial in system %q", r.System)
		}
		sets[group][key] = true
		groups[group] = append(groups[group], r)
	}
	for model, groupNames := range models {
		reference := sets[groupNames[0]]
		for _, g := range groupNames[1:] {
			if len(sets[g]) != len(reference) {
				return nil, fmt.Errorf("unmatched task sets for model %q", model)
			}
			for k := range reference {
				if !sets[g][k] {
					return nil, fmt.Errorf("task, trial, environment, budget or model settings differ for model %q", model)
				}
			}
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	reports := []any{}
	for _, k := range keys {
		rows := groups[k]
		successes := 0
		cost := 0.0
		input, output := int64(0), int64(0)
		calls, searches, retries, starts := 0, 0, 0, 0
		peakRSS := int64(0)
		latencies := []float64{}
		for _, r := range rows {
			if *r.Success {
				successes++
			}
			cost += *r.CostUSD
			input += *r.InputTokens
			output += *r.OutputTokens
			calls += r.ToolCalls
			searches += r.Searches
			retries += r.Retries
			starts += r.BackendStarts
			peakRSS = max(peakRSS, r.ProcessTreeRSSBytes)
			latencies = append(latencies, *r.LatencyMS)
		}
		sort.Float64s(latencies)
		var costPerSuccess any
		if successes > 0 {
			costPerSuccess = cost / float64(successes)
		}
		reports = append(reports, map[string]any{"system": rows[0].System, "model": rows[0].Model, "runs": len(rows), "successes": successes, "success_rate": float64(successes) / float64(len(rows)), "total_cost_usd": cost, "cost_per_success_usd": costPerSuccess, "input_tokens": input, "output_tokens": output, "p50_latency_ms": latencies[len(latencies)/2], "p95_latency_ms": latencies[(len(latencies)-1)*95/100], "tool_calls": calls, "searches": searches, "retries": retries, "backend_starts": starts, "peak_process_tree_rss_bytes": peakRSS})
	}
	return map[string]any{"reports": reports, "note": "Measured records supplied by an external harness; tap-bench does not independently verify outcomes. Matching task/trial/environment/budget/settings is enforced. No confidence intervals or competitor winner claims are inferred."}, nil
}
