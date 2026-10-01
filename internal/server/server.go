// Package server exposes tap's two-tool MCP surface over stdio. The SDK answers tools/list and
// wraps every result, so tap follows whichever MCP revision each client speaks; tap supplies the
// tools' definitions, argument checks, results and its two JSON-RPC refusals.
package server

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"strings"
	"sync"

	"github.com/fschrhunt/tap/internal/registry"
	"github.com/fschrhunt/tap/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const instructions = "MCP tools are reached lazily. Call plugin_search with a capability query to get matching tools as `server.tool` ids plus the input schema, then call one with plugin_call. Nothing else is loaded up front."
const definitions = "[{\"name\":\"plugin_search\",\"title\":\"Search MCP integrations\",\"description\":\"Find the MCP tools available to you. With a query, returns matching tools with the `id`, input schema, and safety hints needed to call them, plus the total match count and any unreachable server. With no query, lists the configured integrations. Call a result with plugin_call.\",\"inputSchema\":{\"type\":\"object\",\"$schema\":\"https://json-schema.org/draft/2020-12/schema\",\"properties\":{\"query\":{\"description\":\"Capability to find. Omit to list the integrations instead.\",\"type\":\"string\"},\"limit\":{\"description\":\"Maximum tools to return. Defaults to 8.\",\"type\":\"integer\",\"minimum\":1,\"maximum\":25}}},\"annotations\":{\"readOnlyHint\":true,\"idempotentHint\":true,\"openWorldHint\":true}},{\"name\":\"plugin_call\",\"title\":\"Call an MCP tool\",\"description\":\"Run a tool discovered with plugin_search. Pass its `id` and an arguments object matching the schema plugin_search returned.\",\"inputSchema\":{\"type\":\"object\",\"$schema\":\"https://json-schema.org/draft/2020-12/schema\",\"properties\":{\"tool\":{\"type\":\"string\",\"description\":\"Tool id from plugin_search, written as server.tool.\"},\"arguments\":{\"description\":\"Arguments for the tool, matching its input schema.\",\"type\":\"object\",\"propertyNames\":{\"type\":\"string\"},\"additionalProperties\":{}}},\"required\":[\"tool\"]}}]"

// Serve runs until stdin closes; its caller closes all downstream connections. The SDK answers
// tools/list and wraps every result, so both follow whichever protocol revision each client speaks;
// tap supplies the two tools' definitions, their argument checks and their results.
func Serve(ctx context.Context, e *registry.Engine) error {
	var tools []*mcp.Tool
	if err := json.Unmarshal([]byte(definitions), &tools); err != nil {
		return err
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "tap", Version: e.Version}, &mcp.ServerOptions{Instructions: instructions, Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}})
	for _, tool := range tools {
		name := tool.Name
		s.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := wire.Object{}
			if len(req.Params.Arguments) > 0 {
				if v, err := wire.Decode(req.Params.Arguments); err == nil {
					args, _ = v.(wire.Object)
				}
			}
			if err := validate(name, args); err != "" {
				return toolError("Input validation error: Invalid arguments for tool " + name + ": " + err), nil
			}
			return invoke(ctx, e, name, args), nil
		})
	}
	// Two refusals keep tap's own JSON-RPC errors: an unknown tool, and arguments that are not an object.
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			p := req.GetParams().(*mcp.CallToolParamsRaw)
			if p.Name != "plugin_search" && p.Name != "plugin_call" {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "Tool " + p.Name + " not found"}
			}
			if len(p.Arguments) > 0 {
				if v, err := wire.Decode(p.Arguments); err == nil {
					if _, ok := v.(wire.Object); !ok {
						issues := []any{wire.Object{{Name: "expected", Value: "record"}, {Name: "code", Value: "invalid_type"}, {Name: "path", Value: []string{"params", "arguments"}}, {Name: "message", Value: "Invalid input: expected record, received " + kind(v, true)}}}
						b, _ := wire.JSON(issues, true)
						return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "Invalid tools/call request: " + string(b)}
					}
				}
			}
			return next(ctx, method, req)
		}
	})
	return s.Run(ctx, &mcp.IOTransport{Reader: &shutdownReader{ReadCloser: os.Stdin, close: e.Close}, Writer: stdoutWriter{os.Stdout}})
}

// text is one text content item.
func text(t string) []mcp.Content { return []mcp.Content{&mcp.TextContent{Text: t}} }

// textResult returns a formatted catalog or search result as MCP text content.
func textResult(value any) *mcp.CallToolResult {
	b, _ := wire.JSON(value, true)
	return &mcp.CallToolResult{Content: text(string(b))}
}

// toolError reports a recoverable tool failure without a JSON-RPC failure.
func toolError(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: text(message), IsError: true}
}

// invoke runs plugin_search or plugin_call. A downstream result passes through as the SDK reads it:
// its content, error flag and structured content.
func invoke(ctx context.Context, e *registry.Engine, name string, args wire.Object) *mcp.CallToolResult {
	if name == "plugin_search" {
		query, _ := args.Get("query").(string)
		var out wire.Object
		var err error
		if query == "" {
			out, err = e.Listing(ctx, false)
		} else {
			limit := float64(8)
			if n, ok := args.Get("limit").(float64); ok {
				limit = n
			}
			out, err = e.Search(ctx, query, limit, false)
		}
		if err != nil {
			return toolError("plugin_search failed: " + registry.Message(err))
		}
		return textResult(out)
	}
	tool := args.Get("tool").(string)
	values := args.Get("arguments")
	if values == nil {
		values = wire.Object{}
	}
	result, err := e.Call(ctx, tool, values, false)
	if err != nil {
		return toolError(tool + " failed: " + registry.Message(err) + ". If the tool id or arguments are wrong, run plugin_search to look them up.")
	}
	content, ok := result.Get("content").([]any)
	if !ok || len(content) == 0 {
		b, _ := wire.JSON(result, true)
		content = []any{wire.Object{{Name: "type", Value: "text"}, {Name: "text", Value: string(b)}}}
	}
	isError, _ := result.Get("isError").(bool)
	out := wire.Object{{Name: "content", Value: content}, {Name: "isError", Value: isError}}
	if result.Has("structuredContent") {
		out.Set("structuredContent", result.Get("structuredContent"))
	}
	b, _ := wire.JSON(out, false)
	var passed mcp.CallToolResult
	if err := json.Unmarshal(b, &passed); err != nil {
		return toolError(tool + " returned a result tap cannot read: " + err.Error())
	}
	return &passed
}

// kind returns Zod's type labels for validation messages.
func kind(v any, present bool) string {
	if !present {
		return "undefined"
	}
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	default:
		return "object"
	}
}

// validate applies the same optional fields, bounds and ordered issues as Zod.
func validate(name string, a wire.Object) string {
	issues := []string{}
	check := func(key, expected string, required bool) {
		if !required && !a.Has(key) {
			return
		}
		got := kind(a.Get(key), a.Has(key))
		want := expected
		if want == "record" {
			want = "object"
		}
		if got != want {
			issues = append(issues, key+": Invalid input: expected "+expected+", received "+got)
		}
	}
	if name == "plugin_search" {
		check("query", "string", false)
		if a.Has("limit") {
			if n, ok := a.Get("limit").(float64); ok {
				switch {
				case n != math.Trunc(n):
					issues = append(issues, "limit: Invalid input: expected int, received number")
				case n < 1:
					issues = append(issues, "limit: Too small: expected number to be >=1")
				case n > 25:
					issues = append(issues, "limit: Too big: expected number to be <=25")
				}
			} else {
				check("limit", "number", false)
			}
		}
	} else {
		check("tool", "string", true)
		check("arguments", "record", false)
	}
	return strings.Join(issues, ", ")
}

// shutdownReader closes downstream sessions on EOF, including pending tool calls.
type shutdownReader struct {
	io.ReadCloser
	once  sync.Once
	close func()
}

// Read starts teardown as soon as the harness ends stdin, without blocking SDK reads.
func (r *shutdownReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err == io.EOF {
		r.once.Do(func() { go r.close() })
	}
	return n, err
}

type stdoutWriter struct{ io.Writer }

// Close leaves process stdout open while the SDK tears down the session.
func (stdoutWriter) Close() error { return nil }
