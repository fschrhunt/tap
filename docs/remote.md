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

By default, `tap remote serve` starts paired-device HTTPS on port `8443`, bound
to all interfaces. It creates a persistent self-signed identity and prints its
SHA-256 fingerprint plus separate, single-use admin and execution pairing codes.
The codes expire after 15 minutes. Pairing codes are credentials: show them only
to people you intend to authorize. mDNS advertises the service when available,
but discovery does not authenticate or trust the host.

On each client, run `tap remote pair NAME https://HOST:8443`. The client shows
the certificate fingerprint it observed; compare it with the fingerprint on the
serving machine before continuing, then enter the one-time execution code. Pairing
uses TLS pinned to that exact certificate, and the device token is stored in an
owner-only local sidecar, separate from the shared config. Use `--role admin` only
for a device that needs to add/remove servers or manage sign-in. On the host,
`tap remote devices` lists paired clients and `tap remote revoke DEVICE_ID`
immediately revokes one. A revoked or lost device must be paired again.

The serving machine retains its self-signed identity and device registry beside
its tap config. Back them up securely if identity continuity matters. Do not copy
the private identity key or device registry to clients. Supply `--tls-cert` and
`--tls-key` to use your own certificate; clients still verify it by the displayed
fingerprint during pairing.

For compatibility, setting `TAP_REMOTE_TOKEN` selects the legacy bearer-token
mode instead. Do not put the token itself in command-line arguments or commit it
to configuration. For a local connection or a TLS reverse proxy's private backend:

```sh
export TAP_REMOTE_TOKEN='your-random-token'
tap remote serve --addr 127.0.0.1:8765
```

In legacy token mode, access from other machines can use HTTPS itself with your
certificate and private key:

```sh
tap remote serve --addr 0.0.0.0:8765 \
  --tls-cert /etc/tap/cert.pem --tls-key /etc/tap/key.pem
```

In legacy token mode, the certificate must be trusted by clients and valid for the
remote's hostname. Open the port in your firewall as appropriate. A reverse proxy
can terminate public TLS instead and forward to the loopback HTTP listener.

Non-loopback plaintext listeners in legacy token mode require explicit
`--allow-insecure` on both ends. This is only for trusted networks or private proxy
backends: bearer tokens and tool data travel in plaintext. Paired mode always uses
TLS and does not permit plaintext.

The serving machine itself doesn't select the remote: it already reads its
connectors locally, and an address bound to a LAN or VPN interface may not be
reachable from the host that binds it. Leave the serving machine on its local
registry.

## Save and select remotes

Give the local tap process the token through its environment, including the
environment of the harness that spawns it:

```sh
export TAP_REMOTE_TOKEN='your-random-token'
tap remote add home https://tap.example.com:8765
tap remote use home
tap search 'read file'
```

Save several endpoints with `tap remote add NAME URL`, see them with `tap remote list`,
and select one with `tap remote use NAME`. Profiles store the URL and token-variable name,
never the token itself. The older `tap remote use URL` form remains available for one-off
selection without a named profile. `tap remote remove NAME` removes a saved profile; `tap
remote off` returns to the local registry without deleting saved profiles.

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

A server waiting for a sign-in reports `needs you to sign in: run "tap auth NAME"`.
Run that command from the client: tap opens the browser locally, relays the callback
to the remote, and keeps the resulting grant on the serving machine. `tap auth NAME
--local` signs in to the local registry instead. Creating or removing remote grants
uses the remote administration credential.

`tap remote status` reports which target is configured but does not probe it; add `--check`
to verify reachability using the configured execution token. `tap remote serve` always hosts
this machine's local registry, even if this CLI has a different remote selected.

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

`tap config` always reads and changes settings for this local tap installation. It does not
configure the selected remote host. `tap import` is local-only because it reads this machine's
agent configs and may copy paths and commands that do not exist on the host.

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
