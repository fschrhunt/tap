// Exercise CLI behavior with isolated config files and local stdio fixtures.
import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const execute = promisify(execFile);
const bin = fileURLToPath(new URL("../bin/tap.mjs", import.meta.url));
const fixture = fileURLToPath(new URL("./fixture.mjs", import.meta.url));
const version = JSON.parse(readFileSync(new URL("../package.json", import.meta.url))).version;

// Create a per-test registry inside this repo and remove it after the test.
function sandbox(t) {
  const dir = mkdtempSync(fileURLToPath(new URL("../.test-cli-", import.meta.url)));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const config = join(dir, "servers.json");
  return {
    config,
    read: () => JSON.parse(readFileSync(config, "utf8")),
    run: async (...args) => {
      try {
        return { status: 0, ...await execute(process.execPath, [bin, ...args], {
          env: { ...process.env, TAP_CONFIG: config, TAP_DEADLINE_MS: "500" }, encoding: "utf8", timeout: 15_000,
        }) };
      } catch (error) {
        if (typeof error.code !== "number") throw error;
        return { status: error.code, stdout: error.stdout, stderr: error.stderr };
      }
    },
  };
}

// Assert successful execution before reading a command's stdout.
function output(result) {
  assert.equal(result.error, undefined);
  assert.equal(result.status, 0, result.stderr);
  return result.stdout.trim();
}

test("version reads package.json", async (t) => {
  assert.equal(output(await sandbox(t).run("version")), version);
});

test("path honors TAP_CONFIG", async (t) => {
  const box = sandbox(t);
  assert.equal(output(await box.run("path")), box.config);
});

test("add stores an HTTP server with headers and bearerTokenEnv", async (t) => {
  const box = sandbox(t);
  output(await box.run("add", "remote", "https://example.invalid/mcp", "--header", "X-Trace=on", "--bearer-token-env", "TEST_TOKEN"));
  assert.deepEqual(box.read(), { servers: { remote: {
    type: "http", url: "https://example.invalid/mcp", headers: { "X-Trace": "on" }, bearerTokenEnv: "TEST_TOKEN",
  } } });
});

test("add stores a stdio command, environment, and cwd", async (t) => {
  const box = sandbox(t);
  output(await box.run("add", "fixture", "--env", "MODE=test", "--cwd", ".", "--", process.execPath, fixture, "--serve"));
  assert.deepEqual(box.read().servers.fixture, {
    type: "stdio", command: [process.execPath, fixture, "--serve"], env: { MODE: "test" }, cwd: ".",
  });
});

test("add rejects dots because tool ids use server.tool", async (t) => {
  const box = sandbox(t);
  const result = await box.run("add", "bad.name", "https://example.invalid/mcp");
  assert.equal(result.status, 1);
  assert.match(result.stderr, /cannot contain a dot.*server\.tool/);
});

test("saved configs are owner-only", { skip: process.platform === "win32" }, async (t) => {
  const box = sandbox(t);
  output(await box.run("add", "remote", "https://example.invalid/mcp"));
  assert.equal(statSync(box.config).mode & 0o777, 0o600);
});

test("remove deletes the named server", async (t) => {
  const box = sandbox(t);
  output(await box.run("add", "remote", "https://example.invalid/mcp"));
  assert.equal(output(await box.run("remove", "remote")), "removed remote");
  assert.deepEqual(box.read(), { servers: {} });
});

test("list reports fixture tools", async (t) => {
  const box = sandbox(t);
  output(await box.run("add", "fixture", "--", process.execPath, fixture, "--serve"));
  assert.deepEqual(JSON.parse(output(await box.run("list", "--json"))), {
    config: box.config, integrations: [{ server: "fixture", tools: 3 }],
  });
});

for (const command of ["list", "search", "call"]) {
  test(`${command} drains server stderr`, async (t) => {
    const box = sandbox(t);
    output(await box.run("add", "fixture", "--", process.execPath, fixture, "--serve", "--noisy"));
    const args = command === "search" ? ["echo"] : command === "call" ? ["fixture.echo", "message=hello"] : [];
    const result = await box.run(command, ...args, "--json");
    const body = JSON.parse(output(result));
    assert.equal(result.stderr, "");
    // Reaching the noisy server proves its stderr was drained instead of blocking it.
    if (command === "list") assert.equal(body.integrations[0].tools, 3);
    if (command === "search") {
      assert.equal(body.matches[0].id, "fixture.echo");
      assert.deepEqual(body.unavailable, []);
    }
  });
}

for (const alias of ["serve", "mcp"]) {
  test(`${alias} is rejected as an unknown command`, async (t) => {
    const result = await sandbox(t).run(alias);
    assert.equal(result.status, 1);
    assert.match(result.stdout, /tap list/);
  });
}

test("local server type is rejected", async (t) => {
  const box = sandbox(t);
  writeFileSync(box.config, JSON.stringify({ servers: { fixture: { type: "local", command: [process.execPath, fixture, "--serve"] } } }));
  const result = JSON.parse(output(await box.run("list", "--json")));
  assert.match(result.integrations[0].error, /unsupported type "local"/);
});

test("corrupt config errors name the file", async (t) => {
  const box = sandbox(t);
  writeFileSync(box.config, "{ invalid");
  const result = await box.run("list");
  assert.equal(result.status, 1);
  assert.ok(result.stderr.includes(box.config));
});

test("add keeps everything after -- as the server command", async (t) => {
  const box = sandbox(t);
  output(await box.run("add", "fs", "--", "some-server", "--cwd", "/srv", "--json"));
  assert.deepEqual(box.read().servers.fs, { type: "stdio", command: ["some-server", "--cwd", "/srv", "--json"] });
});
