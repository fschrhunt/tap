#!/usr/bin/env node
// `tap` entry point.
//
// No arguments runs the stdio MCP server a harness spawns. The subcommands
// exist so a human (or a shell) can exercise the same engine without a harness
// in the loop, and so the server list is editable without hand-writing JSON.
import { serve } from "../src/server.mjs";
import {
  VERSION,
  addServer,
  call,
  closeAll,
  configPath,
  listing,
  removeServer,
  search,
  toMessage,
} from "../src/mcp.mjs";

const USAGE = `tap ${VERSION} — Less noise. Better agents.

  tap                      run the MCP server over stdio (what a harness spawns)
  tap list                 list configured integrations and their tool counts
  tap add <name> <url>     add a streamable-HTTP server
  tap add <name> -- <cmd>  add a stdio server (everything after -- is the command)
  tap remove <name>        remove a server
  tap search <query>       find tools, with the schemas needed to call them
  tap call <server.tool> [k=v ...] [--args '<json>']
  tap path                 print the config file tap reads
  tap version              print the version

Add flags: --env K=V, --header K=V, --cwd DIR, --bearer-token-env NAME
           (--env and --header may repeat)
Other flags: --json prints raw output; --limit N caps search results.`;

// Remove one option and return its following value, if present.
function takeFlag(args, name) {
  const index = args.indexOf(name);
  if (index === -1) return undefined;
  return args.splice(index, 2)[1];
}

// Remove repeated options; reject any option without a value.
function takeRepeats(args, name) {
  const values = [];
  for (let index = args.indexOf(name); index !== -1; index = args.indexOf(name)) {
    const value = args.splice(index, 2)[1];
    if (value === undefined) throw new Error(`${name} needs a value`);
    values.push(value);
  }
  return values;
}

// Turn KEY=VALUE arguments into an object, rejecting malformed pairs.
function parsePairs(pairs, label) {
  const out = {};
  for (const pair of pairs) {
    const eq = pair.indexOf("=");
    if (eq < 1) throw new Error(`expected ${label} KEY=VALUE, got "${pair}"`);
    out[pair.slice(0, eq)] = pair.slice(eq + 1);
  }
  return out;
}

// Remove a boolean option and report whether it was present.
function takeBool(args, name) {
  const index = args.indexOf(name);
  if (index === -1) return false;
  args.splice(index, 1);
  return true;
}

// Combine JSON arguments with string key=value overrides.
function parseToolArgs(args) {
  let values;
  const raw = takeFlag(args, "--args");
  if (raw) values = JSON.parse(raw);
  for (const pair of args) {
    const eq = pair.indexOf("=");
    if (eq < 1) throw new Error(`expected key=value, got "${pair}"`);
    values ??= {};
    values[pair.slice(0, eq)] = pair.slice(eq + 1);
  }
  return values ?? {};
}

// Print one value, optionally encoded as JSON.
const print = (value, json) =>
  process.stdout.write(json ? `${JSON.stringify(value, null, 2)}\n` : `${value}\n`);

let serving = false;

// Dispatch the CLI; only a bare invocation serves MCP over stdio.
async function main() {
  const args = process.argv.slice(2);
  const command = args.shift();

  if (!command) {
    serving = true;
    return serve();
  }

  // Everything after the first "--" belongs to a server command, never to tap's own flags.
  const dash = args.indexOf("--");
  const rest = dash === -1 ? [] : args.splice(dash);
  const json = takeBool(args, "--json");

  if (command === "help" || command === "-h" || command === "--help") return print(USAGE, false);
  if (command === "version" || command === "-v" || command === "--version") return print(VERSION, false);
  if (command === "path") return print(configPath(), false);

  if (command === "add") {
    const name = args.shift();
    if (!name) throw new Error("usage: tap add <name> <url> | tap add <name> -- <command> [args...]");
    const env = parsePairs(takeRepeats(args, "--env"), "--env");
    const headers = parsePairs(takeRepeats(args, "--header"), "--header");
    const cwd = takeFlag(args, "--cwd");
    const bearerTokenEnv = takeFlag(args, "--bearer-token-env");

    if (rest.length) {
      const command = rest.slice(1);
      if (!command.length) throw new Error("usage: tap add <name> -- <command> [args...]");
      addServer(name, {
        type: "stdio",
        command,
        ...(Object.keys(env).length ? { env } : {}),
        ...(cwd ? { cwd } : {}),
      });
      return print(`added ${name} (stdio)`, false);
    }

    const url = args.shift();
    if (!url) throw new Error("usage: tap add <name> <url> | tap add <name> -- <command> [args...]");
    addServer(name, {
      type: "http",
      url,
      ...(Object.keys(headers).length ? { headers } : {}),
      ...(bearerTokenEnv ? { bearerTokenEnv } : {}),
    });
    return print(`added ${name}`, false);
  }

  if (command === "remove") {
    const [name] = args;
    if (!name) throw new Error("usage: tap remove <name>");
    return print(removeServer(name) ? `removed ${name}` : `no server named ${name}`, false);
  }

  if (command === "list") {
    const { config, integrations: rows } = await listing({ quiet: true });
    if (json) return print({ config, integrations: rows }, true);
    if (!rows.length) return print(`no servers configured (${config})`, false);
    return print(
      rows
        .map((row) => `${row.server} — ${row.error ? `unavailable: ${row.error}` : `${row.tools} tools`}`)
        .join("\n"),
      false,
    );
  }

  if (command === "search") {
    const limit = Number(takeFlag(args, "--limit") ?? 8);
    const result = await search(args.join(" "), { limit, quiet: true });
    if (json) return print(result, true);
    const lines = result.matches.map(
      (tool) => `${tool.id}\n  ${tool.description ?? ""}`.trimEnd(),
    );
    if (!lines.length) lines.push(result.hint ?? `no matching tools for "${result.query}"`);
    if (result.total > result.matches.length) {
      lines.push(`(${result.total - result.matches.length} more; raise --limit to see them)`);
    }
    for (const row of result.unavailable) {
      lines.push(`unavailable: ${row.server} — ${row.error}`);
    }
    return print(lines.join("\n"), false);
  }

  if (command === "call") {
    const toolId = args.shift();
    if (!toolId) throw new Error("usage: tap call <server.tool> [key=value ...]");
    const result = await call(toolId, parseToolArgs(args), { quiet: true });
    if (json) return print(result, true);
    const text = (result?.content ?? [])
      .map((part) => (part.type === "text" ? part.text : JSON.stringify(part)))
      .join("\n");
    print(text || JSON.stringify(result, null, 2), false);
    // The CLI itself succeeded; the tool it ran may still have failed.
    if (result?.isError) process.stderr.write(`tap: ${toolId} reported an error\n`);
    return;
  }

  print(USAGE, false);
  process.exitCode = 1;
}

main()
  .then(async () => {
    if (!serving) await closeAll();
  })
  .catch(async (error) => {
    process.stderr.write(`tap: ${toMessage(error)}\n`);
    await closeAll();
    process.exitCode = 1;
  });
