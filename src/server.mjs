// The MCP surface `tap` presents to a harness.
//
// Exactly two tools, so a session pays almost no context for MCP being
// available: `plugin_search` reveals what exists (and its input schemas) only
// when asked, and `plugin_call` runs one. There is no third "list everything"
// tool, and no schemas are embedded here — they come from the live registry at
// search time.
import { McpServer } from "@modelcontextprotocol/server";
import { serveStdio } from "@modelcontextprotocol/server/stdio";
import { z } from "zod";
import { VERSION, call, closeAll, listing, search, toMessage } from "./mcp.mjs";

// Encode a catalog or search result as MCP text content.
const asText = (value) => ({
  content: [
    {
      type: "text",
      text: typeof value === "string" ? value : JSON.stringify(value, null, 2),
    },
  ],
});

// Return an MCP tool failure without failing the protocol request.
const asError = (message) => ({ content: [{ type: "text", text: message }], isError: true });

// Build the two-tool MCP surface, connecting downstream only on demand.
function buildServer() {
  const server = new McpServer(
    { name: "tap", version: VERSION },
    {
      capabilities: { tools: {} },
      instructions:
        "MCP tools are reached lazily. Call plugin_search with a capability query to " +
        "get matching tools as `server.tool` ids plus the input schema, then call one " +
        "with plugin_call. Nothing else is loaded up front.",
    },
  );

  server.registerTool(
    "plugin_search",
    {
      title: "Search MCP integrations",
      description:
        "Find the MCP tools available to you. With a query, returns matching tools " +
        "with the `id`, input schema, and safety hints needed to call them, plus the " +
        "total match count and any unreachable server. With no query, lists the " +
        "configured integrations. Call a result with plugin_call.",
      annotations: {
        readOnlyHint: true,
        idempotentHint: true,
        openWorldHint: true,
      },
      inputSchema: z.object({
        query: z
          .string()
          .optional()
          .describe("Capability to find. Omit to list the integrations instead."),
        limit: z
          .number()
          .int()
          .min(1)
          .max(25)
          .optional()
          .describe("Maximum tools to return. Defaults to 8."),
      }),
    },
    async ({ query, limit }) => {
      try {
        if (!query) return asText(await listing());
        return asText(await search(query, { limit: limit ?? 8 }));
      } catch (error) {
        return asError(`plugin_search failed: ${toMessage(error)}`);
      }
    },
  );

  server.registerTool(
    "plugin_call",
    {
      title: "Call an MCP tool",
      description:
        "Run a tool discovered with plugin_search. Pass its `id` and an arguments " +
        "object matching the schema plugin_search returned.",
      inputSchema: z.object({
        tool: z.string().describe("Tool id from plugin_search, written as server.tool."),
        arguments: z
          .record(z.string(), z.unknown())
          .optional()
          .describe("Arguments for the tool, matching its input schema."),
      }),
    },
    async ({ tool, arguments: args }) => {
      try {
        const result = await call(tool, args ?? {});
        const content =
          Array.isArray(result?.content) && result.content.length
            ? result.content
            : [{ type: "text", text: JSON.stringify(result, null, 2) }];
        // Pass the downstream result through instead of stringifying it: the
        // harness should see the real text, not a JSON-quoted copy of it.
        return {
          content,
          isError: Boolean(result?.isError),
          ...(result?.structuredContent !== undefined
            ? { structuredContent: result.structuredContent }
            : {}),
        };
      } catch (error) {
        return asError(
          `${tool} failed: ${toMessage(error)}. ` +
            "If the tool id or arguments are wrong, run plugin_search to look them up.",
        );
      }
    },
  );

  return server;
}

// Start serving tap over stdio; returns a handle whose close() tears the connection down. When
// the harness closes stdin, tap closes every downstream server and exits, so nothing is orphaned.
export function serve() {
  const handle = serveStdio(buildServer, {
    onerror: (error) => process.stderr.write(`tap: ${error.message}\n`),
  });
  process.stdin.once("end", () => {
    Promise.resolve(handle.close?.())
      .then(closeAll)
      .finally(() => process.exit(0));
  });
  return handle;
}
