// Package config reads and atomically edits tap's ordered server registry. It
// expands environment and home references only when a connection is opened.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fschrhunt/tap/internal/wire"
)

// Path returns TAP_CONFIG, or the default home-relative path when it is empty.
func Path() string {
	if p := os.Getenv("TAP_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".tap", "servers.json")
}

// Load reads the servers object; a missing file is an empty registry.
func Load(path string) (wire.Object, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return wire.Object{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %s", path, err)
	}
	v, err := wire.Decode(b)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %s", path, JSONError(b, err))
	}
	if o, ok := v.(wire.Object); ok {
		if s, ok := o.Get("servers").(wire.Object); ok {
			return s, nil
		}
	}
	return wire.Object{}, nil
}

// Save atomically writes owner-only JSON through an existing symlink.
func Save(path string, servers wire.Object) error {
	target := path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		target = real
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	b, err := wire.JSON(wire.Object{{Name: "servers", Value: servers}}, true)
	if err != nil {
		return err
	}
	temp := fmt.Sprintf("%s.%d.tmp", target, os.Getpid())
	if err = os.WriteFile(temp, append(b, '\n'), 0600); err != nil {
		return err
	}
	if err = os.Rename(temp, target); err != nil {
		os.Remove(temp)
	}
	return err
}

// Add saves one server with a name that can be used in server.tool identifiers.
func Add(path, name string, def wire.Object) error {
	if strings.Contains(name, ".") {
		return fmt.Errorf("server names cannot contain a dot: tool ids use server.tool")
	}
	servers, err := Load(path)
	if err != nil {
		return err
	}
	servers.Set(name, def)
	return Save(path, servers)
}

// Remove deletes a named server, reporting whether it existed.
func Remove(path, name string) (bool, error) {
	servers, err := Load(path)
	if err != nil {
		return false, err
	}
	if !servers.Has(name) {
		return false, nil
	}
	servers.Delete(name)
	return true, Save(path, servers)
}

var variable = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Expand replaces ${NAME} in an arbitrary config value; unknown variables are empty.
func Expand(value any) string {
	return variable.ReplaceAllStringFunc(wire.String(value), func(s string) string { return os.Getenv(s[2 : len(s)-1]) })
}

// Home resolves only ~ and ~/ prefixes, matching the shared config's contract.
func Home(value string) string {
	home, _ := os.UserHomeDir()
	if value == "~" {
		return home
	}
	if strings.HasPrefix(value, "~/") {
		return filepath.Join(home, value[2:])
	}
	return value
}
