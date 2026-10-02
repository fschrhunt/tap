# How it works

## What the agent sees

An agent connected to tap loads two tools, whatever number of servers sit behind it:

- **`plugin_search`** finds tools. With a query, it returns matching tools as `server.tool` ids,
  each with its description, input schema and safety hints, plus the total match count and any
  server that could not be reached. With no query, it lists the configured servers.
- **`plugin_call`** runs a tool by id with an arguments object. The tool's content, error flag,
  structured content and result metadata are forwarded through the MCP SDK. Invalid values
  such as `structuredContent: null` are not preserved.

A typical exchange:

```text
agent → plugin_search { "query": "create issue" }
tap   ← { "matches": [{ "id": "issues.create", "inputSchema": {...} }], ... }
agent → plugin_call { "tool": "issues.create", "arguments": { "title": "Login fails" } }
tap   ← the issue server's own result
```

The agent's context holds two short tool definitions instead of every tool's schema. It reads a
schema only when it needs that tool.

## Connections

- **Lazy startup.** tap connects to no downstream servers during initialization or `tools/list`.
  A direct call opens only its target. A search needs the catalog from every configured server;
  it is not selective downstream discovery.
- **Deadline.** Connecting and listing tools share a 5-second deadline per server. A server that
  misses it is reported as unavailable. Failed connections are temporarily backed off so one
  dead server does not impose the full deadline on every subsequent search.
  `TAP_DEADLINE_MS` changes the limit.
- **Cache.** Tool catalogs are cached in memory and persisted privately for reuse across tap
  processes. Expired catalogs remain searchable while tap refreshes them in the background;
  they describe last-known tools, not a promise the server is currently available. Catalog
  pagination is followed. For compatibility, the no-query catalog omits reachable servers
  that expose no tools.
  Stale catalog rows and matches carry `stale: true`; a failed refresh is also reported as
  an error/unavailable server without discarding those matches. Fresh memory catalogs last
  one minute. Restored disk catalogs answer the search that asked for them, and are always
  revalidated in the background a quarter second later.
- **Changes.** Additions are discovered on the next search. tap reads the config again
  whenever the file's size or modification time has changed, or was modified in the last two
  seconds. Changed or removed server definitions
  invalidate their sessions and cached tools; a removed server cannot still be called through
  an old resident connection.
- **Resources.** Cold discovery is bounded rather than starting every connector at once. Idle
  sessions are closed without forgetting their tool catalogs. Warm searches do not need a
  downstream connection.
- **Calls.** Tool execution has no fixed five-second timeout: long-running tools may legitimately
  take longer. The caller's cancellation and shutdown end pending work. Tap does not automatically
  replay a failed tool call, because it could have already performed a non-idempotent operation.
- **Shutdown.** When the agent closes tap's stdin, tap closes every connection and exits.

tap is built on the official MCP Go SDK.

The private index is `<config path>.tools.json` (mode `0600`); corrupt, public or
definition-mismatched indexes are ignored. Removing it discards saved discovery
data without changing connectors. Writes are coalesced and do not block searches.
The index is optional when the config directory is not writable.

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `TAP_DEADLINE_MS` | `5000` | Per-server connect/list budget; not tool execution |
| `TAP_FAIL_TTL_MS` | `5000` | Retry backoff after discovery/connect failure; `0` disables it |
| `TAP_IDLE_TTL_MS` | `300000` | Close unused downstream sessions after five minutes |

HTTP sessions use the SDK directly so negotiated protocol headers and idle tool-change
notifications work. Stdio responses retain raw capture for compatibility. Application
request/result metadata is forwarded; protocol identity and progress tokens belong to
each hop and are not copied between them.

## Remote mode

A [remote](remote.md) shares one registry, tool index and set of downstream sessions across agents
and machines. Harnesses still launch `tap` over stdio and see the same two tools. Local tap relays
to the remote lazily; connectors and credentials stay on the remote machine. Adding a connector
there needs no harness configuration update or restart of an existing relay session.

Remote mode uses the remote registry only, not a merged local/remote namespace. Local definitions
remain saved for use after disabling the remote or with CLI `--local`. A remote failure is reported
as an error, never silently redirected to local tools.
