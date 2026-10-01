<h1 align="center">tap</h1>
<p align="center">Less noise. Better agents.</p>
<p align="center"><a href="https://github.com/fschrhunt/tap/actions/workflows/ci.yml"><img src="https://github.com/fschrhunt/tap/actions/workflows/ci.yml/badge.svg" alt="CI"></a></p>

tap is one MCP server that stands in for all of yours. Your agent loads two tools,
`plugin_search` and `plugin_call`, instead of every tool from every server, and finds the rest
when it needs them.

## Install

```sh
git clone https://github.com/fschrhunt/tap.git ~/.local/share/tap
cd ~/.local/share/tap && npm ci --omit=dev
ln -s ~/.local/share/tap/bin/tap.mjs ~/.local/bin/tap
```

Requires Node 20+. Then point your agent at it, e.g. `claude mcp add --scope user tap -- tap`.

## Use

```sh
tap add docs https://docs.example.com/mcp          # an HTTP server
tap add files -- npx -y @modelcontextprotocol/server-filesystem ~/notes
tap list                                           # servers and tool counts
tap search "create issue"                          # what your agent would find
```

## Docs

[Install](docs/install.md) · [Servers](docs/servers.md) · [Command line](docs/cli.md) ·
[How it works](docs/how-it-works.md)

## License

MIT
