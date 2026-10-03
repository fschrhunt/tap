# Remote

Run connectors once on a machine that your agents can reach. Every agent still
launches `tap` over stdio and loads just `plugin_search` and `plugin_call`. Local
tap forwards discovery and calls; connector processes, upstream credentials and
tool caches stay on the remote.

No VPN, Tailscale, dotfile synchronization or harness-specific remote configuration
is required. Install tap and register it with each harness once, then select a
remote on each machine.

## Start the remote

On the machine hosting the connectors, configure them normally:

```sh
tap add files -- npx -y @modelcontextprotocol/server-filesystem /srv/notes
tap add docs https://docs.example.com/mcp --bearer-token-env DOCS_TOKEN
```

Set a strong, randomly generated token in the remote process's environment. Do
not put the token itself in command-line arguments or commit it to configuration.
For a local connection or a TLS reverse proxy's private backend:

```sh
export TAP_REMOTE_TOKEN='your-random-token'
tap remote serve --addr 127.0.0.1:8765
```

For access from other machines, tap can serve HTTPS itself using your certificate
and private key:

```sh
tap remote serve --addr 0.0.0.0:8765 \
  --tls-cert /etc/tap/cert.pem --tls-key /etc/tap/key.pem
```

The certificate must be trusted by clients and valid for the remote's hostname.
Tap does not provision certificates or bypass certificate verification. Open the
port in your firewall as appropriate. A reverse proxy can terminate public TLS
instead and forward to the loopback HTTP listener.

Non-loopback plaintext listeners require explicit `--allow-insecure`. Clients
likewise require explicit consent for non-loopback HTTP. This is only for trusted
networks or private proxy backends: bearer tokens and tool data travel in plaintext.

The serving machine itself doesn't select the remote: it already reads its
connectors locally, and an address bound to a LAN or VPN interface may not be
reachable from the host that binds it. Leave the serving machine on its local
registry.

## Select it on each client

Give the local tap process the token through its environment, including the
environment of the harness that spawns it:

```sh
export TAP_REMOTE_TOKEN='your-random-token'
tap remote use https://tap.example.com:8765
tap search 'read file'
```

For a local deployment, use `http://127.0.0.1:8765`. `--token-env NAME` selects
another environment variable instead of `TAP_REMOTE_TOKEN`. Configuration stores
the variable's name, not its secret value.

Harness configurations do not change: `claude mcp add --scope user tap -- tap`,
Codex's `command = "tap"`, or OpenCode's local `command: ["tap"]` still work.
The connection is opened only when needed, not during session initialization.

## Add once, discover everywhere

With a remote selected, the ordinary CLI commands use it:

```sh
tap add issues https://issues.example.com/mcp --bearer-token-env ISSUES_TOKEN
tap list
tap remove issues
```

The next search from an already-running agent sees additions. A changed or removed
definition invalidates the remote's old connector session. No restart, schema
push into model context or dotfile synchronization is needed.

All command paths, filesystem access, working directories and environment
references resolve **on the remote**, not on the client. For example,
`--bearer-token-env ISSUES_TOKEN` reads the remote process's environment.

A server waiting for a sign-in reports it through the remote too —
`needs you to sign in: run "tap auth NAME" on the machine running tap remote serve` —
because the sign-in lives on that machine. Every other connector error stays
generic, so the relay never carries host diagnostics.

`tap remote off` returns to the saved local registry. CLI `--local` explicitly
uses local connectors while a remote is configured. Local and remote registries
are not merged, avoiding ambiguous tool IDs. Remote outages are errors, never a
silent switch to a similarly named local tool.

## Credentials and trust

The default token authorizes both execution and administration. For agents that
should call tools but not add executable commands, set a distinct
`TAP_REMOTE_ADMIN_TOKEN` on the remote and give them only `TAP_REMOTE_TOKEN`.
Administrative CLI commands must then use the admin credential; an administrator
can set `TAP_REMOTE_ADMIN_TOKEN` on their client, which `add` and `remove` use
instead of the execution token. Ordinary agents should not receive that variable.

HTTP results preserve fields supported by the MCP SDK, including content,
structured output and application metadata. Unknown protocol extensions are not
an opaque byte-for-byte relay.

The listener defaults to `127.0.0.1:7777`; examples use an explicit port so you
can choose one appropriate for your deployment.

The host retains at most 64 active MCP sessions. New session admissions are serialized;
busy admissions and exhausted capacity return HTTP 429 with `Retry-After`. Close unused
sessions explicitly; the SDK also cleans up failed initializations and closes sessions after
30 minutes without client requests. Active calls are not idle-expired.

Both the MCP endpoint (`/mcp`) and the administrative server endpoints are
authenticated. Clients reject redirects instead of sending credentials to
another endpoint. The admin API does not publish configured secrets.

This is a **single-owner service**: agents share upstream identities and connector
sessions. It does not isolate mutually untrusted users or support full OAuth,
sampling, elicitation, progress forwarding or asynchronous MCP continuation
workflows. Upstream tool execution is not automatically retried after a failure,
since an operation may have taken effect before the connection was lost.

Remote CLI searches use the MCP limit range (`1`–`25`), unlike the legacy local
CLI's unconstrained `--limit` behavior. Connection setup and administrative edits
have ten-second deadlines; tool execution remains governed by caller cancellation.

Approving `plugin_call` broadly may approve destructive downstream tools too;
search safety hints are not access control. See [Security](../SECURITY.md).
