# Contributing

Smallness is the point. Prefer the fewest moving parts that solve the problem.
Keep changes focused and write one test for each behavior worth protecting.

Use Node.js 20 or later:

```sh
npm ci
npm test
```

Tests use local fixtures and temporary configs, without network access or your
own server config. Add a CHANGELOG entry for user-visible changes.

Report issues with `tap version` and `tap list` output, what you expected, and
what happened. Remove sensitive values before sharing output.

By contributing, you agree that your contributions are licensed under this
project's MIT license.
