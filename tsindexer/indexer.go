// Package tsindexer shells out to ts-callgraph and writes TypeScript symbols
// and edges to the store.
package tsindexer

import (
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"

	"github.com/urechandro/scout/store"
)

// Config controls what gets indexed.
type Config struct {
	// TsconfigPath is the path to the tsconfig.json file.
	TsconfigPath string
	// Root is the project root directory (passed as --root to ts-callgraph).
	Root string
	// Command is the binary to run. Default: "ts-callgraph".
	// For non-global installs, use "node" with CommandArgs set to the script path.
	Command string
	// CommandArgs are extra arguments prepended before ts-callgraph flags.
	// Example: for "node /path/to/cli.js", set Command="node" and
	// CommandArgs=["/path/to/cli.js"].
	CommandArgs []string
	// ExcludePatterns are comma-separated patterns passed to --exclude.
	ExcludePatterns []string
}

// Indexer parses TypeScript files via ts-callgraph and writes symbols to a Store.
type Indexer struct {
	cfg   Config
	store *store.Store
}

type output struct {
	Symbols []tsSymbol `json:"symbols"`
	Edges   []tsEdge   `json:"edges"`
}

type tsSymbol struct {
	ID        string `json:"id"`
	Package   string `json:"package"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Signature string `json:"signature"`
	Docstring string `json:"docstring"`
	File      string `json:"file"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Body      string `json:"body"`
}

type tsEdge struct {
	FromID string `json:"from_id"`
	ToID   string `json:"to_id"`
	Kind   string `json:"kind"`
}

// New creates an Indexer for the given config and store.
func New(cfg Config, s *store.Store) *Indexer {
	if cfg.Command == "" {
		cfg.Command = "ts-callgraph"
	}
	return &Indexer{cfg: cfg, store: s}
}

// Run executes a full TypeScript index: runs ts-callgraph, clears stale TS
// symbols, and upserts all symbols and edges from the output.
func (idx *Indexer) Run() error {
	out, err := idx.exec()
	if err != nil {
		return err
	}

	if err := idx.deleteStaleSymbols(out); err != nil {
		return fmt.Errorf("delete stale TS symbols: %w", err)
	}

	return idx.upsert(out)
}

// RunFiles re-runs the full ts-callgraph analysis (the TypeScript compiler
// needs the whole program for type info) and upserts results for the given
// files. Other files in the output are also upserted since ts-callgraph
// returns the full project.
func (idx *Indexer) RunFiles(files []string) error {
	return idx.Run()
}

func (idx *Indexer) exec() (*output, error) {
	args := append([]string{}, idx.cfg.CommandArgs...)
	args = append(args, "--tsconfig", idx.cfg.TsconfigPath)
	if idx.cfg.Root != "" {
		args = append(args, "--root", idx.cfg.Root)
	}
	if len(idx.cfg.ExcludePatterns) > 0 {
		args = append(args, "--exclude", strings.Join(idx.cfg.ExcludePatterns, ","))
	}

	cmd := exec.Command(idx.cfg.Command, args...)
	raw, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("ts-callgraph failed: %s", string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("ts-callgraph exec: %w", err)
	}

	var out output
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse ts-callgraph output: %w", err)
	}

	return &out, nil
}

// deleteStaleSymbols removes symbols for TS files that are in the store but
// not in the current ts-callgraph output (e.g. deleted files).
func (idx *Indexer) deleteStaleSymbols(out *output) error {
	freshFiles := make(map[string]bool, len(out.Symbols))
	for _, sym := range out.Symbols {
		freshFiles[sym.File] = true
	}

	stale, err := idx.store.GetFilesByExtensions([]string{".ts", ".tsx"})
	if err != nil {
		return err
	}

	for _, f := range stale {
		if !freshFiles[f] {
			if err := idx.store.DeleteByFile(f); err != nil {
				return fmt.Errorf("delete %s: %w", f, err)
			}
		}
	}
	return nil
}

func (idx *Indexer) upsert(out *output) error {
	for _, sym := range out.Symbols {
		s := store.Symbol{
			ID:        sym.ID,
			Package:   sym.Package,
			Name:      sym.Name,
			Kind:      sym.Kind,
			Signature: sym.Signature,
			Docstring: sym.Docstring,
			File:      sym.File,
			LineStart: sym.LineStart,
			LineEnd:   sym.LineEnd,
			Body:      sym.Body,
		}
		if err := idx.store.UpsertSymbol(s); err != nil {
			return fmt.Errorf("upsert symbol %s: %w", sym.ID, err)
		}
	}

	for _, edge := range out.Edges {
		e := store.Edge{
			FromID: edge.FromID,
			ToID:   edge.ToID,
			Kind:   edge.Kind,
		}
		if err := idx.store.UpsertEdge(e); err != nil {
			return fmt.Errorf("upsert edge %s->%s: %w", edge.FromID, edge.ToID, err)
		}
	}

	log.Printf("ts indexer: indexed %d symbols and %d edges", len(out.Symbols), len(out.Edges))
	return nil
}
