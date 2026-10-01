// tap's own MCP client and registry.
//
// tap loads its own config, connects to configured servers with the MCP SDK,
// and answers two questions — "search" and "call".
// Connections and tool lists are cached per process so repeated searches stay
// cheap without hiding a server that was just added.
import { Client, StreamableHTTPClientTransport } from "@modelcontextprotocol/client";
import { StdioClientTransport } from "@modelcontextprotocol/client/stdio";
import { existsSync, mkdirSync, readFileSync, realpathSync, renameSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";

const pkg = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8"));
export const VERSION = pkg.version;

export const CONFIG_PATH =
  process.env.TAP_CONFIG || join(homedir(), ".tap", "servers.json");

const CACHE_TTL_MS = 60_000;
const DEADLINE_MS = 5_000;

// Return the config path fixed at process startup.
export function configPath() {
  return CONFIG_PATH;
}

// Convert a thrown value to a printable error message.
export function toMessage(error) {
  return error instanceof Error ? error.message : String(error);
}

// Read the configured registry; a missing file means no servers.
export function loadConfig() {
  if (!existsSync(CONFIG_PATH)) return { servers: {} };
  let parsed;
  try {
    parsed = JSON.parse(readFileSync(CONFIG_PATH, "utf8"));
  } catch (error) {
    throw new Error(`${CONFIG_PATH} is not valid JSON: ${toMessage(error)}`);
  }
  return { servers: parsed.servers ?? {} };
}

// Write through a temp file so a crash cannot leave a half-written config, and
// keep it owner-only because entries may carry headers or tokens.
// A symlinked config (say into a synced dotfiles folder) is written through, not replaced.
function saveConfig(config) {
  const target = existsSync(CONFIG_PATH) ? realpathSync(CONFIG_PATH) : CONFIG_PATH;
  mkdirSync(dirname(target), { recursive: true });
  const temporary = `${target}.${process.pid}.tmp`;
  writeFileSync(temporary, `${JSON.stringify(config, null, 2)}\n`, { mode: 0o600 });
  renameSync(temporary, target);
}

// Save one server definition; names must work in server.tool ids.
export function addServer(name, definition) {
  if (name.includes(".")) {
    throw new Error("server names cannot contain a dot: tool ids use server.tool");
  }
  const config = loadConfig();
  config.servers[name] = definition;
  saveConfig(config);
}

// Remove a configured server and report whether it existed.
export function removeServer(name) {
  const config = loadConfig();
  if (!Object.hasOwn(config.servers, name)) return false;
  delete config.servers[name];
  saveConfig(config);
  return true;
}

// Return the configured server names without connecting.
export function servers() {
  return Object.keys(loadConfig().servers);
}

// Expand ${NAME} in headers and environment values; unknown names become empty strings.
function expand(value) {
  return String(value).replace(/\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g, (_, name) => process.env[name] ?? "");
}

// Expand static headers and add an environment-sourced bearer token.
function headersFor(definition) {
  const headers = Object.fromEntries(
    Object.entries(definition.headers ?? {}).map(([key, value]) => [key, expand(value)]),
  );
  const token = definition.bearerTokenEnv ? process.env[definition.bearerTokenEnv] : undefined;
  if (token) headers.Authorization = `Bearer ${token}`;
  return headers;
}

// Resolve a leading ~ so one shared config works on every machine, where the
// home directory differs.
function expandHome(value) {
  if (typeof value !== "string") return value;
  if (value === "~") return homedir();
  if (value.startsWith("~/")) return join(homedir(), value.slice(2));
  return value;
}

// Build an HTTP or stdio transport; quiet CLI runs drain stderr.
function transportFor(name, definition, { quiet = false } = {}) {
  const type = definition.type ?? (definition.url ? "http" : "stdio");

  if (type === "stdio") {
    const [command, ...args] = definition.command ?? [];
    if (!command) throw new Error(`server "${name}" has no command`);
    const env = Object.fromEntries(
      Object.entries(definition.env ?? {}).map(([key, value]) => [key, expand(value)]),
    );
    const transport = new StdioClientTransport({
      command: expandHome(command),
      args,
      cwd: expandHome(definition.cwd),
      env: { ...process.env, ...env },
      stderr: quiet ? "pipe" : "inherit",
    });
    if (quiet) transport.stderr?.resume();
    return transport;
  }

  if (type !== "http") throw new Error(`server "${name}" has unsupported type "${type}"`);
  if (!definition.url) throw new Error(`server "${name}" has no url`);
  const headers = headersFor(definition);
  return new StreamableHTTPClientTransport(new URL(definition.url), {
    requestInit: Object.keys(headers).length ? { headers } : undefined,
  });
}

const connections = new Map();
const toolCache = new Map();

// Bound an operation without waiting for an unresponsive server to finish.
async function withDeadline(operation) {
  let timer;
  try {
    return await Promise.race([
      operation(),
      new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error("server deadline exceeded (5000 ms)")), DEADLINE_MS);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

// Evict only this connection and close it without delaying a failed search.
function discard(name, entry) {
  if (connections.get(name) === entry) {
    connections.delete(name);
    toolCache.delete(name);
  }
  if (entry) void entry.client.close().catch(() => {});
}

// Share an in-flight connection; evict failures so the next request can retry.
async function connect(name, options = {}) {
  const existing = connections.get(name);
  if (existing) return existing.ready;
  const config = loadConfig();
  const definition = Object.hasOwn(config.servers, name) ? config.servers[name] : undefined;
  if (!definition) {
    throw new Error(
      `unknown server "${name}" (configured: ${Object.keys(config.servers).join(", ") || "none"})`,
    );
  }

  const client = new Client({ name: "tap", version: VERSION });
  const entry = { client };
  client.onclose = () => {
    if (connections.get(name) === entry) {
      connections.delete(name);
      toolCache.delete(name);
    }
  };
  connections.set(name, entry);
  entry.ready = withDeadline(() => client.connect(transportFor(name, definition, options)))
    .then(() => client)
    .catch((error) => {
      discard(name, entry);
      throw error;
    });
  return entry.ready;
}

// Cache schemas for one minute; connecting and listing share a five-second budget.
async function toolsFor(name, { refresh = false, quiet = false } = {}) {
  const cached = toolCache.get(name);
  if (!refresh && cached && Date.now() - cached.at < CACHE_TTL_MS) return cached.tools;
  const pending = connect(name, { quiet });
  const entry = connections.get(name);
  try {
    const { tools } = await withDeadline(async () => (await pending).listTools());
    if (connections.get(name) === entry) toolCache.set(name, { at: Date.now(), tools });
    return tools;
  } catch (error) {
    discard(name, entry);
    throw error;
  }
}

// Every tool across every configured server, tagged with its server. Servers
// are queried in parallel so one slow server does not stall the rest, and a
// server that fails to connect is reported rather than thrown, so one broken
// entry never hides the others.
export async function allTools({ refresh = false, quiet = false } = {}) {
  const perServer = await Promise.all(
    servers().map(async (name) => {
      try {
        const tools = await toolsFor(name, { refresh, quiet });
        return tools.map((tool) => ({ server: name, ...tool }));
      } catch (error) {
        return [{ server: name, error: toMessage(error) }];
      }
    }),
  );
  return perServer.flat();
}

// The server catalog when no query is given: name, reachability, tool count.
export async function catalog({ refresh = false, quiet = false } = {}) {
  const byServer = new Map();
  for (const entry of await allTools({ refresh, quiet })) {
    const row = byServer.get(entry.server) ?? { server: entry.server, tools: 0 };
    if (entry.error) row.error = entry.error;
    else row.tools += 1;
    byServer.set(entry.server, row);
  }
  return [...byServer.values()];
}

// The no-query answer: which servers are configured, where that config lives,
// and how many tools each one offers. Shared by the MCP surface and the CLI so
// both name the same fields.
export async function listing({ refresh = false, quiet = false } = {}) {
  return { config: CONFIG_PATH, integrations: await catalog({ refresh, quiet }) };
}

// Split text into comparable tokens: lowercase, and break camelCase and
// punctuation so "createIssue", "create_issue", and "create issue" agree.
function tokenize(text) {
  return String(text)
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .toLowerCase()
    .split(/[^a-z0-9]+/)
    .filter(Boolean);
}

// What an agent needs to call one tool: a `server.tool` id, the schema, and
// the server's safety hints. Transport detail stays out.
function describeTool(tool) {
  return {
    id: `${tool.server}.${tool.name}`,
    title: tool.title,
    description: tool.description,
    inputSchema: tool.inputSchema,
    annotations: tool.annotations,
  };
}

// Rank a tool only when every query token appears in its searchable fields.
function score(tool, tokens) {
  const haystack =
    `${tool.name} ${tool.server} ${tool.title ?? ""} ${tool.description ?? ""}`.toLowerCase();
  const nameTokens = new Set(tokenize(tool.name));
  let total = 0;
  for (const token of tokens) {
    if (!haystack.includes(token)) return 0;
    total += 1;
    if (nameTokens.has(token)) total += 2;
  }
  return total;
}

// Rank tools against a free-text query. Every token must appear somewhere,
// which keeps the shortlist relevant instead of merely fuzzy. The reply names
// the total before the limit and any server that could not be reached, so a
// short list is never mistaken for an empty one, and hints how to recover when
// nothing matches at all.
export async function search(query, { limit = 8, refresh = false, quiet = false } = {}) {
  const tokens = tokenize(query);
  const tools = await allTools({ refresh, quiet });
  const ranked = tools
    .filter((tool) => !tool.error)
    .map((tool) => ({ tool, rank: score(tool, tokens) }))
    .filter((entry) => entry.rank > 0)
    .sort((a, b) => b.rank - a.rank)
    .map((entry) => entry.tool);
  const unavailable = tools
    .filter((tool) => tool.error)
    .map((tool) => ({ server: tool.server, error: tool.error }));
  return {
    query,
    total: ranked.length,
    matches: ranked.slice(0, limit).map(describeTool),
    unavailable,
    ...(ranked.length || unavailable.length
      ? {}
      : {
          hint:
            "No tool matched every term. Try fewer or broader terms, " +
            "or omit the query to list the configured servers.",
        }),
  };
}

// Call a server.tool id and return the downstream MCP result unchanged.
export async function call(toolId, args = {}, { quiet = false } = {}) {
  const dot = toolId.indexOf(".");
  if (dot < 1) throw new Error(`invalid tool id "${toolId}" (expected server.tool)`);
  const server = toolId.slice(0, dot);
  const tool = toolId.slice(dot + 1);
  return (await connect(server, { quiet })).callTool({ name: tool, arguments: args });
}

// Close every transport so a one-shot CLI command can exit.
export async function closeAll() {
  const entries = [...connections.values()];
  connections.clear();
  toolCache.clear();
  await Promise.allSettled(entries.map(({ client }) => client.close()));
}
