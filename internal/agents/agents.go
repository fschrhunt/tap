// Package agents reads the MCP servers that coding agents keep in their own config files and
// turns them into tap's server definitions. It only reads: nothing in an agent's config is
// changed.
package agents

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/fschrhunt/tap/internal/wire"
)

// Server is one server an agent has, in tap's shape, under a name tap accepts.
type Server struct {
	Name string
	Def  wire.Object
	// Note says what changed on the way over, such as a new name. It is empty for most.
	Note string
}

// Skipped is a server tap cannot take over, and why.
type Skipped struct{ Name, Why string }

// Config is what one agent's file holds.
type Config struct {
	// Agent is the agent's name as people know it; Path is the file that was read.
	Agent, Path string
	Servers     []Server
	Skipped     []Skipped
}

// Has reports whether the file holds a server by this name, even one tap skips.
func (c *Config) Has(name string) bool {
	for _, server := range c.Servers {
		if server.Name == name {
			return true
		}
	}
	for _, skip := range c.Skipped {
		if skip.Name == name {
			return true
		}
	}
	return false
}

// Place is a file where an agent may keep servers.
type Place struct {
	// ID is what a person types to pick the agent: claude, codex, opencode, cursor or vscode.
	ID, Agent, Path string
}

// Places lists the files tap knows to look in, for a home directory and a working directory.
func Places(home, cwd string) []Place {
	return []Place{
		{"claude", "Claude Code", filepath.Join(home, ".claude.json")},
		{"claude", "Claude Code", filepath.Join(cwd, ".mcp.json")},
		{"codex", "Codex", filepath.Join(home, ".codex", "config.toml")},
		{"opencode", "OpenCode", filepath.Join(home, ".config", "opencode", "opencode.jsonc")},
		{"opencode", "OpenCode", filepath.Join(home, ".config", "opencode", "opencode.json")},
		{"opencode", "OpenCode", filepath.Join(cwd, "opencode.jsonc")},
		{"opencode", "OpenCode", filepath.Join(cwd, "opencode.json")},
		{"cursor", "Cursor", filepath.Join(home, ".cursor", "mcp.json")},
		{"vscode", "VS Code", filepath.Join(cwd, ".vscode", "mcp.json")},
	}
}

// Read loads the servers in one file. cwd picks the project whose servers Claude Code keeps
// in its user file. A file with no servers in it gives a Config with none.
func Read(place Place, cwd string) (*Config, error) {
	b, err := os.ReadFile(place.Path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s", place.Path)
	}
	c := &Config{Agent: place.Agent, Path: place.Path}
	if strings.HasSuffix(place.Path, ".toml") {
		return c, c.codex(b)
	}
	v, err := wire.Decode(plain(b))
	if err != nil {
		return nil, fmt.Errorf("%s is not valid JSON", place.Path)
	}
	root, _ := v.(wire.Object)
	// Each agent names its table differently; a file is read under every name it has.
	for _, table := range []any{root.Get("mcpServers"), root.Get("servers"), root.Get("mcp")} {
		c.table(table)
	}
	if projects, ok := root.Get("projects").(wire.Object); ok {
		if project, ok := projects.Get(cwd).(wire.Object); ok {
			c.table(project.Get("mcpServers"))
		}
	}
	return c, nil
}

// table takes every server in one JSON table. OpenCode nests its table one level down.
func (c *Config) table(v any) {
	servers, _ := v.(wire.Object)
	if nested, ok := servers.Get("servers").(wire.Object); ok && !servers.Has("command") && !servers.Has("url") {
		servers = nested
	}
	for _, f := range servers {
		if entry, ok := f.Value.(wire.Object); ok {
			c.take(f.Name, entry)
		}
	}
}

// take converts one server. command may be a string with args beside it or a whole argv;
// env and environment, and headers and http_headers, are the same thing under two names.
func (c *Config) take(name string, entry wire.Object) {
	skip := func(why string) { c.Skipped = append(c.Skipped, Skipped{name, why}) }
	text := func(key string) string { s, _ := entry.Get(key).(string); return s }
	list := func(key string) []any {
		out := []any{}
		list, _ := entry.Get(key).([]any)
		for _, v := range list {
			out = append(out, wire.String(v))
		}
		return out
	}
	object := func(keys ...string) wire.Object {
		for _, key := range keys {
			if o, ok := entry.Get(key).(wire.Object); ok && len(o) > 0 {
				out := wire.Object{}
				for _, f := range o {
					out.Set(f.Name, wire.String(f.Value))
				}
				return out
			}
		}
		return nil
	}
	if entry.Get("enabled") == false || entry.Get("disabled") == true {
		skip("it is turned off in " + c.Agent)
		return
	}
	var def wire.Object
	command := list("command")
	if s := text("command"); s != "" {
		command = append([]any{s}, list("args")...)
	}
	switch {
	case len(command) > 0:
		if filepath.Base(wire.String(command[0])) == "tap" {
			skip("it is tap itself")
			return
		}
		def = wire.Object{{Name: "type", Value: "stdio"}, {Name: "command", Value: command}}
		if env := object("env", "environment"); env != nil {
			def.Set("env", env)
		}
		if cwd := text("cwd"); cwd != "" {
			def.Set("cwd", cwd)
		}
	case text("url") != "":
		if kind := text("type"); kind == "sse" || kind == "websocket" || kind == "ws" {
			skip("it uses the " + kind + " transport, and tap speaks Streamable HTTP")
			return
		}
		def = wire.Object{{Name: "type", Value: "http"}, {Name: "url", Value: text("url")}}
		headers := object("headers", "http_headers")
		// Codex names environment variables whose values become headers.
		if named, ok := entry.Get("env_http_headers").(wire.Object); ok {
			if headers == nil {
				headers = wire.Object{}
			}
			for _, f := range named {
				headers.Set(f.Name, "${"+wire.String(f.Value)+"}")
			}
		}
		if headers != nil {
			def.Set("headers", headers)
		}
		if token := text("bearer_token_env_var"); token != "" {
			def.Set("bearerTokenEnv", token)
		}
	default:
		skip("it has neither a command nor a url")
		return
	}
	server := Server{Name: name, Def: def}
	// Tool ids are server.tool, so a server's name cannot hold a dot.
	if strings.Contains(name, ".") {
		server.Name = strings.ReplaceAll(name, ".", "-")
		server.Note = "renamed from " + name + ", since a name cannot hold a dot"
	}
	c.Servers = append(c.Servers, server)
}

// codex reads the mcp_servers tables of Codex's TOML config, in the file's order.
func (c *Config) codex(b []byte) error {
	var root struct {
		Servers map[string]map[string]any `toml:"mcp_servers"`
	}
	meta, err := toml.Decode(string(b), &root)
	if err != nil {
		return fmt.Errorf("%s is not valid TOML", c.Path)
	}
	seen := map[string]bool{}
	for _, key := range meta.Keys() {
		if len(key) < 2 || key[0] != "mcp_servers" || seen[key[1]] {
			continue
		}
		seen[key[1]] = true
		c.take(key[1], ordered(root.Servers[key[1]]))
	}
	return nil
}

// ordered turns a decoded TOML table into the ordered objects take reads, its keys sorted.
func ordered(table map[string]any) wire.Object {
	o := wire.Object{}
	for _, name := range slices.Sorted(maps.Keys(table)) {
		switch v := table[name].(type) {
		case map[string]any:
			o.Set(name, ordered(v))
		default:
			o.Set(name, v)
		}
	}
	return o
}

// plain turns JSON with comments and trailing commas, as agents allow in their configs, into
// JSON. Text inside strings is left alone.
func plain(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		switch c := b[i]; {
		case c == '"':
			start := i
			for i++; i < len(b) && b[i] != '"'; i++ {
				if b[i] == '\\' {
					i++
				}
			}
			out = append(out, b[start:min(i+1, len(b))]...)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				i++
			}
			out = append(out, '\n')
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			for i += 2; i+1 < len(b) && !(b[i] == '*' && b[i+1] == '/'); i++ {
			}
			i++
		case c == ',':
			// A comma is trailing when only space and comments stand before the closing bracket.
			rest := strings.TrimLeft(string(plainAhead(b[i+1:])), " \t\r\n")
			if !strings.HasPrefix(rest, "}") && !strings.HasPrefix(rest, "]") {
				out = append(out, c)
			}
		default:
			out = append(out, c)
		}
	}
	return out
}

// plainAhead returns the input with any comments at its start removed, up to the next token.
func plainAhead(b []byte) []byte {
	for {
		trimmed := []byte(strings.TrimLeft(string(b), " \t\r\n"))
		switch {
		case len(trimmed) > 1 && trimmed[0] == '/' && trimmed[1] == '/':
			at := strings.IndexByte(string(trimmed), '\n')
			if at < 0 {
				return nil
			}
			b = trimmed[at:]
		case len(trimmed) > 1 && trimmed[0] == '/' && trimmed[1] == '*':
			at := strings.Index(string(trimmed), "*/")
			if at < 0 {
				return nil
			}
			b = trimmed[at+2:]
		default:
			return trimmed
		}
	}
}
