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

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func cmdDoctor(args []string) {
	fs := flag.NewFlagSet("scout doctor", flag.ExitOnError)
	root := fs.String("root", ".", "Project root to inspect.")
	_ = fs.Parse(args)
	if err := runDoctor(*root, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "scout doctor: %v\n", err)
		os.Exit(1)
	}
}

func runDoctor(root string, out io.Writer) error {
	checks := []doctorCheck{}
	cfg, err := config.Load(root)
	if err != nil {
		checks = append(checks, doctorCheck{Name: "config", Status: "error", Detail: err.Error()})
		return encodeDoctor(out, checks)
	}
	if cfg.Navigation == nil || cfg.Navigation.TelemetryPath == "" {
		checks = append(checks, doctorCheck{Name: "navigation.telemetry", Status: "warn", Detail: "telemetry_path is not configured"})
		return encodeDoctor(out, checks)
	}
	path := cfg.Navigation.TelemetryPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	file, err := os.Open(path)
	if err != nil {
		checks = append(checks, doctorCheck{Name: "navigation.telemetry", Status: "error", Detail: err.Error()})
		return encodeDoctor(out, checks)
	}
	defer file.Close()
	if _, err := navigation.SummarizeTelemetry(file); err != nil {
		checks = append(checks, doctorCheck{Name: "navigation.telemetry", Status: "error", Detail: err.Error()})
	} else {
		checks = append(checks, doctorCheck{Name: "navigation.telemetry", Status: "ok", Detail: path})
	}
	return encodeDoctor(out, checks)
}

func encodeDoctor(out io.Writer, checks []doctorCheck) error {
	return json.NewEncoder(out).Encode(map[string]any{"checks": checks})
}
