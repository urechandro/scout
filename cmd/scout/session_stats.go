package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/urechandro/scout/telemetry"
)

func cmdSessionStats(args []string) {
	fs := flag.NewFlagSet("scout session-stats", flag.ExitOnError)
	path := fs.String("path", "", "Session telemetry JSONL path.")
	_ = fs.Parse(args)
	if *path == "" {
		fmt.Fprintln(os.Stderr, "scout session-stats: --path is required")
		os.Exit(2)
	}
	file, err := os.Open(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scout session-stats: open telemetry: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()
	if err := writeSessionStats(os.Stdout, file); err != nil {
		fmt.Fprintf(os.Stderr, "scout session-stats: %v\n", err)
		os.Exit(1)
	}
}

func writeSessionStats(out io.Writer, input io.Reader) error {
	stats, err := telemetry.Summarize(input)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(out).Encode(stats); err != nil {
		return fmt.Errorf("write session stats: %w", err)
	}
	return nil
}
