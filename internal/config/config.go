// Package config reads and atomically edits tap's ordered server registry. It
// expands environment and home references only when a connection is opened.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

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

// Remote selects the relay endpoint and the environment variable holding its token.
type Remote struct {
	URL           string `json:"url"`
	TokenEnv      string `json:"tokenEnv"`
	AllowInsecure bool   `json:"allowInsecure,omitempty"`
}

// NormalizeURL accepts a remote base URL or its /mcp endpoint, without credentials.
func NormalizeURL(raw string, insecure bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") || u.Opaque != "" {
		return "", fmt.Errorf("invalid remote URL")
	}
	if u.Path != "" && u.Path != "/" && u.Path != "/mcp" && u.Path != "/mcp/" {
		return "", fmt.Errorf("remote URL must be a base URL or /mcp endpoint")
	}
	if u.Scheme == "http" && !Loopback(u.Hostname()) && !insecure {
		return "", fmt.Errorf("HTTP remote requires loopback or --allow-insecure")
	}
	u.Path = "/mcp"
	u.RawPath = ""
	return u.String(), nil
}

// Loopback accepts literal loopback addresses and localhost, without DNS resolution.
func Loopback(host string) bool {
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
}

// loadRoot validates the config container before any edit can discard data.
func loadRoot(path string) (wire.Object, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return wire.Object{{Name: "servers", Value: wire.Object{}}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read config")
	}
	v, err := wire.Decode(b)
	if err != nil {
		message := JSONError(b, err)
		// Config parse errors must not echo nearby credentials from malformed JSON.
		if strings.HasPrefix(message, "Unexpected token") || (strings.HasPrefix(message, "\"") && string(b) != "undefined" && string(b) != "NaN" && string(b) != "Infinity" && string(b) != "[object Object]") {
			message = "invalid JSON"
		}
		return nil, fmt.Errorf("%s is not valid JSON: %s", path, message)
	}
	root, ok := v.(wire.Object)
	if !ok {
		return nil, fmt.Errorf("config must be an object")
	}
	if root.Has("servers") {
		servers, ok := root.Get("servers").(wire.Object)
		if !ok {
			return nil, fmt.Errorf("config servers must be an object")
		}
		for _, f := range servers {
			if _, ok := f.Value.(wire.Object); !ok {
				return nil, fmt.Errorf("server definitions must be objects")
			}
		}
	} else {
		root.Set("servers", wire.Object{})
	}
	if root.Has("remote") {
		if _, err := remoteFrom(root.Get("remote")); err != nil {
			return nil, err
		}
	}
	return root, nil
}

// remoteFrom rejects malformed relay settings rather than silently choosing local mode.
func remoteFrom(v any) (*Remote, error) {
	o, ok := v.(wire.Object)
	if !ok {
		return nil, fmt.Errorf("config remote must be an object")
	}
	r := &Remote{}
	r.URL, ok = o.Get("url").(string)
	if !ok {
		return nil, fmt.Errorf("config remote requires url")
	}
	r.TokenEnv = "TAP_REMOTE_TOKEN"
	if o.Has("tokenEnv") {
		r.TokenEnv, ok = o.Get("tokenEnv").(string)
		if !ok {
			return nil, fmt.Errorf("invalid remote tokenEnv")
		}
	}
	if !environmentName.MatchString(r.TokenEnv) {
		return nil, fmt.Errorf("invalid remote tokenEnv")
	}
	if o.Has("allowInsecure") {
		r.AllowInsecure, ok = o.Get("allowInsecure").(bool)
		if !ok {
			return nil, fmt.Errorf("invalid remote allowInsecure")
		}
	}
	var err error
	r.URL, err = NormalizeURL(r.URL, r.AllowInsecure)
	if err != nil {
		return nil, err
	}
	for _, f := range o {
		if f.Name != "url" && f.Name != "tokenEnv" && f.Name != "allowInsecure" {
			return nil, fmt.Errorf("unknown remote config field")
		}
	}
	return r, nil
}

// Load reads the local servers while validating the entire config.
func Load(path string) (wire.Object, error) {
	root, err := loadRoot(path)
	if err != nil {
		return nil, err
	}
	return root.Get("servers").(wire.Object), nil
}

// LoadRemote returns nil only when the remote block is absent.
func LoadRemote(path string) (*Remote, error) {
	root, err := loadRoot(path)
	if err != nil {
		return nil, err
	}
	if !root.Has("remote") {
		return nil, nil
	}
	return remoteFrom(root.Get("remote"))
}

// edit serializes read-modify-write edits across processes using an advisory lock.
func edit(path string, change func(*wire.Object) error) error {
	target := path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		target = real
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	lock, err := os.OpenFile(target+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	root, err := loadRoot(target)
	if err != nil {
		return err
	}
	if err = change(&root); err != nil {
		return err
	}
	b, err := wire.JSON(root, true)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".tap-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(append(b, '\n')); err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temp.Name(), target)
}

// Save replaces local servers while preserving remote and other root settings.
func Save(path string, servers wire.Object) error {
	return edit(path, func(root *wire.Object) error { root.Set("servers", servers); return nil })
}

// SetRemote installs a validated relay block, or removes it when nil.
func SetRemote(path string, r *Remote) error {
	var value any
	if r != nil {
		b, _ := json.Marshal(r)
		value, _ = wire.Decode(b)
		if _, err := remoteFrom(value); err != nil {
			return err
		}
	}
	return edit(path, func(root *wire.Object) error {
		if r == nil {
			root.Delete("remote")
		} else {
			root.Set("remote", value)
		}
		return nil
	})
}

// Add saves one server without losing concurrent edits or relay settings.
func Add(path, name string, def wire.Object) error {
	if name == "" {
		return fmt.Errorf("server names cannot be empty")
	}
	if strings.Contains(name, ".") {
		return fmt.Errorf("server names cannot contain a dot: tool ids use server.tool")
	}
	return edit(path, func(root *wire.Object) error {
		servers := root.Get("servers").(wire.Object)
		servers.Set(name, def)
		root.Set("servers", servers)
		return nil
	})
}

// Remove deletes a named local server, reporting whether it existed.
func Remove(path, name string) (bool, error) {
	removed := false
	err := edit(path, func(root *wire.Object) error {
		servers := root.Get("servers").(wire.Object)
		removed = servers.Has(name)
		servers.Delete(name)
		root.Set("servers", servers)
		return nil
	})
	return removed, err
}

var variable = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
