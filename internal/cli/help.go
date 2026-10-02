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
	{name: "import", group: "Servers", summary: "bring over the servers your agents already have", page: "servers.md#bringing-over-an-agents-servers",
		usage: []string{"tap import [SOURCE...] [--dry-run] [--force]"},
		about: "Reads the MCP servers that coding agents keep in their own configs and adds them to tap.\n" +
			"With no SOURCE it looks in every place it knows. SOURCE is claude, codex, opencode, cursor,\n" +
			"vscode, or the path of a config file. The agents' files are only read, never changed.",
		examples: []string{"tap import --dry-run        # see what would be added", "tap import                  # add them", "tap import codex", "tap import ./team/mcp.json"},
		flags: [][2]string{{"-n, --dry-run", "show what would be added and add nothing"}, {"-f, --force", "replace servers tap already has under the same name"},
			{"    --json", "print what was found as JSON"}}},
	{name: "add", group: "Servers", summary: "add a server", page: "servers.md#adding-servers",
		usage: []string{"tap add NAME URL [--header KEY=VALUE]... [--bearer-token-env VARIABLE]", "tap add NAME [--env KEY=VALUE]... [--cwd DIR] -- COMMAND [ARGUMENT...]"},
		about: "Adds an HTTP server by its address, or a stdio server by the command that starts it.\n" +
			"Everything after -- is the command. Adding a name tap already has replaces that server.",
		examples: []string{"tap add docs https://docs.example.com/mcp", "tap add issues https://issues.example.com/mcp --bearer-token-env ISSUES_TOKEN",
			"tap add files -- npx -y @modelcontextprotocol/server-filesystem ~/notes", "tap add db --env DATABASE_URL='${DATABASE_URL}' --cwd ~/code/app -- node mcp/server.mjs"},
		flags: [][2]string{{"    --header KEY=VALUE", "send this header with every request; repeat for more"},
			{"    --bearer-token-env VARIABLE", "send the token held in this environment variable"},
			{"    --env KEY=VALUE", "set this variable for the command; repeat for more"}, {"    --cwd DIR", "start the command in this directory"},
			{"    --idle-timeout-ms N", "stop the command after this long unused; it starts again when needed"},
			{"    --allow-tool PATTERN", "offer only the tools that match, such as read_*; repeat for more"},
			{"    --deny-tool PATTERN", "never offer the tools that match; repeat for more"},
			{"    --reference-to SERVER", "let results kept from this server be passed to that one; repeat for more"}}},
	{name: "remove", group: "Servers", summary: "remove a server", page: "servers.md#adding-servers",
		usage: []string{"tap remove NAME"}, about: "Removes a server from tap, with any sign-in saved for it.",
		examples: []string{"tap remove docs"}},
	{name: "list", group: "Servers", summary: "show the servers and how many tools each has", page: "cli.md",
		usage: []string{"tap list [--cached] [--json]"}, about: "Asks every server for its tools and prints how many it offers, or why it could not be reached.",
		examples: []string{"tap list", "tap list --cached   # what tap last saw, without asking", "tap list --json"},
		flags:    [][2]string{{"    --cached", "print the tool lists tap has saved, without asking the servers"}, {"    --json", "print the servers as JSON"}}},
	{name: "refresh", group: "Servers", summary: "ask one server for its tools again", page: "cli.md#list",
		usage: []string{"tap refresh [NAME] [--json]"}, about: "Asks one server, or all of them, for its tools again and saves what it answers.",
		examples: []string{"tap refresh github", "tap refresh"}, flags: [][2]string{{"    --json", "print the servers as JSON"}}},
	{name: "auth", group: "Servers", summary: "sign in to a server", page: "servers.md#signing-in",
		usage: []string{"tap auth NAME [--client-id ID] [--port PORT] [--no-browser]", "tap auth NAME --remove"},
		about: "Signs in to an HTTP server through your browser and saves the result, so your agents can\n" +
			"use the server without asking again. tap renews the sign-in by itself while it can.\n" +
			"On a machine with no browser, open the page it prints on another one and paste the\n" +
			"address the browser ends on.",
		examples: []string{"tap auth linear", "tap auth linear --no-browser", "tap auth github --client-id Iv1.abc123 --port 8765", "tap auth linear --remove"},
		flags: [][2]string{{"    --client-id ID", "an app you registered with the provider, for those that need one"},
			{"    --client-secret-file FILE", "that app's secret, read from a file (- for standard input)"},
			{"-p, --port PORT", "the port your browser is sent back to on this machine"},
			{"    --no-browser", "print the page's address instead of opening it"}, {"    --remove", "forget the sign-in"}}},
	{name: "search", group: "Tools", summary: "find tools, the way your agent does", page: "cli.md#search",
		usage: []string{"tap search QUERY... [--server NAME] [--limit N] [--json]", "tap search --server NAME"},
		about: "Prints the tools that best match the query, with the id to call each by. Words are\n" +
			"matched against a tool's name, its server, its description and its parameters. With\n" +
			"--server and no query, it lists that server's tools.",
		examples: []string{"tap search create issue", "tap search screenshot --limit 3", "tap search --server github", "tap search read file --json   # with input schemas, as an agent gets them"},
		flags: [][2]string{{"    --server NAME", "look only in this server"}, {"    --limit N", "how many tools to print (default 8)"},
			{"    --offset N", "skip this many matches, to continue a longer list"}, {"    --refresh", "ask the servers for their tools first"},
			{"    --json", "print the matches as JSON, with input schemas"},
			{"    --detail auto|full|summary", "with --json: whole schemas, or summaries (default full)"},
			{"    --max-bytes N", "with --json: the most the answer may hold"}}},
	{name: "inspect", group: "Tools", summary: "print one tool's whole contract", page: "cli.md#search",
		usage: []string{"tap inspect SERVER.TOOL"}, about: "Prints a tool's description and its input and output schemas, as JSON.",
		examples: []string{"tap inspect github.issue_write"}},
	{name: "call", group: "Tools", summary: "call a tool", page: "cli.md#call",
		usage: []string{"tap call SERVER.TOOL [KEY=VALUE]... [--args JSON] [--json]"},
		about: "Calls a tool by the id that search prints. Give arguments as KEY=VALUE strings, as JSON\n" +
			"with --args, or both: a KEY=VALUE replaces the same key in the JSON.",
		examples: []string{"tap call files.read_text_file path=~/notes/todo.md", `tap call issues.create --args '{"title": "Login fails", "labels": ["bug"]}'`, "tap call db.query --args - < query.json"},
		flags:    [][2]string{{"    --args JSON", "the arguments as a JSON object (- reads standard input)"}, {"    --json", "print the tool's whole result as JSON"}}},
	{name: "remote", group: "More than one machine", summary: "share one set of servers between machines", page: "remote.md",
		usage: []string{"tap remote serve [--addr HOST:PORT] [--tls-cert FILE --tls-key FILE] [--allow-insecure]",
			"tap remote use URL [--token-env VARIABLE] [--allow-insecure]", "tap remote off", "tap remote status"},
		about: "One machine serves its servers; the others use them. serve needs a token in\n" +
			"TAP_REMOTE_TOKEN, and so does every machine that uses it. Plain HTTP beyond this\n" +
			"machine needs --allow-insecure on both ends.",
		examples: []string{"tap remote serve --addr 0.0.0.0:8765 --tls-cert cert.pem --tls-key key.pem", "tap remote use https://tap.example.com:8765", "tap remote status", "tap remote off"},
		flags: [][2]string{{"    --addr HOST:PORT", "where serve listens (default 127.0.0.1:7777)"}, {"    --tls-cert FILE, --tls-key FILE", "serve HTTPS with this certificate"},
			{"    --token-env VARIABLE", "the variable holding the token (default TAP_REMOTE_TOKEN)"}, {"    --allow-insecure", "allow plain HTTP beyond this machine"}}},
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
		{"tap import", "bring over the servers your agents already have"},
		{"tap add docs https://docs.example.com/mcp", "or add one yourself"},
		{"tap list", "check that they connect"},
		{"tap search create issue", "see what your agent would find"},
	})
	fmt.Fprintf(w, "\nRun \"tap --help\" for every command, or \"tap COMMAND --help\" for one.\nDocs: %s\n", docs)
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
	for _, example := range []string{"tap import", "tap add docs https://docs.example.com/mcp", "tap add files -- npx -y @modelcontextprotocol/server-filesystem ~/notes",
		"tap list", "tap search create issue", "tap call files.read_text_file path=~/notes/todo.md"} {
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
		{"    --json", "print JSON instead of text (import, list, refresh, search, call)"},
		{"    --local", "use this machine's servers while a remote is selected"}})
	fmt.Fprintln(w)
	p.heading("Environment")
	p.rows([][2]string{{"TAP_CONFIG", "the config file (default ~/.tap/servers.json)"}, {"TAP_DEADLINE_MS", "how long a server may take to connect and list its tools (default 5000)"},
		{"TAP_REFERENCES", "set to on to let an agent keep large results as references"},
		{"TAP_REMOTE_TOKEN", "the token a remote and its clients share"}, {"NO_COLOR", "set to print help without bold headings"}})
	fmt.Fprintf(w, "\nRun \"tap COMMAND --help\" for a command's examples and flags.\nDocs:   %s\nIssues: %s\n", docs, issues)
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
