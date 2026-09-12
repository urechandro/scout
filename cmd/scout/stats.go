package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/urechandro/scout/config"
	"github.com/urechandro/scout/navigation"
)

func cmdStats(args []string) {
	fs := flag.NewFlagSet("scout stats", flag.ExitOnError)
	root := fs.String("root", ".", "Project root used to load .scout/config.yaml.")
	pathFlag := fs.String("path", "", "Telemetry JSONL path (overrides configured navigation.telemetry_path).")
	_ = fs.Parse(args)

	path := *pathFlag
	if path == "" {
		cfg, err := config.Load(*root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scout stats: %v\n", err)
			os.Exit(1)
		}
		if cfg.Navigation == nil || cfg.Navigation.TelemetryPath == "" {
			fmt.Fprintln(os.Stderr, "scout stats: navigation.telemetry_path is not configured")
			os.Exit(1)
		}
		path = cfg.Navigation.TelemetryPath
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(*root, path)
	}
	file, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scout stats: open telemetry: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()
	if err := writeStats(os.Stdout, file); err != nil {
		fmt.Fprintf(os.Stderr, "scout stats: %v\n", err)
		os.Exit(1)
	}
}

func writeStats(out interface{ Write([]byte) (int, error) }, telemetry *os.File) error {
	stats, err := navigation.SummarizeTelemetry(telemetry)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(out).Encode(stats); err != nil {
		return fmt.Errorf("write stats: %w", err)
	}
	return nil
}
