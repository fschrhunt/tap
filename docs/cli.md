# Command line

Started by an agent, `tap` with no arguments is the MCP server. The commands let you set tap up
from a shell and see what your agent sees. Run in a terminal with no arguments, `tap` prints a
short introduction instead of waiting for an agent that is not there.

| Command | Does |
| --- | --- |
| `tap` | Serve MCP over stdio |
| `tap import [SOURCE...]` | Add the servers your agents already have (see [Servers](servers.md)) |
| `tap add NAME URL` | Add an HTTP server |
| `tap add NAME -- COMMAND [ARGUMENT...]` | Add a stdio server |
| `tap remove NAME` | Remove a server and its saved sign-in |
| `tap list` | The servers and how many tools each has |
| `tap auth NAME` | Sign in to a server |
| `tap search QUERY...` | Find tools, with what is needed to call them |
| `tap call SERVER.TOOL [KEY=VALUE]... [--args JSON]` | Call a tool |
| `tap remote serve` | Host the registry over authenticated HTTP or HTTPS |
| `tap remote use URL` | Select a remote for CLI commands and stdio relays |
| `tap remote off` | Return to the saved local registry |
| `tap remote status` | Print the selected relay endpoint |
| `tap path` | Print the config file tap reads |
| `tap version` | Print the version |
| `tap help [COMMAND]` | Help for tap, or for one command |

## Help

`tap --help` lists every command, the flags they share and the environment tap reads.
`tap COMMAND --help`, `tap COMMAND -h` and `tap help COMMAND` show one command's usage, examples
and flags. `-h` and `--help` mean help wherever they stand, so adding one to a command you are
unsure of never runs it.

## Flags

Flags may come before, between or after a command's arguments, as `--flag value` or
`--flag=value`. A flag a command does not have is an error, with the nearest one it does have.
Everything after `--` is taken as written: for `tap add`, it is the server's command.

| Flag | Commands | Meaning |
| --- | --- | --- |
| `-h`, `--help` | all | Show help |
| `--version` | `tap` | Print the version |
| `--json` | `import`, `list`, `search`, `call` | Print JSON instead of text |
| `--local` | `import`, `add`, `remove`, `list`, `search`, `call`, `auth` | Use this machine's servers while a remote is selected |
| `--limit N` | `search` | How many tools to print: a whole number of at least 1 (default 8) |
| `--args JSON` | `call` | The arguments as a JSON object; `-` reads standard input |
| `-n`, `--dry-run` | `import` | Show what would be added and add nothing |
| `-f`, `--force` | `import` | Replace servers tap already has under the same name |

With a remote selected, `add`, `remove`, `list`, `search` and `call` use it; `--local` uses the
saved local registry. See [Remote](remote.md) for listener, TLS and authentication options.
`path` still prints the local config file that stores the remote selection.

## Output and exit codes

What you asked for is printed on standard output; anything tap says about it, such as an error,
a hint for what to run next, or a note that it is waiting on the servers, goes to standard
error. Hints and waiting notes are only written to a terminal, so pipes and logs stay clean.
Use `--json` in scripts: the text for people may change.

| Exit code | Meaning |
| --- | --- |
| `0` | The command did what was asked |
| `1` | It failed: a server could not be reached, a tool reported an error, a name does not exist |
| `2` | It was called wrongly: an unknown command or flag, a missing or malformed argument. Nothing was changed |

A command called wrongly prints the mistake, the command's usage and where its examples are:

```text
$ tap list --jsno
tap: list has no flag --jsno. Did you mean --json?

Usage: tap list [--json]
Run "tap list --help" for examples.
```

## List

`tap list` connects to every server and prints how many tools it has, or why it is unavailable:

```text
files   13 tools
linear  unavailable: needs you to sign in: run "tap auth linear"
docs    unavailable: its address could not be reached
```

## Search

```sh
tap search echo
```

```text
files.echo
  Echo the supplied message.
```

Every term must match a tool's name, server, title or description. `--json` shows the input schema
too, exactly what the agent gets:

```sh
tap search echo --json
```

```json
{
  "query": "echo",
  "total": 1,
  "matches": [
    {
      "id": "files.echo",
      "description": "Echo the supplied message.",
      "inputSchema": {
        "type": "object",
        "properties": { "message": { "type": "string" } },
        "required": ["message"],
        "$schema": "https://json-schema.org/draft/2020-12/schema"
      }
    }
  ],
  "unavailable": []
}
```

`tap list` and `tap search` wait for each server to list its tools afresh, so what they print is
how things are now. An agent's tap answers at once from the catalog it has saved and refreshes
it in the background; see [How it works](how-it-works.md).

## Call

Give arguments as `key=value` strings, as JSON with `--args`, or both (`key=value` wins):

```sh
tap call files.echo message=hi
tap call issues.create --args '{"title": "Login fails", "labels": ["bug"]}'
tap call db.query --args - < query.json
```

The tool's text is printed; `--json` prints the downstream result (HTTP uses the SDK's supported
fields; stdio retains raw response fields). When the tool reports an error, its content is still
printed, and the command exits 1:

```text
Input validation error: Invalid arguments for tool echo: message: Invalid input: expected string, received undefined
tap: files.echo reported an error
```
