# Install

tap is a single binary. It needs no Node runtime. Downstream servers may need their own runtimes.
Release binaries support macOS and Linux on `amd64` (x86-64) and `arm64`.

## Install with the script

```sh
curl -fsSL https://raw.githubusercontent.com/fschrhunt/tap/main/install.sh | sh
```

It downloads the release for this computer, checks it against the release's `checksums.txt`,
and installs into `~/.local/bin`. Run it again to update. It takes `--version vX.Y.Z` to install
one release and `--dir DIR` for another folder. Pass options to the piped script with `sh -s --`:

```sh
curl -fsSL https://raw.githubusercontent.com/fschrhunt/tap/main/install.sh | sh -s -- --version vX.Y.Z
curl -fsSL https://raw.githubusercontent.com/fschrhunt/tap/main/install.sh | sh -s -- --dir "$HOME/bin"
```

Replace `vX.Y.Z` with an existing release tag. Make sure the install directory is on your
`PATH`, for example `export PATH="$HOME/.local/bin:$PATH"` in your shell's startup file.

## Install with Homebrew

tap's repository is its own tap:

```sh
brew tap fschrhunt/tap https://github.com/fschrhunt/tap
brew install fschrhunt/tap/tap
```

Update with `brew update && brew upgrade fschrhunt/tap/tap`.

## Install from Releases

Download the `linux` or `darwin` archive for `amd64` or `arm64` from
[Releases](https://github.com/fschrhunt/tap/releases), along with `checksums.txt` from the same
release. Verify the archive **before** extracting or running it, then install the binary:

```sh
sha256sum tap_vVERSION_linux_amd64.tar.gz
# On macOS: shasum -a 256 tap_vVERSION_darwin_arm64.tar.gz
# Compare the digest with this archive's entry in checksums.txt; stop if it differs.
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

Requires Go 1.26 or newer:

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

Run the install script again to move to the latest release, or replace the binary with a newer
release from [Releases](https://github.com/fschrhunt/tap/releases), or repeat `go install` above.
For a source checkout:

```sh
git pull
go build -o tap ./cmd/tap
install -m 755 tap ~/.local/bin/tap
```

## Connect your agent

First confirm that your shell can find tap:

```sh
tap version
tap help
```

If your agents already have servers, preview and copy them before removing any original entries:

```sh
tap import --dry-run
tap import
tap list
```

Sign in with `tap auth NAME` if a server asks for it. Import reads your agents' configs but
never changes them; see [Servers](servers.md#bringing-over-an-agents-servers).

Register `tap` as an MCP server. Started by an agent with no arguments, it serves MCP over
stdio; run bare in a terminal, it shows an introduction. tap connects the coding agents it
finds on this machine:

```sh
tap connect             # every agent tap finds
tap connect claude      # one of them: claude, codex or opencode
```

It runs the agent's own command — `claude mcp add`, `codex mcp add`, `opencode mcp add` — and
reports each agent as `connected`, or `has tap already` when its config has tap in it already.
Only the agent's own command writes the agent's config. After verifying the copied servers,
remove their original entries from each agent's config, leaving tap. Start a new agent session
so it loads the new setup. Otherwise the agent may load both tap and the original servers.

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

**OpenCode**, in `~/.config/opencode/opencode.jsonc` for all projects, or `opencode.jsonc`
for one project. Merge this entry into the existing config rather than replacing the file:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
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

tap exposes two tools, `plugin_search` and `plugin_call`, however many servers you add.
It proxies tools, not resources, prompts or other MCP features. Keep a direct connection for
servers whose non-tool features you need.

To share connectors across machines, keep these same harness settings, start `tap remote serve`
on the host, then run `tap remote pair NAME https://HOST:8443` on each client. See
[Remote](remote.md) for the pairing steps. No VPN is required.

## Troubleshooting setup

- **`tap: command not found`:** add the install directory to `PATH` and open a new shell.
  If an agent cannot find tap, use the binary's absolute path in its MCP config; GUI apps
  may not inherit your shell's `PATH`.
- **A server is unavailable:** run `tap list`, then `tap auth NAME` if it asks for sign-in.
  Check the endpoint, command and required runtime. Example addresses are placeholders.
- **Tools are missing:** use `tap refresh NAME`, then `tap search --server NAME`.
  Also check that the server's allow/deny policy permits them.
- **Duplicate tools:** remove the imported servers' original agent entries and start a new session.
- **Unexpected config or target:** run `tap path` and `tap remote status`.

## Uninstall

Take tap out of each agent's config, for example `claude mcp remove tap`, and put back any
servers you still want there. Stop running tap processes. Remove a Homebrew installation
with `brew uninstall fschrhunt/tap/tap`; for a script or Go install, remove the installed binary:

```sh
rm ~/.local/bin/tap      # or ~/go/bin/tap
```

Optionally remove `~/.tap` after backing up any server definitions you want to keep. This
deletes saved sign-ins, tool lists, remote profiles, pairing credentials and the host identity.
If you used `TAP_CONFIG`, check that location and its sidecar files too. Deleting local files
does not revoke provider tokens or a device's access to a remote: sign out or revoke it first.
