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

`--json` prints raw output; `--limit N` caps search results (default 8).

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
        "required": ["message"]
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

A tool that reports an error prints it and exits 1:

```text
Input validation error: Invalid arguments for tool echo: message: Invalid input: expected string, received undefined
tap: files.echo reported an error
```
