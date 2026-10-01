// Package cli dispatches tap's shell commands. Bare invocations serve MCP; all
// other commands use the same resident engine, drain child stderr and then close.
package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/registry"
	"github.com/fschrhunt/tap/internal/server"
	"github.com/fschrhunt/tap/internal/wire"
)

const usage = `tap %s — Less noise. Better agents.

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
Other flags: --json prints raw output; --limit N caps search results.`

// Run executes a command and returns its process exit code, writing only CLI output.
func Run(ctx context.Context, args []string, version string, stdout, stderr io.Writer) int {
	e := registry.New(config.Path(), version)
	defer e.Close()
	status, err := run(ctx, append([]string(nil), args...), e, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "tap: "+registry.Message(err))
		return 1
	}
	return status
}

// take removes the first named flag and its following value, if present.
func take(args *[]string, name string) (string, bool) {
	for i, s := range *args {
		if s == name {
			value := ""
			hasValue := i+1 < len(*args)
			end := i + 1
			if end < len(*args) {
				value = (*args)[end]
				end++
			}
			*args = append((*args)[:i], (*args)[end:]...)
			return value, hasValue
		}
	}
	return "", false
}

// boolean removes exactly one occurrence of a boolean option.
func boolean(args *[]string, name string) bool {
	for i, s := range *args {
		if s == name {
			*args = append((*args)[:i], (*args)[i+1:]...)
			return true
		}
	}
	return false
}

// pairs parses repeated KEY=VALUE options in insertion order.
func pairs(args *[]string, name string) (wire.Object, error) {
	o := wire.Object{}
	for {
		index := -1
		for i, s := range *args {
			if s == name {
				index = i
				break
			}
		}
		if index < 0 {
			return o, nil
		}
		if index+1 == len(*args) {
			return nil, fmt.Errorf("%s needs a value", name)
		}
		value, _ := take(args, name)
		eq := strings.Index(value, "=")
		if eq < 1 {
			return nil, fmt.Errorf("expected %s KEY=VALUE, got \"%s\"", name, value)
		}
		o.Set(value[:eq], value[eq+1:])
	}
}

// print emits a value with the same JSON indentation and trailing newline as Node.
func print(w io.Writer, value any, json bool) {
	if json {
		b, _ := wire.JSON(value, true)
		fmt.Fprintln(w, string(b))
	} else {
		fmt.Fprintln(w, value)
	}
}

// run preserves tap's permissive option dispatch, including command arguments after --.
func run(ctx context.Context, args []string, e *registry.Engine, out, errout io.Writer) (int, error) {
	if len(args) == 0 {
		return 0, server.Serve(ctx, e)
	}
	command := args[0]
	args = args[1:]
	rest := []string{}
	for i, s := range args {
		if s == "--" {
			rest = append(rest, args[i+1:]...)
			args = args[:i]
			rest = append([]string{"--"}, rest...)
			break
		}
	}
	json := boolean(&args, "--json")
	switch command {
	case "help", "-h", "--help":
		print(out, fmt.Sprintf(usage, e.Version), false)
		return 0, nil
	case "version", "-v", "--version":
		print(out, e.Version, false)
		return 0, nil
	case "path":
		print(out, e.Path, false)
		return 0, nil
	case "add":
		if len(args) == 0 || args[0] == "" {
			return 1, fmt.Errorf("usage: tap add <name> <url> | tap add <name> -- <command> [args...]")
		}
		name := args[0]
		args = args[1:]
		env, err := pairs(&args, "--env")
		if err != nil {
			return 1, err
		}
		headers, err := pairs(&args, "--header")
		if err != nil {
			return 1, err
		}
		cwd, _ := take(&args, "--cwd")
		bearer, _ := take(&args, "--bearer-token-env")
		if len(rest) > 0 {
			if len(rest) == 1 {
				return 1, fmt.Errorf("usage: tap add <name> -- <command> [args...]")
			}
			def := wire.Object{{Name: "type", Value: "stdio"}, {Name: "command", Value: rest[1:]}}
			if len(env) > 0 {
				def.Set("env", env)
			}
			if cwd != "" {
				def.Set("cwd", cwd)
			}
			if err = config.Add(e.Path, name, def); err != nil {
				return 1, err
			}
			print(out, "added "+name+" (stdio)", false)
			return 0, nil
		}
		if len(args) == 0 || args[0] == "" {
			return 1, fmt.Errorf("usage: tap add <name> <url> | tap add <name> -- <command> [args...]")
		}
		def := wire.Object{{Name: "type", Value: "http"}, {Name: "url", Value: args[0]}}
		if len(headers) > 0 {
			def.Set("headers", headers)
		}
		if bearer != "" {
			def.Set("bearerTokenEnv", bearer)
		}
		if err = config.Add(e.Path, name, def); err != nil {
			return 1, err
		}
		print(out, "added "+name, false)
		return 0, nil
	case "remove":
		if len(args) == 0 || args[0] == "" {
			return 1, fmt.Errorf("usage: tap remove <name>")
		}
		removed, err := config.Remove(e.Path, args[0])
		if err != nil {
			return 1, err
		}
		if removed {
			print(out, "removed "+args[0], false)
		} else {
			print(out, "no server named "+args[0], false)
		}
		return 0, nil
	case "list":
		result, err := e.Listing(ctx, true)
		if err != nil {
			return 1, err
		}
		if json {
			print(out, result, true)
			return 0, nil
		}
		rows := result.Get("integrations").([]any)
		if len(rows) == 0 {
			print(out, "no servers configured ("+e.Path+")", false)
			return 0, nil
		}
		lines := []string{}
		for _, v := range rows {
			row := v.(wire.Object)
			summary := fmt.Sprintf("%v tools", row.Get("tools"))
			if row.Has("error") {
				summary = "unavailable: " + wire.String(row.Get("error"))
			}
			lines = append(lines, wire.String(row.Get("server"))+" — "+summary)
		}
		print(out, strings.Join(lines, "\n"), false)
		return 0, nil
	case "search":
		limit := float64(8)
		raw, present := take(&args, "--limit")
		if present {
			limit = wire.Number(raw)
		}
		result, err := e.Search(ctx, strings.Join(args, " "), limit, true)
		if err != nil {
			return 1, err
		}
		if json {
			print(out, result, true)
			return 0, nil
		}
		matches := result.Get("matches").([]any)
		lines := []string{}
		for _, v := range matches {
			t := v.(wire.Object)
			desc, _ := t.Get("description").(string)
			lines = append(lines, strings.TrimRight(wire.String(t.Get("id"))+"\n  "+desc, " \t\r\n"))
		}
		if len(lines) == 0 {
			hint, _ := result.Get("hint").(string)
			if hint == "" {
				hint = "no matching tools for \"" + wire.String(result.Get("query")) + "\""
			}
			lines = append(lines, hint)
		}
		total := result.Get("total").(int)
		if total > len(matches) {
			lines = append(lines, fmt.Sprintf("(%d more; raise --limit to see them)", total-len(matches)))
		}
		for _, v := range result.Get("unavailable").([]any) {
			row := v.(wire.Object)
			lines = append(lines, "unavailable: "+wire.String(row.Get("server"))+" — "+wire.String(row.Get("error")))
		}
		print(out, strings.Join(lines, "\n"), false)
		return 0, nil
	case "call":
		if len(args) == 0 || args[0] == "" {
			return 1, fmt.Errorf("usage: tap call <server.tool> [key=value ...]")
		}
		id := args[0]
		args = args[1:]
		values, err := toolArgs(args)
		if err != nil {
			return 1, err
		}
		result, err := e.Call(ctx, id, values, true)
		if err != nil {
			return 1, err
		}
		if json {
			print(out, result, true)
			return 0, nil
		}
		lines := []string{}
		if content, ok := result.Get("content").([]any); ok {
			for _, v := range content {
				part, ok := v.(wire.Object)
				if ok && part.Get("type") == "text" {
					lines = append(lines, wire.String(part.Get("text")))
				} else {
					b, _ := wire.JSON(v, false)
					lines = append(lines, string(b))
				}
			}
		}
		text := strings.Join(lines, "\n")
		if text == "" {
			b, _ := wire.JSON(result, true)
			text = string(b)
		}
		print(out, text, false)
		if result.Get("isError") == true {
			fmt.Fprintln(errout, "tap: "+id+" reported an error")
		}
		return 0, nil
	default:
		print(out, fmt.Sprintf(usage, e.Version), false)
		return 1, nil
	}
}

// toolArgs combines JSON input with string key=value overrides, in argument order.
func toolArgs(args []string) (any, error) {
	raw, _ := take(&args, "--args")
	var values any
	if raw != "" {
		v, err := wire.Decode([]byte(raw))
		if err != nil {
			return nil, fmt.Errorf("%s", config.JSONError([]byte(raw), err))
		}
		values = v
	}
	for _, pair := range args {
		eq := strings.Index(pair, "=")
		if eq < 1 {
			return nil, fmt.Errorf("expected key=value, got \"%s\"", pair)
		}
		if values == nil {
			values = wire.Object{}
		}
		o, ok := values.(wire.Object)
		if !ok {
			return nil, fmt.Errorf("Cannot create property '%s' on %s '%v'", pair[:eq], typeName(values), values)
		}
		o.Set(pair[:eq], pair[eq+1:])
		values = o
	}
	if values == nil {
		values = wire.Object{}
	}
	return values, nil
}

// typeName labels primitive JSON values for JavaScript assignment errors.
func typeName(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	default:
		return "number"
	}
}
