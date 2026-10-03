// Package cli is tap's command line. Run bare by an agent, tap serves MCP; every other
// invocation is a command for a person, on the same engine, which is closed before returning.
//
// Output a person asked for goes to stdout; what tap says about it goes to stderr. Exit codes
// are 0 for success, 1 for a command that failed, and 2 for a command that was called wrongly.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/fschrhunt/tap/internal/agents"
	"github.com/fschrhunt/tap/internal/auth"
	"github.com/fschrhunt/tap/internal/config"
	"github.com/fschrhunt/tap/internal/registry"
	"github.com/fschrhunt/tap/internal/remote"
	"github.com/fschrhunt/tap/internal/server"
	"github.com/fschrhunt/tap/internal/wire"
	"golang.org/x/term"
)

// misuse is a mistake in how a command was called. It is reported with the command's usage
// and exits 2.
type misuse struct{ command, message string }

func (m *misuse) Error() string { return m.message }

func wrong(command, format string, args ...any) error {
	return &misuse{command, fmt.Sprintf(format, args...)}
}

// shell is one invocation: where it writes, whether a person is watching, and the engine.
type shell struct {
	ctx         context.Context
	engine      *registry.Engine
	out, errOut io.Writer
	// watched is true when stderr is a terminal: hints and progress are for a person, and are
	// left out of logs and pipes.
	watched bool
	// plain holds the arguments after --, which are never flags.
	plain []string
}

// terminal reports whether w is an interactive terminal.
func terminal(w any) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// Run executes a command and returns its process exit code.
func Run(ctx context.Context, args []string, version string, stdout, stderr io.Writer) int {
	e := registry.New(config.Path(), version)
	defer e.Close()
	s := &shell{ctx: ctx, engine: e, out: wire.HumanWriter{Writer: stdout}, errOut: wire.HumanWriter{Writer: stderr}, watched: terminal(stderr)}
	stderr = s.errOut
	status, err := s.run(append([]string(nil), args...))
	if err == nil {
		return status
	}
	fmt.Fprintln(stderr, "tap: "+registry.Message(err))
	var m *misuse
	if !errors.As(err, &m) {
		return 1
	}
	if t := find(m.command); t != nil {
		fmt.Fprintln(stderr)
		for i, line := range t.usage {
			fmt.Fprintln(stderr, map[bool]string{true: "Usage: ", false: "       "}[i == 0]+line)
		}
		fmt.Fprintf(stderr, "Run \"tap %s --help\" for examples.\n", m.command)
	} else {
		fmt.Fprintln(stderr, "Run \"tap --help\" for the commands.")
	}
	return 2
}

// hint tells a watching person what to do next.
func (s *shell) hint(format string, args ...any) {
	if s.watched {
		fmt.Fprintf(s.errOut, format+"\n", args...)
	}
}

// during shows a watching person that tap is waiting on the network, once work has run long
// enough to look stuck, and clears the line when the work is done.
func (s *shell) during(message string, work func() error) error {
	if !s.watched {
		return work()
	}
	shown := make(chan bool, 1)
	timer := time.AfterFunc(200*time.Millisecond, func() { fmt.Fprint(s.errOut, message); shown <- true })
	err := work()
	if !timer.Stop() {
		<-shown
		if human, ok := s.errOut.(wire.HumanWriter); ok {
			fmt.Fprint(human.Writer, "\r\033[K")
		}
	}
	return err
}

// repeated collects a flag that may be given more than once.
type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

// parse reads a command's flags wherever they stand among its arguments and returns the
// arguments. It turns the flag package's errors into ones that say what to type instead.
func parse(command string, fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	positional := []string{}
	for {
		if err := fs.Parse(args); err != nil {
			text := err.Error()
			if name, ok := strings.CutPrefix(text, "flag provided but not defined: -"); ok {
				name = "--" + strings.TrimLeft(name, "-")
				known := []string{}
				fs.VisitAll(func(f *flag.Flag) {
					if len(f.Name) > 1 {
						known = append(known, "--"+f.Name)
					}
				})
				if meant := nearest(name, known); meant != "" {
					return nil, wrong(command, "%s has no flag %s. Did you mean %s?", command, name, meant)
				}
				return nil, wrong(command, "%s has no flag %s", command, name)
			}
			if name, ok := strings.CutPrefix(text, "flag needs an argument: -"); ok {
				return nil, wrong(command, "--%s needs a value", strings.TrimLeft(name, "-"))
			}
			return nil, wrong(command, "%s", strings.Replace(text, " -", " --", 1))
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional, args = append(positional, args[0]), args[1:]
	}
}

// pairs turns KEY=VALUE flags into an object, in the order given.
func pairs(command, name string, values []string) (wire.Object, error) {
	o := wire.Object{}
	for _, value := range values {
		eq := strings.Index(value, "=")
		if eq < 1 {
			return nil, wrong(command, "--%s takes KEY=VALUE, and \"%s\" is not that", name, value)
		}
		o.Set(value[:eq], value[eq+1:])
	}
	return o, nil
}

// print preserves JSON bytes for scripts and uses the human writer for terminal text.
func print(w io.Writer, value any, json bool) {
	if json {
		if human, ok := w.(wire.HumanWriter); ok {
			w = human.Writer
		}
		b, _ := wire.JSON(value, true)
		fmt.Fprintln(w, string(b))
	} else {
		fmt.Fprintln(w, value)
	}
}

// run dispatches one invocation.
func (s *shell) run(args []string) (int, error) {
	e := s.engine
	// Arguments after -- belong to a server's command, so help and flags are looked for before it.
	before, after, split := args, []string(nil), false
	for i, arg := range args {
		if arg == "--" {
			before, after, split = args[:i], args[i+1:], true
			break
		}
	}
	if len(before) == 0 && !split {
		// A person who runs tap bare gets help; an agent, whose end is a pipe, gets the server.
		if terminal(os.Stdin) {
			brief(s.out, e.Version)
			return 0, nil
		}
		backend, closer, err := s.backend(false)
		if err != nil {
			return 1, err
		}
		defer closer()
		if backend == server.Backend(e) {
			// The agent's first search usually comes a moment after it starts tap.
			go e.Warm()
		}
		return 0, server.Serve(s.ctx, backend, e.Version, e.Settings())
	}
	if len(before) == 0 {
		return 2, wrong("", "a command comes before --")
	}
	command, rest := before[0], before[1:]
	for _, arg := range rest {
		if arg == "-h" || arg == "--help" {
			rest = []string{command}
			command = "help"
			break
		}
	}
	s.plain = after
	switch command {
	case "-h", "--help":
		full(s.out, e.Version)
		return 0, nil
	case "help":
		if len(rest) == 0 {
			full(s.out, e.Version)
			return 0, nil
		}
		t := find(rest[0])
		if t == nil {
			return 2, s.unknown(rest[0])
		}
		explain(s.out, t)
		return 0, nil
	case "version", "--version", "-v":
		print(s.out, e.Version, false)
		return 0, nil
	case "config":
		return s.config(rest)
	case "path":
		if len(rest) > 0 {
			return 2, wrong("path", "path takes no arguments")
		}
		print(s.out, e.Path, false)
		return 0, nil
	case "add":
		return s.add(rest, after, split)
	case "remove":
		return s.remove(rest)
	case "list":
		return s.list(rest)
	case "refresh":
		return s.refresh(rest)
	case "inspect":
		return s.inspect(rest)
	case "search":
		return s.search(rest)
	case "call":
		return s.call(rest)
	case "import":
		return s.imports(rest)
	case "auth":
		return s.auth(rest)
	case "remote":
		return s.remote(rest)
	}
	return 2, s.unknown(command)
}

// config shows tap's settings, or one of them, and changes them: set keeps a value, unset
// returns a setting to the general value or its default. --server reads and writes a server's
// own value, for the settings a server may have. Settings live in this machine's config.
func (s *shell) config(args []string) (int, error) {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	json := fs.Bool("json", false, "")
	only := fs.String("server", "", "")
	args, err := parse("config", fs, args)
	if err != nil {
		return 2, err
	}
	setting := func(name string) (config.Setting, error) {
		found, ok := config.Find(name)
		if ok {
			return found, nil
		}
		names := []string{}
		for _, known := range config.Settings {
			names = append(names, known.Name)
		}
		if meant := nearest(name, names); meant != "" {
			return found, wrong("config", "there is no setting \"%s\". Did you mean \"%s\"?", name, meant)
		}
		return found, wrong("config", "there is no setting \"%s\". Run \"tap config\" to see them", name)
	}
	// value is what a setting stands at, for one server when one is named.
	value := func(name string) (string, error) {
		if *only != "" {
			owned, err := config.Owned(s.engine.Path)
			if err != nil {
				return "", err
			}
			for _, own := range owned {
				if own.Server == *only && own.Name == name {
					return own.Text, nil
				}
			}
		}
		current, err := config.Current(s.engine.Path)
		if err != nil {
			return "", err
		}
		for _, v := range current {
			if v.Name == name {
				return v.Text, nil
			}
		}
		return "", nil
	}
	if len(args) == 0 || (len(args) == 1 && args[0] != "set" && args[0] != "unset") {
		if *only != "" && len(args) == 0 {
			return 2, wrong("config", "--server needs the name of a setting, such as \"tap config start --server %s\"", *only)
		}
		if len(args) == 1 {
			found, err := setting(args[0])
			if err != nil {
				return 2, err
			}
			if *only != "" && !found.PerServer {
				return 2, wrong("config", "%s applies to every server, so it has no value for one", found.Name)
			}
			text, err := value(found.Name)
			if err != nil {
				return 1, err
			}
			print(s.out, text, false)
			return 0, nil
		}
		current, err := config.Current(s.engine.Path)
		if err != nil {
			return 1, err
		}
		owned, err := config.Owned(s.engine.Path)
		if err != nil {
			return 1, err
		}
		if *json {
			out := wire.Object{}
			for _, v := range current {
				kept, _ := v.Parse(v.Text)
				out.Set(v.Name, wire.Object{{Name: "value", Value: kept}, {Name: "from", Value: v.From}})
			}
			servers := wire.Object{}
			for _, own := range owned {
				o, _ := servers.Get(own.Server).(wire.Object)
				found, _ := config.Find(own.Name)
				kept, _ := found.Parse(own.Text)
				servers.Set(own.Server, append(o, wire.Field{Name: own.Name, Value: kept}))
			}
			print(s.out, wire.Object{{Name: "settings", Value: out}, {Name: "servers", Value: servers}}, true)
			return 0, nil
		}
		name, text, from := 0, 0, 0
		for _, v := range current {
			name, text, from = max(name, len(v.Name)), max(text, len(v.Text)), max(from, len(v.From))
		}
		for _, v := range current {
			fmt.Fprintf(s.out, "%-*s  %-*s  %-*s  %s\n", name, v.Name, text, v.Text, from, v.From, v.About)
		}
		if len(owned) > 0 {
			fmt.Fprintln(s.out, "\nServers with their own:")
			width := 0
			for _, own := range owned {
				width = max(width, len(own.Server))
			}
			for _, own := range owned {
				fmt.Fprintf(s.out, "%-*s  %s: %s\n", width, own.Server, own.Name, own.Text)
			}
		}
		return 0, nil
	}
	verb := args[0]
	if (verb == "set" && len(args) != 3) || (verb == "unset" && len(args) != 2) {
		return 2, wrong("config", "%s takes a setting's name%s", verb, map[bool]string{true: " and its value", false: ""}[verb == "set"])
	}
	found, err := setting(args[1])
	if err != nil {
		return 2, err
	}
	if *only != "" && !found.PerServer {
		return 2, wrong("config", "%s applies to every server and cannot be set for one", found.Name)
	}
	var kept any
	if verb == "set" {
		if kept, err = found.Parse(args[2]); err != nil {
			return 2, wrong("config", "%s takes %s", found.Name, err)
		}
	}
	if err = config.Set(s.engine.Path, *only, found.Name, kept); err != nil {
		return 1, err
	}
	text, err := value(found.Name)
	if err != nil {
		return 1, err
	}
	switch {
	case *only != "" && verb == "set":
		print(s.out, fmt.Sprintf("%s is now %s for %s", found.Name, text, *only), false)
	case *only != "":
		print(s.out, fmt.Sprintf("%s follows the general %s again: %s", *only, found.Name, text), false)
	default:
		print(s.out, fmt.Sprintf("%s is now %s", found.Name, text), false)
	}
	if found.Env != "" && os.Getenv(found.Env) != "" {
		s.hint("%s is set in this shell and wins over the config here.", found.Env)
	}
	s.hint("An agent's tap reads its settings when it starts: start a new agent session to use this.")
	return 0, nil
}

// unknown names a command tap does not have and, when it can, the one that was meant.
func (s *shell) unknown(command string) error {
	switch command {
	case "serve", "server", "mcp", "start", "run":
		return wrong("", "there is no \"%s\" command. Run with no command, tap serves MCP: that is how an agent starts it", command)
	}
	names := []string{}
	for _, t := range topics {
		names = append(names, t.name)
	}
	if meant := nearest(command, names); meant != "" {
		return wrong("", "there is no \"%s\" command. Did you mean \"%s\"?", command, meant)
	}
	return wrong("", "there is no \"%s\" command", command)
}

// backend returns the registry a command works on: the selected remote, or this machine's
// when none is selected or local is asked for. The second result releases it.
func (s *shell) backend(local bool) (server.Backend, func(), error) {
	if !local {
		cfg, err := config.LoadRemote(s.engine.Path)
		if err != nil {
			return nil, nil, err
		}
		if cfg != nil {
			relay, err := remote.New(*cfg, s.engine.Version)
			if err != nil {
				return nil, nil, err
			}
			return relay, relay.Close, nil
		}
	}
	return s.engine, func() {}, nil
}

// add saves one server: an address for an HTTP server, or the command after -- for a stdio one.
func (s *shell) add(args, command []string, split bool) (int, error) {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	var envs, headers repeated
	fs.Var(&envs, "env", "")
	fs.Var(&headers, "header", "")
	cwd := fs.String("cwd", "", "")
	bearer := fs.String("bearer-token-env", "", "")
	var allow, deny, referenceTo repeated
	fs.Var(&allow, "allow-tool", "")
	fs.Var(&deny, "deny-tool", "")
	fs.Var(&referenceTo, "reference-to", "")
	idle := fs.Int("idle-timeout-ms", 0, "")
	local := fs.Bool("local", false, "")
	args, err := parse("add", fs, args)
	if err != nil {
		return 2, err
	}
	if len(args) == 0 || args[0] == "" {
		return 2, wrong("add", "add needs a name for the server")
	}
	name := args[0]
	env, err := pairs("add", "env", envs)
	if err != nil {
		return 2, err
	}
	header, err := pairs("add", "header", headers)
	if err != nil {
		return 2, err
	}
	policy := wire.Object{}
	for _, f := range []struct {
		flag, key string
		values    []string
	}{{"allow-tool", "allow", allow}, {"deny-tool", "deny", deny}, {"reference-to", "referenceTo", referenceTo}} {
		list := []any{}
		for _, v := range f.values {
			if v == "" {
				return 2, wrong("add", "--%s needs a value", f.flag)
			}
			if _, err := path.Match(v, ""); err != nil && f.key != "referenceTo" {
				return 2, wrong("add", "--%s takes a pattern like \"read_*\", and \"%s\" is not one", f.flag, v)
			}
			list = append(list, v)
		}
		if len(list) > 0 {
			policy.Set(f.key, list)
		}
	}
	idleSet := false
	fs.Visit(func(f *flag.Flag) { idleSet = idleSet || f.Name == "idle-timeout-ms" })
	if idleSet && (*idle < 1 || *idle > 86400000) {
		return 2, wrong("add", "--idle-timeout-ms takes a number of milliseconds from 1 to 86400000")
	}
	var def wire.Object
	kind := "http"
	switch {
	case split:
		kind = "stdio"
		if len(command) == 0 {
			return 2, wrong("add", "nothing follows --; the server's command goes there")
		}
		if len(args) > 1 {
			return 2, wrong("add", "\"%s\" stands before --, where only the name and flags go", args[1])
		}
		if len(header) > 0 || *bearer != "" {
			return 2, wrong("add", "--header and --bearer-token-env are for an address, not a command")
		}
		argv := make([]any, len(command))
		for i, part := range command {
			argv[i] = part
		}
		def = wire.Object{{Name: "type", Value: "stdio"}, {Name: "command", Value: argv}}
		if len(env) > 0 {
			def.Set("env", env)
		}
		if *cwd != "" {
			def.Set("cwd", *cwd)
		}
		if idleSet {
			def.Set("idleTimeoutMs", *idle)
		}
	case len(args) == 2:
		at, err := url.Parse(args[1])
		if err != nil || at.Host == "" || (at.Scheme != "http" && at.Scheme != "https") {
			return 2, wrong("add", "\"%s\" is not an http or https address. For a command, put it after --", args[1])
		}
		if len(env) > 0 || *cwd != "" || idleSet {
			return 2, wrong("add", "--env, --cwd and --idle-timeout-ms are for a command, not an address")
		}
		def = wire.Object{{Name: "type", Value: "http"}, {Name: "url", Value: args[1]}}
		if len(header) > 0 {
			def.Set("headers", header)
		}
		if *bearer != "" {
			def.Set("bearerTokenEnv", *bearer)
		}
	case len(args) == 1:
		return 2, wrong("add", "add needs the server's address, or its command after --")
	default:
		return 2, wrong("add", "add takes one address; \"%s\" is one too many. A command goes after --", args[2])
	}
	if len(policy) > 0 {
		def.Set("policy", policy)
	}
	verb := "added"
	backend, closer, err := s.backend(*local)
	if err != nil {
		return 1, err
	}
	defer closer()
	if relay, ok := backend.(*remote.Client); ok {
		if _, err = relay.Edit(s.ctx, name, def); err != nil {
			return 1, err
		}
	} else {
		if had, _ := config.Load(s.engine.Path); had.Has(name) {
			verb = "replaced"
		}
		if err = config.Add(s.engine.Path, name, def); err != nil {
			return 1, err
		}
	}
	print(s.out, fmt.Sprintf("%s %s (%s)", verb, name, kind), false)
	s.hint("Run \"tap list\" to check that it connects.")
	return 0, nil
}

// remove deletes a server and any sign-in saved for it.
func (s *shell) remove(args []string) (int, error) {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	local := fs.Bool("local", false, "")
	args, err := parse("remove", fs, args)
	if err != nil {
		return 2, err
	}
	if len(args) != 1 || args[0] == "" {
		return 2, wrong("remove", "remove takes the name of one server")
	}
	backend, closer, err := s.backend(*local)
	if err != nil {
		return 1, err
	}
	defer closer()
	removed := false
	if relay, ok := backend.(*remote.Client); ok {
		removed, err = relay.Edit(s.ctx, args[0], nil)
	} else {
		if _, err = auth.Remove(s.engine.Path, args[0]); err == nil {
			removed, err = config.Remove(s.engine.Path, args[0])
		}
	}
	if err != nil {
		return 1, err
	}
	if !removed {
		return 1, fmt.Errorf("there is no server named \"%s\". Run \"tap list\" to see the ones there are", args[0])
	}
	print(s.out, "removed "+args[0], false)
	return 0, nil
}

var layers = regexp.MustCompile(`^(?:(?:calling|sending) "[^"]*": )+`)

// reason rewrites why a server is unavailable for a person: what went wrong, without the
// layers it passed through, and what to try where tap knows.
func reason(text string) string {
	text = layers.ReplaceAllString(text, "")
	switch {
	case strings.HasPrefix(text, "spawn ") && strings.HasSuffix(text, " ENOENT"):
		return "its command was not found: " + strings.TrimSuffix(strings.TrimPrefix(text, "spawn "), " ENOENT")
	case strings.HasPrefix(text, "spawn ") && strings.HasSuffix(text, " EACCES"):
		return "its command is not executable: " + strings.TrimSuffix(strings.TrimPrefix(text, "spawn "), " EACCES")
	case text == "fetch failed":
		return "its address could not be reached"
	case text == "Connection closed":
		return "it closed the connection before answering"
	case strings.HasPrefix(text, "server deadline exceeded"):
		return "it did not answer in time " + strings.TrimPrefix(text, "server deadline exceeded ")
	case text == "Method Not Allowed", text == "Not Found", text == "Bad Request", text == "Forbidden":
		return "its address did not answer as an MCP server (" + text + ")"
	case text == "Unauthorized":
		return "it refused the credential it was sent"
	}
	return text
}

// tools counts tools in words.
func tools(count any) string {
	if wire.String(count) == "1" {
		return "1 tool"
	}
	return wire.String(count) + " tools"
}

// rows prints each server with its tool count, or why it is unavailable.
func (s *shell) rows(result wire.Object, backend server.Backend) {
	rows := result.Get("integrations").([]any)
	if len(rows) == 0 {
		where := s.engine.Path
		if _, relayed := backend.(*remote.Client); relayed {
			where = "the remote"
		}
		print(s.out, "No servers yet in "+where+".", false)
		s.hint("Run \"tap import\" to bring over the ones your agents have, or \"tap add --help\" to add one.")
		return
	}
	width := 0
	for _, v := range rows {
		width = max(width, len(wire.String(v.(wire.Object).Get("server"))))
	}
	for _, v := range rows {
		row := v.(wire.Object)
		summary := tools(row.Get("tools"))
		switch {
		case row.Has("error"):
			summary = "unavailable: " + reason(wire.String(row.Get("error")))
			if wire.String(row.Get("tools")) != "0" {
				summary += " (" + tools(row.Get("tools")) + " when last reached)"
			}
		case row.Get("source") == "cache":
			summary += " (as last listed; not checked now)"
		}
		fmt.Fprintf(s.out, "%-*s  %s\n", width, wire.String(row.Get("server")), summary)
	}
}

// list shows every server with its tool count, or why it is unavailable. It asks the servers,
// so what it prints is how things are now; --cached prints what tap last saw without asking.
func (s *shell) list(args []string) (int, error) {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	json := fs.Bool("json", false, "")
	local := fs.Bool("local", false, "")
	cached := fs.Bool("cached", false, "")
	args, err := parse("list", fs, args)
	if err != nil {
		return 2, err
	}
	if len(args) > 0 {
		return 2, wrong("list", "list takes no arguments; \"tap search %s\" looks for tools", strings.Join(args, " "))
	}
	backend, closer, err := s.backend(*local)
	if err != nil {
		return 1, err
	}
	defer closer()
	var result wire.Object
	err = s.during("Asking the servers for their tools…", func() error {
		if *cached {
			result, err = backend.Listing(s.ctx, true)
		} else {
			result, err = backend.Refresh(s.ctx, "", true)
		}
		return err
	})
	if err != nil {
		return 1, err
	}
	if *json {
		print(s.out, result, true)
		return 0, nil
	}
	s.rows(result, backend)
	return 0, nil
}

// refresh asks one server, or all of them, for its tools again and prints what it found.
func (s *shell) refresh(args []string) (int, error) {
	fs := flag.NewFlagSet("refresh", flag.ContinueOnError)
	json := fs.Bool("json", false, "")
	local := fs.Bool("local", false, "")
	args, err := parse("refresh", fs, args)
	if err != nil {
		return 2, err
	}
	if len(args) > 1 {
		return 2, wrong("refresh", "refresh takes the name of one server, or none for all of them")
	}
	name := ""
	if len(args) == 1 {
		name = args[0]
	}
	backend, closer, err := s.backend(*local)
	if err != nil {
		return 1, err
	}
	defer closer()
	var result wire.Object
	if err = s.during("Asking the servers for their tools…", func() error { result, err = backend.Refresh(s.ctx, name, true); return err }); err != nil {
		return 1, err
	}
	if *json {
		print(s.out, result, true)
		return 0, nil
	}
	s.rows(result, backend)
	return 0, nil
}

// inspect prints one tool's whole contract: its description and its schemas.
func (s *shell) inspect(args []string) (int, error) {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	local := fs.Bool("local", false, "")
	fs.Bool("json", true, "")
	args, err := parse("inspect", fs, args)
	if err != nil {
		return 2, err
	}
	if len(args) != 1 {
		return 2, wrong("inspect", "inspect takes the id of one tool, as \"tap search\" prints it")
	}
	if dot := strings.Index(args[0], "."); dot < 1 || dot == len(args[0])-1 {
		return 2, wrong("inspect", "\"%s\" is not a tool id. An id is server.tool, as \"tap search\" prints it", args[0])
	}
	backend, closer, err := s.backend(*local)
	if err != nil {
		return 1, err
	}
	defer closer()
	var result wire.Object
	err = s.during("Asking the server for the tool…", func() error {
		result, err = backend.Discover(s.ctx, registry.SearchOptions{IDs: args, Detail: "full", Limit: 25, MaxBytes: 16777216}, true)
		return err
	})
	if err != nil {
		return 1, err
	}
	print(s.out, result, true)
	return 0, nil
}

// search prints the tools that match a query, as an agent would find them, or every tool of
// one server when only --server is given. It answers from the tool lists tap has saved, as an
// agent's tap does; --refresh asks the servers first.
func (s *shell) search(args []string) (int, error) {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	json := fs.Bool("json", false, "")
	local := fs.Bool("local", false, "")
	limit := fs.String("limit", strconv.Itoa(s.engine.Settings().SearchLimit), "")
	only := fs.String("server", "", "")
	detail := fs.String("detail", "full", "")
	offset := fs.Int("offset", 0, "")
	budget := fs.Int("max-bytes", 16777216, "")
	refresh := fs.Bool("refresh", false, "")
	args, err := parse("search", fs, args)
	if err != nil {
		return 2, err
	}
	count, err := strconv.Atoi(*limit)
	if err != nil || count < 1 {
		return 2, wrong("search", "--limit takes a whole number of at least 1, and \"%s\" is not one", *limit)
	}
	if *detail != "auto" && *detail != "full" && *detail != "summary" {
		return 2, wrong("search", "--detail takes auto, full or summary")
	}
	if *offset < 0 || *offset > 1000000 {
		return 2, wrong("search", "--offset takes a number from 0 to 1000000")
	}
	if *budget < 1024 || *budget > 16777216 {
		return 2, wrong("search", "--max-bytes takes a number from 1024 to 16777216")
	}
	query := strings.TrimSpace(strings.Join(append(args, s.plain...), " "))
	if query == "" && *only == "" {
		return 2, wrong("search", "search needs something to look for, or --server to list one server's tools")
	}
	backend, closer, err := s.backend(*local)
	if err != nil {
		return 1, err
	}
	defer closer()
	var result wire.Object
	err = s.during("Asking the servers for their tools…", func() error {
		result, err = backend.Discover(s.ctx, registry.SearchOptions{Query: query, Server: *only, Detail: *detail, Limit: float64(count), Offset: *offset, MaxBytes: *budget, Refresh: *refresh}, true)
		return err
	})
	if err != nil {
		return 1, err
	}
	if *json {
		print(s.out, result, true)
		return 0, nil
	}
	matches := result.Get("matches").([]any)
	lines := []string{}
	saved := false
	for _, v := range matches {
		t := v.(wire.Object)
		desc, _ := t.Get("description").(string)
		saved = saved || t.Get("stale") == true
		lines = append(lines, strings.TrimRight(wire.String(t.Get("id"))+"\n  "+desc, " \t\r\n"))
	}
	if len(lines) == 0 {
		if query == "" {
			lines = append(lines, *only+" lists no tools.")
		} else {
			lines = append(lines, fmt.Sprintf("No tool matches \"%s\". Try other words, or \"tap list\" for the servers.", query))
		}
	}
	if result.Has("nextOffset") {
		next := int(wire.Number(wire.String(result.Get("nextOffset"))))
		total := int(wire.Number(wire.String(result.Get("total"))))
		lines = append(lines, fmt.Sprintf("(%d more; continue with --offset %d)", total-next, next))
	}
	for _, v := range result.Get("unavailable").([]any) {
		row := v.(wire.Object)
		lines = append(lines, wire.String(row.Get("server"))+" is unavailable: "+reason(wire.String(row.Get("error"))))
	}
	print(s.out, strings.Join(lines, "\n"), false)
	if saved {
		s.hint("These are from the tool lists tap saved. Add --refresh to ask the servers first.")
	}
	return 0, nil
}

// call runs one tool and prints what it returns. A tool that reports an error exits 1.
func (s *shell) call(args []string) (int, error) {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	json := fs.Bool("json", false, "")
	local := fs.Bool("local", false, "")
	raw := fs.String("args", "", "")
	args, err := parse("call", fs, args)
	if err != nil {
		return 2, err
	}
	args = append(args, s.plain...)
	if len(args) == 0 || args[0] == "" {
		return 2, wrong("call", "call needs the id of a tool, as \"tap search\" prints it")
	}
	id := args[0]
	if dot := strings.Index(id, "."); dot < 1 || dot == len(id)-1 {
		return 2, wrong("call", "\"%s\" is not a tool id. An id is server.tool, as \"tap search\" prints it", id)
	}
	if *raw == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return 1, fmt.Errorf("cannot read the arguments from standard input")
		}
		*raw = string(b)
	}
	values, err := toolArgs(*raw, args[1:])
	if err != nil {
		return 2, err
	}
	backend, closer, err := s.backend(*local)
	if err != nil {
		return 1, err
	}
	defer closer()
	var result wire.Object
	if err = s.during("Calling "+id+"…", func() error { result, err = backend.Call(s.ctx, id, values, true); return err }); err != nil {
		return 1, err
	}
	failed := result.Get("isError") == true
	if *json {
		print(s.out, result, true)
	} else {
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
		print(s.out, text, false)
	}
	if failed {
		fmt.Fprintln(s.errOut, "tap: "+id+" reported an error")
		return 1, nil
	}
	return 0, nil
}

// toolArgs combines JSON arguments with KEY=VALUE strings, which replace the same keys.
// Numbers in the JSON keep their spelling.
func toolArgs(raw string, pairs []string) (any, error) {
	var values any = wire.Object{}
	if raw != "" {
		v, err := wire.DecodeExact([]byte(raw))
		if err != nil {
			return nil, wrong("call", "--args is not valid JSON: %s", config.JSONError([]byte(raw), err))
		}
		values = v
	}
	for _, pair := range pairs {
		eq := strings.Index(pair, "=")
		if eq < 1 {
			return nil, wrong("call", "an argument is KEY=VALUE, and \"%s\" is not that", pair)
		}
		o, ok := values.(wire.Object)
		if !ok {
			return nil, wrong("call", "--args must be a JSON object for %s to be added to it", pair)
		}
		o.Set(pair[:eq], pair[eq+1:])
		values = o
	}
	return values, nil
}

// imports adds the servers that coding agents already have. It reads the agents' files and
// changes only tap's.
func (s *shell) imports(args []string) (int, error) {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	var dry, force bool
	fs.BoolVar(&dry, "dry-run", false, "")
	fs.BoolVar(&dry, "n", false, "")
	fs.BoolVar(&force, "force", false, "")
	fs.BoolVar(&force, "f", false, "")
	json := fs.Bool("json", false, "")
	local := fs.Bool("local", false, "")
	args, err := parse("import", fs, args)
	if err != nil {
		return 2, err
	}
	if remoteCfg, err := config.LoadRemote(s.engine.Path); err != nil {
		return 1, err
	} else if remoteCfg != nil && !*local {
		return 1, fmt.Errorf("a remote is selected, and this machine's commands and paths would not work there. Run \"tap import --local\" to add them to this machine's servers")
	}
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	known := agents.Places(home, cwd)
	places := []agents.Place{}
	if len(args) == 0 {
		for _, place := range known {
			if _, err := os.Stat(place.Path); err == nil {
				places = append(places, place)
			}
		}
		if len(places) == 0 {
			looked := []string{}
			for _, place := range known {
				looked = append(looked, "  "+place.Path)
			}
			return 1, fmt.Errorf("no agent's config was found. tap looked for:\n%s\nPass the path of a config file to read another", strings.Join(looked, "\n"))
		}
	}
	for _, source := range args {
		found := false
		for _, place := range known {
			if place.ID == source {
				if _, err := os.Stat(place.Path); err == nil {
					places, found = append(places, place), true
				}
			}
		}
		if found {
			continue
		}
		if _, err := os.Stat(source); err == nil {
			places = append(places, agents.Place{ID: "file", Agent: filepath.Base(source), Path: source})
			continue
		}
		ids := []string{"claude", "codex", "opencode", "cursor", "vscode"}
		for _, id := range ids {
			if id == source {
				return 1, fmt.Errorf("no config for %s was found on this machine", source)
			}
		}
		if meant := nearest(source, ids); meant != "" {
			return 2, wrong("import", "\"%s\" is not an agent tap knows or a file. Did you mean %s?", source, meant)
		}
		return 2, wrong("import", "\"%s\" is not an agent tap knows or a file. The agents are %s", source, strings.Join(ids, ", "))
	}
	existing, err := config.Load(s.engine.Path)
	if err != nil {
		return 1, err
	}
	report := []any{}
	added, literal := 0, false
	for _, place := range places {
		found, err := agents.Read(place, cwd)
		if err != nil {
			return 1, err
		}
		if len(found.Servers)+len(found.Skipped) == 0 {
			continue
		}
		lines := []string{}
		entry := wire.Object{{Name: "agent", Value: found.Agent}, {Name: "path", Value: found.Path}}
		servers, skipped := []any{}, []any{}
		for _, server := range found.Servers {
			same := false
			if had, ok := existing.Get(server.Name).(wire.Object); ok {
				a, _ := wire.JSON(had, false)
				b, _ := wire.JSON(server.Def, false)
				same = string(a) == string(b)
			}
			kind := wire.String(server.Def.Get("type"))
			switch {
			case same:
				lines = append(lines, "  has "+server.Name+" already")
				skipped = append(skipped, wire.Object{{Name: "name", Value: server.Name}, {Name: "why", Value: "tap has it already"}})
				continue
			case existing.Has(server.Name) && !force:
				lines = append(lines, "  skipped "+server.Name+": tap has another server by that name; --force replaces it")
				skipped = append(skipped, wire.Object{{Name: "name", Value: server.Name}, {Name: "why", Value: "tap has another server by that name"}})
				continue
			}
			if !dry {
				if err = config.Add(s.engine.Path, server.Name, server.Def); err != nil {
					return 1, err
				}
			}
			line := map[bool]string{true: "  would add ", false: "  added "}[dry] + server.Name + " (" + kind + ")"
			if server.Note != "" {
				line += ", " + server.Note
			}
			lines = append(lines, line)
			existing.Set(server.Name, server.Def)
			servers = append(servers, wire.Object{{Name: "name", Value: server.Name}, {Name: "definition", Value: server.Def}})
			added++
			for _, key := range []string{"env", "headers"} {
				values, _ := server.Def.Get(key).(wire.Object)
				for _, f := range values {
					if v := wire.String(f.Value); v != "" && !strings.Contains(v, "${") {
						literal = true
					}
				}
			}
		}
		for _, skip := range found.Skipped {
			lines = append(lines, "  skipped "+skip.Name+": "+skip.Why)
			skipped = append(skipped, wire.Object{{Name: "name", Value: skip.Name}, {Name: "why", Value: skip.Why}})
		}
		entry.Set("servers", servers)
		entry.Set("skipped", skipped)
		report = append(report, entry)
		if !*json {
			print(s.out, "From "+found.Agent+" ("+found.Path+")\n"+strings.Join(lines, "\n"), false)
		}
	}
	if *json {
		print(s.out, wire.Object{{Name: "dryRun", Value: dry}, {Name: "sources", Value: report}}, true)
		return 0, nil
	}
	switch {
	case len(report) == 0:
		print(s.out, "No servers were found in the configs tap read.", false)
	case dry:
		s.hint("Nothing was changed. Run \"tap import\" to add them.")
	case added > 0:
		if literal {
			s.hint("Some values were copied as written, secrets included. To keep one out of tap's config, put it in an environment variable and write ${NAME} in its place.")
		}
		s.hint("Run \"tap list\" to check that they connect. Then take them out of each agent's config and leave tap there: your agent loads two tools from then on.")
	}
	return 0, nil
}

// browser opens a page in the person's browser.
func browser(page string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	path, err := exec.LookPath(opener)
	if err != nil {
		return err
	}
	return exec.Command(path, page).Start()
}

// auth signs in to a server that uses OAuth, or forgets its sign-in.
func (s *shell) auth(args []string) (int, error) {
	fs := flag.NewFlagSet("auth", flag.ContinueOnError)
	client := fs.String("client-id", "", "")
	secretFile := fs.String("client-secret-file", "", "")
	var port int
	fs.IntVar(&port, "port", 0, "")
	fs.IntVar(&port, "p", 0, "")
	quiet := fs.Bool("no-browser", false, "")
	forget := fs.Bool("remove", false, "")
	local := fs.Bool("local", false, "")
	args, err := parse("auth", fs, args)
	if err != nil {
		return 2, err
	}
	if len(args) != 1 || args[0] == "" {
		return 2, wrong("auth", "auth takes the name of one server")
	}
	name := args[0]
	if port < 0 || port > 65535 {
		return 2, wrong("auth", "--port takes a port from 1 to 65535")
	}
	if remoteCfg, err := config.LoadRemote(s.engine.Path); err != nil {
		return 1, err
	} else if remoteCfg != nil && !*local {
		return 1, fmt.Errorf("a remote is selected, and a sign-in is kept on the machine that runs the server. Run \"tap auth %s\" there, or pass --local for this machine's servers", name)
	}
	servers, err := config.Load(s.engine.Path)
	if err != nil {
		return 1, err
	}
	def, ok := servers.Get(name).(wire.Object)
	if !ok {
		return 1, fmt.Errorf("there is no server named \"%s\". Run \"tap list\" to see the ones there are", name)
	}
	endpoint, _ := def.Get("url").(string)
	if endpoint == "" {
		return 1, fmt.Errorf("%s runs a command, and only a server with an address signs in this way", name)
	}
	if *forget {
		removed, err := auth.Remove(s.engine.Path, name)
		if err != nil {
			return 1, err
		}
		print(s.out, map[bool]string{true: "signed out of " + name, false: name + " had no sign-in"}[removed], false)
		return 0, nil
	}
	secret := ""
	if *secretFile != "" {
		var b []byte
		if *secretFile == "-" {
			b, err = io.ReadAll(os.Stdin)
		} else {
			b, err = os.ReadFile(*secretFile)
		}
		if err != nil {
			return 1, fmt.Errorf("cannot read the client secret from %s", *secretFile)
		}
		secret = strings.TrimSpace(string(b))
	}
	o := auth.Options{ConfigPath: s.engine.Path, Name: name, Endpoint: endpoint, Headers: map[string][]string{}, ClientID: *client, ClientSecret: secret,
		Port: port, Say: s.errOut, Version: s.engine.Version}
	if h, ok := def.Get("headers").(wire.Object); ok {
		for _, f := range h {
			o.Headers.Set(f.Name, config.Expand(f.Value))
		}
	}
	if !*quiet {
		o.Open = browser
	}
	if terminal(os.Stdin) && *secretFile != "-" {
		o.Paste = os.Stdin
	}
	had := auth.Has(s.engine.Path, name, endpoint)
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Minute)
	defer cancel()
	result, err := auth.Authorize(ctx, o)
	if err != nil {
		return 1, err
	}
	switch {
	case result.SignedIn:
		print(s.out, fmt.Sprintf("signed in to %s (%s)", name, tools(result.Tools)), false)
		s.hint("Agents that are already running find its tools on their next search.")
	case had:
		print(s.out, fmt.Sprintf("%s is signed in already (%s)", name, tools(result.Tools)), false)
		s.hint("To sign in as someone else, run \"tap auth %s --remove\" first.", name)
	default:
		print(s.out, fmt.Sprintf("%s did not ask for a sign-in (%s)", name, tools(result.Tools)), false)
	}
	return 0, nil
}

// remote selects a relay, or serves this machine's servers to others.
func (s *shell) remote(args []string) (int, error) {
	path, version := s.engine.Path, s.engine.Version
	if len(args) == 0 {
		return 2, wrong("remote", "remote needs one of serve, use, off or status")
	}
	command, args := args[0], args[1:]
	fs := flag.NewFlagSet("remote", flag.ContinueOnError)
	addr := fs.String("addr", "", "")
	cert := fs.String("tls-cert", "", "")
	key := fs.String("tls-key", "", "")
	tokenEnv := fs.String("token-env", "TAP_REMOTE_TOKEN", "")
	insecure := fs.Bool("allow-insecure", false, "")
	args, err := parse("remote", fs, args)
	if err != nil {
		return 2, err
	}
	switch command {
	case "serve":
		if len(args) != 0 {
			return 2, wrong("remote", "remote serve takes only flags; \"%s\" is not one", args[0])
		}
		return 0, remote.Serve(s.ctx, path, version, remote.Options{Addr: *addr, TLSCert: *cert, TLSKey: *key, AllowInsecure: *insecure})
	case "use":
		if len(args) != 1 {
			return 2, wrong("remote", "remote use takes the remote's address")
		}
		endpoint, err := config.NormalizeURL(args[0], *insecure)
		if err != nil {
			return 1, err
		}
		if err = config.SetRemote(path, &config.Remote{URL: endpoint, TokenEnv: *tokenEnv, AllowInsecure: *insecure}); err != nil {
			return 1, err
		}
		print(s.out, "remote configured: "+endpoint, false)
		if os.Getenv(*tokenEnv) == "" {
			s.hint("%s is not set here. Set it to the remote's token before using tap.", *tokenEnv)
		} else {
			s.hint("Run \"tap list\" to see the remote's servers.")
		}
		return 0, nil
	case "off":
		if len(args) != 0 {
			return 2, wrong("remote", "remote off takes no arguments")
		}
		if err := config.SetRemote(path, nil); err != nil {
			return 1, err
		}
		print(s.out, "remote off", false)
		return 0, nil
	case "status":
		if len(args) != 0 {
			return 2, wrong("remote", "remote status takes no arguments")
		}
		cfg, err := config.LoadRemote(path)
		if err != nil {
			return 1, err
		}
		if cfg == nil {
			print(s.out, "remote off", false)
		} else {
			print(s.out, "remote: "+cfg.URL+" (token env: "+cfg.TokenEnv+")", false)
		}
		return 0, nil
	}
	if meant := nearest(command, []string{"serve", "use", "off", "status"}); meant != "" {
		return 2, wrong("remote", "remote has no \"%s\". Did you mean \"%s\"?", command, meant)
	}
	return 2, wrong("remote", "remote has no \"%s\"; it has serve, use, off and status", command)
}
