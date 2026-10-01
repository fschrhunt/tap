# How it works

## What the agent sees

An agent connected to tap loads two tools, whatever number of servers sit behind it:

- **`plugin_search`** finds tools. With a query, it returns matching tools as `server.tool` ids,
  each with its description, input schema and safety hints, plus the total match count and any
  server that could not be reached. With no query, it lists the configured servers.
- **`plugin_call`** runs a tool by id with an arguments object. The tool's content, error flag and
  structured content come back unchanged.

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

- **Lazy.** tap connects to a server the first time a search or call needs it, not at startup.
- **Deadline.** Connecting and listing tools share a 5-second deadline per server. A server that
  misses it is reported as unavailable for that search and tried again on the next one.
  `TAP_DEADLINE_MS` changes the limit.
- **Cache.** Each server's tool list is kept for one minute. For compatibility, the no-query
  catalog omits reachable servers that expose no tools.
- **Shutdown.** When the agent closes tap's stdin, tap closes every connection and exits.

tap is built on the official MCP Go SDK.
