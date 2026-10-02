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
| `tap call SERVER.TOOL [K=V ...] [--args JSON]` | Call a tool |
| `tap path` | Print the config file tap reads |
| `tap version` | Print the version |
| `tap remote serve` | Host the registry over authenticated HTTP or HTTPS |
| `tap remote use URL` | Select a remote for CLI commands and stdio relays |
| `tap remote off` | Return to the saved local registry |
| `tap remote status` | Print the selected relay endpoint |

With a remote selected, `add`, `remove`, `list`, `search` and `call` use it.
`--local` explicitly uses the saved local registry. See [Remote](remote.md) for
listener, TLS and authentication options. `path` still prints the local config
file that stores the remote selection.

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

Every term must match a tool's name, server, title or description. `--json` shows the input schema
too, exactly what the agent gets:

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
  "unavailable": []
}
```

## Call

Give arguments as `key=value` strings, as JSON with `--args`, or both (`key=value` wins):

```sh
tap call files.echo message=hi
tap call issues.create --args '{"title": "Login fails", "labels": ["bug"]}'
```

Without `--json`, a tool that reports an error prints its content and a diagnostic on stderr.
`--json` prints the downstream result (HTTP uses the SDK's supported fields; stdio retains
raw response fields). For compatibility, both forms exit 0 for a
tool-reported error; command and protocol failures exit 1:

```text
Input validation error: Invalid arguments for tool echo: message: Invalid input: expected string, received undefined
tap: files.echo reported an error
```
