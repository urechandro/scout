package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"sync"

	"github.com/urechandro/scout/config"
	"github.com/urechandro/scout/embedder"
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
		if cb, embedStartup := buildWatcherEmbedCallback(*watch, s, logger); cb != nil {
			wcfg.PostReindex = cb
			// Drain any embedding backlog in the background so the first
			// post-save callback isn't on the hook for an entire corpus.
			// The callback's mutex serializes startup vs save passes.
			go embedStartup()
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

// buildWatcherEmbedCallback returns a PostReindex hook plus a one-shot
// startup function that drains the embedding backlog. Both are nil when no
// embedder is configured.
//
// The callback and the startup function share one mutex so passes serialize:
// if a save arrives while startup is still embedding the backlog, the save's
// pass waits. We deliberately do NOT drop concurrent calls — the second pass
// might cover symbols the first hadn't seen yet.
func buildWatcherEmbedCallback(rootDir string, s *store.Store, logger *slog.Logger) (func(kind string, files []string), func()) {
	cfg, err := config.Load(rootDir)
	if err != nil {
		logger.Warn("load embedder config for watcher", "err", err)
		return nil, nil
	}
	if cfg.Embedder == nil {
		return nil, nil
	}
	if cfg.Embedder.Kind != config.EmbedderOllama {
		logger.Warn("watcher embedder skipped: unsupported kind", "kind", cfg.Embedder.Kind)
		return nil, nil
	}

	client := embedder.NewOllamaClient(cfg.Embedder.Host, cfg.Embedder.Model)
	logger.Info("watcher: embedder enabled", "model", cfg.Embedder.Model)
	var mu sync.Mutex

	runPass := func(kind string, files []string) {
		mu.Lock()
		defer mu.Unlock()
		stats, err := embedder.Run(context.Background(), s, client, embedder.Options{
			Logger: slogPrintfAdapter{logger},
		})
		if err != nil {
			logger.Warn("watcher embedder pass", "err", err, "kind", kind)
			return
		}
		if stats.Embedded > 0 || stats.Failed > 0 {
			logger.Info("watcher embedder pass complete",
				"kind", kind, "files", len(files),
				"embedded", stats.Embedded, "failed", stats.Failed)
		}
	}

	startup := func() { runPass("startup", nil) }
	return runPass, startup
}
