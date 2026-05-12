// Command indexer parses a Go codebase and writes symbols and edges to the index.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/urechandro/scout/indexer"
	"github.com/urechandro/scout/protoindexer"
	"github.com/urechandro/scout/store"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	dbPath := flag.String("db", "/data/index.db", "Path to SQLite database.")
	dir := flag.String("dir", "", "Root directory of a single Go module to index.")
	root := flag.String("root", "/workspace", "Root directory to search for Go modules (go.mod files). Skips hidden dirs.")
	patterns := flag.String("patterns", "./...", "Comma-separated Go package patterns.")
	files := flag.String("files", "", "Comma-separated list of specific files to re-index (incremental mode).")
	excludeGenerated := flag.Bool("exclude-generated", true, "Skip generated files and directories (gen, vendor, *.pb.go, etc).")
	exclude := flag.String("exclude", "", "Comma-separated package path substrings to skip, e.g. cmd/localserver,cmd/migration.")
	method := flag.String("method", "rta", "Call graph algorithm: rta (precise, default) or cha (fast, conservative). Only used for full index.")
	flag.Parse()

	s, err := store.New(*dbPath)
	if err != nil {
		logger.Error("open store", "err", err)
		os.Exit(1)
	}
	defer s.Close()

	// Determine which module directories to index.
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

	for _, d := range dirs {
		var excludePaths []string
		if *exclude != "" {
			excludePaths = strings.Split(*exclude, ",")
		}
		cfg := indexer.Config{
			Dir:              d,
			Patterns:         strings.Split(*patterns, ","),
			ExcludeGenerated: *excludeGenerated,
			ExcludePaths:     excludePaths,
			CallGraph:        cgMethod,
		}
		idx := indexer.New(cfg, s)

		if *files != "" {
			changed := strings.Split(*files, ",")
			logger.Info("incremental index", "dir", d, "files", len(changed))
			if err := idx.RunFiles(changed); err != nil {
				logger.Error("incremental index failed", "dir", d, "err", err)
				os.Exit(1)
			}
		} else {
			logger.Info("full index", "dir", d, "patterns", *patterns)
			if err := idx.Run(); err != nil {
				logger.Error("full index failed", "dir", d, "err", err)
				os.Exit(1)
			}
		}
	}

	// Index .proto files from the workspace root.
	protoDir := *root
	if *dir != "" {
		protoDir = *dir
	}
	var excludePaths []string
	if *exclude != "" {
		excludePaths = strings.Split(*exclude, ",")
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

	// Link proto RPCs to their Go implementations via "implements" edges.
	if err := linkProtoToGo(s, logger); err != nil {
		logger.Warn("proto-go linking failed", "err", err)
	}

	// Index conventions.yaml if present in the workspace.
	conventionsDir := *root
	if *dir != "" {
		conventionsDir = *dir
	}
	if err := indexer.IndexConventions(conventionsDir, s); err != nil {
		logger.Warn("conventions index failed", "err", err)
	}

	if err := s.SetMeta("last_indexed", fmt.Sprintf("%d", nowUnix())); err != nil {
		logger.Warn("set meta failed", "err", err)
	}

	logger.Info("indexing complete")
}

// linkProtoToGo finds all proto RPC symbols and creates "implements" edges to
// their corresponding Go method implementations (matched by name, preferring svc packages).
func linkProtoToGo(s *store.Store, logger *slog.Logger) error {
	rpcs, err := s.GetByKind("rpc")
	if err != nil {
		return fmt.Errorf("get rpcs: %w", err)
	}

	var linked int
	for _, rpc := range rpcs {
		methods, err := s.GetByNameAndKind(rpc.Name, "method")
		if err != nil || len(methods) == 0 {
			continue
		}
		// Prefer svc packages.
		impl := methods[0]
		for _, m := range methods {
			if strings.Contains(m.Package, "svc") {
				impl = m
				break
			}
		}
		edge := store.Edge{
			FromID: impl.ID,
			ToID:   rpc.ID,
			Kind:   "implements",
		}
		if err := s.UpsertEdge(edge); err != nil {
			logger.Warn("link proto-go edge", "from", impl.ID, "to", rpc.ID, "err", err)
			continue
		}
		linked++
	}

	logger.Info("proto-go linking complete", "rpcs", len(rpcs), "linked", linked)
	return nil
}

// findModuleDirs walks root and returns every directory containing a go.mod,
// skipping hidden directories (dot-prefixed).
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

func nowUnix() int64 {
	f, _ := os.CreateTemp("", "ts")
	if f != nil {
		info, _ := f.Stat()
		f.Close()
		os.Remove(f.Name())
		if info != nil {
			return info.ModTime().Unix()
		}
	}

	return 0
}
