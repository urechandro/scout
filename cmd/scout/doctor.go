package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/urechandro/scout/config"
	"github.com/urechandro/scout/navigation"
	_ "modernc.org/sqlite"
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
	checks = append(checks, checkRoot(root))
	checks = append(checks, checkLanguageTooling(root))
	checks = append(checks, checkDatabase(filepath.Join(root, config.Dir, "index.db")))
	cfg, err := config.Load(root)
	if err != nil {
		checks = append(checks, doctorCheck{Name: "config", Status: "error", Detail: err.Error()})
		return encodeDoctor(out, checks)
	}
	checks = append(checks, checkEmbedder(cfg))
	checks = append(checks, checkMCPConfig(root))
	checks = append(checks, checkAgentConfig(root))
	checks = append(checks, checkWatcher(root))
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

func checkEmbedder(cfg *config.Config) doctorCheck {
	if cfg.Embedder == nil {
		return doctorCheck{Name: "ollama.model", Status: "warn", Detail: "semantic embedding is not configured"}
	}
	return doctorCheck{Name: "ollama.model", Status: "ok", Detail: fmt.Sprintf("%s @ %s", cfg.Embedder.Model, cfg.Embedder.Host)}
}

func checkMCPConfig(root string) doctorCheck {
	path := filepath.Join(root, ".mcp.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return doctorCheck{Name: "mcp.config", Status: "warn", Detail: ".mcp.json is not configured"}
		}
		return doctorCheck{Name: "mcp.config", Status: "error", Detail: err.Error()}
	}
	var cfg struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return doctorCheck{Name: "mcp.config", Status: "error", Detail: err.Error()}
	}
	if _, ok := cfg.MCPServers["scout"]; !ok {
		return doctorCheck{Name: "mcp.config", Status: "warn", Detail: "scout server is not configured"}
	}
	return doctorCheck{Name: "mcp.config", Status: "ok", Detail: path}
}

func checkAgentConfig(root string) doctorCheck {
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		path := filepath.Join(root, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), "scout:start") || strings.Contains(strings.ToLower(string(raw)), "scout") {
			return doctorCheck{Name: "client.guidance", Status: "ok", Detail: path}
		}
	}
	return doctorCheck{Name: "client.guidance", Status: "warn", Detail: "no Scout client guidance file found"}
}

func checkWatcher(root string) doctorCheck {
	raw, err := os.ReadFile(filepath.Join(root, ".mcp.json"))
	if err != nil {
		return doctorCheck{Name: "watcher.config", Status: "warn", Detail: "watcher state is unavailable"}
	}
	if strings.Contains(string(raw), `"--watch"`) {
		return doctorCheck{Name: "watcher.config", Status: "ok", Detail: "MCP server watcher is enabled"}
	}
	return doctorCheck{Name: "watcher.config", Status: "warn", Detail: "MCP server watcher is not enabled"}
}

func checkRoot(root string) doctorCheck {
	info, err := os.Stat(root)
	if err != nil {
		return doctorCheck{Name: "repository.root", Status: "error", Detail: err.Error()}
	}
	if !info.IsDir() {
		return doctorCheck{Name: "repository.root", Status: "error", Detail: "path is not a directory"}
	}
	return doctorCheck{Name: "repository.root", Status: "ok", Detail: root}
}

func checkLanguageTooling(root string) doctorCheck {
	markers := []string{"go.mod", "package.json", "pyproject.toml", "Cargo.toml", "pom.xml", "requirements.txt"}
	found := make([]string, 0, len(markers))
	for _, marker := range markers {
		if _, err := os.Stat(filepath.Join(root, marker)); err == nil {
			found = append(found, marker)
		}
	}
	if len(found) == 0 {
		return doctorCheck{Name: "language.tooling", Status: "warn", Detail: "no recognized project manifest found"}
	}
	return doctorCheck{Name: "language.tooling", Status: "ok", Detail: strings.Join(found, ", ")}
}

func checkDatabase(path string) doctorCheck {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return doctorCheck{Name: "database.schema", Status: "warn", Detail: "index database does not exist"}
		}
		return doctorCheck{Name: "database.schema", Status: "error", Detail: err.Error()}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return doctorCheck{Name: "database.schema", Status: "error", Detail: err.Error()}
	}
	defer db.Close()
	const query = `SELECT count(*) FROM sqlite_master WHERE type IN ('table', 'view') AND name IN (?, ?, ?, ?, ?, ?)`
	var count int
	if err := db.QueryRow(query, "symbols", "edges", "index_meta", "conventions", "symbols_fts", "conventions_fts").Scan(&count); err != nil {
		return doctorCheck{Name: "database.schema", Status: "error", Detail: err.Error()}
	}
	if count != 6 {
		return doctorCheck{Name: "database.schema", Status: "error", Detail: fmt.Sprintf("expected 6 schema objects, found %d", count)}
	}
	return doctorCheck{Name: "database.schema", Status: "ok", Detail: path}
}

func encodeDoctor(out io.Writer, checks []doctorCheck) error {
	return json.NewEncoder(out).Encode(map[string]any{"checks": checks})
}
