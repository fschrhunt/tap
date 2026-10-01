# Install

tap needs Node 20 or newer.

```sh
git clone https://github.com/fschrhunt/tap.git ~/.local/share/tap
cd ~/.local/share/tap
npm ci --omit=dev
mkdir -p ~/.local/bin
ln -s ~/.local/share/tap/bin/tap.mjs ~/.local/bin/tap
```

Make sure `~/.local/bin` is on your `PATH`. Or install globally from GitHub:

```sh
npm i -g github:fschrhunt/tap
```

The npm package named `tap` is an unrelated test runner, so install from GitHub, not by name.

## Update

```sh
git -C ~/.local/share/tap pull
cd ~/.local/share/tap && npm ci --omit=dev
```

## Connect your agent

Register `tap` as the one MCP server your agent uses. Run with no arguments, it serves MCP over
stdio.

**Claude Code:**

```sh
claude mcp add --scope user tap -- tap
```

**Codex**, in `~/.codex/config.toml`:

```toml
[mcp_servers.tap]
command = "tap"
```

**Opencode**, in `opencode.jsonc`:

```jsonc
{
  "mcp": {
    "servers": {
      "tap": { "type": "local", "command": ["tap"] }
    }
  }
}
```

**Any other MCP client:**

```json
{
  "mcpServers": {
    "tap": { "command": "tap" }
  }
}
```

Then move your other MCP servers out of the agent's config and into tap's (see
[Servers](servers.md)). The agent now loads two tools, `plugin_search` and `plugin_call`, however
many servers you add.
