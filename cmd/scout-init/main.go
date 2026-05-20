// Command init bootstraps scout in a new project.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/charmbracelet/huh"
)

const scoutStart = "<!-- scout -->"
const scoutEnd = "<!-- /scout -->"

// claudeMDBlock is the <!-- scout --> section written to CLAUDE.md.
// Based on benchmark findings: "Only if" gating and the explicit prohibition
// on grep/find/Read are the two highest-impact instructions.
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

// wizardConfig holds all settings collected by the TUI or derived from flags.
type wizardConfig struct {
	Root                string
	DBPath              string
	Languages           []string // "go", "ts", "proto"
	TSConfig            string
	TSCommand           string
	Exclude             string
	IndexDeps           bool
	EnableWatch         bool
	ScaffoldConventions bool
}

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
	rootFlag := flag.String("root", ".", "Project root to initialize.")
	dbFlag := flag.String("db", "", "SQLite database path (default: <root>/.scout/index.db).")
	tsconfigFlag := flag.String("tsconfig", "", "tsconfig.json path (auto-detected if absent).")
	tsCommandFlag := flag.String("ts-command", "ts-callgraph", "ts-callgraph binary or 'node /path/to/cli.js'.")
	excludeFlag := flag.String("exclude", "", "Comma-separated package path substrings to skip.")
	skipIndexFlag := flag.Bool("skip-index", false, "Write config files only; skip running the indexer.")
	yes := flag.Bool("yes", false, "Non-interactive: accept all defaults (CI-safe).")
	flag.BoolVar(yes, "y", false, "Alias for --yes.")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	absRoot, err := filepath.Abs(*rootFlag)
	if err != nil {
		logger.Error("resolve root", "err", err)
		os.Exit(1)
	}

	detected := detectProjectType(absRoot)
	conventionsExist := checkConventionsExist(absRoot)

	cfg := buildDefaults(absRoot, *dbFlag, *tsconfigFlag, *tsCommandFlag, *excludeFlag, *skipIndexFlag, detected, conventionsExist)

	if shouldRunTUI(*yes) {
		if err := runWizard(&cfg, conventionsExist); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				fmt.Fprintln(os.Stderr, "Aborted.")
				os.Exit(0)
			}
			logger.Error("wizard error", "err", err)
			os.Exit(1)
		}
	}

	// Re-resolve absolute paths — user may have edited Root in the wizard.
	absRoot, err = filepath.Abs(cfg.Root)
	if err != nil {
		logger.Error("resolve root", "err", err)
		os.Exit(1)
	}
	absDB, err := filepath.Abs(cfg.DBPath)
	if err != nil {
		logger.Error("resolve db path", "err", err)
		os.Exit(1)
	}

	printSummary(cfg)

	if err := os.MkdirAll(filepath.Dir(absDB), 0o755); err != nil {
		logger.Error("create .scout dir", "err", err)
		os.Exit(1)
	}
	logger.Info("ensured .scout/")

	if err := ensureGitignore(absRoot, logger); err != nil {
		logger.Warn("gitignore update", "err", err)
	}

	serverBin := resolveBin("scout-server", logger)

	if err := writeMCPJSON(absRoot, absDB, serverBin, cfg.TSConfig, cfg.TSCommand, cfg.EnableWatch, logger); err != nil {
		logger.Error("write .mcp.json", "err", err)
		os.Exit(1)
	}

	if err := updateCLAUDEMD(absRoot, logger); err != nil {
		logger.Error("update CLAUDE.md", "err", err)
		os.Exit(1)
	}

	if cfg.ScaffoldConventions {
		if err := scaffoldConventions(absRoot, logger); err != nil {
			logger.Warn("scaffold conventions.yaml", "err", err)
		}
	}

	if cfg.IndexDeps {
		if err := runIndex(absRoot, absDB, cfg.TSConfig, cfg.TSCommand, cfg.Exclude, logger); err != nil {
			logger.Error("indexing failed", "err", err)
			os.Exit(1)
		}
	}

	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "scout init complete!")
	fmt.Fprintln(os.Stderr, "  1. Reload Claude Code to pick up the MCP server.")
	if cfg.ScaffoldConventions {
		fmt.Fprintln(os.Stderr, "  2. Customize conventions.yaml with your project's patterns.")
	}
}

// shouldRunTUI returns true when stdin is an interactive terminal and --yes was not set.
func shouldRunTUI(yes bool) bool {
	if yes {
		return false
	}
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// buildDefaults populates a wizardConfig from flags and auto-detection.
func buildDefaults(absRoot, dbFlag, tsconfigFlag, tsCommand, exclude string, skipIndex bool, detected []string, conventionsExist bool) wizardConfig {
	dbPath := dbFlag
	if dbPath == "" {
		dbPath = filepath.Join(absRoot, ".scout", "index.db")
	}

	tsconfig := tsconfigFlag
	if tsconfig == "" && slices.Contains(detected, "ts") {
		tsconfig = detectTSConfig(absRoot)
	}

	// Strip "unknown" — multi-select only shows real languages.
	langs := make([]string, 0, len(detected))
	for _, l := range detected {
		if l != "unknown" {
			langs = append(langs, l)
		}
	}

	return wizardConfig{
		Root:                absRoot,
		DBPath:              dbPath,
		Languages:           langs,
		TSConfig:            tsconfig,
		TSCommand:           tsCommand,
		Exclude:             exclude,
		IndexDeps:           !skipIndex,
		EnableWatch:         true,
		ScaffoldConventions: !conventionsExist,
	}
}

// runWizard runs the huh TUI form and mutates cfg with the user's choices.
func runWizard(cfg *wizardConfig, conventionsExist bool) error {
	// Group 1: core project settings.
	groupProject := huh.NewGroup(
		huh.NewInput().
			Title("Project root").
			Description("Directory to initialize Scout in.").
			Value(&cfg.Root),

		huh.NewInput().
			Title("Database path").
			Description("Where to store the SQLite index.").
			Value(&cfg.DBPath),

		huh.NewMultiSelect[string]().
			Title("Languages to index").
			Options(
				huh.NewOption("Go", "go"),
				huh.NewOption("TypeScript", "ts"),
				huh.NewOption("Protobuf", "proto"),
			).
			Value(&cfg.Languages),
	)

	// Group 2: tsconfig — only shown when TypeScript is selected.
	// WithHideFunc is evaluated dynamically when the form navigates to this group.
	groupTS := huh.NewGroup(
		huh.NewInput().
			Title("tsconfig.json path").
			Description("Path to tsconfig.json for TypeScript indexing.").
			Value(&cfg.TSConfig),
	).WithHideFunc(func() bool {
		return !slices.Contains(cfg.Languages, "ts")
	})

	// Group 3: behavior options.
	groupBehavior := huh.NewGroup(
		huh.NewConfirm().
			Title("Run indexer now?").
			Description("Runs scout-index --deps to build the initial index.").
			Value(&cfg.IndexDeps),

		huh.NewConfirm().
			Title("Enable file watcher?").
			Description("Adds --watch to the MCP server so the index updates on save.").
			Value(&cfg.EnableWatch),
	)

	// Group 4: conventions — only shown when no conventions file exists yet.
	groupConventions := huh.NewGroup(
		huh.NewConfirm().
			Title("Scaffold conventions.yaml?").
			Description("Creates a starter file for documenting architectural patterns.").
			Value(&cfg.ScaffoldConventions),
	).WithHideFunc(func() bool {
		return conventionsExist
	})

	return huh.NewForm(groupProject, groupTS, groupBehavior, groupConventions).
		WithTheme(huh.ThemeCharm()).
		Run()
}

// printSummary prints what scout is about to do before executing side effects.
func printSummary(cfg wizardConfig) {
	fmt.Fprintln(os.Stderr, "\nScout will configure the following:")
	fmt.Fprintf(os.Stderr, "  Root:        %s\n", cfg.Root)
	fmt.Fprintf(os.Stderr, "  DB:          %s\n", cfg.DBPath)
	fmt.Fprintf(os.Stderr, "  Languages:   %s\n", strings.Join(cfg.Languages, ", "))
	if cfg.TSConfig != "" {
		fmt.Fprintf(os.Stderr, "  tsconfig:    %s\n", cfg.TSConfig)
	}
	fmt.Fprintf(os.Stderr, "  Watch:       %v\n", cfg.EnableWatch)
	fmt.Fprintf(os.Stderr, "  Index now:   %v\n", cfg.IndexDeps)
	fmt.Fprintf(os.Stderr, "  Conventions: %v\n", cfg.ScaffoldConventions)
	fmt.Fprintln(os.Stderr)
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
	var hasGo, hasTS, hasProto bool

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

	var types []string
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

// checkConventionsExist reports whether a conventions file already exists.
func checkConventionsExist(root string) bool {
	for _, name := range []string{"conventions.yaml", "conventions.yml"} {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			return true
		}
	}
	return false
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
func writeMCPJSON(root, dbPath, serverBin, tsconfig, tsCommand string, enableWatch bool, logger *slog.Logger) error {
	mcpPath := filepath.Join(root, ".mcp.json")

	cfg := mcpFile{MCPServers: map[string]json.RawMessage{}}

	if raw, err := os.ReadFile(mcpPath); err == nil {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			logger.Warn(".mcp.json parse error, overwriting", "err", err)
			cfg = mcpFile{MCPServers: map[string]json.RawMessage{}}
		}
	}

	args := []string{"--db", dbPath}
	if enableWatch {
		args = append(args, "--watch", root)
	}
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
		updated = content[:startIdx] + claudeMDBlock + content[endIdx+len(scoutEnd):]
		logger.Info("replaced existing scout block in CLAUDE.md")
	} else {
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

// scaffoldConventions writes a starter conventions.yaml.
func scaffoldConventions(root string, logger *slog.Logger) error {
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

	args := []string{"--db", dbPath, "--root", root, "--deps"}
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
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
