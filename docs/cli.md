# Command line

`tap` with no arguments is the MCP server an agent runs. The other commands let you use the same
engine from a shell, to set it up and to see what your agent sees.

| Command | Does |
| --- | --- |
| `tap` | Serve MCP over stdio |
| `tap list` | Configured servers and their tool counts |
| `tap add NAME URL` | Add an HTTP server (see [Servers](servers.md)) |
| `tap add NAME -- CMD [ARGS...]` | Add a stdio server |
| `tap remove NAME` | Remove a server |
| `tap search QUERY` | Find tools, with what is needed to call them |
| `tap search --server NAME` | Browse one integration's tools |
| `tap inspect SERVER.TOOL` | Inspect an exact tool's full contract |
| `tap refresh [NAME]` | Refresh cached metadata and check live availability |
| `tap call SERVER.TOOL [K=V ...] [--args JSON]` | Call a tool |
| `tap path` | Print the config file tap reads |
| `tap version` | Print the version |

`--json` prints raw output; `--limit N` caps search results (default 8). MCP search validates
limits from 1 to 25. For compatibility, the CLI keeps the original unconstrained limit handling.

## Search

```sh
tap search echo
```

```text
files.echo
  Echo the supplied message.
```

Search ranks word overlap across tool/provider names, titles, descriptions and parameter metadata;
query filler no longer excludes otherwise relevant tools. `--json` shows the full input schema by
default for shell commands:

```sh
tap search echo --json
```

```json
{
  "query": "echo",
  "total": 1,
  "matches": [
    {
      "id": "files.echo",
      "description": "Echo the supplied message.",
      "inputSchema": {
        "type": "object",
        "properties": { "message": { "type": "string" } },
        "required": ["message"],
        "$schema": "https://json-schema.org/draft/2020-12/schema"
      }
    }
  ],
  "unavailable": [],
  "catalogs": [{"server":"files","source":"cache","availability":"not_checked","observedAt":"2026-10-02T00:00:00Z"}]
}
```

The displayed JSON above is abbreviated: matches also include `schemaLoaded` and, for full
contracts, optional output schemas and untrusted server guidance.

```sh
tap search --server issues --detail summary --limit 5
tap search --server issues --detail summary --offset 5 --limit 5
tap search 'create issue' --server issues --detail auto --max-bytes 32768 --json
tap inspect issues.create_issue
tap refresh issues
tap list --refresh
```

Search `--detail` is `full` by default on the CLI; MCP defaults to `auto`. Full schemas are never
truncated to meet `--max-bytes` (1024–16777216). Auto can return labeled summaries; summary omits
schemas. Follow `nextOffset` in JSON output for results withheld by count or byte budget.
`--refresh` bypasses fresh metadata caches. Cached tool counts do not establish current availability.
See [How it works](how-it-works.md) for reference operations, which are MCP-session-only and cannot
be shared between separate CLI invocations.

## Call

Give arguments as `key=value` strings, as JSON with `--args`, or both (`key=value` wins):

```sh
tap call files.echo message=hi
tap call issues.create --args '{"title": "Login fails", "labels": ["bug"]}'
```

Without `--json`, a tool that reports an error prints its content and a diagnostic on stderr.
`--json` prints the downstream result directly. For compatibility, both forms exit 0 for a
tool-reported error; command and protocol failures exit 1:

```text
tap: /message: required field is missing
```

The example is a gateway validation failure (exit 1): it never reaches the backend. Transport or
protocol failure after sending a call may mean a write already happened. Tap never retries calls.
