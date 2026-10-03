// Package config reads and atomically edits tap's ordered server registry and its settings.
// It expands environment and home references only when a connection is opened.
package config

import (
	"encoding/hex"
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

// Remote selects a legacy token endpoint or a paired device whose credential is loaded separately.
type Remote struct {
	URL           string `json:"url"`
	TokenEnv      string `json:"tokenEnv"`
	AllowInsecure bool   `json:"allowInsecure,omitempty"`
	PeerID        string `json:"peerID,omitempty"`
	Fingerprint   string `json:"fingerprint,omitempty"`
	Token         string `json:"-"`
}

// RemoteProfileSet keeps named endpoints and the one selected for ordinary registry commands.
type RemoteProfileSet struct {
	Profiles map[string]Remote
	Selected string
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
	if root.Has("remotes") || root.Has("selectedRemote") {
		if _, err := remoteProfilesFrom(root); err != nil {
			return nil, err
		}
	}
	if err := checkSettings(root); err != nil {
		return nil, err
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
	r.PeerID, _ = o.Get("peerID").(string)
	r.Fingerprint, _ = o.Get("fingerprint").(string)
	if r.PeerID == "" {
		r.TokenEnv = "TAP_REMOTE_TOKEN"
	}
	if o.Has("tokenEnv") {
		r.TokenEnv, ok = o.Get("tokenEnv").(string)
		if !ok {
			return nil, fmt.Errorf("invalid remote tokenEnv")
		}
	}
	if r.PeerID == "" && !environmentName.MatchString(r.TokenEnv) {
		return nil, fmt.Errorf("invalid remote tokenEnv")
	}
	if r.PeerID != "" {
		fingerprint, err := hex.DecodeString(r.Fingerprint)
		if err != nil || len(fingerprint) != 32 {
			return nil, fmt.Errorf("paired remote requires a SHA-256 certificate fingerprint")
		}
		if !strings.HasPrefix(r.URL, "https://") {
			return nil, fmt.Errorf("paired remotes require HTTPS")
		}
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
		if f.Name != "url" && f.Name != "tokenEnv" && f.Name != "allowInsecure" && f.Name != "peerID" && f.Name != "fingerprint" {
			return nil, fmt.Errorf("unknown remote config field")
		}
	}
	return r, nil
}

func remoteProfilesFrom(root wire.Object) (RemoteProfileSet, error) {
	set := RemoteProfileSet{Profiles: map[string]Remote{}}
	if root.Has("remotes") {
		profiles, ok := root.Get("remotes").(wire.Object)
		if !ok {
			return set, fmt.Errorf("config remotes must be an object")
		}
		for _, field := range profiles {
			if field.Name == "" || strings.ContainsAny(field.Name, ". /\\") {
				return set, fmt.Errorf("invalid remote profile name")
			}
			profile, err := remoteFrom(field.Value)
			if err != nil {
				return set, fmt.Errorf("remote %s: %w", field.Name, err)
			}
			set.Profiles[field.Name] = *profile
		}
	}
	if root.Has("selectedRemote") {
		selected, ok := root.Get("selectedRemote").(string)
		if !ok {
			return set, fmt.Errorf("config selectedRemote must be a string")
		}
		if selected != "" {
			if _, ok := set.Profiles[selected]; !ok {
				return set, fmt.Errorf("selected remote profile does not exist")
			}
		}
		set.Selected = selected
	}
	return set, nil
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
	profiles, err := remoteProfilesFrom(root)
	if err != nil {
		return nil, err
	}
	if profiles.Selected != "" {
		profile := profiles.Profiles[profiles.Selected]
		if profile.PeerID != "" {
			profile.Token, err = LoadRemoteSecret(path, profile.PeerID)
			if err != nil {
				return nil, err
			}
		}
		return &profile, nil
	}
	if !root.Has("remote") {
		return nil, nil
	}
	return remoteFrom(root.Get("remote"))
}

// LoadRemoteProfiles reads named remote profiles and their selected default.
func LoadRemoteProfiles(path string) (RemoteProfileSet, error) {
	root, err := loadRoot(path)
	if err != nil {
		return RemoteProfileSet{}, err
	}
	return remoteProfilesFrom(root)
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
			root.Delete("selectedRemote")
		} else {
			root.Set("remote", value)
			root.Delete("selectedRemote")
		}
		return nil
	})
}

// SaveRemoteProfile adds or replaces a named endpoint without selecting it.
func SaveRemoteProfile(path, name string, profile Remote) error {
	if name == "" || strings.ContainsAny(name, ". /\\") {
		return fmt.Errorf("invalid remote profile name")
	}
	b, _ := json.Marshal(profile)
	value, err := wire.Decode(b)
	if err != nil {
		return fmt.Errorf("invalid remote profile")
	}
	if _, err := remoteFrom(value); err != nil {
		return err
	}
	before, err := LoadRemoteProfiles(path)
	if err != nil {
		return err
	}
	err = edit(path, func(root *wire.Object) error {
		profiles, _ := root.Get("remotes").(wire.Object)
		profiles.Set(name, value)
		root.Set("remotes", profiles)
		return nil
	})
	if err != nil {
		return err
	}
	if old := before.Profiles[name].PeerID; old != "" && old != profile.PeerID {
		after, err := LoadRemoteProfiles(path)
		if err != nil {
			return err
		}
		used := false
		for _, current := range after.Profiles {
			used = used || current.PeerID == old
		}
		if !used {
			return RemoveRemoteSecret(path, old)
		}
	}
	return nil
}

// SelectRemoteProfile makes one named endpoint the default for registry commands.
func SelectRemoteProfile(path, name string) error {
	return edit(path, func(root *wire.Object) error {
		if name == "" {
			root.Delete("selectedRemote")
			return nil
		}
		profiles, _ := root.Get("remotes").(wire.Object)
		if !profiles.Has(name) {
			return fmt.Errorf("there is no remote named %q", name)
		}
		root.Set("selectedRemote", name)
		root.Delete("remote")
		return nil
	})
}

// RemoveRemoteProfile removes a saved endpoint and clears it if it was selected.
func RemoveRemoteProfile(path, name string) (bool, error) {
	removed := false
	peerID := ""
	err := edit(path, func(root *wire.Object) error {
		profiles, _ := root.Get("remotes").(wire.Object)
		removed = profiles.Has(name)
		if profile, err := remoteFrom(profiles.Get(name)); err == nil {
			peerID = profile.PeerID
		}
		profiles.Delete(name)
		root.Set("remotes", profiles)
		if root.Get("selectedRemote") == name {
			root.Delete("selectedRemote")
		}
		return nil
	})
	if err == nil && peerID != "" {
		profiles, loadErr := LoadRemoteProfiles(path)
		if loadErr != nil {
			return removed, loadErr
		}
		stillUsed := false
		for _, profile := range profiles.Profiles {
			stillUsed = stillUsed || profile.PeerID == peerID
		}
		if !stillUsed {
			err = RemoveRemoteSecret(path, peerID)
		}
	}
	return removed, err
}

// Add saves one server without losing concurrent edits or relay settings.
func Add(path, name string, def wire.Object) error {
	_, err := Put(path, name, def)
	return err
}

// Put adds or replaces one server and reports whether the name already existed atomically.
func Put(path, name string, def wire.Object) (bool, error) {
	if name == "" {
		return false, fmt.Errorf("server names cannot be empty")
	}
	if strings.Contains(name, ".") {
		return false, fmt.Errorf("server names cannot contain a dot: tool ids use server.tool")
	}
	replaced := false
	err := edit(path, func(root *wire.Object) error {
		servers := root.Get("servers").(wire.Object)
		replaced = servers.Has(name)
		servers.Set(name, def)
		root.Set("servers", servers)
		return nil
	})
	return replaced, err
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
