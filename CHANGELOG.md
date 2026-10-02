# tap releases

## Unreleased

- `tap auth NAME` signs in to an MCP server through your browser, and tap renews the sign-in
  by itself. It follows the MCP authorization spec, OAuth with the SDK's client: discovery,
  client registration, PKCE. `--no-browser` and a pasted address cover machines
  reached over SSH; `--client-id` covers providers that do not register clients. Sign-ins are
  kept in an owner-only file beside the config. A server waiting for one says so in `tap list`
  and in searches: `needs you to sign in: run "tap auth NAME"`.
- `tap import` adds the servers that Claude Code, Codex, OpenCode, Cursor and VS Code already
  have, or those in any config file you name. `--dry-run` shows what it would do. It only
  reads the agents' files.
- A command line for people. Run in a terminal with no arguments, `tap` prints a short
  introduction instead of waiting silently. `-h` and `--help` work on every command and never
  run it; help leads with examples. Unknown commands and flags are errors that name the
  nearest real one, where they used to be ignored or searched for. Errors say what to do next,
  and reasons a server is unavailable are written for a person.
- **Changed:** a command called wrongly now exits 2 and prints its usage. `tap call` exits 1
  when the tool reports an error, and `tap remove` when there is no such server; both used to
  exit 0. `tap search` needs a query, and `--limit` a whole number of at least 1. `tap add`
  refuses an address that is not http or https, and says `replaced` when the name existed.
  `tap list` and `tap search` wait for each server to answer afresh instead of printing a saved
  catalog marked stale. Text output is aligned in columns; use `--json` in scripts.
- Faster first answers. A search from a new process reads the saved tool index without
  decoding every schema, JSON is read in one pass, and query words are split without regular
  expressions. A tap that serves an agent answers from the saved index first and revalidates
  it a quarter second later. An unchanged, settled config file is not read again on every
  search and call. With
  five servers and 213 tools, a first tool result from a cold start took 6.9 ms, down from
  10.0 ms, and a later call adds 0.19 ms, down from 0.30 ms: medians of 30 local Linux/amd64
  runs. Search results are byte-for-byte the same.
- `tap remote serve` hosts shared connectors over authenticated HTTP or native TLS;
  `tap remote use` selects a lazy stdio relay without changing harness configurations.
  Server additions are discovered by existing agents on their next search. Optional
  separate admin credentials protect connector configuration. No Tailscale dependency.
- Discovery backs off unavailable servers, retains stale tool catalogs during refresh,
  persists private indexes across processes, bounds cold fan-out and closes idle sessions.
  Changed or removed definitions invalidate old sessions and tools. Tool pagination is read.
- Preserve top-level tool result metadata, clean up raw-response capture bookkeeping,
  and serialize configuration mutations using unique atomic temporary files.
- Keep HTTP sessions unwrapped so the SDK sends negotiated protocol headers and
  receives idle tool-change notifications. Forward application call metadata without
  letting connector metadata override tap's protocol identity. Stale catalogs are marked
  explicitly; index writes are coalesced and do not delay discovery.
- Reject MCP HTTP redirects to prevent configured credentials leaking to other
  destinations, and omit endpoint URLs from network-error diagnostics.
- Document remote trust boundaries, approval limitations and cancellation behavior;
  run the test suite with the race detector in CI.

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
