# Command line

Started by an agent, `tap` with no arguments is the MCP server. The commands let you set tap up
from a shell and see what your agent sees. Run in a terminal with no arguments, `tap` prints a
short introduction instead of waiting for an agent that is not there.

| Command | Does |
| --- | --- |
| `tap` | Serve MCP over stdio |
| `tap connect [AGENT...]` | Point your coding agents at tap (see [Install](install.md)) |
| `tap import [SOURCE...]` | Add the servers your agents already have (see [Servers](servers.md)) |
| `tap add NAME URL` | Add an HTTP server |
| `tap add NAME -- COMMAND [ARGUMENT...]` | Add a stdio server |
| `tap remove NAME` | Remove a server and its saved sign-in |
| `tap list` | The servers and how many tools each has |
| `tap refresh [NAME]` | Ask one server, or all of them, for its tools again |
| `tap auth NAME` | Sign in to a server |
| `tap search QUERY...` | Find tools, with what is needed to call them |
| `tap inspect SERVER.TOOL` | Print one tool's whole contract |
| `tap call SERVER.TOOL [KEY=VALUE]... [--args JSON]` | Call a tool |
| `tap remote serve` | Host the registry with paired-device HTTPS |
| `tap remote pair NAME HTTPS_URL` | Pair this device after manually verifying the host fingerprint |
| `tap remote devices` | List devices paired to this serving machine |
| `tap remote revoke DEVICE_ID` | Revoke one paired device on this serving machine |
| `tap remote use NAME` | Select a saved remote for CLI commands and stdio relays |
| `tap remote list` | List saved remote profiles and the selected one |
| `tap remote remove NAME` | Remove a saved remote profile |
| `tap remote off` | Return to the saved local registry |
| `tap remote status [--check]` | Show the selected target; optionally check reachability |
| `tap config [NAME]` | Show the [settings](settings.md), or one of them |
| `tap config set NAME VALUE` | Change a setting; `--server NAME` for one server's own |
| `tap config unset NAME` | Return a setting to its default, or a server to the general value |
| `tap path` | Print the config file tap reads |
| `tap version` | Print the version |
| `tap help [COMMAND]` | Help for tap, or for one command |

## Help

Use `tap help` for every command, shared flags and environment variables. Use
`tap help COMMAND` for one command's usage, examples and flags:

```sh
tap help
tap help add
tap help remote
```

`tap --help`, `tap COMMAND --help` and `tap COMMAND -h` are supported aliases.
Help flags before `--` show help without running the command. After `--`, they belong to
the downstream command: `tap add files -- node server.mjs --help` adds that command as written.

## Flags

Flags may come before, between or after a command's arguments, as `--flag value` or
`--flag=value`. A flag a command does not have is an error, with the nearest one it does have.
Everything after `--` is taken as written: for `tap add`, it is the server's command.

| Flag | Commands | Meaning |
| --- | --- | --- |
| `-h`, `--help` | all | Show help |
| `--version` | `tap` | Print the version |
| `--json` | `import`, `list`, `refresh`, `search`, `call`, `config` | Print JSON instead of text |
| `--local` | `import`, `add`, `remove`, `list`, `refresh`, `search`, `inspect`, `call`, `auth` | Use this machine's servers while a remote is selected |
| `--cached` | `list` | Print the tool lists tap has saved, without asking the servers |
| `--server NAME` | `search`, `config` | Look only in this server; with no query, list its tools. For `config`, the server whose own setting to read or change |
| `--limit N` | `search` | How many tools to print (default: `searchLimit`, 8). Local: at least 1; remote: 1–25 |
| `--offset N` | `search` | Skip this many matches, to continue a longer list |
| `--refresh` | `search` | Ask the servers for their tools first |
| `--detail auto\|full\|summary` | `search` | Schema detail, visible with `--json`: whole schemas or summaries (default `full`) |
| `--max-bytes N` | `search` | Discovery response budget, 1024–16777216 bytes (default 16777216); applies before text or JSON rendering |
| `--args JSON` | `call` | The arguments as a JSON object; `-` reads standard input |
| `-n`, `--dry-run` | `import` | Show what would be added and add nothing |
| `-f`, `--force` | `import` | Replace servers tap already has under the same name |

With a remote selected, registry commands including `auth` use it; `--local` uses the
saved local registry. `config` always manages this local tap installation. `import` reads
this machine's agent files and requires `--local` when a remote is selected. See
[Remote](remote.md) for listener, TLS and authentication options.
`path` still prints the local config file that stores the remote selection.

## Output and exit codes

What you asked for is printed on standard output; anything tap says about it, such as an error,
a hint for what to run next, or a note that it is waiting on the servers, goes to standard
error. Hints and waiting notes are only written to a terminal, so pipes and logs stay clean.
Use `--json` in scripts: the text for people may change. Human output, diagnostics and
downstream stderr render terminal controls (including OSC sequences and Unicode format
controls) as visible escapes, preserving tabs and newlines. JSON output preserves data.

| Exit code | Meaning |
| --- | --- |
| `0` | The command did what was asked |
| `1` | It failed: a server could not be reached, a tool reported an error, a name does not exist |
| `2` | It was called wrongly: an unknown command or flag, a missing or malformed argument. Nothing was changed |

A command called wrongly prints the mistake, the command's usage and where its examples are:

```text
$ tap list --jsno
tap: list has no flag --jsno. Did you mean --json?

Usage: tap list [--cached] [--json]
Run "tap help list" for examples.
```

## List

`tap list` connects to every server and prints how many tools it has, or why it is unavailable:

```text
files   13 tools
linear  unavailable: needs you to sign in: run "tap auth linear"
docs    unavailable: its address could not be reached
```

`tap list --cached` prints the tool lists tap has saved, without starting or asking any server:

```text
files   13 tools (as last listed; not checked now)
```

`tap refresh NAME` asks one server for its tools again and saves what it answers; with no name,
it asks them all.

## Search

The following output is illustrative, for a server named `files` with an `echo` tool:

```sh
tap search echo --server files
```

```text
files.echo
  Echo the supplied message.
```

Words are matched against a tool's name, its server, its title and description, and its
parameters. Queries accept at most 4096 bytes and use the first 32 distinct normalized terms;
scopes over 65536 candidate tools must be narrowed with `--server`. A query that is a tool's name or id finds that tool. Otherwise the tools that hold
every retained query term are printed, best first, and when none holds them all, the tools that hold any.
`--server NAME` looks in one server only, and with no query lists that server's tools.
`--json` shows the input schema too, as the agent gets it:

```sh
tap search echo --server files --json
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
      },
      "schemaLoaded": true
    }
  ],
  "unavailable": [],
  "catalogs": [
    { "server": "files", "tools": 13, "source": "live", "observedAt": "2026-10-02T08:00:00Z", "availability": "reachable", "stale": false }
  ]
}
```

`tap search`, like an agent's tap, answers at once from the tool lists tap has saved, without
starting their servers; `stale: true` marks a match whose server has not answered since. The
[start setting](settings.md#when-servers-start) changes that. `--refresh` asks the servers first. `tap list` always asks them, so what it prints
is how things are now. See [How it works](how-it-works.md).

`tap inspect SERVER.TOOL` prints one tool's description and its input and output schemas as
JSON, asking only the server that has it.

## Call

Use the ID and schema returned by `tap search` or `tap inspect`. The tool names below are
examples, not built-in tap tools.

Give arguments as `key=value` strings, as JSON with `--args`, or both (`key=value` wins).
Use JSON for numbers, booleans, arrays and objects; `key=value` always supplies a string:

```sh
tap call files.echo message=hi
tap call issues.create --args '{"title": "Login fails", "labels": ["bug"]}'
tap call db.query --args - < query.json
```

tap does not expand `~` in tool arguments. For a local path, let your shell expand it:

```sh
tap call files.read_text_file path="$HOME/notes/todo.md"
```

With a remote selected, paths must exist on the remote; your shell's `$HOME` still refers to
the client. Use the remote path explicitly instead.

The tool's text is printed; `--json` prints the downstream result (HTTP uses the SDK's supported
fields; stdio retains raw response fields). When the tool reports an error, its content is still
printed, and the command exits 1:

```text
Input validation error: Invalid arguments for tool echo: message: Invalid input: expected string, received undefined
tap: files.echo reported an error
```
