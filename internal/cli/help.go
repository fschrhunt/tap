package cli

import (
	"fmt"
	"io"
	"os"
)

const (
	docs   = "https://github.com/fschrhunt/tap/tree/main/docs"
	issues = "https://github.com/fschrhunt/tap/issues"
)

// topic is the help for one command: how to call it, what it does, examples first and then
// its flags. group places it in the full help; page is its page in the docs.
type topic struct {
	name, group, summary, page string
	usage                      []string
	about                      string
	examples                   []string
	flags                      [][2]string
}

// topics lists the commands in the order the full help shows them.
var topics = []topic{
	{name: "connect", group: "Servers", summary: "point your coding agents at tap", page: "install.md#connect-your-agent",
		usage: []string{"tap connect [AGENT...]"},
		about: "Runs each coding agent's own command to add tap as an MCP server, and reports what\n" +
			"it found: connected, or has tap already. With no AGENT it connects every agent it\n" +
			"finds on this machine; AGENT is claude, codex or opencode, or several of them. Only\n" +
			"the agent's own command writes the agent's config; an agent that already has tap is\n" +
			"left as it is. Import and check existing servers before removing their original\n" +
			"agent entries, then start a new agent session to load tap.",
		examples: []string{"tap connect                 # every agent on this machine", "tap connect claude", "tap connect codex opencode"}},
	{name: "import", group: "Servers", summary: "bring over the servers your agents already have", page: "servers.md#bringing-over-an-agents-servers",
		usage: []string{"tap import [SOURCE...] [--dry-run] [--force]"},
		about: "Reads the MCP servers that coding agents keep in their own configs and adds them to tap.\n" +
			"With no SOURCE it looks in every place it knows. SOURCE is claude, codex, opencode, cursor,\n" +
			"vscode, or the path of a config file. The agents' files are only read, never changed.\n" +
			"After checking the imported servers, remove their original agent entries and leave tap.\n" +
			"With a remote selected, use --local: import only writes this machine's registry.",
		examples: []string{"tap import --dry-run        # see what would be added", "tap import                  # add them", "tap import codex", "tap import ./team/mcp.json"},
		flags: [][2]string{{"-n, --dry-run", "show what would be added and add nothing"}, {"-f, --force", "replace servers tap already has under the same name"},
			{"    --json", "print what was found as JSON"}}},
	{name: "add", group: "Servers", summary: "add a server", page: "servers.md#adding-servers",
		usage: []string{"tap add NAME URL [--header KEY=VALUE]... [--bearer-token-env VARIABLE]", "tap add NAME [--env KEY=VALUE]... [--cwd DIR] -- COMMAND [ARGUMENT...]"},
		about: "Adds an HTTP server by its address, or a stdio server by the command that starts it.\n" +
			"Everything after -- is the command. Adding a name tap already has replaces that server.\n" +
			"Example URLs are placeholders; replace them with your server's endpoint. Stdio commands\n" +
			"need their own runtimes. Environment references are read by the tap process serving tools.",
		examples: []string{"tap add docs https://docs.example.com/mcp", "tap add issues https://issues.example.com/mcp --bearer-token-env ISSUES_TOKEN",
			"tap add files -- npx -y @modelcontextprotocol/server-filesystem ~/notes", "tap add db --env DATABASE_URL='${DATABASE_URL}' --cwd ~/code/app -- node mcp/server.mjs"},
		flags: [][2]string{{"    --header KEY=VALUE", "send this header with every request; repeat for more"},
			{"    --bearer-token-env VARIABLE", "send the token held in this environment variable"},
			{"    --env KEY=VALUE", "set this variable for the command; repeat for more"}, {"    --cwd DIR", "start the command in this directory"},
			{"    --idle-timeout-ms N", "stop the command after this long unused; it starts again when needed"},
			{"    --allow-tool PATTERN", "offer only matching tool names; quote globs like 'read_*'; repeat for more"},
			{"    --deny-tool PATTERN", "never offer the tools that match; repeat for more"},
			{"    --reference-to SERVER", "let results kept from this server be passed to that one; repeat for more"}}},
	{name: "remove", group: "Servers", summary: "remove a server", page: "servers.md#adding-servers",
		usage: []string{"tap remove NAME"}, about: "Removes a server from tap, with any sign-in saved for it.",
		examples: []string{"tap remove docs"}},
	{name: "list", group: "Servers", summary: "show the servers and how many tools each has", page: "cli.md",
		usage: []string{"tap list [--cached] [--json]"}, about: "Asks every server for its tools and prints how many it offers, or why it could not be reached.",
		examples: []string{"tap list", "tap list --cached   # what tap last saw, without asking", "tap list --json"},
		flags:    [][2]string{{"    --cached", "print the tool lists tap has saved, without asking the servers"}, {"    --json", "print the servers as JSON"}}},
	{name: "refresh", group: "Servers", summary: "ask one or all servers for their tools again", page: "cli.md#list",
		usage: []string{"tap refresh [NAME] [--json]"}, about: "Asks one server, or all of them, for its tools again and saves what it answers.",
		examples: []string{"tap refresh github", "tap refresh"}, flags: [][2]string{{"    --json", "print the servers as JSON"}}},
	{name: "auth", group: "Servers", summary: "sign in to a server", page: "servers.md#signing-in",
		usage: []string{"tap auth NAME [--client-id ID] [--port PORT] [--no-browser] [--local]", "tap auth NAME --remove [--local]"},
		about: "Signs in to an HTTP server through your browser and saves the result, so your agents can\n" +
			"use the server without asking again. tap renews the sign-in by itself while it can.\n" +
			"With a remote selected, tap opens the browser here and relays the callback; the remote\n" +
			"keeps the sign-in. Use --local to sign in to this machine's own server instead.",
		examples: []string{"tap auth linear", "tap auth linear --no-browser", "tap auth github --client-id Iv1.abc123 --port 8765", "tap auth linear --remove"},
		flags: [][2]string{{"    --client-id ID", "an app you registered with the provider, for those that need one"},
			{"    --client-secret-file FILE", "that app's secret, read from a file (- for standard input)"},
			{"-p, --port PORT", "the callback port (for providers that need a registered address)"},
			{"    --no-browser", "print the page's address instead of opening it"},
			{"    --local", "use this machine's registry instead of the selected remote"},
			{"    --remove", "forget the sign-in"}}},
	{name: "search", group: "Tools", summary: "find tools, the way your agent does", page: "cli.md#search",
		usage: []string{"tap search QUERY... [--server NAME] [--limit N] [--json]", "tap search --server NAME"},
		about: "Prints the tools that best match the query, with the id to call each by. Words are\n" +
			"matched against a tool's name, its server, its title, description and parameters. With\n" +
			"--server and no query, it lists that server's tools. Saved tool lists may be stale;\n" +
			"use --refresh to wait for live lists, or inspect an id before calling it.",
		examples: []string{"tap search create issue", "tap search screenshot --limit 3", "tap search --server github", "tap search read file --json   # with input schemas, as an agent gets them"},
		flags: [][2]string{{"    --server NAME", "look only in this server"}, {"    --limit N", "how many tools to print (default searchLimit, 8); remote range 1–25"},
			{"    --offset N", "skip this many matches, to continue a longer list"}, {"    --refresh", "ask the servers for their tools first"},
			{"    --json", "print the matches as JSON, with input schemas"},
			{"    --detail auto|full|summary", "schema detail, visible with --json (default full)"},
			{"    --max-bytes N", "discovery byte budget, before rendering (default 16777216; range 1024–16777216)"}}},
	{name: "inspect", group: "Tools", summary: "print one tool's whole contract", page: "cli.md#search",
		usage: []string{"tap inspect SERVER.TOOL"}, about: "Prints a tool's description and its input and output schemas, as JSON.",
		examples: []string{"tap inspect github.issue_write"}},
	{name: "call", group: "Tools", summary: "call a tool", page: "cli.md#call",
		usage: []string{"tap call SERVER.TOOL [KEY=VALUE]... [--args JSON] [--json]"},
		about: "Calls a tool by the id that search prints. Give arguments as KEY=VALUE strings, as JSON\n" +
			"with --args, or both: a KEY=VALUE replaces the same key in the JSON. Use JSON for\n" +
			"numbers, booleans, arrays and objects. Tool ids below are examples; use search or inspect\n" +
			"for your server's actual schema. With a remote selected, paths refer to that machine.",
		examples: []string{`tap call files.read_text_file path="$HOME/notes/todo.md"`, `tap call issues.create --args '{"title": "Login fails", "labels": ["bug"]}'`, "tap call db.query --args - < query.json"},
		flags:    [][2]string{{"    --args JSON", "the arguments as a JSON object (- reads standard input)"}, {"    --json", "print the tool's whole result as JSON"}}},
	{name: "remote", group: "More than one machine", summary: "share one set of servers between machines", page: "remote.md",
		usage: []string{"tap remote serve [--addr HOST:PORT] [--tls-cert FILE --tls-key FILE]",
			"tap remote pair NAME HTTPS_URL [--role execution|admin]", "tap remote devices", "tap remote revoke DEVICE_ID",
			"tap remote use NAME", "tap remote list", "tap remote remove NAME",
			"tap remote off", "tap remote status [--check]"},
		about: "serve hosts this machine's servers over paired-device HTTPS. pair requires an interactive\n" +
			"terminal: compare the host's certificate fingerprint, then enter its one-time code.\n" +
			"Pairing saves and selects the profile. Execution devices can use tools; admin devices\n" +
			"can also edit servers and manage sign-ins. devices and revoke run on the host.",
		examples: []string{"tap remote serve", "tap remote pair home https://tap-host:8443", "tap remote devices", "tap remote use home", "tap remote status --check", "tap remote off"},
		flags: [][2]string{{"    --addr HOST:PORT", "where serve listens (default 0.0.0.0:8443)"}, {"    --tls-cert FILE, --tls-key FILE", "serve with this certificate; paired mode pins its fingerprint"},
			{"    --role execution|admin", "paired device permissions (default execution)"}, {"    --check", "check remote reachability with the configured credential"}}},
	{name: "config", group: "Other", summary: "show or change tap's settings", page: "settings.md",
		usage: []string{"tap config [--json]", "tap config NAME [--server NAME]", "tap config set NAME VALUE [--server NAME]", "tap config unset NAME [--server NAME]"},
		about: "Shows each setting, its value and where the value comes from: its default, the\n" +
			"config, or an environment variable, which wins over both. set keeps a value in the\n" +
			"config; unset removes it. With --server, start and idleTimeoutMs are kept for one\n" +
			"server and win over the general value; idleTimeoutMs is positive and stdio-only.\n" +
			"TAP_REFERENCES, TAP_DEADLINE_MS, TAP_IDLE_TTL_MS\n" +
			"and TAP_FAIL_TTL_MS override the corresponding general settings. Server overrides\n" +
			"still win. Settings are read when tap starts; restart the serving tap to apply changes.\n" +
			"config always edits this machine's settings, not the selected remote's.",
		examples: []string{"tap config", "tap config set start search          # refresh stale tool lists during search",
			"tap config set start start --server playwright   # start it with tap and keep it running",
			"tap config set references on", "tap config unset start --server playwright"},
		flags: [][2]string{{"    --server NAME", "the server whose own value to read, set or unset"}, {"    --json", "print the settings as JSON"}}},
	{name: "path", group: "Other", summary: "print the config file tap reads", usage: []string{"tap path"}, page: "servers.md#the-config-file",
		about: "Prints the config file. Set TAP_CONFIG to use another."},
	{name: "version", group: "Other", summary: "print the version", usage: []string{"tap version"}, page: "install.md"},
	{name: "help", group: "Other", summary: "show help for tap or one command", usage: []string{"tap help [COMMAND]"}, page: "cli.md"},
}

// find returns a command's help.
func find(name string) *topic {
	for i := range topics {
		if topics[i].name == name {
			return &topics[i]
		}
	}
	return nil
}

// printer writes help with headings that are bold on a terminal and plain anywhere else.
type printer struct {
	w    io.Writer
	bold bool
}

// styled reports whether text written to w may carry escape codes: only a terminal that has
// not asked for none.
func styled(w io.Writer) bool {
	return terminal(w) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

func (p printer) heading(text string) {
	if p.bold {
		text = "\033[1m" + text + "\033[0m"
	}
	fmt.Fprintln(p.w, text)
}

func (p printer) rows(rows [][2]string) {
	width := 0
	for _, row := range rows {
		width = max(width, len(row[0]))
	}
	for _, row := range rows {
		fmt.Fprintf(p.w, "  %-*s  %s\n", width, row[0], row[1])
	}
}

const pitch = "one MCP server that stands in for all of yours."

// brief is what tap prints when a person runs it bare in a terminal: what it is, how to
// start, and where the rest is.
func brief(w io.Writer, version string) {
	p := printer{w, styled(w)}
	fmt.Fprintf(w, "tap %s: %s\n\n", version, pitch)
	fmt.Fprint(w, "Your agent starts tap and loads two tools, plugin_search and plugin_call, instead of\n"+
		"every tool from every server. Started by an agent, tap serves MCP. Here, it shows this.\n\n")
	p.heading("Start here")
	p.rows([][2]string{
		{"tap connect", "point your coding agents at tap"},
		{"tap import", "bring over the servers your agents already have"},
		{"tap add docs https://docs.example.com/mcp", "or add one yourself"},
		{"tap list", "check that they connect"},
		{"tap search create issue", "see what your agent would find"},
	})
	fmt.Fprintln(w, "\nImport copies servers without changing agent configs. Check them with tap list, then\nremove their original agent entries and start a new session to load tap.")
	fmt.Fprintf(w, "\nRun \"tap help\" for every command, or \"tap help COMMAND\" for one.\nDocs: %s\n", docs)
}

// full is the whole reference: every command by group, the flags they share, and the
// environment tap reads.
func full(w io.Writer, version string) {
	p := printer{w, styled(w)}
	fmt.Fprintf(w, "tap %s: %s\n\n", version, pitch)
	p.heading("Usage")
	p.rows([][2]string{{"tap", "serve MCP over stdio; this is what an agent starts"}, {"tap COMMAND [ARGUMENT...] [FLAG...]", "everything else"}})
	fmt.Fprintln(w)
	p.heading("Examples")
	for _, example := range []string{"tap connect", "tap import", "tap add docs https://docs.example.com/mcp", "tap add files -- npx -y @modelcontextprotocol/server-filesystem ~/notes",
		"tap list", "tap search create issue", `tap call files.read_text_file path="$HOME/notes/todo.md"`} {
		fmt.Fprintln(w, "  "+example)
	}
	group := ""
	rows := [][2]string{}
	flush := func() {
		if len(rows) > 0 {
			fmt.Fprintln(w)
			p.heading(group)
			p.rows(rows)
			rows = rows[:0]
		}
	}
	for _, t := range topics {
		if t.group != group {
			flush()
			group = t.group
		}
		rows = append(rows, [2]string{t.name, t.summary})
	}
	flush()
	fmt.Fprintln(w)
	p.heading("Flags")
	p.rows([][2]string{{"-h, --help", "show help for tap or for a command"}, {"    --version", "print the version"},
		{"    --json", "print JSON instead of text (import, list, refresh, search, call, config)"},
		{"    --local", "use this machine's servers while a remote is selected"}})
	fmt.Fprintln(w)
	p.heading("Environment")
	p.rows([][2]string{{"TAP_CONFIG", "the config file (default ~/.tap/servers.json)"},
		{"NO_COLOR", "set to print help without bold headings"}})
	fmt.Fprintln(w, "  Some settings have variables of their own; see \"tap help config\".")
	fmt.Fprintf(w, "\nRun \"tap help COMMAND\" for a command's examples and flags.\nDocs:   %s\nIssues: %s\n", docs, issues)
}

// explain prints one command's help: usage, what it does, examples, flags, and its page.
func explain(w io.Writer, t *topic) {
	p := printer{w, styled(w)}
	p.heading("Usage")
	for _, line := range t.usage {
		fmt.Fprintln(w, "  "+line)
	}
	if t.about != "" {
		fmt.Fprintln(w, "\n"+t.about)
	}
	if len(t.examples) > 0 {
		fmt.Fprintln(w)
		p.heading("Examples")
		for _, example := range t.examples {
			fmt.Fprintln(w, "  "+example)
		}
	}
	if len(t.flags) > 0 {
		fmt.Fprintln(w)
		p.heading("Flags")
		p.rows(t.flags)
	}
	fmt.Fprintf(w, "\nDocs: %s/%s\n", docs, t.page)
}

// nearest returns the candidate closest to word, if one is close enough to be what was meant.
func nearest(word string, candidates []string) string {
	best, score := "", max(1, len(word)/3)+1
	for _, candidate := range candidates {
		if d := distance(word, candidate); d < score {
			best, score = candidate, d
		}
	}
	return best
}

// distance is the number of single-character edits between two words.
func distance(a, b string) int {
	row := make([]int, len(b)+1)
	for j := range row {
		row[j] = j
	}
	for i := 1; i <= len(a); i++ {
		previous := row[0]
		row[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			previous, row[j] = row[j], min(row[j]+1, row[j-1]+1, previous+cost)
		}
	}
	return row[len(b)]
}
