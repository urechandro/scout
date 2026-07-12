package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urechandro/scout/config"
	"github.com/urechandro/scout/embedder"
	"github.com/urechandro/scout/indexer"
	"github.com/urechandro/scout/protoindexer"
	"github.com/urechandro/scout/store"
	"github.com/urechandro/scout/tsindexer"
)

func cmdIndex(args []string) {
	fs := flag.NewFlagSet("scout index", flag.ExitOnError)
	dbPath := fs.String("db", "/data/index.db", "Path to SQLite database.")
	dir := fs.String("dir", "", "Root directory of a single Go module to index.")
	root := fs.String("root", "/workspace", "Root directory to search for Go modules.")
	patterns := fs.String("patterns", "./...", "Comma-separated Go package patterns.")
	excludeGenerated := fs.Bool("exclude-generated", true, "Skip generated files and directories.")
	exclude := fs.String("exclude", "", "Comma-separated package path substrings to skip.")
	method := fs.String("method", "rta", "Call graph algorithm: rta (default) or cha.")
	deps := fs.Bool("deps", false, "Index exported signatures from external dependency packages.")
	tsconfig := fs.String("tsconfig", "", "Path to tsconfig.json for TypeScript indexing.")
	tsCommand := fs.String("ts-command", "ts-callgraph", "Path to ts-callgraph binary.")
	_ = fs.Parse(args)

	logger := newLogger(false)

	s, err := store.New(*dbPath)
	if err != nil {
		logger.Error("open store", "err", err)
		os.Exit(1)
	}
	defer s.Close()

	// Snapshot existing embeddings before the reset so the embedder pass
	// after the reindex can skip symbols whose source text didn't change.
	// Without this, every full index would re-embed the whole corpus —
	// ~90s on a 14k-symbol repo. Warn-and-continue on snapshot failure:
	// the embedder pass will simply re-embed everything.
	snapshots, err := s.SnapshotEmbeddings()
	if err != nil {
		logger.Warn("snapshot embeddings", "err", err)
	}

	// Full index starts from a clean slate so that symbols, edges, and
	// conventions removed since the last run don't linger as orphans.
	// Incremental updates go through `scout reindex` / the watcher's RunFiles
	// path, which deletes per file.
	if err := s.ResetIndex(); err != nil {
		logger.Error("reset index", "err", err)
		os.Exit(1)
	}

	var dirs []string
	if *dir != "" {
		dirs = []string{*dir}
	} else {
		dirs, err = findModuleDirs(*root)
		if err != nil {
			logger.Error("discover modules", "err", err)
			os.Exit(1)
		}
		logger.Info("discovered modules", "count", len(dirs), "root", *root)
	}

	var cgMethod indexer.CallGraphMethod
	switch strings.ToLower(*method) {
	case "rta":
		cgMethod = indexer.CallGraphRTA
	case "cha":
		cgMethod = indexer.CallGraphCHA
	case "ast":
		cgMethod = indexer.CallGraphAST
	default:
		logger.Error("unknown --method", "method", *method)
		os.Exit(1)
	}

	var excludePaths []string
	if *exclude != "" {
		excludePaths = strings.Split(*exclude, ",")
	}

	for _, d := range dirs {
		cfg := indexer.Config{
			Dir:              d,
			Patterns:         strings.Split(*patterns, ","),
			ExcludeGenerated: *excludeGenerated,
			ExcludePaths:     excludePaths,
			CallGraph:        cgMethod,
			IndexDeps:        *deps,
		}
		idx := indexer.New(cfg, s)
		logger.Info("full index", "dir", d, "patterns", *patterns)
		if err := idx.Run(); err != nil {
			logger.Error("full index failed", "dir", d, "err", err)
			os.Exit(1)
		}
	}

	protoDir := *root
	if *dir != "" {
		protoDir = *dir
	}
	pidx := protoindexer.New(protoindexer.Config{
		Dir:          protoDir,
		ExcludePaths: excludePaths,
	}, s)
	logger.Info("indexing protos", "dir", protoDir)
	if err := pidx.Run(); err != nil {
		logger.Error("proto index failed", "err", err)
		os.Exit(1)
	}

	linked, err := indexer.LinkProtoToGo(s)
	if err != nil {
		logger.Warn("proto-go linking failed", "err", err)
	} else {
		logger.Info("proto-go linking complete", "linked", linked)
	}

	conventionsDir := *root
	if *dir != "" {
		conventionsDir = *dir
	}
	if err := indexer.IndexConventions(conventionsDir, s); err != nil {
		logger.Warn("conventions index failed", "err", err)
	}

	if *tsconfig != "" {
		tsRoot := *root
		if *dir != "" {
			tsRoot = *dir
		}
		tsBin, tsArgs := parseTSCommand(*tsCommand)
		tidx := tsindexer.New(tsindexer.Config{
			TsconfigPath: *tsconfig,
			Root:         tsRoot,
			Command:      tsBin,
			CommandArgs:  tsArgs,
		}, s)
		logger.Info("indexing typescript", "tsconfig", *tsconfig)
		if err := tidx.Run(); err != nil {
			logger.Error("ts index failed", "err", err)
			os.Exit(1)
		}
	}

	// Restore vectors whose source text survived the reindex. The embedder
	// pass below only embeds what remains (new symbols + ones whose
	// name/signature/docstring changed).
	if len(snapshots) > 0 {
		restored, err := s.RestoreEmbeddings(snapshots)
		if err != nil {
			logger.Warn("restore embeddings", "err", err)
		} else if restored > 0 {
			logger.Info("restored embeddings",
				"restored", restored, "snapshotted", len(snapshots))
		}
	}

	if err := runEmbedderPass(*root, *dir, s, logger); err != nil {
		// Never fail the index over an embedder issue — semantic search is
		// opt-in and degrades to FTS-only when vectors are missing.
		logger.Warn("embedder pass failed", "err", err)
	}

	if err := s.SetMeta("last_indexed", fmt.Sprintf("%d", time.Now().Unix())); err != nil {
		logger.Warn("set meta failed", "err", err)
	}

	logger.Info("indexing complete")
}

// runEmbedderPass loads .scout/config.yaml from the project root and, if an
// embedder is configured, vectorizes every symbol the store lists as
// unembedded. No-op when the config file is absent or has no embedder block.
func runEmbedderPass(rootFlag, dirFlag string, s *store.Store, logger *slog.Logger) error {
	configRoot := rootFlag
	if dirFlag != "" {
		configRoot = dirFlag
	}
	cfg, err := config.Load(configRoot)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.Embedder == nil {
		return nil
	}
	if cfg.Embedder.Kind != config.EmbedderOllama {
		return fmt.Errorf("unsupported embedder kind: %q", cfg.Embedder.Kind)
	}

	client := embedder.NewOllamaClient(cfg.Embedder.Host, cfg.Embedder.Model)
	logger.Info("embedder pass starting",
		"model", cfg.Embedder.Model, "host", cfg.Embedder.Host)
	stats, err := embedder.Run(context.Background(), s, client, embedder.Options{
		Logger: slogPrintfAdapter{logger},
	})
	if err != nil {
		return err
	}
	logger.Info("embedder pass complete",
		"considered", stats.Considered,
		"embedded", stats.Embedded,
		"failed", stats.Failed,
	)
	return nil
}

// slogPrintfAdapter lets the embedder package use whatever logger the CLI
// already configured without forcing the package to depend on slog.
type slogPrintfAdapter struct{ l *slog.Logger }

func (a slogPrintfAdapter) Printf(format string, args ...any) {
	a.l.Info(fmt.Sprintf(format, args...))
}

func cmdReindex(args []string) {
	fs := flag.NewFlagSet("scout reindex", flag.ExitOnError)
	dbPath := fs.String("db", "/data/index.db", "Path to SQLite database.")
	root := fs.String("root", "/workspace", "Root directory (used for proto and conventions).")
	files := fs.String("files", "", "Comma-separated list of files to reindex (required).")
	tsconfig := fs.String("tsconfig", "", "Path to tsconfig.json (required for .ts/.tsx files).")
	tsCommand := fs.String("ts-command", "ts-callgraph", "Path to ts-callgraph binary.")
	_ = fs.Parse(args)

	logger := newLogger(false)

	if *files == "" {
		fmt.Fprintln(os.Stderr, "scout reindex: --files is required")
		fs.Usage()
		os.Exit(1)
	}

	s, err := store.New(*dbPath)
	if err != nil {
		logger.Error("open store", "err", err)
		os.Exit(1)
	}
	defer s.Close()

	changed := strings.Split(*files, ",")
	logger.Info("incremental reindex", "files", len(changed))

	// Partition Go files by their nearest go.mod so each module loads exactly
	// once with only its own changed files.
	var goFiles []string
	for _, f := range changed {
		if strings.HasSuffix(f, ".go") {
			goFiles = append(goFiles, f)
		}
	}
	byModule := partitionByModule(goFiles)
	for moduleDir, files := range byModule {
		cfg := indexer.Config{
			Dir:      moduleDir,
			Patterns: []string{"./..."},
		}
		idx := indexer.New(cfg, s)
		if err := idx.RunFiles(files); err != nil {
			logger.Error("reindex failed", "dir", moduleDir, "err", err)
			os.Exit(1)
		}
	}

	// Reindex any changed proto files.
	var protoFiles []string
	for _, f := range changed {
		if strings.HasSuffix(f, ".proto") {
			protoFiles = append(protoFiles, f)
		}
	}
	if len(protoFiles) > 0 {
		pidx := protoindexer.New(protoindexer.Config{Dir: *root}, s)
		if err := pidx.RunFiles(protoFiles); err != nil {
			logger.Warn("proto reindex failed", "err", err)
		}
	}

	// Reindex any changed TS files.
	var tsFiles []string
	for _, f := range changed {
		if strings.HasSuffix(f, ".ts") || strings.HasSuffix(f, ".tsx") {
			tsFiles = append(tsFiles, f)
		}
	}
	if len(tsFiles) > 0 && *tsconfig != "" {
		tsBin, tsArgs := parseTSCommand(*tsCommand)
		tidx := tsindexer.New(tsindexer.Config{
			TsconfigPath: *tsconfig,
			Root:         *root,
			Command:      tsBin,
			CommandArgs:  tsArgs,
		}, s)
		if err := tidx.RunFiles(tsFiles); err != nil {
			logger.Warn("ts reindex failed", "err", err)
		}
	}

	// Go and proto changes both invalidate proto↔Go implements edges (the
	// per-package reindex clears them), so relink before finishing. Without
	// this, get_impact and the get_callers RPC fallback stay stale until
	// the next full `scout index` — and this command is what the pre-commit
	// hook runs.
	if len(goFiles) > 0 || len(protoFiles) > 0 {
		if linked, err := indexer.LinkProtoToGo(s); err != nil {
			logger.Warn("proto-go relink failed", "err", err)
		} else {
			logger.Info("proto-go relink complete", "linked", linked)
		}
	}

	if err := s.SetMeta("last_indexed", fmt.Sprintf("%d", time.Now().Unix())); err != nil {
		logger.Warn("set meta failed", "err", err)
	}

	logger.Info("reindex complete", "files", len(changed))
}

// findModuleDirs walks root and returns every directory containing a go.mod.
func findModuleDirs(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == "go.mod" {
			dirs = append(dirs, filepath.Dir(path))
		}
		return nil
	})
	return dirs, err
}

// partitionByModule groups files by the directory of their nearest go.mod
// ancestor. Files with no go.mod above them are grouped under their parent
// directory as a fallback.
func partitionByModule(files []string) map[string][]string {
	out := map[string][]string{}
	cache := map[string]string{}
	for _, f := range files {
		dir := filepath.Dir(f)
		modDir, ok := cache[dir]
		if !ok {
			modDir = findModuleRoot(dir)
			if modDir == "" {
				modDir = dir
			}
			cache[dir] = modDir
		}
		out[modDir] = append(out[modDir], f)
	}
	return out
}

// findModuleRoot walks up from dir looking for a go.mod. Returns "" if none found.
func findModuleRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
