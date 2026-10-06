# tap docs

tap is one MCP server that stands in for all of yours. Your agent loads two tools instead of every
tool from every server, and finds the rest when it needs them.

| Page | What it covers |
| --- | --- |
| [Install](install.md) | Getting tap, and connecting it to Claude Code, Codex or OpenCode |
| [Servers](servers.md) | Adding MCP servers, and the config file in full |
| [Command line](cli.md) | Every `tap` command, with sample output |
| [Settings](settings.md) | When servers start, references, search size, deadlines and timeouts |
| [How it works](how-it-works.md) | What the agent sees, connections, deadlines and caching |
| [Remote](remote.md) | Sharing servers across machines over paired-device HTTPS |

Start with [Install](install.md), then [Servers](servers.md) to import or add servers.
Addresses under `example.com`, tool IDs and paths in examples are illustrative; replace them
with your own. Discover a server's actual tool IDs with `tap search --server NAME` before calling.

Quick reference: `tap help`. For one command: `tap help COMMAND`.
