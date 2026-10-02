// Command fixture is an offline MCP peer with deterministic success, error,
// structured and hung responses. --serve is required; --http serves on localhost.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fschrhunt/tap/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const tools = `[{"name":"echo","description":"Echo the supplied message.","inputSchema":{"type":"object","$schema":"https://json-schema.org/draft/2020-12/schema","properties":{"message":{"type":"string"}},"required":["message"]}},{"name":"fail","description":"Return a tool error.","inputSchema":{"type":"object","$schema":"https://json-schema.org/draft/2020-12/schema","properties":{}}},{"name":"data","description":"Return structured data.","inputSchema":{"type":"object","$schema":"https://json-schema.org/draft/2020-12/schema","properties":{}}}]`

// has reports a fixture mode selected on the command line.
func has(flag string) bool {
	for _, s := range os.Args[1:] {
		if s == flag {
			return true
		}
	}
	return false
}

// option returns a mode's following argument or an empty string.
func option(flag string) string {
	for i, s := range os.Args {
		if s == flag && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

// trace records connect and list activity for cache and lazy-connection tests.
func trace(method string) {
	if path := option("--trace"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err == nil {
			fmt.Fprintln(f, method)
			f.Close()
		}
	}
}

// fixture builds the same three tools as the original Node fixture, with optional
// transport and lifecycle probes for behavior that was not covered by that suite.
func fixture() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0.0"}, &mcp.ServerOptions{Instructions: "Fixture guidance: supply the documented fields. This is server-provided content.", Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}})
	ts, _ := wire.Decode([]byte(tools))
	var catalogMu sync.Mutex
	if has("--large-schema") {
		list := ts.([]any)
		echo := list[0].(wire.Object)
		schema := echo.Get("inputSchema").(wire.Object)
		schema.Set("description", strings.Repeat("schema documentation ", 3000))
		echo.Set("inputSchema", schema)
		list[0] = echo
		ts = list
	}
	if has("--output-schema") {
		list := ts.([]any)
		data := list[2].(wire.Object)
		data.Set("outputSchema", wire.Object{{Name: "type", Value: "object"}, {Name: "properties", Value: wire.Object{{Name: "count", Value: wire.Object{{Name: "type", Value: "integer"}}}}}})
		list[2] = data
		ts = list
	}
	if has("--remote-schema") {
		list := ts.([]any)
		echo := list[0].(wire.Object)
		echo.Set("inputSchema", wire.Object{{Name: "type", Value: "object"}, {Name: "$ref", Value: "https://example.invalid/schema.json"}})
		list[0] = echo
		ts = list
	}
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			trace(method)
			if method == "initialize" && has("--delay-connect") {
				time.Sleep(300 * time.Millisecond)
			}
			if method == "tools/list" {
				if has("--hang-list") {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				if has("--delay-list") {
					time.Sleep(300 * time.Millisecond)
				}
				catalogMu.Lock()
				raw, _ := wire.JSON(ts, false)
				list, _ := wire.Decode(raw)
				catalogMu.Unlock()
				if has("--empty") {
					list = []any{}
				}
				if has("--paged") {
					cursor := req.GetParams().(*mcp.ListToolsParams).Cursor
					if cursor == "" {
						return &wire.Result{Value: wire.Object{{Name: "tools", Value: list.([]any)[:1]}, {Name: "nextCursor", Value: "second"}}}, nil
					}
					return &wire.Result{Value: wire.Object{{Name: "tools", Value: list.([]any)[1:]}}}, nil
				}
				return &wire.Result{Value: wire.Object{{Name: "tools", Value: list}}}, nil
			}
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			p := req.GetParams().(*mcp.CallToolParamsRaw)
			if has("--hang-call") {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			var result wire.Object
			switch p.Name {
			case "echo":
				a, _ := wire.Decode(p.Arguments)
				o, _ := a.(wire.Object)
				message, ok := o.Get("message").(string)
				if !ok {
					kind := "undefined"
					if o.Has("message") {
						switch o.Get("message").(type) {
						case nil:
							kind = "null"
						case float64:
							kind = "number"
						default:
							kind = "object"
						}
					}
					result = text("Input validation error: Invalid arguments for tool echo: message: Invalid input: expected string, received " + kind)
					result.Set("isError", true)
				} else {
					if has("--inspect") {
						cwd, _ := os.Getwd()
						b, _ := wire.JSON(wire.Object{{Name: "cwd", Value: cwd}, {Name: "mode", Value: os.Getenv("MODE")}}, false)
						message = string(b)
					}
					result = text(message)
				}
			case "fail":
				result = text("fixture failure")
				result.Set("isError", true)
			case "data":
				result = text("count: 3")
				result.Set("structuredContent", wire.Object{{Name: "count", Value: 3}})
				if has("--numeric") {
					v, _ := wire.DecodeExact(req.GetParams().(*mcp.CallToolParamsRaw).Arguments)
					args, _ := v.(wire.Object)
					result.Set("structuredContent", wire.Object{{Name: "id", Value: args.Get("id")}})
				}
				if has("--large-result") {
					rows := []any{}
					for i := 0; i < 1000; i++ {
						rows = append(rows, wire.Object{{Name: "index", Value: i}, {Name: "text", Value: strings.Repeat("payload ", 30)}})
					}
					result.Set("structuredContent", wire.Object{{Name: "rows", Value: rows}, {Name: "text", Value: "copied without model recitation"}, {Name: "a/b~c", Value: nil}})
				}
				if has("--rich") {
					result.Set("content", []any{wire.Object{{Name: "type", Value: "image"}, {Name: "data", Value: "AA=="}, {Name: "mimeType", Value: "image/png"}, {Name: "annotations", Value: wire.Object{{Name: "audience", Value: []string{"user"}}}}, {Name: "_meta", Value: wire.Object{{Name: "extra", Value: true}}}}})
					result.Set("structuredContent", nil)
				}
				if has("--notify") {
					catalogMu.Lock()
					list := ts.([]any)
					echo := list[0].(wire.Object)
					echo.Set("description", "Updated echo contract after tools/list_changed.")
					list[0] = echo
					ts = list
					catalogMu.Unlock()
					// Adding a signal tool emits a real SDK notification; catalog middleware controls its visible list.
					s.AddTool(&mcp.Tool{Name: "changed", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
						return &mcp.CallToolResult{}, nil
					})
				}
				if has("--protocol-error") {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "simulated failure after executing the operation"}
				}
			default:
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "Tool " + p.Name + " not found"}
			}
			return &wire.Result{Value: result}, nil
		}
	})
	return s
}

// text makes the fixture's unstructured result with the original field order.
func text(message string) wire.Object {
	return wire.Object{{Name: "content", Value: []any{wire.Object{{Name: "type", Value: "text"}, {Name: "text", Value: message}}}}}
}

// main starts the selected transport and exits when its stdin is closed.
func main() {
	if !has("--serve") || has("--exit") {
		return
	}
	if has("--noisy") {
		fmt.Fprint(os.Stderr, strings.Repeat("fixture stderr\n", 10000))
	}
	if has("--hang-connect") {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	if path := option("--exit-file"); path != "" {
		defer os.WriteFile(path, []byte("closed\n"), 0600)
	}
	s := fixture()
	if has("--http") {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			panic(err)
		}
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
		httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for i, arg := range os.Args {
				if arg == "--expect-header" && i+1 < len(os.Args) {
					key, value, _ := strings.Cut(os.Args[i+1], "=")
					if r.Header.Get(key) != value {
						http.Error(w, "bad header", http.StatusUnauthorized)
						return
					}
				}
			}
			handler.ServeHTTP(w, r)
		})}
		fmt.Println("http://" + listener.Addr().String() + "/mcp")
		go func() { _, _ = io.Copy(io.Discard, os.Stdin); _ = httpServer.Close() }()
		_ = httpServer.Serve(listener)
		return
	}
	_ = s.Run(context.Background(), &mcp.StdioTransport{})
}
