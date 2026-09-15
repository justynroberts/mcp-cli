// Command mcp-cli queries MCP servers from the shell and prints JSON.
//
// It reads server definitions (and how to authenticate to them) from a config
// file, connects over stdio or HTTP, and writes a single JSON document to
// stdout. Diagnostics go to stderr, so the output is always safe to pipe into
// jq or another program.
package main

import (
	"os"

	"github.com/justynroberts/mcp-cli/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
