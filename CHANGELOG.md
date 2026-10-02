# tap releases

## Unreleased

- Built with Go 1.27; building from source needs Go 1.26 or newer. The MCP SDK's dependencies
  (golang.org/x/oauth2, sync, sys and time, segmentio/asm) are on their latest releases.
- Clients on MCP 2026-07-28 (current Claude Code) can list and call tap's tools again: the SDK now
  builds those results, with `resultType`, `ttlMs` and `cacheScope`, instead of tap writing them
  itself. Tools are listed by name, and a downstream `structuredContent: null` is no longer passed on.
- Rewritten in Go with the official MCP Go SDK: one binary, no Node runtime needed for tap.
  Release archives for Linux and macOS on amd64 and arm64, plus `go install` and source builds.
  The two tools, downstream transports, config format and CLI remain compatible.
- Startup to an MCP initialize reply: 3.0 ms versus 102.2 ms for the previous implementation.
  Resident memory after a fixture search: 12.0 MiB versus 90.8 MiB. These are medians of
  15 local Linux/amd64 runs with the same stdio fixture; memory is tap's RSS, excluding its child.
- Offline Go black-box tests replace the previous suite, including Streamable HTTP coverage.

- Brand assets: the tap logo, wordmark and lockup in black and white under `assets/`, and the
  lockup in the README header.
- Docs with examples in `docs/`: install and connecting each agent, servers and the config file,
  every command, and how tap works. The README is now a short overview.
- `TAP_DEADLINE_MS` overrides the 5 second per-server deadline; the test suite uses it and runs in
  seconds instead of nearly a minute.
- Prepare the first open-source release: two lazy MCP tools, five-second server
  deadlines with retries, quieter CLI commands, dot-free server names, and offline
  tests. Remove the `serve`, `mcp`, and `local` aliases.
