# Evaluating tap

Optimize **correct task completion per dollar and per second**, not just the initial tool catalog.
These tools run locally without API keys or paid model calls. LazyMCP is not a tap dependency;
competitors should run separately as pinned benchmark baselines.

## Retrieval

```sh
go run ./cmd/bench --method legacy
go run ./cmd/bench --method tap
go run ./cmd/bench --input heldout.json --rankings rankings.json
go test ./internal/discovery -run '^$' -bench . -benchmem
```

`corpus.json` freezes 20 illustrative tools, 24 positive queries and two absent-capability queries.
It includes provider collisions, filler, schema-only vocabulary, spelling, Spanish text and
semantic-only cases. It is a **development dataset**, not an independent test or competitor
leaderboard. Do not tune against an external held-out corpus and then call it held out.

Tools have `id`, `server` and a complete MCP `definition`; queries have `query` and `accept`, a
list of acceptable ids. An empty accept list means no capability should be returned. An alternative
solution is not a failure merely because it uses a different valid tool.

Recorded rankings are a JSON object mapping each exact query to its ordered tool ids. Missing
queries, duplicate ids and unknown ids are rejected. The comparator reports recall@1/5/8, mean
reciprocal rank, absent-capability correctness and full top-eight definition **bytes**. Bytes are
not token counts, and definition bytes alone do not include disclosure/agent-loop overhead.
Recorded rankings have no invented latency measurements. Native retrieval timing includes index
construction; `BenchmarkWarmSearch` measures immutable-index reuse separately.

The initial development baseline had 62.5% recall@1/5/8. Field-weighted search reached 95.8% on this
same set (23/24), while preserving the two absent cases. It still misses “notify the team”: no
semantic model is present. These figures demonstrate a local regression baseline, **not** a win
against LazyMCP, ToolHive, MCPProxy, Speakeasy, native tool search, or Code Mode.

For larger evaluation, collect pinned authentic MCP definitions and independently labeled queries.
[MCP-Zero](https://github.com/xfey/MCP-Zero) offers a larger discovery dataset;
[MCP-Atlas](https://github.com/scaleapi/mcp-atlas) and
[MCP-Bench](https://github.com/Accenture/mcp-bench) offer real task suites. Check licenses, schema
differences, live-service drift and judge limitations before adapting them. Synthetic queries
generated from descriptions alone tend to overstate retrieval quality.

## Agent task runs

Run the same agent/model against direct tools, current tap, original tap, hierarchical discovery,
hybrid retrieval and Code Mode where their execution semantics permit a fair comparison. Use the
same user task, environment snapshot, tool catalogs, model settings and total resource budgets.
Pin versions, randomize system ordering, restore task state, repeat trials and verify outcomes.
Use deterministic state verifiers for writes; a claimed successful answer is not sufficient.

The external harness must record **actual** provider token usage and cost, including cached token
pricing and failed attempts. It must also include real agent-loop latency, searches, retries,
backend starts and process-tree RSS (including children). This repository does not ship a provider
agent harness or independently judge those outcomes.

Evaluate recorded runs without spending further credits:

```sh
go run ./cmd/bench --runs runs.json
```

The file is an array of records:

```json
[
  {
    "system":"tap-pinned-revision",
    "model":"provider/pinned-model-id",
    "model_settings":"temperature=0;reasoning=fixed;prompt=HASH",
    "task":"task-001",
    "trial":0,
    "environment":"fixture-snapshot-HASH",
    "budget":"calls=40;seconds=120;tokens=20000",
    "verified":true,
    "success":true,
    "input_tokens":1234,
    "output_tokens":321,
    "cost_usd":0.01,
    "latency_ms":1200,
    "tool_calls":4,
    "searches":1,
    "retries":0,
    "backend_starts":1,
    "process_tree_rss_bytes":20000000
  }
]
```

These values are an example, **not measurements**. `verified` is the external verifier's assertion,
not validation performed by bench. Missing outcomes/usage, negative measurements, duplicate
runs and unmatched task/trial/environment/budget/model settings across systems are rejected.
Reports are grouped by system/model. Cost per success includes money spent on unsuccessful runs;
if none succeeds it is null, not a misleading zero. Latency is reported across successes and
failures, so timeout policies must be matched.

Publish traces and verifier evidence, failures by category, pinned versions, sample size and
uncertainty alongside any claims. Bootstrap paired differences by task rather than treating
repeated trials as independent examples. No confidence interval or winner is inferred by this
simple evaluator. Do not hide regressions on small catalogs behind a large-catalog average.

## Runtime and failure coverage

```sh
go test ./...
TAP_TEST_RACE=1 go test -race ./...
go vet ./...
gofmt -l .
```

Tests use temporary `TAP_CONFIG` and cache paths, local stdio/HTTP fixtures and no personal servers.
`TAP_TEST_RACE=1` also race-builds the gateway and fixture subprocesses, not only the test harness.
Black-box tests cover cold scoping, cross-process metadata reuse, credential changes, stale
metadata, notifications, pagination, config replacement, idle restart, concurrency, local schema
rejection, denied operations, unknown write outcomes and reference transfers. Unit tests protect
numeric precision, expiry and retention overflow.

Still required before a broad competitive claim: real multi-model agent runs, independent
large-catalog retrieval, calibrated absent-tool tests, adversarial metadata and permission tests,
whole-process-tree startup/memory measurements, and matched Code Mode workflows. Embedding-based
retrieval, arbitrary code execution and OAuth are not implemented by this change; their inclusion
should be justified by these measurements rather than feature parity alone.
