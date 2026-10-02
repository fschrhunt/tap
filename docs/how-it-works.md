# How it works

tap exposes two MCP tools, with no model or embedding service inside the gateway. Local
initialization includes a bounded integration-name overview without starting downstream servers.

## Discover, browse, inspect

`plugin_search` supports capability search, server browsing, and exact inspection:

```json
{"query":"create github issue"}
{"server":"github","detail":"summary","limit":8}
{"ids":["github.create_issue"],"detail":"full"}
```

Without query/server/ids it lists integrations and upstream tool counts. Browsing and search
respect allow/deny policy. Inspection only opens servers owning the requested ids. A query
mentioning exactly one configured provider also scopes discovery there; ambiguous mentions do not.

Ranking uses field-weighted BM25 over identifiers, providers, titles, descriptions and parameter
names/descriptions/enum labels. Unicode tokenization, camelCase splitting, conservative English
plural normalization, query filler removal and unique one-edit typo recovery reduce missed matches.
A query that is a tool's name or id, word for word, returns the tools so named. Otherwise the
tools holding every word of the query are returned, and when none does, the tools holding any.
No index is built: each query reads the catalogs it is given, which takes under a millisecond for
hundreds of tools and leaves nothing to prepare before the first search. Scores are not
probabilities. Lexical retrieval cannot reliably infer semantic-only requests such as “notify the
team”; browse or refine instead.

`detail` defaults to `auto`: include complete schemas when they fit `maxBytes` (default 32768),
otherwise return labeled summaries with `schemaLoaded: false`. Inspect those ids with `detail: full`
before calling. Full schemas are never truncated. Summaries omit schemas and clip long descriptions
with `textTruncated: true`. Full views preserve output schemas and label initialization guidance as
`untrusted_server_content`. Descriptions/guidance/results are data, not overriding instructions.

`limit` defaults to 8 (1–25 over MCP); `offset` defaults to 0. Follow `nextOffset` for more results.
Byte budgets are not token estimates; increase `maxBytes` up to 16777216 or scope discovery when
metadata cannot fit. `ids` and `query` are mutually exclusive. The CLI defaults to full disclosure.

## Connections and metadata

- **Lazy startup:** initialization and tools/list open no downstream connections. While it
  answers them, a serving tap reads its saved index, so the first search does not wait for it,
  and starts only servers whose [start setting](settings.md#when-servers-start) is `start`.
  By default (`call`) a search answers from saved tools without starting their servers; a
  server tap has never listed is started once to list it. With `search`, scoped discovery
  contacts no unrelated providers. Unscoped cold discovery contacts all selected providers, with
  at most eight simultaneous connection/discovery operations. Calls open only their target.
- **Budgets/backoff:** connection and paginated listing share a per-server deadline. Failed servers
  back off rather than blocking every subsequent search. Tool execution itself has no fixed
  five-second timeout; caller cancellation and shutdown still end work. No tool call is replayed.
- **Resilient catalogs:** fresh memory catalogs last one minute. Expired and restored disk catalogs
  remain searchable with `stale: true`; a running server, or with `start: search` any server,
  refreshes them in one shared background refresh. A failed refresh
  reports diagnostics without discarding last-known tools. Disk catalogs always revalidate.
  `refresh: true` waits for live discovery. Cached catalogs are not availability promises.
- **Observability:** integration rows and `catalogs` report `source`, `observedAt`, and `availability`.
  Cached rows use `availability: not_checked`; an unobserved timestamp may be zero for restored
  indexes. Stale matches remain labeled even when a refresh has failed.
- **Pagination:** tools/list follows at most 100 pages / 16 MiB; repeated cursors and duplicate or
  empty tool names are refused. Tool-change notifications invalidate memory and queue index updates.
- **Unchanged lists:** each catalog carries a SHA-256 digest of what its server answered. A
  refresh that gets the same answer keeps the tools already decoded and writes nothing.
- **Config changes:** each operation snapshots definitions and fingerprints together. Changed or
  removed definitions retire their old sessions/catalogs; removed servers cannot still be called.
  tap reads the config again whenever the file's size or modification time has changed, or it
  was modified in the last two seconds.
- **Persistence:** the private index is `<config path>.tools.json` (mode 0600), using symlink-safe
  atomic replacement and coalesced writes outside discovery. It is JSON with one line per tool,
  which lets tap find and decode the tools side by side. Corrupt, public, oversized,
  definition-mismatched or differently laid out indexes are ignored. Removing it clears saved metadata, not connectors.
  `TAP_CACHE_DIR=off` disables persistence. Unwritable index directories do not prevent tool use.
  Fingerprints hash expanded connection inputs/credentials; stdio also includes working directory
  and inherited environment. Connection definitions and credentials are not stored in the index.
  Schemas/guidance may still contain sensitive metadata.
- **Idle sessions:** unused downstream sessions close after five minutes. A stdio server may override it
  with `idleTimeoutMs` (1–86400000). Active operations are never idle-stopped; later calls restart
  the backend. Process-local state is lost. Set `TAP_IDLE_TTL_MS` appropriately for stateful servers.
- **Shutdown:** EOF closes sessions, waits for workers and flushes queued index writes. References
  are memory-only and disappear when the hosting process ends.

Deadlines, backoff, idle shutdown, start and references are [settings](settings.md), read when a
tap process starts; their environment variables (`TAP_DEADLINE_MS`, `TAP_FAIL_TTL_MS`,
`TAP_IDLE_TTL_MS`, `TAP_REFERENCES`) win over the config.

HTTP sessions remain unwrapped so SDK protocol headers and idle notifications work. Stdio retains
raw capture, and reads tool lists from it alone rather than decoding them twice. Application request/result metadata is forwarded; protocol identity and progress
tokens belong to each hop and are never blindly copied between hops.

## Validate and call

```json
{"tool":"github.create_issue","arguments":{"owner":"acme","repo":"app","title":"Login fails"}}
```

User-configured tool policy is checked before connecting. Calls require a successful catalog from
the live calling session, not restored/stale descriptors. Complete input schemas are validated
locally before tools/call: draft-07/2020-12 and local references work; remote schema fetching is
disabled. Unsupported/malformed schemas fail closed. Tap does not coerce values, apply defaults or
guess repairs. Common violations identify JSON Pointer fields without echoing argument values;
complex violations request full-schema inspection. Backend semantic validity remains its responsibility.

Gateway errors carry `structuredContent.code`, `message`, and `recovery`: `server_unavailable`,
`catalog_unavailable`, `unknown_tool`, `invalid_arguments`, `schema_unavailable`, `permission_denied`,
and `stale_schema` distinguish pre-call failures. A protocol/transport error after sending a call
is `call_outcome_unknown`: a write may have happened. Verify external state before retrying.
Tap **never automatically retries calls**, regardless of advertised idempotence.

Default `resultMode: inline` forwards content, `_meta`, error flags and structured content through
the SDK. Invalid explicit-null structured content is dropped. Backend tool-reported errors are
preserved, not reclassified. Stdio preserves raw numeric spelling; HTTP retains SDK-supported fields.

## Opt-in result references

References are off unless the `references` [setting](settings.md) is on. Off, `plugin_call` offers
only `tool` and `arguments`, which keeps the two definitions an agent loads near 300 tokens, and
a call that uses a reference field is refused with `reference_unavailable` before anything is
sent. On, `plugin_call` also describes the fields below.

```json
{"tool":"db.query","arguments":{"sql":"SELECT * FROM orders"},"resultMode":"reference"}
{"operation":"inspect","reference":"REFERENCE","pointer":"/structuredContent/rows","offset":0,"limit":5}
{"operation":"inspect","reference":"REFERENCE","pointer":"/content/0/text"}
{"operation":"drop","reference":"REFERENCE"}
```

Reference mode returns a receipt (opaque reference, byte count, expiry, content types, original
error flag), not an invented summary. Stdio retains complete raw result JSON, including images,
error content and numeric precision; HTTP retains the SDK-decoded result. References are bound
to the actual MCP session, including through authenticated remote relays; stateless HTTP callers
must establish a session before using references. Another session cannot
inspect, drop or copy them even if it learns an id. Memory limits apply across the hosting engine.

Pointers follow RFC 6901 (`~1` for `/`, `~0` for `~`). Empty pointer selects the result root.
Arrays page by index; objects page by sorted property name. Default limit is 100 (1–1000), default
inspection budget is 32768 bytes. Oversized selections fail without truncation: choose a deeper
pointer, smaller page or larger budget. Missing properties differ from JSON null.

Explicit argument copies avoid model recitation:

```json
{"tool":"crm.import_records","arguments":{},"argumentRefs":[{"target":"/records","reference":"REFERENCE","pointer":"/structuredContent/rows"}]}
```

Targets cannot replace the argument root or overlap. Parent objects/arrays must exist; a final
object property may be added. Ordinary argument markers are never implicitly interpreted. Copied
values are schema-validated. Cross-server copies require the **source** server's `policy.referenceTo`
grant; same-server copies remain subject to destination tool policy. See [Servers](servers.md).

References expire after ten minutes and retain at most 128 entries / 32 MiB, evicting oldest entries.
Results over 8 MiB fall back inline with an `_meta.tap` warning without changing execution success.
Never repeat a write merely to recreate an expired reference. Inline remains the default because
references add inspection turns and do not benefit every workload.

## Remote mode

A [remote](remote.md) shares one registry, index and downstream sessions across agents/machines.
Harnesses still launch tap over stdio and see two tools. The local relay connects lazily; connector
credentials stay on the host. Additions need no harness restart. Discovery, inspection, refresh,
validation and result operations use the remote backend, not a merged local/remote namespace.
Local definitions remain available after remote off or CLI `--local`. Remote failures never silently
fall back to local tools. Shared upstream credentials/sessions are not a multi-tenant sandbox.

tap uses Go and the official MCP SDK; it embeds neither LazyMCP nor an LLM/vector database/script
runtime. It proxies tools, not every MCP feature (resources/prompts/sampling/elicitation).
