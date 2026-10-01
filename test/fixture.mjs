// A stdio MCP fixture with success, error, structured, and hung responses.
// Start with --serve; importing or discovering this file does not start a server.
import { McpServer } from "@modelcontextprotocol/server";
import { serveStdio } from "@modelcontextprotocol/server/stdio";
import { z } from "zod";

// Build deterministic tools; hang modes exercise initialization and listing deadlines.
function fixture() {
  const server = new McpServer(
    { name: "fixture", version: "1.0.0" },
    { capabilities: { tools: {} } },
  );
  server.registerTool("echo", {
    description: "Echo the supplied message.",
    inputSchema: z.object({ message: z.string() }),
  }, async ({ message }) => ({ content: [{ type: "text", text: message }] }));
  server.registerTool("fail", {
    description: "Return a tool error.", inputSchema: z.object({}),
  }, async () => ({ content: [{ type: "text", text: "fixture failure" }], isError: true }));
  server.registerTool("data", {
    description: "Return structured data.", inputSchema: z.object({}),
  }, async () => ({ content: [{ type: "text", text: "count: 3" }], structuredContent: { count: 3 } }));
  return server;
}

if (process.argv.includes("--serve")) {
  if (process.argv.includes("--noisy")) process.stderr.write("fixture stderr\n".repeat(10_000));
  if (process.argv.includes("--hang-connect")) {
    process.stdin.resume();
  } else if (process.argv.includes("--hang-list")) {
    // Answer initialization but never tools/list, independent of SDK timeouts.
    process.stdin.setEncoding("utf8");
    let buffer = "";
    process.stdin.on("data", (chunk) => {
      buffer += chunk;
      let newline;
      while ((newline = buffer.indexOf("\n")) !== -1) {
        const request = JSON.parse(buffer.slice(0, newline));
        buffer = buffer.slice(newline + 1);
        if (request.method === "initialize") {
          process.stdout.write(`${JSON.stringify({ jsonrpc: "2.0", id: request.id, result: {
            protocolVersion: request.params.protocolVersion,
            capabilities: { tools: {} }, serverInfo: { name: "fixture", version: "1.0.0" },
          } })}\n`);
        }
      }
    });
  } else {
    serveStdio(fixture);
  }
}
