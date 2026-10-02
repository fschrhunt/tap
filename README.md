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
when it needs them.

Search or browse capabilities, inspect complete contracts, and call with validated arguments.
Saved tools are searched without starting their servers, even across sessions; a server starts
when one of its tools is called. With `tap config set references on`, large results can stay
behind session-local references rather than filling the agent's context.

## Install

One binary; no Node runtime is needed. The script downloads the release for your machine and
checks it against the release's checksums:

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

Or build from source:

```sh
git clone https://github.com/fschrhunt/tap.git
cd tap
go build -o tap ./cmd/tap
```

Then point your agent at it, e.g. `claude mcp add --scope user tap -- tap`.
See [Install](docs/install.md) for details.

## Use

```sh
tap import                                         # bring over the servers your agents have
tap add docs https://docs.example.com/mcp          # or add an HTTP server
tap add files -- npx -y @modelcontextprotocol/server-filesystem ~/notes
tap auth linear                                    # sign in to a server
tap list                                           # servers and tool counts
tap search create issue                            # what your agent would find
```

`tap --help` lists every command, and `tap COMMAND --help` has examples for each.

## Docs

[Install](docs/install.md) · [Servers](docs/servers.md) · [Command line](docs/cli.md) · [Settings](docs/settings.md) ·
[How it works](docs/how-it-works.md) · [Remote](docs/remote.md)

[Benchmarks](bench/README.md) explain reproducible retrieval comparisons and measured task-run
evaluation. No competitor superiority claim is made without matched end-to-end measurements.

## License

MIT
