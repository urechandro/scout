// Command scout — fast, token-efficient codebase navigation for Claude Code.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

const usage = `scout — fast, token-efficient codebase navigation for Claude Code

Usage:
  scout <command> [flags]

Commands:
  init     Bootstrap scout in a new project (interactive TUI)
  index    Build or update the symbol index
  reindex  Incrementally reindex specific files
  serve    Run the MCP server over stdio

Run 'scout <command> --help' for command-specific flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "init":
		cmdInit(args)
	case "index":
		cmdIndex(args)
	case "reindex":
		cmdReindex(args)
	case "serve":
		cmdServe(args)
	case "help", "--help", "-h":
		fmt.Fprint(os.Stderr, usage)
	default:
		fmt.Fprintf(os.Stderr, "scout: unknown command %q\n\n%s", cmd, usage)
		os.Exit(1)
	}
}

// parseTSCommand splits a ts-command flag value like "node /path/to/cli.js"
// into the binary and extra args.
func parseTSCommand(cmd string) (string, []string) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return "ts-callgraph", nil
	}
	return parts[0], parts[1:]
}

func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
