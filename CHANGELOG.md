# tap releases

## Unreleased

- Releases: `scripts/release.sh` names the Unreleased section in a PR, then tags it once CI has
  passed; the tag builds checksummed archives for macOS and Linux with build provenance, release
  notes from the changelog, the Homebrew formula on main, and an install of the release as people
  will run it. Install with
  `curl -fsSL https://raw.githubusercontent.com/fschrhunt/tap/main/install.sh | sh` or
  `brew tap fschrhunt/tap https://github.com/fschrhunt/tap && brew install fschrhunt/tap/tap`.
- Settings, in a `settings` block of `servers.json` and with `tap config`: `tap config` shows
  each one, its value and where it comes from; `tap config set` and `unset` change them, and
  `--server` keeps `start` or `idleTimeoutMs` for one server. A mistyped setting is refused with
  what it takes. `TAP_REFERENCES`, `TAP_DEADLINE_MS`, `TAP_IDLE_TTL_MS` and `TAP_FAIL_TTL_MS`
  still work, and win over the config. See [Settings](docs/settings.md).
- **Changed:** tap is lazier by default. The `start` setting says when a server starts: `call`,
  the new default, starts it only for a call, so a search answers from saved tools without
  starting anything; `search` is how tap behaved before; `start` starts a server with tap and
  keeps it running. A call still checks its tool against the live server either way.
- Result references and the search defaults are settings: `references`, `searchLimit` and
  `searchMaxBytes`. The defaults an agent is told in `plugin_search` follow them.
- **Changed:** `cmd/tap-bench` is `cmd/bench`.

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
  exit 0. `tap search` needs a query or `--server`, and `--limit` a whole number of at least 1.
  `tap add` refuses an address that is not http or https, and says `replaced` when the name
  existed. `tap list` asks each server afresh instead of printing a saved catalog marked stale;
  `tap list --cached` prints the saved one. Text output is aligned in columns; use `--json` in
  scripts.
- **Changed:** the two tools an agent loads take about 300 tokens, down from about 870 with
  references described. `plugin_call` offers `tool` and `arguments`; result references, their
  fields and their guidance are offered only when `TAP_REFERENCES=on` is set where tap runs,
  and are refused with `reference_unavailable` otherwise.
- **Changed:** a search returns the tools that hold every word of the query, and only when none
  does, the tools that hold any. A query that is a tool's name or id returns that tool. Results
  used to include every tool sharing one word, so `get me` no longer buries `github.get_me`.
  Scores and their order are otherwise the same, and the frozen retrieval benchmark is unchanged
  at 95.8% recall@1 with a third of the bytes in the top eight definitions.
- Faster first answers, with less memory. JSON is read and written in one pass. Searches read
  the catalogs directly instead of building an index first. The saved tool index is laid out
  one tool to a line, read side by side, and read while a serving tap answers its first
  messages. A server that answers the tool list it last answered is recognised by digest and
  is not decoded, or saved, again; over stdio the SDK no longer decodes tool lists a second
  time. An unchanged, settled config file is not read again on every search and call. With five
  servers and 213 real tools, a cold tap is ready in 3.3 ms and returns a first tool result in
  7.5 ms, where the build before this work took 18.4 ms, and its memory high-water mark is
  17 MiB, down from 25 MiB: medians of 30 local Linux/amd64 runs.
- Agent-oriented discovery while retaining two MCP tools: provider-scoped BM25 ranking, Unicode
  tokenization, conservative typo recovery, server browsing, exact inspection, paging and adaptive
  schema disclosure. Full schemas are never truncated; output schemas and untrusted server guidance
  are exposed when available. Initialization gives a bounded integration-name overview.
- Persistent, credential/config/environment-scoped metadata caching with explicit freshness and
  unverified cached availability. Live catalog refresh, pagination and tool-list-change invalidation;
  config changes retire old sessions. Per-server stdio idle timeout overrides never stop active operations.
- Complete local input-schema validation before calling, precise value-free diagnostics and
  structured recovery errors. User-configured tool allow/deny rules fail closed. No automatic call
  retries or argument coercion; post-send protocol failures report unknown execution outcome.
- Opt-in result references (raw/lossless stdio, SDK-supported HTTP fields) with bounded session memory, JSON Pointer inspection,
  deterministic paging, release and explicit argument copies. Cross-server copies require source
  policy grants. References are bound to actual MCP sessions, including authenticated remote relays.
  Results too large to retain stay inline without changing the execution outcome.
- CLI scoped search, full inspection, metadata refresh and policy/idle flags. Offline retrieval
  benchmark compares original search and recorded competitor rankings; measured-run evaluation
  reports verified task outcomes, real token usage, cost per success and latency under matched
  task/trial/environment/budget/model settings. Development scores are not independent leaderboard
  claims. Black-box tests cover cache scope, notifications, concurrency, policy and references.
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
