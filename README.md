<h1 align="center">tap</h1>
<p align="center">Less noise. Better agents.</p>
<p align="center"><a href="https://github.com/fschrhunt/tap/actions/workflows/ci.yml"><img src="https://github.com/fschrhunt/tap/actions/workflows/ci.yml/badge.svg" alt="CI"></a></p>

## Install

Requires Node.js 20 or later.

```sh
git clone https://github.com/fschrhunt/tap.git ~/.local/share/tap
cd ~/.local/share/tap
npm ci --omit=dev
mkdir -p ~/.local/bin
ln -s ~/.local/share/tap/bin/tap.mjs ~/.local/bin/tap
```

Put `~/.local/bin` on your `PATH`. Or install globally from GitHub:

```sh
npm i -g github:fschrhunt/tap
```

The npm package `tap` is an unrelated test runner. Install this project from GitHub.

## Use

Add the `tap` command to your harness's MCP config:

```json
{
  "mcpServers": {
    "tap": { "command": "tap" }
  }
}
```

Bare `tap` runs the MCP server over stdio. Register downstream servers with:

```sh
tap add <name> <url>
tap add <name> -- <command> [args...]
```

Server names cannot contain dots. `tap list` shows configured servers and tool counts.
`tap search <query>` finds tools; `tap call <server.tool> --args '<json>'` calls one.
Use `tap remove <name>` to remove a server and `tap path` to print the config path.

## How it works

A harness loads exactly two tools: `plugin_search` and `plugin_call`.
`plugin_search` returns matching `server.tool` IDs and input schemas on demand.
Without a query, it lists configured servers. `plugin_call` sends arguments to a
selected tool and passes back its content, error flag, and structured content.

Connections are lazy. Tool lists are cached for one minute. Connecting and listing
share a five-second deadline per server; an unavailable server is reported and
retried on a later search. tap uses the official MCP TypeScript SDK v2.

## Configuration

The default config is `~/.tap/servers.json`. Set `TAP_CONFIG` to use another file.

```json
{
  "servers": {
    "docs": {
      "type": "http",
      "url": "https://example.invalid/mcp",
      "headers": { "X-Client": "tap" },
      "bearerTokenEnv": "DOCS_TOKEN"
    },
    "files": {
      "type": "stdio",
      "command": ["node", "server.mjs"],
      "cwd": ".",
      "env": { "MODE": "read" }
    }
  }
}
```

HTTP servers use Streamable HTTP with static headers or a bearer token from the
environment variable named by `bearerTokenEnv`. There is no OAuth support.
Header and environment values expand `${NAME}` from tap's environment.
Stdio commands and working directories can begin with `~`.
