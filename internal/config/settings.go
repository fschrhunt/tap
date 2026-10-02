package config

import (
	"fmt"
	"os"
	"slices"
	"strconv"

	"github.com/fschrhunt/tap/internal/wire"
)

// Setting is one choice a person may make about how tap behaves. It is kept under its Name in
// the config's settings block, and PerServer ones also in a server's own definition, where
// they win over the block. Env, when set, names the environment variable that wins over both.
type Setting struct {
	Name, About, Default, Env string
	PerServer                 bool
	// value turns text, as typed or as an environment variable holds it, into the value
	// kept in the config, refusing anything outside the setting's range.
	value func(string) (any, error)
}

// Settings lists every setting, in the order tap shows them.
var Settings = []Setting{
	{Name: "start", About: "when tap starts a server: call, search or start", Default: "call", PerServer: true, value: choice("call", "search", "start")},
	{Name: "references", About: "let an agent keep large results as references: on or off", Default: "off", Env: "TAP_REFERENCES", value: toggle},
	{Name: "searchLimit", About: "how many tools a search returns unless asked for more (1 to 25)", Default: "8", value: whole(1, 25)},
	{Name: "searchMaxBytes", About: "how much a search may return unless asked for more (1024 to 16777216)", Default: "32768", value: whole(1024, 16777216)},
	{Name: "deadlineMs", About: "how long a server may take to connect and list its tools", Default: "5000", Env: "TAP_DEADLINE_MS", value: whole(1, 2147483647)},
	{Name: "idleTimeoutMs", About: "how long an unused server keeps running", Default: "300000", Env: "TAP_IDLE_TTL_MS", PerServer: true, value: whole(0, 86400000)},
	{Name: "retryAfterMs", About: "how long tap waits before trying a failed server again", Default: "5000", Env: "TAP_FAIL_TTL_MS", value: whole(0, 86400000)},
}

// Starts are the values of the start setting, from laziest to most eager.
const (
	StartOnCall   = "call"   // only a call starts the server, once tap knows its tools
	StartOnSearch = "search" // a search starts it too, to check its tools again
	StartWithTap  = "start"  // it starts with tap and keeps running
)

// Values are the settings a tap process runs with.
type Values struct {
	Start                                   string
	References                              bool
	SearchLimit, SearchMaxBytes             int
	DeadlineMs, IdleTimeoutMs, RetryAfterMs int
}

// Value is one setting as it stands: what it is, as text, and where it came from: "default",
// "config" or the environment variable's name.
type Value struct {
	Setting
	Text, From string
}

// Find returns the setting with this name.
func Find(name string) (Setting, bool) {
	i := slices.IndexFunc(Settings, func(s Setting) bool { return s.Name == name })
	if i < 0 {
		return Setting{}, false
	}
	return Settings[i], true
}

// Parse turns text into the value the setting keeps, or says what it takes.
func (s Setting) Parse(text string) (any, error) { return s.value(text) }

// Current reads every setting: its default, then the config's settings block, then its
// environment variable. A variable whose value the setting does not take is ignored.
func Current(path string) ([]Value, error) {
	root, err := loadRoot(path)
	if err != nil {
		return nil, err
	}
	return current(root), nil
}

// current reads every setting from a config already read.
func current(root wire.Object) []Value {
	block, _ := root.Get("settings").(wire.Object)
	out := make([]Value, len(Settings))
	for i, s := range Settings {
		out[i] = Value{s, s.Default, "default"}
		if block.Has(s.Name) {
			out[i].Text, out[i].From = text(block.Get(s.Name)), "config"
		}
		if v, ok := os.LookupEnv(s.Env); ok && s.Env != "" {
			if parsed, err := s.value(v); err == nil {
				out[i].Text, out[i].From = text(parsed), s.Env
			}
		}
	}
	return out
}

// LoadValues reads the values a tap process runs with. A config that cannot be read gives
// the defaults and the environment, since whatever reads it next reports why.
func LoadValues(path string) Values {
	root, err := loadRoot(path)
	if err != nil {
		root = wire.Object{}
	}
	got := map[string]any{}
	for _, v := range current(root) {
		got[v.Name], _ = v.value(v.Text)
	}
	return Values{
		Start:          got["start"].(string),
		References:     got["references"].(bool),
		SearchLimit:    int(got["searchLimit"].(float64)),
		SearchMaxBytes: int(got["searchMaxBytes"].(float64)),
		DeadlineMs:     int(got["deadlineMs"].(float64)),
		IdleTimeoutMs:  int(got["idleTimeoutMs"].(float64)),
		RetryAfterMs:   int(got["retryAfterMs"].(float64)),
	}
}

// Own is a setting a server has of its own.
type Own struct{ Server, Name, Text string }

// Owned lists the settings servers have of their own, in the config's order.
func Owned(path string) ([]Own, error) {
	servers, err := Load(path)
	if err != nil {
		return nil, err
	}
	out := []Own{}
	for _, f := range servers {
		def := f.Value.(wire.Object)
		for _, s := range Settings {
			if s.PerServer && def.Has(s.Name) {
				out = append(out, Own{f.Name, s.Name, text(def.Get(s.Name))})
			}
		}
	}
	return out, nil
}

// Set keeps a value for a setting: in the settings block, or in one server's definition. A
// nil value removes it, so the default or the block applies again.
func Set(path, server, name string, value any) error {
	s, ok := Find(name)
	if !ok {
		return fmt.Errorf("there is no setting %q", name)
	}
	if server != "" && !s.PerServer {
		return fmt.Errorf("%s applies to every server and cannot be set for one", name)
	}
	return edit(path, func(root *wire.Object) error {
		var holder wire.Object
		if server == "" {
			holder, _ = root.Get("settings").(wire.Object)
		} else {
			servers := root.Get("servers").(wire.Object)
			def, ok := servers.Get(server).(wire.Object)
			if !ok {
				return fmt.Errorf("there is no server named %q", server)
			}
			holder = def
		}
		if value == nil {
			holder.Delete(name)
		} else {
			holder.Set(name, value)
		}
		if server == "" {
			if len(holder) == 0 {
				root.Delete("settings")
			} else {
				root.Set("settings", holder)
			}
			return nil
		}
		servers := root.Get("servers").(wire.Object)
		servers.Set(server, holder)
		root.Set("servers", servers)
		return nil
	})
}

// checkSettings refuses a settings block, or a server's own setting, that tap would not take.
func checkSettings(root wire.Object) error {
	if root.Has("settings") {
		block, ok := root.Get("settings").(wire.Object)
		if !ok {
			return fmt.Errorf("config settings must be an object")
		}
		for _, f := range block {
			if err := checkSetting(f.Name, f.Value); err != nil {
				return err
			}
		}
	}
	for _, f := range root.Get("servers").(wire.Object) {
		if def := f.Value.(wire.Object); def.Has("start") {
			if err := checkSetting("start", def.Get("start")); err != nil {
				return fmt.Errorf("server %s: %w", f.Name, err)
			}
		}
	}
	return nil
}

// checkSetting accepts a kept value only when it is what the setting makes of its own text,
// so a number kept as a string, say, is refused rather than guessed at.
func checkSetting(name string, v any) error {
	s, ok := Find(name)
	if !ok {
		return fmt.Errorf("config has no setting %q", name)
	}
	parsed, err := s.value(text(v))
	if err != nil || parsed != v {
		return fmt.Errorf("config setting %s takes %s", name, s.takes())
	}
	return nil
}

// takes says what a setting accepts, for errors.
func (s Setting) takes() string {
	_, err := s.value("\x00")
	return err.Error()
}

// text writes a kept value as a person types it.
func text(v any) string {
	if b, ok := v.(bool); ok {
		return map[bool]string{true: "on", false: "off"}[b]
	}
	return wire.String(v)
}

// choice takes one of the options as written.
func choice(options ...string) func(string) (any, error) {
	return func(s string) (any, error) {
		if slices.Contains(options, s) {
			return s, nil
		}
		return nil, fmt.Errorf("%s", or(options))
	}
}

// toggle takes on or off, and true, false, 1 or 0 as the same.
func toggle(s string) (any, error) {
	switch s {
	case "on", "true", "1":
		return true, nil
	case "off", "false", "0":
		return false, nil
	}
	return nil, fmt.Errorf("on or off")
}

// whole takes a whole number from low to high, kept as JSON keeps numbers.
func whole(low, high int) func(string) (any, error) {
	return func(s string) (any, error) {
		n, err := strconv.Atoi(s)
		if err != nil || n < low || n > high {
			return nil, fmt.Errorf("a whole number from %d to %d", low, high)
		}
		return float64(n), nil
	}
}

// or lists options as a person would say them: a, b or c.
func or(options []string) string {
	out := ""
	for i, o := range options {
		switch {
		case i == 0:
		case i == len(options)-1:
			out += " or "
		default:
			out += ", "
		}
		out += o
	}
	return out
}
