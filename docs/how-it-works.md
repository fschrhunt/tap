# How it works

tap exposes two MCP tools, with no model or embedding service inside the gateway. Initialization
includes a bounded overview of configured integration names without starting their servers.

## Discover, browse, inspect

`plugin_search` supports three ways to find a capability:

```json
{"query":"create github issue"}
{"server":"github","detail":"summary","limit":8}
{"ids":["github.create_issue"],"detail":"full"}
```

Without a query, server or ids, it lists integrations and upstream tool counts. Browsing a server
returns its tools, respecting its configured allow/deny policy. Exact inspection only opens the
servers owning those ids. A query mentioning exactly one configured server name also scopes the
search to that server; ambiguous provider mentions search across integrations instead.

Search uses field-weighted BM25 over identifiers, providers, titles, descriptions, and parameter
names/descriptions/enum labels. It normalizes camelCase, punctuation, Unicode words, common English
query filler and conservative English plurals. A unique one-edit spelling correction is allowed
only for unknown words of at least five characters. An immutable index is reused until the scoped
catalog changes. Extra query words no longer eliminate every candidate. Ranking scores are not
probabilities, and lexical matching cannot reliably infer semantic-only requests such as
“notify the team” when a tool only documents “send message”. Browse or refine the query instead.

`detail` is `auto` by default: complete schemas are returned when they fit `maxBytes` (default
32768). Otherwise the tool is a labeled summary with `schemaLoaded: false`; inspect its id with
`detail: full` before calling. Full schemas are never truncated. Summaries omit schemas and clip
long textual descriptions with `textTruncated: true`. Full views preserve `outputSchema` when
published, and label initialization guidance as `untrusted_server_content`.

`limit` defaults to 8 (1–25 over MCP); `offset` defaults to 0. Follow `nextOffset` for more matches.
The byte budget is not a token estimate. Increase it up to 16777216, use summary detail, or scope
discovery if metadata will not fit. `ids` and `query` are mutually exclusive.

## Connections and metadata

- **Lazy:** fresh cached metadata can be searched without launching a backend, including across
  tap process restarts. Unknown/expired catalogs require live discovery. Unscoped cold discovery
  still contacts all selected servers; scoped search does not contact unrelated ones.
- **Deadline:** connecting and listing all catalog pages share a 5-second per-server budget.
  `TAP_DEADLINE_MS` overrides it. Failed connections are retried on a later request, not in a loop.
  Tools/list is limited to 100 pages and 16 MiB; repeated cursors and duplicate names are refused.
- **Freshness:** catalogs expire after one minute. `refresh: true` explicitly refreshes the selected
  servers. `catalogs` / integration rows report `source`, `observedAt`, and `availability`.
  `source: cache` always means `availability: not_checked`, never that authentication or the server
  is currently healthy. Tool-list-change notifications invalidate both memory and disk metadata.
- **Persistent cache:** metadata is stored under the OS cache directory's `tap/` folder, with
  owner-only files and atomic replacement. `TAP_CACHE_DIR` overrides it; `off` disables persistence.
  Cache keys hash the config path, launch definition, version, working directory and inherited
  environment, including credentials; configured credentials themselves are not written into the
  metadata file. This deliberately invalidates caches on unrelated environment changes too.
  Cache I/O failure is nonfatal. Schemas and server guidance can contain sensitive metadata;
  disable persistence when necessary. Expired files are ignored, not automatically deleted.
- **Config changes:** changed launch definitions retire old connections on subsequent requests;
  removed servers cannot be called through retained catalogs.
- **Idle shutdown:** disabled unless a stdio server sets `idleTimeoutMs`. Only unused initialized
  backends are stopped; active operations are not interrupted by idle shutdown. A later call
  starts a new backend. In-memory backend state is lost; do not enable it for stateful sessions
  unless that is acceptable.
- **Shutdown:** EOF closes connections and ends the process. Stored result references are then lost.

## Validate and call

```json
{"tool":"github.create_issue","arguments":{"owner":"acme","repo":"app","title":"Login fails"}}
```

The user-configured tool policy is checked before connecting. Tap uses a live session's advertised
schema for calls, even if discovery came from disk, and checks the complete schema locally before
sending `tools/call`. Supported schema drafts are draft-07 and 2020-12; local references work but
remote schema fetching is disabled. Unsupported or malformed schemas fail closed. Tap does not
coerce values, apply defaults or guess repairs. Common type/required/enum errors include JSON
Pointer field paths without echoing actual argument values. More complex violations request a
full-schema inspection. Semantic validity beyond the schema remains the backend's responsibility.

Gateway errors carry `structuredContent.code`, `message` and `recovery`. Authentication/connection
failures before execution are `server_unavailable`; catalog failures are `catalog_unavailable`.
`unknown_tool`, `invalid_arguments`, `schema_unavailable`, and `permission_denied` distinguish
other pre-call failures. A transport/protocol error after sending a call is
`call_outcome_unknown`: a write may have happened. Verify external state before retrying.
Tap **never automatically retries tool calls**, including tools labeled idempotent by a server.

Default `resultMode: inline` forwards downstream content, `_meta`, `isError` and structured
content through the SDK. As before, invalid explicit-null structured content is dropped by the
SDK. Backend tool-reported errors are preserved rather than reclassified as gateway failures.

## Opt-in result references

Large results can stay outside the model context without introducing arbitrary code execution:

```json
{"tool":"db.query","arguments":{"sql":"SELECT * FROM orders"},"resultMode":"reference"}
```

The response contains an opaque reference, byte count, expiry, content types and original error
flag. It is a receipt, not a summary of the result. The full raw downstream JSON is retained in
memory, including images, error content and numeric precision. Read a selected value or page:

```json
{"operation":"inspect","reference":"REFERENCE","pointer":"/structuredContent/rows","offset":0,"limit":5}
{"operation":"inspect","reference":"REFERENCE","pointer":"/content/0/text"}
{"operation":"drop","reference":"REFERENCE"}
```

Pointers follow RFC 6901 (`~1` for `/`, `~0` for `~`). Empty pointer selects the result root.
Arrays page by index; objects page by sorted property name. The default limit is 100 (1–1000);
default inspection `maxBytes` is 32768. Oversized selections fail without truncating data: select a
deeper pointer, a smaller page, or a larger byte budget. Missing properties differ from JSON null.

Explicitly copy a retained value into a later call's arguments:

```json
{
  "tool":"crm.import_records",
  "arguments":{},
  "argumentRefs":[{"target":"/records","reference":"REFERENCE","pointer":"/structuredContent/rows"}]
}
```

Reference targets cannot replace the whole argument object or overlap. Parent objects/arrays must
already exist; a final object property may be added. Ordinary argument strings/objects are never
interpreted as hidden reference markers. Copied arguments are validated against the target schema.
Cross-server copies require the **source** server's `policy.referenceTo` grant to the destination.
Same-server copies are allowed, subject to the destination tool policy. See [Servers](servers.md).

References expire ten minutes after creation, are limited to 128 entries / 32 MiB total, and evict
oldest entries when needed. Individual results above 8 MiB fall back to inline with an `_meta.tap`
warning; an already-executed successful operation is not turned into a failure. References are
process/session-local, not disk files. Never repeat a write merely to recreate an expired reference.
Inline remains the default: references add inspection turns and do not benefit every workload.

tap uses Go and the official MCP Go SDK. It does not embed LazyMCP, an LLM, a vector database or a
script runtime. It proxies tools, not every MCP feature (resources/prompts/sampling/elicitation).
