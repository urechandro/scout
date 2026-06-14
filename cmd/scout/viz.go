package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/urechandro/scout/query"
	"github.com/urechandro/scout/store"
)

func cmdViz(args []string) {
	fs := flag.NewFlagSet("scout viz", flag.ExitOnError)
	dbPath := fs.String("db", "/data/index.db", "Path to SQLite database.")
	symbol := fs.String("symbol", "", "Symbol ID to visualize (required).")
	direction := fs.String("direction", "both", "Edge direction: callers, callees, or both.")
	depth := fs.Int("depth", 2, "BFS depth (max 4).")
	_ = fs.Parse(args)

	if *symbol == "" && fs.NArg() > 0 {
		*symbol = fs.Arg(0)
	}
	if *symbol == "" {
		fmt.Fprintln(os.Stderr, "scout viz: --symbol is required")
		fs.Usage()
		os.Exit(1)
	}

	s, err := store.New(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scout viz: open store: %v\n", err)
		os.Exit(1)
	}
	defer s.Close()

	engine := query.New(s, query.Options{})
	result, err := engine.GetViz(*symbol, *direction, *depth)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scout viz: %v\n", err)
		os.Exit(1)
	}

	fmt.Print(result.DOT)
}
