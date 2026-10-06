<p align="center">
  <picture>
    <source srcset="assets/white/lockup.svg" media="(prefers-color-scheme: dark)">
    <source srcset="assets/black/lockup.svg" media="(prefers-color-scheme: light)">
    <img src="assets/black/lockup.svg" alt="tap" height="48">
  </picture>
</p>
<p align="center">Less noise. Better agents.</p>
<p align="center"><a href="https://github.com/fschrhunt/tap/actions/workflows/ci.yml"><img src="https://github.com/fschrhunt/tap/actions/workflows/ci.yml/badge.svg" alt="CI"></a></p>

---

tap is one MCP server that stands in for all of yours. Your agent loads two tools,
`plugin_search` and `plugin_call`, instead of every tool from every server, and finds the rest
when it needs them. Initialization instructions and the search tool description show which
integrations tap contains, so the agent knows to use tap when you name one.

Search or browse capabilities, inspect complete contracts, and call with validated arguments.
Saved tools are searched without starting their servers, even across sessions; a server starts
when one of its tools is called. With `tap config set references on`, large results can stay
behind session-local references rather than filling the agent's context.

## Install

One binary for macOS or Linux; tap itself needs no Node runtime. The script downloads the
release for your machine and checks it against the release's checksums:

```sh
curl -fsSL https://raw.githubusercontent.com/fschrhunt/tap/main/install.sh | sh
```

With Homebrew:

```sh
brew tap fschrhunt/tap https://github.com/fschrhunt/tap
brew install fschrhunt/tap/tap
```

With Go 1.26 or newer:

```sh
go install github.com/fschrhunt/tap/cmd/tap@latest
```

Or build from source with Go 1.26 or newer:

```sh
git clone https://github.com/fschrhunt/tap.git
cd tap
go build -o tap ./cmd/tap
mkdir -p ~/.local/bin
install -m 755 tap ~/.local/bin/tap
```

Make sure the install directory is on your `PATH`, then follow the setup below.
See [Install](docs/install.md) for other clients, updates and troubleshooting.

## Use

```sh
tap import --dry-run                               # preview your agents' existing servers
tap import                                         # copy them into tap
tap connect                                        # register tap with your coding agents
tap list                                           # check connections and tool counts
tap search create issue                            # see what your agent would find
```

After checking the imported servers, remove their original entries from your agents' configs,
leaving tap. Import never changes those files. Start a new agent session to load tap.

To add servers yourself:

```sh
tap add docs https://docs.example.com/mcp            # replace with your server's endpoint
tap add files -- npx -y @modelcontextprotocol/server-filesystem ~/notes
tap add linear https://mcp.linear.app/mcp
tap auth linear                                    # sign in after adding the server
```

The filesystem example needs Node.js and an existing `~/notes` directory. Addresses under
`example.com` are placeholders, not working MCP servers.

`tap help` lists every command; `tap help COMMAND` shows its usage, flags and examples.

## Docs

[Install](docs/install.md) · [Servers](docs/servers.md) · [Command line](docs/cli.md) · [Settings](docs/settings.md) ·
[How it works](docs/how-it-works.md) · [Remote](docs/remote.md)

[Benchmarks](bench/README.md) explain reproducible retrieval comparisons and measured task-run
evaluation. No competitor superiority claim is made without matched end-to-end measurements.

## License

MIT
