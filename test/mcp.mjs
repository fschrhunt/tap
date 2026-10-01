// Test the stdio MCP surface as a harness, using only local fixture servers.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const bin = fileURLToPath(new URL("../bin/tap.mjs", import.meta.url));
const fixture = fileURLToPath(new URL("./fixture.mjs", import.meta.url));
const definition = (...args) => ({ type: "stdio", command: [process.execPath, fixture, "--serve", ...args] });

// Start tap with an isolated registry and correlate newline-delimited JSON-RPC replies.
function harness(t, servers = { fixture: definition() }) {
  const dir = mkdtempSync(fileURLToPath(new URL("../.test-mcp-", import.meta.url)));
  const config = join(dir, "servers.json");
  writeFileSync(config, JSON.stringify({ servers }));
  const child = spawn(process.execPath, [bin], {
    env: { ...process.env, TAP_CONFIG: config }, stdio: ["pipe", "pipe", "pipe"],
  });
  const waiters = new Map();
  let buffer = "";
  let nextId = 1;
  let stderr = "";
  child.stderr.on("data", (chunk) => { stderr += chunk; });
  child.stdout.setEncoding("utf8");
  child.stdout.on("data", (chunk) => {
    buffer += chunk;
    let newline;
    while ((newline = buffer.indexOf("\n")) !== -1) {
      const message = JSON.parse(buffer.slice(0, newline));
      buffer = buffer.slice(newline + 1);
      const waiter = waiters.get(message.id);
      if (waiter) {
        waiters.delete(message.id);
        waiter(message);
      }
    }
  });
  // Reject outstanding requests promptly if tap cannot start or exits early.
  function failRequests(error) {
    for (const waiter of waiters.values()) waiter({ error: { message: error.message } });
    waiters.clear();
  }
  child.on("error", failRequests);
  child.on("exit", (code) => failRequests(new Error(`tap exited (${code}): ${stderr}`)));
  child.stdin.on("error", failRequests);
  t.after(async () => {
    child.stdin.end();
    if (child.exitCode === null && child.signalCode === null) {
      const exited = once(child, "exit");
      const timer = setTimeout(() => child.kill("SIGKILL"), 3_000);
      await exited;
      clearTimeout(timer);
    }
    rmSync(dir, { recursive: true, force: true });
  });

  // Bound harness requests so regressions fail instead of hanging the suite.
  function request(method, params = {}) {
    const id = nextId++;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        waiters.delete(id);
        reject(new Error(`${method} timed out: ${stderr}`));
      }, 12_000);
      waiters.set(id, (message) => {
        clearTimeout(timer);
        if (message.error) reject(new Error(message.error.message));
        else resolve(message.result);
      });
      child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", id, method, params })}\n`);
    });
  }

  // Negotiate MCP before sending tools requests, as a harness must.
  async function initialize() {
    const result = await request("initialize", {
      protocolVersion: "2025-11-25", capabilities: {}, clientInfo: { name: "test", version: "1" },
    });
    child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" })}\n`);
    return result;
  }
  return { config, child, request, initialize, stderr: () => stderr };
}

// Decode tap's catalog and search responses from their MCP text content.
async function search(h, args = {}) {
  const result = await h.request("tools/call", { name: "plugin_search", arguments: args });
  assert.notEqual(result.isError, true);
  return JSON.parse(result.content[0].text);
}

// Call a fixture tool through tap's plugin_call surface.
function call(h, tool, args = {}) {
  return h.request("tools/call", { name: "plugin_call", arguments: { tool: `fixture.${tool}`, arguments: args } });
}

test("initialize identifies tap and its tools capability", async (t) => {
  const h = harness(t);
  const init = await h.initialize();
  assert.equal(init.serverInfo.name, "tap");
  assert.ok(init.capabilities.tools);
  assert.equal(init.protocolVersion, "2025-11-25");
});

test("tap exposes exactly plugin_search and plugin_call", async (t) => {
  const h = harness(t);
  await h.initialize();
  const result = await h.request("tools/list");
  assert.deepEqual(result.tools.map(({ name }) => name).sort(), ["plugin_call", "plugin_search"]);
});

test("plugin_search with a query returns matching ids and input schemas", async (t) => {
  const h = harness(t);
  await h.initialize();
  const result = await search(h, { query: "echo message" });
  assert.equal(result.total, 1);
  assert.equal(result.matches[0].id, "fixture.echo");
  assert.equal(result.matches[0].inputSchema.properties.message.type, "string");
  assert.deepEqual(result.matches[0].inputSchema.required, ["message"]);
  assert.deepEqual(result.unavailable, []);
});

test("plugin_search without a query returns the server catalog", async (t) => {
  const h = harness(t);
  await h.initialize();
  assert.deepEqual(await search(h), { config: h.config, integrations: [{ server: "fixture", tools: 3 }] });
});

test("plugin_search with nonsense returns no matches and a recovery hint", async (t) => {
  const h = harness(t);
  await h.initialize();
  const result = await search(h, { query: "zzzznonexistent" });
  assert.equal(result.total, 0);
  assert.deepEqual(result.matches, []);
  assert.deepEqual(result.unavailable, []);
  assert.match(result.hint, /fewer or broader terms/);
});

test("plugin_call passes through content", async (t) => {
  const h = harness(t);
  await h.initialize();
  const result = await call(h, "echo", { message: "hello from the harness" });
  assert.deepEqual(result.content, [{ type: "text", text: "hello from the harness" }]);
});

test("plugin_call passes through isError", async (t) => {
  const h = harness(t);
  await h.initialize();
  const result = await call(h, "fail");
  assert.equal(result.isError, true);
  assert.deepEqual(result.content, [{ type: "text", text: "fixture failure" }]);
});

test("plugin_call passes through structuredContent", async (t) => {
  const h = harness(t);
  await h.initialize();
  assert.deepEqual((await call(h, "data")).structuredContent, { count: 3 });
});

for (const mode of ["connect", "list"]) {
  test(`a hung ${mode} becomes unavailable within the per-server deadline`, async (t) => {
    const h = harness(t, { fixture: definition(), hung: definition(`--hang-${mode}`) });
    await h.initialize();
    const started = performance.now();
    const result = await search(h, { query: "echo" });
    const elapsed = performance.now() - started;
    assert.ok(elapsed >= 4_500 && elapsed < 8_000, `deadline took ${elapsed} ms`);
    assert.equal(result.matches[0].id, "fixture.echo");
    assert.deepEqual(result.unavailable, [{ server: "hung", error: "server deadline exceeded (5000 ms)" }]);
  });
}

test("a timed-out server is retried on a later search", async (t) => {
  const h = harness(t, { fixture: definition("--hang-connect") });
  await h.initialize();
  const first = await search(h, { query: "echo" });
  assert.equal(first.unavailable.length, 1);
  writeFileSync(h.config, JSON.stringify({ servers: { fixture: definition() } }));
  const second = await search(h, { query: "echo" });
  assert.deepEqual(second.unavailable, []);
  assert.equal(second.matches[0].id, "fixture.echo");
});

test("MCP mode inherits downstream stderr", async (t) => {
  const h = harness(t, { fixture: definition("--noisy") });
  await h.initialize();
  await search(h);
  assert.match(h.stderr(), /fixture stderr/);
});

test("tap exits on its own when the harness closes stdin", async (t) => {
  const h = harness(t);
  await h.initialize();
  await call(h, "echo", { message: "connect the stdio server first" });
  const exited = once(h.child, "exit");
  h.child.stdin.end();
  const timer = setTimeout(() => h.child.kill("SIGKILL"), 3_000);
  const [code, signal] = await exited;
  clearTimeout(timer);
  assert.equal(signal, null, "tap had to be killed instead of exiting");
  assert.equal(code, 0);
});
