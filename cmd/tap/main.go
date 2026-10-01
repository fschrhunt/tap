// Command tap serves the MCP gateway or dispatches a one-shot CLI command.
package main

import (
	"context"
	"os"

	"github.com/fschrhunt/tap/internal/cli"
)

// version is populated by release builds through -X main.version.
var version = "dev"

// main keeps process entry and exit separate from the CLI and gateway packages.
func main() { os.Exit(cli.Run(context.Background(), os.Args[1:], version, os.Stdout, os.Stderr)) }
