# Working on tap

## Commands

- `npm ci` installs locked dependencies.
- `npm test` runs the offline Node.js test suite.
- `node bin/tap.mjs` serves MCP over stdio.
- `node bin/tap.mjs help` prints CLI usage.

Set `TAP_CONFIG` to a temporary file when exercising config commands. Never use
or modify a personal server config for tests.

## Code map

- `bin/tap.mjs`: CLI dispatch and output.
- `src/mcp.mjs`: config, downstream connections, deadlines, search, and calls.
- `src/server.mjs`: the two-tool MCP surface.
- `test/fixture.mjs`: local stdio fixture, started with `--serve`.
- `test/cli.mjs`: CLI behavior with temporary configs.
- `test/mcp.mjs`: MCP behavior through a spawned tap process.
- `docs/`: user docs with examples; update them with any user-visible change.
- `assets/`: the logo, wordmark and lockup SVGs in black and white; see `assets/README.md`.

## Conventions

Use the fewest moving parts. Keep Node.js ESM and the official MCP SDK v2.
Comment modules and functions with their purpose and contract; avoid line-by-line
comments. Update comments and docs when behavior changes.

Write one focused test per behavior change, using `node:test`. Tests must be
hermetic and offline. Add a CHANGELOG entry for user-visible changes.
Preserve unrelated changes and never expose secrets.
