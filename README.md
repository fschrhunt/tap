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

## Install

Download the archive for your OS and architecture from
[Releases](https://github.com/fschrhunt/tap/releases), extract it, and put `tap` on your `PATH`.
It is one binary; no Node runtime is needed.

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
tap add docs https://docs.example.com/mcp          # an HTTP server
tap add files -- npx -y @modelcontextprotocol/server-filesystem ~/notes
tap list                                           # servers and tool counts
tap search "create issue"                          # what your agent would find
```

## Docs

[Install](docs/install.md) · [Servers](docs/servers.md) · [Command line](docs/cli.md) ·
[How it works](docs/how-it-works.md) · [Remote](docs/remote.md)

## License

MIT
