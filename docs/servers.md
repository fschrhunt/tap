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
| `--allow-tool GLOB` | Only these tool names may be discovered/called; repeat to allow more. |
| `--deny-tool GLOB` | Block these tool names; deny wins over allow. Repeatable. |
| `--reference-to SERVER` | Permit retained values from this server to flow to that destination. Repeatable. |
| `--idle-timeout-ms N` | Override the default idle timeout for stdio (1–86400000 ms); backend state is lost. |

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

## Tool policy and retained-data transfer

All tools remain allowed by default for compatibility. To restrict a server:

```sh
tap add issues https://issues.example.com/mcp --allow-tool 'get_*' --allow-tool 'list_*' --deny-tool '*delete*'
tap add db --reference-to crm -- node db-server.mjs
```

The equivalent server entry can contain:

```json
{
  "command":["node","db-server.mjs"],
  "policy":{
    "allow":["query","list_*"],
    "deny":["delete_*"],
    "referenceTo":["crm"]
  }
}
```

Patterns use Go path globs against the **tool name**, without a server prefix: `*`, `?` and
character classes are supported. An absent `allow` means unrestricted; `"allow": []` allows
nothing. Deny always wins. Invalid policy fields, values and patterns fail closed. A denied call
does not start a backend. Discovery omits denied tools; upstream catalog counts are not filtered.

`referenceTo` contains exact destination **server** names, not globs. Its absence blocks cross-server
reference copies; same-server copies are permitted. This rule does not prevent an agent from
reading inline data and passing it manually to another tool. It is not a complete data-loss
prevention system. Policy files must be controlled by a trusted operator; an agent with shell/file
access as your user may also be able to edit them. Tap does not implement a trusted approval UI.

Because calls all route through `plugin_call`, native host permissions for original MCP tool
names may no longer apply. Configure tap policy and host restrictions explicitly. Server
`readOnlyHint` / `destructiveHint` annotations inform the agent but never grant authorization.

`idleTimeoutMs` is an optional stdio-only override of the default five-minute timeout
(`TAP_IDLE_TTL_MS`). Use a suitably long timeout for a browser, transaction or other stateful
backend unless losing its process-local state is acceptable.
