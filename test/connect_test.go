package tap_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// connectBox is a sandbox for tap connect: a home directory, a PATH holding only the stub
// commands it may run, and no inherited agent-config environment.
type connectBox struct {
	*box
	home, stubs string
}

// connectSandbox prepares an empty machine for tap connect. Every stub must be written with
// stub, since a PATH with nothing in it is how a machine without agents is reached.
func connectSandbox(t *testing.T) *connectBox {
	t.Helper()
	root := t.TempDir()
	c := &connectBox{box: sandbox(t), home: filepath.Join(root, "home"), stubs: filepath.Join(root, "stubs")}
	for _, dir := range []string{c.home, c.stubs} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	c.env = []string{"HOME=" + c.home, "PATH=" + c.stubs, "CLAUDE_CONFIG_DIR=", "CODEX_HOME=", "XDG_CONFIG_HOME="}
	return c
}

// stub writes an agent's own command as a script. It logs the arguments it is given, prints
// out when it has any, and exits with status.
func (c *connectBox) stub(t *testing.T, name, out string, status int) {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> '%s'\n", filepath.Join(c.stubs, name+".argv"))
	if out != "" {
		script += "printf '%s\\n' '" + out + "'\n"
	}
	if status != 0 {
		script += fmt.Sprintf("exit %d\n", status)
	}
	if err := os.WriteFile(filepath.Join(c.stubs, name), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
}

// invoked returns the arguments a stub was given, or "" when it never ran.
func (c *connectBox) invoked(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(c.stubs, name+".argv"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// seed writes a config file at path relative to the connect sandbox's home.
func (c *connectBox) seed(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(c.home, path)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.home, path), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestConnect pins which commands tap connect runs, what it reports, and how it fails. The
// agents are stubs: the config files they would write are seeded by hand.
func TestConnect(t *testing.T) {
	t.Run("bare connect runs each agent's own command", func(t *testing.T) {
		c := connectSandbox(t)
		c.stub(t, "claude", "", 0)
		c.stub(t, "codex", "", 0)
		c.stub(t, "opencode", "", 0)
		r := c.run("connect")
		equal(t, r, result{0, "claude    connected\ncodex     connected\nopencode  connected\n", ""})
		equal(t, c.invoked(t, "claude"), "mcp add --scope user tap -- tap")
		equal(t, c.invoked(t, "codex"), "mcp add tap -- tap")
		equal(t, c.invoked(t, "opencode"), "mcp add --global tap -- tap")
	})

	t.Run("an agent whose config has tap is left as it is", func(t *testing.T) {
		c := connectSandbox(t)
		c.stub(t, "claude", "", 0)
		c.seed(t, ".claude.json", `{"mcpServers":{"tap":{"command":"tap"}}}`)
		r := c.run("connect", "claude")
		equal(t, r, result{0, "claude  has tap already\n", ""})
		equal(t, c.invoked(t, "claude"), "")
	})

	t.Run("a name connects only that agent", func(t *testing.T) {
		c := connectSandbox(t)
		c.stub(t, "claude", "", 0)
		c.stub(t, "codex", "", 0)
		r := c.run("connect", "codex")
		equal(t, r, result{0, "codex  connected\n", ""})
		equal(t, c.invoked(t, "claude"), "")
		equal(t, c.invoked(t, "codex"), "mcp add tap -- tap")
	})

	t.Run("an unknown agent is refused before anything runs", func(t *testing.T) {
		c := connectSandbox(t)
		c.stub(t, "claude", "", 0)
		r := c.run("connect", "claud")
		equal(t, r.status, 2)
		equal(t, r.stdout, "")
		contains(t, r.stderr, `Did you mean "claude"?`)
		equal(t, c.invoked(t, "claude"), "")
	})

	t.Run("a named agent whose command is missing fails", func(t *testing.T) {
		c := connectSandbox(t)
		r := c.run("connect", "codex")
		equal(t, r.status, 1)
		equal(t, r.stdout, "")
		contains(t, r.stderr, "no codex command was found on this machine, so tap cannot connect codex")
	})

	t.Run("bare connect with no agent found fails", func(t *testing.T) {
		c := connectSandbox(t)
		r := c.run("connect")
		equal(t, r.status, 1)
		equal(t, r.stdout, "")
		contains(t, r.stderr, "tap looked for claude, codex, opencode")
	})

	t.Run("claude refusing a second tap is success", func(t *testing.T) {
		c := connectSandbox(t)
		c.stub(t, "claude", "MCP server tap already exists in user config", 1)
		r := c.run("connect", "claude")
		equal(t, r, result{0, "claude  has tap already\n", ""})
		equal(t, c.invoked(t, "claude"), "mcp add --scope user tap -- tap")
	})

	t.Run("a command that fails is tap's failure", func(t *testing.T) {
		c := connectSandbox(t)
		c.stub(t, "claude", "permission denied writing config", 1)
		r := c.run("connect", "claude")
		equal(t, r.status, 1)
		equal(t, r.stdout, "")
		contains(t, r.stderr, "claude could not add tap: permission denied writing config")
	})

	t.Run("places follow each agent's own environment", func(t *testing.T) {
		c := connectSandbox(t)
		for _, name := range []string{"claude", "codex", "opencode"} {
			c.stub(t, name, "", 0)
		}
		claude, codex, xdg := filepath.Join(c.home, "custom"), filepath.Join(c.home, "codex-home"), filepath.Join(c.home, "xdg")
		for path, content := range map[string]string{
			filepath.Join(claude, ".claude.json"):            `{"mcpServers":{"tap":{"command":"tap"}}}`,
			filepath.Join(codex, "config.toml"):              "[mcp_servers.tap]\ncommand = \"tap\"\n",
			filepath.Join(xdg, "opencode", "opencode.jsonc"): `{"mcp":{"servers":{"tap":{"type":"local","command":["tap"]}}}}`,
		} {
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
		c.env = append(c.env, "CLAUDE_CONFIG_DIR="+claude, "CODEX_HOME="+codex, "XDG_CONFIG_HOME="+xdg)
		r := c.run("connect")
		equal(t, r, result{0, "claude    has tap already\ncodex     has tap already\nopencode  has tap already\n", ""})
		for _, name := range []string{"claude", "codex", "opencode"} {
			equal(t, c.invoked(t, name), "")
		}
	})

	t.Run("an unreadable config stops the command", func(t *testing.T) {
		c := connectSandbox(t)
		c.stub(t, "claude", "", 0)
		c.seed(t, ".claude.json", "not json")
		r := c.run("connect", "claude")
		equal(t, r.status, 1)
		equal(t, r.stdout, "")
		contains(t, r.stderr, "is not valid JSON")
		equal(t, c.invoked(t, "claude"), "")
	})
}
