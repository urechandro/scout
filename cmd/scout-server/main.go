// Command server runs the MCP server over stdio.
package main

import (
	"flag"
	"log/slog"
	"os"
	"strings"

	"github.com/urechandro/scout/indexer"
	"github.com/urechandro/scout/mcp"
	"github.com/urechandro/scout/query"
	"github.com/urechandro/scout/store"
	"github.com/urechandro/scout/tsindexer"
)

func main() {
	dbPath := flag.String("db", "/data/index.db", "Path to SQLite database.")
	watch := flag.String("watch", "", "Root directory to watch for file changes (enables live reindexing).")
	tsconfig := flag.String("tsconfig", "", "Path to tsconfig.json for TypeScript live reindexing (requires --watch).")
	tsCommand := flag.String("ts-command", "ts-callgraph", "Path to ts-callgraph binary.")
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

	if *watch != "" {
		idx := indexer.New(indexer.Config{
			Dir:      *watch,
			Patterns: []string{"./..."},
		}, s)
		wcfg := indexer.WatcherConfig{Root: *watch}
		if *tsconfig != "" {
			tsBin, tsArgs := parseTSCommand(*tsCommand)
			wcfg.TSIndexer = tsindexer.New(tsindexer.Config{
				TsconfigPath: *tsconfig,
				Root:         *watch,
				Command:      tsBin,
				CommandArgs:  tsArgs,
			}, s)
		}
		w := indexer.NewWatcher(idx, s, wcfg)
		go func() {
			if err := w.Run(); err != nil {
				logger.Error("watcher error", "err", err)
			}
		}()
	}

	engine := query.New(s)
	server := mcp.New(logger, engine, s)

	if err := server.Run(); err != nil {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
}

func parseTSCommand(cmd string) (string, []string) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return "ts-callgraph", nil
	}
	return parts[0], parts[1:]
}
