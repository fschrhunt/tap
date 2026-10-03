# Servers

tap reads its servers from `~/.tap/servers.json`. Set `TAP_CONFIG` to use another file.

Selecting a [remote](remote.md) switches discovery, calls and CLI server edits to
the remote registry. Local servers remain saved; use CLI `--local` to edit them.
Environment references and paths in a remote server definition resolve on the
remote machine. Remote selection stores the URL and token environment-variable
name alongside `servers`, never the token itself.

## Bringing over an agent's servers

If your agents already have MCP servers, `tap import` adds them to tap in one step:

```sh
tap import --dry-run     # see what would be added
tap import               # add them
```

```text
From Claude Code (/home/you/.claude.json)
  added github (http)
  added files (stdio)
  skipped legacy: it uses the sse transport, and tap speaks Streamable HTTP
From Codex (/home/you/.codex/config.toml)
  has files already
  added db (stdio)
```

With no argument it reads every place it knows:

| Agent | Files |
| --- | --- |
| Claude Code (`claude`) | `~/.claude.json`, with the servers of the project you are in, and `./.mcp.json` |
| Codex (`codex`) | `~/.codex/config.toml` |
| OpenCode (`opencode`) | `~/.config/opencode/opencode.json` or `.jsonc`, and `./opencode.json` or `.jsonc` |
| Cursor (`cursor`) | `~/.cursor/mcp.json` |
| VS Code (`vscode`) | `./.vscode/mcp.json` |

Name one or more agents to read only those, or give the path of any config file with an
`mcpServers`, `servers` or `mcp` table: `tap import codex`, `tap import ./team/mcp.json`.

tap only reads those files. Afterwards, take the servers out of each agent's config yourself and
leave tap there (see [Install](install.md#connect-your-agent)), so the agent loads two tools.

- A server tap already has under the same name is left alone unless you pass `--force`.
- Values are copied as written, including tokens. To keep one out of tap's config, put it in an
  environment variable and write `${NAME}` in its place.
- A name with a dot is imported with a dash, since tool ids are `server.tool`.
- Servers that are turned off, that use the older SSE transport, or that are tap itself are
  skipped, each with its reason.
- With a remote selected, `tap import` needs `--local`: one machine's commands and paths would
  not work on another.

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

Server names cannot contain dots, since tools are named `server.tool`. Adding a name tap
already has replaces that server, and says so.

Remove one with `tap remove NAME`. Check them all with `tap list`:

```text
files  3 tools
docs   unavailable: its address could not be reached
```

## Signing in

Many hosted MCP servers have you sign in, with OAuth, instead of pasting a token. Add the
server by its address, then sign in once:

```sh
tap add linear https://mcp.linear.app/mcp
tap auth linear
```

`tap auth` opens the provider's sign-in page in your browser and waits for you to finish. tap
then saves the sign-in and renews it by itself, so your agents use the server without asking.
Agents that are already running find the server's tools on their next search. With a remote
selected, tap runs the OAuth exchange and stores the grant on that remote while relaying the
browser callback automatically; no SSH session or copied redirect address is needed. Use
`tap auth NAME --local` when you mean a server in this machine's own registry.

- **No browser opener.** Run `tap auth NAME --no-browser`, open the printed page manually in
  your browser, and finish sign-in. The callback still returns to tap automatically.
- **A provider that does not register clients.** tap registers itself with the provider when
  the provider allows it. When it does not, create an app there with the redirect address
  `http://127.0.0.1:PORT/callback`, and run `tap auth NAME --client-id ID --port PORT`. The
  callback port is opened on the machine running tap, including when a remote is selected. For
  an app with a secret, add `--client-secret-file FILE`; the secret is read from the file, never
  from a flag.
- **Signing out.** `tap auth NAME --remove` forgets the sign-in. `tap remove NAME` does too,
  locally and through a remote. Running processes check the owner-only store before lending
  tokens and invalidate that server's session and catalog on their next operation. Removing
  and re-adding the same server does not restore its sign-in or authorized catalog. Already
  sent requests may finish; provider-side token revocation remains a separate action.

`tap list` says when a server is waiting for this:

```text
linear  unavailable: needs you to sign in: run "tap auth linear"
```

Sign-ins are kept in `servers.json.auth.json` beside the config, readable only by you. tap
refuses to use the file if anyone else can read it. A sign-in belongs to the address it was
given for: change a server's `url` and it is asked for again. A server with a
`bearerTokenEnv` or an `Authorization` header uses that instead and is never sent a sign-in.
With a remote selected, `tap auth NAME` stores the sign-in on the remote. `--local` explicitly
targets this machine's registry. Authenticated remote administration is required to create or
remove a remote sign-in; the grant itself never travels back to the client.

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
| `start` | both | When this server starts: `call`, `search` or `start`; see [Settings](settings.md#when-servers-start) |
| `idleTimeoutMs` | stdio | How long this server keeps running unused |

- Values in `headers` and `env` expand `${NAME}` from tap's environment, so secrets stay out of the
  file: `"env": { "API_KEY": "${API_KEY}" }`.
- `command` and `cwd` may start with `~`.
- A server you sign in to needs no key here: see [Signing in](#signing-in).
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

Downstream tool names containing `/` are omitted from discovery and refused on calls,
including with unrestricted policy. This keeps path-glob separators from bypassing deny rules.
Patterns use Go path globs against the **slash-free tool name**, without a server prefix: `*`, `?` and
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

`idleTimeoutMs` is an optional stdio-only override of the `idleTimeoutMs` [setting](settings.md),
five minutes by default. Use a suitably long timeout for a browser, transaction or other stateful
backend unless losing its process-local state is acceptable.
