# Contributing

Smallness is the point. Prefer the fewest moving parts that solve the problem.
Keep changes focused and write one test for each behavior worth protecting.

Use Go 1.26 or newer:

```sh
go build -o tap ./cmd/tap
test -z "$(gofmt -l .)"
go vet ./...
go test ./...
```

Tests use local fixtures and temporary configs, without network access or your
own server config. `TestMain` builds tap and the Go MCP fixture once. The tests exercise the CLI
and MCP over stdio, with a local Streamable HTTP peer too. Add a CHANGELOG entry for
user-visible changes.

Report issues with `tap version` and `tap list` output, what you expected, and
what happened. Remove sensitive values before sharing output.

By contributing, you agree that your contributions are licensed under this
project's MIT license.
