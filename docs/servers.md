# Servers

tap reads its servers from `~/.tap/servers.json`. Set `TAP_CONFIG` to use another file.

Selecting a [remote](remote.md) switches discovery, calls and CLI server edits to
the remote registry. Local servers remain saved; use CLI `--local` to edit them.
Environment references and paths in a remote server definition resolve on the
remote machine. Remote selection stores the URL and token environment-variable
name alongside `servers`, never the token itself.

## Adding servers

An HTTP server (Streamable HTTP):

```sh
tap add docs https://docs.example.com/mcp
tap add issues https://issues.example.com/mcp --bearer-token-env ISSUES_TOKEN
tap add search https://search.example.com/mcp --header "X-Client=tap"
```

A stdio server: everything after `--` is the command.

```sh
tap add files -- npx -y @modelcontextprotocol/server-filesystem ~/notes
tap add db --env DATABASE_URL='${DATABASE_URL}' --cwd ~/code/app -- node mcp/server.mjs
```

| Flag | Meaning |
| --- | --- |
| `--header K=V` | A static HTTP header. Repeat for more. |
| `--bearer-token-env NAME` | Send `Authorization: Bearer $NAME`, read from tap's environment. |
| `--env K=V` | An environment variable for a stdio server. Repeat for more. |
| `--cwd DIR` | The directory a stdio server starts in. |

Server names cannot contain dots, since tools are named `server.tool`.

Remove one with `tap remove NAME`. Check them all with `tap list`:

```text
files — 3 tools
docs — unavailable: fetch failed
```

## The config file

`tap add` and `tap remove` edit this file; you can also edit it by hand.

```json
{
  "servers": {
    "docs": {
      "type": "http",
      "url": "https://docs.example.com/mcp",
      "headers": { "X-Client": "tap" },
      "bearerTokenEnv": "DOCS_TOKEN"
    },
    "files": {
      "type": "stdio",
      "command": ["node", "server.mjs"],
      "cwd": "~/code/files",
      "env": { "MODE": "read" }
    }
  }
}
```

| Key | For | Meaning |
| --- | --- | --- |
| `type` | both | `http` or `stdio` |
| `url` | http | The server's Streamable HTTP endpoint |
| `headers` | http | Static headers |
| `bearerTokenEnv` | http | Name of the environment variable holding a bearer token |
| `command` | stdio | The command and its arguments, as an array |
| `cwd` | stdio | Directory to start in |
| `env` | stdio | Extra environment variables |

- Values in `headers` and `env` expand `${NAME}` from tap's environment, so secrets stay out of the
  file: `"env": { "API_KEY": "${API_KEY}" }`.
- `command` and `cwd` may start with `~`.
- There is no OAuth support; use a token in an environment variable.
- HTTP redirects are rejected: configure the final MCP endpoint so static
  authentication headers cannot be forwarded to another destination.
- If `servers.json` is a symlink, `tap add` and `tap remove` write through it to the real file.
