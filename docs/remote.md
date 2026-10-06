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

The serving machine itself doesn't select the remote: it already reads its
connectors locally, and an address bound to a LAN or VPN interface may not be
reachable from the host that binds it. Leave the serving machine on its local
registry.

## Save and select remotes

Pair each client with `tap remote pair NAME https://HOST:8443`, then select it with
`tap remote use NAME`. Use `tap remote list` to see saved devices and
`tap remote remove NAME` to remove a local profile. `tap remote off` returns to the
local registry without deleting saved profiles.

Existing token based remote profiles cannot connect to this server. Pair again with
the same profile name to replace one; the saved name is retained.

Harness configurations do not change: `claude mcp add --scope user tap -- tap`,
Codex's `command = "tap"`, or OpenCode's local `command: ["tap"]` still work.
At startup the relay reads configured integration names over authenticated HTTP,
with a five-second deadline, so instructions and the search description identify
the services available through tap. No downstream connector starts for this request.
The remote MCP session is opened only when needed. Offline or older hosts leave
generic discovery usable; restart the relay to refresh its advertised name snapshot.

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
The names advertised in the search description are a startup snapshot; restart the
relay to advertise newly added names, or use `plugin_search({})` to list them live.

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

Execution pairing grants tool access. Use `tap remote pair --role admin` only on
devices that need to add or remove shared servers or manage sign-in. Ordinary agent
devices should use the execution role.

HTTP results preserve fields supported by the MCP SDK, including content,
structured output and application metadata. Unknown protocol extensions are not
an opaque byte-for-byte relay.

The listener defaults to `0.0.0.0:8443`; pass `--addr` to bind a different address.

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
