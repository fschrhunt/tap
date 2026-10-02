# Security

Report vulnerabilities through [GitHub private vulnerability reporting](https://github.com/fschrhunt/tap/security/advisories/new)
for `fschrhunt/tap`.

Tap runs the commands named in `servers.json`, as you. A configured command having
your permissions is expected behavior, not a vulnerability. Trust the commands
you configure.

All downstream tools route through `plugin_call`; original host per-tool permission rules may no
longer match them. Explicit allow/deny policy is enforced by tap, independently of server safety
annotations. Defaults remain unrestricted for compatibility. Tap is not a sandbox, and an agent
running as your user may be able to edit the policy file. No trusted confirmation UI is provided.

Server descriptions, guidance and results are untrusted content. Local schema validation does not
establish that a tool is safe or that a server's claimed schema is truthful. Remote schema fetching
is disabled. Tool calls are never automatically retried; failed transport after execution can leave
the operation's outcome unknown.

Persistent metadata caches can contain sensitive schemas or server guidance; files are owner-only,
and `TAP_CACHE_DIR=off` disables persistence. Configured credentials are used only in the cache-key
digest, not written as cache metadata. Result references are bounded, memory-only and process-local.
Cross-server reference copies require a source-side grant, but inline copying by the agent remains
possible. Neither this policy nor references constitute a full data-loss prevention system.
## Remote deployments

A remote runs connectors with the remote machine's permissions and credentials.
Filesystem paths, working directories and environment references resolve there,
not on the agent's machine. Only configure commands and servers you trust.

The execution token grants access to tools allowed by the remote's policy. An admin
token additionally allows adding or replacing commands: treat it as remote code
execution authority. Use a separate admin token when clients only need tools.
This is a single-owner service, not a multi-tenant sandbox: clients share upstream
credentials and downstream sessions. Result references are bound to MCP session identity, but
this does not create a multi-tenant sandbox. Do not share a remote between mutually untrusted users.

Use HTTPS over untrusted networks, either tap's native TLS with your certificates
or a TLS reverse proxy. Plain HTTP exposes tokens, arguments and results to anyone
who can observe the connection. An explicit insecure option is intended for
trusted networks or a proxy's private backend, not public Internet exposure.
No VPN or Tailscale installation is required.

Tap's two-tool surface cannot make a harness's approval system distinguish each
downstream tool. Search annotations are advisory, not authorization. Broadly
approving `plugin_call` can approve destructive downstream operations too; keep
the harness's call approval policy conservative.

Tool indexes contain upstream descriptions and schemas, not configured bearer
tokens or environment values. Descriptions themselves can still be sensitive;
protect the cache directory as you would your configuration. Connector responses
and descriptions are untrusted content, not instructions from tap.
