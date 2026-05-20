// Command init bootstraps scout in a new project.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const scoutStart = "<!-- scout -->"
const scoutEnd = "<!-- /scout -->"

// claudeMDBlock is appended to or replaces the <!-- scout --> section in CLAUDE.md.
// Based on benchmark findings: "Only if" gating and explicit prohibition on
// grep/find/Read are the two most impactful instructions.
const claudeMDBlock = scoutStart + `
## Code Navigation — Use Scout Tools

This project has a Scout MCP server. Use its tools for ALL code navigation.
Do NOT use grep, find, or the Read tool to explore the codebase.

### Playbook

1. **Orient** (always first): call get_relevant_context("your query")
2. **Only if** you need to read or modify a specific symbol: call get_body(id) or get_flow(id)
3. **Before implementing a new RPC/handler**: call get_pattern("ClosestExistingRPC")
4. **Before renaming or changing a type**: call get_impact(id) for blast radius
5. **For architectural patterns**: call get_conventions("topic")

### Rules
- Never use grep, find, or Read to navigate code.
- Never read a file to understand a symbol — call get_body instead.
- Never search for callers with grep — call get_callers instead.
` + scoutEnd

const conventionsStarter = `# conventions.yaml — Architectural pattern registry for scout
#
# Each entry teaches Claude Code a repeating pattern in this codebase.
# The indexer loads this file automatically on every run.
# Claude calls get_conventions("topic") to retrieve patterns before implementing.
#
# Fill in examples with real symbol IDs from your codebase:
#   sqlite3 .scout/index.db "SELECT id, kind FROM symbols LIMIT 30"
#
# Fields:
#   name:        Unique slug
#   terms:       Search terms that trigger this convention
#   description: What the pattern is and WHY it exists
#   structure:   Pseudocode showing the repeating shape
#   examples:    Symbol IDs (suffix is fine — fuzzy-matched)

# - name: my-pattern
#   terms:
#     - keyword
#   description: |
#     What this pattern is and why it exists.
#   structure: |
#     func Example() {
#         // pseudocode
#     }
#   examples:
#     - yourpkg.TypeName.MethodName
`

type mcpServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

type mcpFile struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
}

func main() {
	root := flag.String("root", ".", "Project root to initialize (default: current directory).")
	db := flag.String("db", "", "SQLite database path (default: <root>/.scout/index.db).")
	tsconfig := flag.String("tsconfig", "", "tsconfig.json path for TypeScript indexing (auto-detected if absent).")
	tsCommand := flag.String("ts-command", "ts-callgraph", "ts-callgraph binary or 'node /path/to/cli.js'.")
	exclude := flag.String("exclude", "", "Comma-separated package path substrings to skip during indexing.")
	skipIndex := flag.Bool("skip-index", false, "Write config files only; skip running the indexer.")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		logger.Error("resolve root", "err", err)
		os.Exit(1)
	}

	dbPath := *db
	if dbPath == "" {
		dbPath = filepath.Join(absRoot, ".scout", "index.db")
	}
	absDB, err := filepath.Abs(dbPath)
	if err != nil {
		logger.Error("resolve db path", "err", err)
		os.Exit(1)
	}

	// 1. Create .scout/ directory.
	if err := os.MkdirAll(filepath.Dir(absDB), 0o755); err != nil {
		logger.Error("create .scout dir", "err", err)
		os.Exit(1)
	}
	logger.Info("ensured .scout/")

	// 2. Add .scout/ to .gitignore.
	if err := ensureGitignore(absRoot, logger); err != nil {
		logger.Warn("gitignore update", "err", err)
	}

	// 3. Detect project type (go, ts, proto).
	detected := detectProjectType(absRoot)
	logger.Info("detected project type", "types", strings.Join(detected, "+"))

	// Auto-detect tsconfig if not provided and project has TypeScript.
	if *tsconfig == "" && contains(detected, "ts") {
		if found := detectTSConfig(absRoot); found != "" {
			*tsconfig = found
			logger.Info("auto-detected tsconfig", "path", *tsconfig)
		}
	}

	// 4. Resolve scout-server binary.
	serverBin := resolveBin("scout-server", logger)

	// 5. Write .mcp.json (merge with existing, don't clobber other servers).
	if err := writeMCPJSON(absRoot, absDB, serverBin, *tsconfig, *tsCommand, logger); err != nil {
		logger.Error("write .mcp.json", "err", err)
		os.Exit(1)
	}

	// 6. Update CLAUDE.md (idempotent via <!-- scout --> marker).
	if err := updateCLAUDEMD(absRoot, logger); err != nil {
		logger.Error("update CLAUDE.md", "err", err)
		os.Exit(1)
	}

	// 7. Scaffold conventions.yaml if absent.
	if err := scaffoldConventions(absRoot, logger); err != nil {
		logger.Warn("scaffold conventions.yaml", "err", err)
	}

	// 8. Run full index with --deps.
	if !*skipIndex {
		if err := runIndex(absRoot, absDB, *tsconfig, *tsCommand, *exclude, logger); err != nil {
			logger.Error("indexing failed", "err", err)
			os.Exit(1)
		}
	}

	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "scout init complete!")
	fmt.Fprintln(os.Stderr, "  1. Reload Claude Code to pick up the MCP server.")
	fmt.Fprintln(os.Stderr, "  2. Customize conventions.yaml with your project's patterns.")
}

// ensureGitignore adds .scout/ to .gitignore if it is not already listed.
func ensureGitignore(root string, logger *slog.Logger) error {
	path := filepath.Join(root, ".gitignore")
	entry := ".scout/"

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	for line := range strings.SplitSeq(string(existing), "\n") {
		if strings.TrimSpace(line) == entry {
			logger.Info(".gitignore already has .scout/")
			return nil
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	prefix := ""
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		prefix = "\n"
	}
	_, err = fmt.Fprintf(f, "%s%s\n", prefix, entry)
	if err == nil {
		logger.Info("added .scout/ to .gitignore")
	}
	return err
}

// detectProjectType returns which languages/formats are present under root.
func detectProjectType(root string) []string {
	var types []string
	hasGo, hasTS, hasProto := false, false, false

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case name == "go.mod":
			hasGo = true
		case name == "tsconfig.json":
			hasTS = true
		case strings.HasSuffix(name, ".proto"):
			hasProto = true
		}
		return nil
	})

	if hasGo {
		types = append(types, "go")
	}
	if hasTS {
		types = append(types, "ts")
	}
	if hasProto {
		types = append(types, "proto")
	}
	if len(types) == 0 {
		types = append(types, "unknown")
	}
	return types
}

// detectTSConfig returns the first tsconfig.json found under root.
func detectTSConfig(root string) string {
	var found string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if name == "tsconfig.json" {
			found = path
		}
		return nil
	})
	return found
}

// resolveBin finds a binary in PATH, falling back to ~/go/bin/<name>.
func resolveBin(name string, logger *slog.Logger) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	fallback := filepath.Join(home, "go", "bin", name)
	logger.Warn("binary not found in PATH, using fallback", "binary", name, "fallback", fallback)
	return fallback
}

// writeMCPJSON writes or updates .mcp.json with the scout server entry.
// Merges with any existing mcpServers entries to avoid clobbering other servers.
func writeMCPJSON(root, dbPath, serverBin, tsconfig, tsCommand string, logger *slog.Logger) error {
	mcpPath := filepath.Join(root, ".mcp.json")

	cfg := mcpFile{MCPServers: map[string]json.RawMessage{}}

	// Read existing file if present.
	if raw, err := os.ReadFile(mcpPath); err == nil {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			logger.Warn(".mcp.json parse error, overwriting", "err", err)
			cfg = mcpFile{MCPServers: map[string]json.RawMessage{}}
		}
	}

	// Build scout server args.
	args := []string{"--db", dbPath, "--watch", root}
	if tsconfig != "" {
		args = append(args, "--tsconfig", tsconfig)
		if tsCommand != "" && tsCommand != "ts-callgraph" {
			args = append(args, "--ts-command", tsCommand)
		}
	}

	scout := mcpServer{
		Type:    "stdio",
		Command: serverBin,
		Args:    args,
		Env:     map[string]string{},
	}

	scoutRaw, err := json.Marshal(scout)
	if err != nil {
		return err
	}
	cfg.MCPServers["scout"] = scoutRaw

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(mcpPath, append(out, '\n'), 0o644); err != nil {
		return err
	}
	logger.Info("wrote .mcp.json")
	return nil
}

// updateCLAUDEMD appends or replaces the <!-- scout --> block in CLAUDE.md.
func updateCLAUDEMD(root string, logger *slog.Logger) error {
	claudePath := filepath.Join(root, "CLAUDE.md")

	existing, err := os.ReadFile(claudePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	content := string(existing)

	startIdx := strings.Index(content, scoutStart)
	endIdx := strings.Index(content, scoutEnd)

	var updated string
	if startIdx >= 0 && endIdx >= 0 && endIdx > startIdx {
		// Replace existing block.
		updated = content[:startIdx] + claudeMDBlock + content[endIdx+len(scoutEnd):]
		logger.Info("replaced existing scout block in CLAUDE.md")
	} else {
		// Append block.
		sep := ""
		if len(content) > 0 && !strings.HasSuffix(content, "\n\n") {
			if strings.HasSuffix(content, "\n") {
				sep = "\n"
			} else {
				sep = "\n\n"
			}
		}
		updated = content + sep + claudeMDBlock + "\n"
		logger.Info("appended scout block to CLAUDE.md")
	}

	return os.WriteFile(claudePath, []byte(updated), 0o644)
}

// scaffoldConventions writes a starter conventions.yaml if one does not exist.
func scaffoldConventions(root string, logger *slog.Logger) error {
	for _, name := range []string{"conventions.yaml", "conventions.yml"} {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			logger.Info("conventions file already exists, skipping scaffold", "file", name)
			return nil
		}
	}
	path := filepath.Join(root, "conventions.yaml")
	if err := os.WriteFile(path, []byte(conventionsStarter), 0o644); err != nil {
		return err
	}
	logger.Info("scaffolded conventions.yaml")
	return nil
}

// runIndex executes scout-index to perform a full index with --deps.
func runIndex(root, dbPath, tsconfig, tsCommand, exclude string, logger *slog.Logger) error {
	indexBin := resolveBin("scout-index", logger)

	args := []string{
		"--db", dbPath,
		"--root", root,
		"--deps",
	}
	if exclude != "" {
		args = append(args, "--exclude", exclude)
	}
	if tsconfig != "" {
		args = append(args, "--tsconfig", tsconfig)
		if tsCommand != "" && tsCommand != "ts-callgraph" {
			args = append(args, "--ts-command", tsCommand)
		}
	}

	logger.Info("running full index", "bin", indexBin, "root", root)
	cmd := exec.Command(indexBin, args...)
	cmd.Stdout = os.Stderr // index logs to stderr; mirror both streams to our stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func contains(slice []string, s string) bool {
	return slices.Contains(slice, s)
}
