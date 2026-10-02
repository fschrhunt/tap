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
