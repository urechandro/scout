// Command server runs the MCP server over stdio.
package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/urechandro/scout/mcp"
	"github.com/urechandro/scout/query"
	"github.com/urechandro/scout/store"
)

func main() {
	dbPath := flag.String("db", "/data/index.db", "Path to SQLite database.")
	debug := flag.Bool("debug", false, "Enable debug logging.")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}

	// MCP uses stdio for the protocol, so logs must go to stderr only.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	s, err := store.New(*dbPath)
	if err != nil {
		logger.Error("open store", "err", err)
		os.Exit(1)
	}
	defer s.Close()

	engine := query.New(s)
	server := mcp.New(logger, engine, s)

	if err := server.Run(); err != nil {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
}
