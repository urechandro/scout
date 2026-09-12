package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/urechandro/scout/config"
	"github.com/urechandro/scout/navigation"
)

func cmdNavigationObserve(args []string) {
	fs := flag.NewFlagSet("scout navigation observe", flag.ExitOnError)
	root := fs.String("root", ".", "Project root used to load .scout/config.yaml.")
	_ = fs.Parse(args)

	cfg, err := config.Load(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scout navigation observe: %v\n", err)
		os.Exit(1)
	}
	observer, cleanup, err := navigationObserver(*root, cfg.Navigation)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scout navigation observe: %v\n", err)
		os.Exit(1)
	}
	defer cleanup()
	if err := runNavigationObserve(os.Stdin, os.Stdout, observer); err != nil {
		fmt.Fprintf(os.Stderr, "scout navigation observe: %v\n", err)
		os.Exit(1)
	}
}

func navigationObserver(root string, cfg *config.NavigationConfig) (navigation.Observer, func(), error) {
	if cfg == nil || cfg.TelemetryPath == "" {
		return navigation.Observer{}, func() {}, nil
	}
	path := cfg.TelemetryPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	recorder, err := navigation.NewRecorder(path)
	if err != nil {
		return navigation.Observer{}, nil, err
	}
	return navigation.Observer{
		Policy: navigation.Policy{
			Enforcement:       cfg.Enforcement,
			MaxWholeFileLines: cfg.MaxWholeFileLines,
			MaxTargetedLines:  cfg.MaxTargetedReadLines,
		},
		Recorder: recorder,
	}, func() { _ = recorder.Close() }, nil
}

func runNavigationObserve(in io.Reader, out io.Writer, observer navigation.Observer) error {
	payload, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("read event: %w", err)
	}
	decision, err := observer.ObserveJSON(payload)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(out).Encode(decision); err != nil {
		return fmt.Errorf("write decision: %w", err)
	}
	return nil
}
