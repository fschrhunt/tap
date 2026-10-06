# Contributing

Smallness is the point. Prefer the fewest moving parts that solve the problem.
Keep changes focused and write one test for each behavior worth protecting.

Use Go 1.26 or newer:

```sh
./x check                 # fmt, vet, build and race tests
./x test ./internal/config # focused package tests
./x build -o tap ./cmd/tap # build the CLI
```

`./x` defaults to `check`, which checks formatting without rewriting sources. CI and
release checks use the same command, including `go test -race ./...`. `./x --help` lists
targets; `./x fmt --check`, `./x lint`, `./x test` and `./x build` run individual checks.
Test and build arguments are forwarded to Go.

Tests use local fixtures and temporary configs, without network access or your
own server config. `TestMain` builds tap and the Go MCP fixture once. The tests exercise the CLI
and MCP over stdio, with a local Streamable HTTP peer too. Add a CHANGELOG entry for
user-visible changes.

Report issues with `tap version` and `tap list` output, what you expected, and
what happened. Remove sensitive values before sharing output.

By contributing, you agree that your contributions are licensed under this
project's MIT license.
