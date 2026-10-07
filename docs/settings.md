# Settings

tap works without settings. These are for when the default is wrong for you: a server slow
enough that you want it ready before the first call, a model with a small context, or result
references you want your agent to use.

```sh
tap config                                        # every setting and where its value comes from
tap config set start search                       # refresh stale tool lists during search
tap config set start start --server playwright    # start this one with tap and keep it running
tap config set references on
tap config unset start --server playwright        # back to the general value
```

```text
$ tap config
start           call    default  when tap starts a server: call, search or start
references      on      config   let an agent keep large results as references: on or off
searchLimit     8       default  how many tools a search returns unless asked for more (1 to 25)
searchMaxBytes  32768   default  how much a search may return unless asked for more (1024 to 16777216)
deadlineMs      5000    default  how long a server may take to connect and list its tools
idleTimeoutMs   300000  default  how long an unused server keeps running
retryAfterMs    5000    default  how long tap waits before trying a failed server again

Servers with their own:
playwright  start: start
```

An agent's tap reads its settings when it starts, so start a new agent session after changing
one. `tap` commands are new processes and use a change at once.
For a remote host, restart the serving `tap remote serve` process after changing its settings.
`tap config unset NAME` removes the saved value; it does not override an environment variable.

## When servers start

`start` is how lazy tap is. Set it for every server, or with `--server` for one.

| Value | A server starts | Its tools are checked again |
| --- | --- | --- |
| `call` (default) | when one of its tools is called | when it runs: for a call, a refresh or a change it announces |
| `search` | when a search needs its tools | on a search, when its list was saved by another session or is a minute old |
| `start` | as soon as tap starts, and keeps running | while it runs |

With `call`, a search can answer from the tools tap last saved without starting the server;
restored or expired catalogs are marked `stale: true`. A search for a server tap has never listed still
starts that server, since there is nothing saved to answer from. A call always starts its
server and checks the tool against what the server lists now, whatever the setting. So `call`
costs nothing in safety: the trade is that a tool a server adds is found by search only after
that server has run again, or after `tap refresh`.

Use `search` if your servers change their tools often: stale lists refresh in the background.
Use `tap refresh NAME` or `tap search QUERY --refresh` when you need to wait for a live list.
Use `start` for a server that is slow to start and that you use in most sessions. It stays
running unless you give that stdio server its own `idleTimeoutMs` override.

## Everything else

| Setting | Default | Environment | What it does |
| --- | --- | --- | --- |
| `references` | `off` | `TAP_REFERENCES` | Offer result references to the agent: see [How it works](how-it-works.md#opt-in-result-references). Off keeps the call contract to `tool` and `arguments` |
| `searchLimit` | `8` | | How many tools a search returns when the agent does not say. `tap search` uses it too |
| `searchMaxBytes` | `32768` | | MCP search byte budget when the agent does not specify one; with `detail: auto`, schemas become summaries when needed. CLI search defaults to full schemas and 16777216 bytes |
| `deadlineMs` | `5000` | `TAP_DEADLINE_MS` | How long a server may take to connect and list its tools. A tool call itself has no limit |
| `idleTimeoutMs` | `300000` | `TAP_IDLE_TTL_MS` | How long an unused session stays open. General range: 0–86400000 ms; `0` closes as soon as idle. A stdio server override is 1–86400000 ms |
| `retryAfterMs` | `5000` | `TAP_FAIL_TTL_MS` | How long tap waits before trying a server that failed again. `0` tries every time |

An environment variable wins over the corresponding general setting in the config, so a
setting can differ for one agent without changing the file. A server's own `start` or
`idleTimeoutMs` overrides the general value. A variable holding an invalid value is ignored.
Set variables in the environment that launches tap; GUI agents may not inherit your shell's environment.

## In the config file

Settings are kept in `servers.json` beside the servers, and a server's own values in its
definition:

```json
{
  "servers": {
    "playwright": { "type": "stdio", "command": ["npx", "-y", "@playwright/mcp@latest"], "start": "start" }
  },
  "settings": { "references": true, "searchLimit": 5 }
}
```

tap refuses a config with a setting it does not know, or a value a setting does not take, and
says which. Numbers are numbers and `references` is `true` or `false`.

With a [remote](remote.md) selected, settings for the servers it runs are kept on the machine
that serves them; `tap config` always shows and changes this machine's file.
