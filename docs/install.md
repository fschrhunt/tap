# Install

tap is a single binary. It needs no Node runtime. Downstream servers may need their own runtimes.

Download the `linux` or `darwin` archive for `amd64` or `arm64` from
[Releases](https://github.com/fschrhunt/tap/releases). Extract it, verify its archive against
`checksums.txt`, and install the binary:

```sh
tar -xzf tap_vVERSION_linux_amd64.tar.gz
mkdir -p ~/.local/bin
install -m 755 tap ~/.local/bin/tap
```

Use the filename of the release you downloaded. Make sure `~/.local/bin` is on your `PATH`.

## Install with Go

With Go 1.26 or newer:

```sh
go install github.com/fschrhunt/tap/cmd/tap@latest
```

Make sure Go's binary directory (normally `~/go/bin`) is on your `PATH`.

## Build from source

```sh
git clone https://github.com/fschrhunt/tap.git
cd tap
go build -o tap ./cmd/tap
mkdir -p ~/.local/bin
install -m 755 tap ~/.local/bin/tap
```

Release builds embed the tag in `tap version`. Go installs and local builds report `dev` unless
built with `-ldflags "-X main.version=VERSION"`.

## Update

Replace the binary with a newer release, or repeat `go install` above. For a source checkout:

```sh
git pull
go build -o tap ./cmd/tap
install -m 755 tap ~/.local/bin/tap
```

## Connect your agent

Register `tap` as the one MCP server your agent uses. Run with no arguments, it serves MCP over
stdio. tap connects the coding agents it finds on this machine:

```sh
tap connect             # every agent tap finds
tap connect claude      # one of them: claude, codex or opencode
```

It runs the agent's own command — `claude mcp add`, `codex mcp add`, `opencode mcp add` — and
reports each agent as `connected`, or `has tap already` when its config has tap in it already.
Only the agent's own command writes the agent's config. Start a new agent session after
connecting, so it loads tap.

To connect by hand, or to connect any other MCP client, add `tap` yourself:

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

Then move your other MCP servers out of the agent's config and into tap's: `tap import` copies
them over, and you delete them from the agent's config (see [Servers](servers.md)). The agent
now loads two tools, `plugin_search` and `plugin_call`, however many servers you add.

To share connectors across machines, keep these same harness settings and run
`tap remote use https://tap.example.com:8765` on each machine. See [Remote](remote.md)
for hosting on an ordinary port, authentication and native TLS. No VPN is required.

## Uninstall

Take tap out of each agent's config, for example `claude mcp remove tap`, and put back any
servers you still want there. Then delete the binary and tap's files:

```sh
rm ~/.local/bin/tap      # or ~/go/bin/tap
rm -r ~/.tap             # servers, saved tool lists and sign-ins
```
