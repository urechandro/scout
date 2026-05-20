package main

import (
	"flag"
	"os"

	"github.com/urechandro/scout/indexer"
	"github.com/urechandro/scout/mcp"
	"github.com/urechandro/scout/query"
	"github.com/urechandro/scout/store"
	"github.com/urechandro/scout/tsindexer"
)

func cmdServe(args []string) {
	fs := flag.NewFlagSet("scout serve", flag.ExitOnError)
	dbPath := fs.String("db", "/data/index.db", "Path to SQLite database.")
	watch := fs.String("watch", "", "Root directory to watch for file changes (enables live reindexing).")
	tsconfig := fs.String("tsconfig", "", "Path to tsconfig.json for TypeScript live reindexing (requires --watch).")
	tsCommand := fs.String("ts-command", "ts-callgraph", "Path to ts-callgraph binary.")
	debug := fs.Bool("debug", false, "Enable debug logging.")
	_ = fs.Parse(args)

	logger := newLogger(*debug)

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
