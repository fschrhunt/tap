# Working on tap

## Commands

- `go build -o tap ./cmd/tap` builds the binary (Go 1.25 or newer).
- `go test ./...` runs the offline unit and black-box suites; TestMain builds tap and its fixture once.
- `go test -race ./...` checks concurrent registry, config and remote behavior (also run in CI).
- `go vet ./...` checks the Go code.
- `gofmt -l .` must print nothing.
- `./tap` serves MCP over stdio.
- `./tap help` prints CLI usage.

Set `TAP_CONFIG` to a temporary file when exercising config commands. Never use
or modify a personal server config for tests.

## Code map

- `cmd/tap/main.go`: process entry point and the link-time version (`dev` by default).
- `internal/cli/`: CLI dispatch and output.
- `internal/config/`: config reads, atomic writes, and environment/home expansion.
- `internal/registry/`: lazy downstream connections, deadlines, search, calls, and tool cache.
- `internal/remote/`: authenticated HTTP hosting, remote administration and lazy client relay.
- `internal/server/`: the two-tool MCP surface.
- `internal/wire/`: ordered JSON and preservation of raw SDK responses.
- `test/fixture/`: local stdio/HTTP fixture with success, error, structured, noisy and hang modes.
- `test/*_test.go`: black-box CLI and MCP tests with temporary configs.
- `test/testdata/`: tool-definition and help snapshots from the original implementation.
- `docs/`: user docs with examples; update them with any user-visible change.
- `assets/`: the logo, wordmark and lockup SVGs in black and white; see `assets/README.md`.

## Conventions

Use the fewest moving parts. Keep Go and the official MCP Go SDK for both server and clients.
Comment modules and functions with their purpose and contract; avoid line-by-line
comments. Update comments and docs when behavior changes.

Write one focused test per behavior change, using `testing`. Tests must be
hermetic and offline. Add a CHANGELOG entry for user-visible changes.
Preserve unrelated changes and never expose secrets.
