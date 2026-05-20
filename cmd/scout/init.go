package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/charmbracelet/huh"
)

const scoutStart = "<!-- scout -->"
const scoutEnd = "<!-- /scout -->"

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

type wizardConfig struct {
	Root                string
	DBPath              string
	Languages           []string // "go", "ts", "proto"
	TSConfig            string
	TSCommand           string
	Exclude             string
	Method              string // "rta", "cha", "ast"
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

func cmdInit(args []string) {
	fs := flag.NewFlagSet("scout init", flag.ExitOnError)
	rootFlag := fs.String("root", ".", "Project root to initialize.")
	dbFlag := fs.String("db", "", "SQLite database path (default: <root>/.scout/index.db).")
	tsconfigFlag := fs.String("tsconfig", "", "tsconfig.json path (auto-detected if absent).")
	tsCommandFlag := fs.String("ts-command", "ts-callgraph", "ts-callgraph binary or 'node /path/to/cli.js'.")
	excludeFlag := fs.String("exclude", "", "Comma-separated package path substrings to skip.")
	skipIndexFlag := fs.Bool("skip-index", false, "Write config files only; skip running the indexer.")
	yes := fs.Bool("yes", false, "Non-interactive: accept all defaults (CI-safe).")
	fs.BoolVar(yes, "y", false, "Alias for --yes.")
	_ = fs.Parse(args)

	logger := newLogger(false)

	absRoot, err := filepath.Abs(*rootFlag)
	if err != nil {
		logger.Error("resolve root", "err", err)
		os.Exit(1)
	}

	detected := detectProjectType(absRoot)
	conventionsExist := checkConventionsExist(absRoot)

	cfg := buildInitDefaults(absRoot, *dbFlag, *tsconfigFlag, *tsCommandFlag, *excludeFlag, *skipIndexFlag, detected, conventionsExist)

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

	// Re-resolve absolute paths — user may have changed Root in the wizard.
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

	printInitSummary(cfg)

	if err := os.MkdirAll(filepath.Dir(absDB), 0o755); err != nil {
		logger.Error("create .scout dir", "err", err)
		os.Exit(1)
	}
	logger.Info("ensured .scout/")

	if err := ensureGitignore(absRoot, logger); err != nil {
		logger.Warn("gitignore update", "err", err)
	}

	scoutBin := resolveBin("scout")
	logger.Info("resolved scout binary", "path", scoutBin)

	if err := writeInitMCPJSON(absRoot, absDB, scoutBin, cfg.TSConfig, cfg.TSCommand, cfg.EnableWatch, logger); err != nil {
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
		if err := runInitIndex(absRoot, absDB, cfg.TSConfig, cfg.TSCommand, cfg.Exclude, cfg.Method, logger); err != nil {
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

func buildInitDefaults(absRoot, dbFlag, tsconfigFlag, tsCommand, exclude string, skipIndex bool, detected []string, conventionsExist bool) wizardConfig {
	dbPath := dbFlag
	if dbPath == "" {
		dbPath = filepath.Join(absRoot, ".scout", "index.db")
	}

	tsconfig := tsconfigFlag
	if tsconfig == "" && slices.Contains(detected, "ts") {
		tsconfig = detectTSConfig(absRoot)
	}

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
		Method:              "rta",
		IndexDeps:           !skipIndex,
		EnableWatch:         true,
		ScaffoldConventions: !conventionsExist,
	}
}

func runWizard(cfg *wizardConfig, conventionsExist bool) error {
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

	// Shown only when TypeScript is selected — WithHideFunc is dynamic.
	groupTS := huh.NewGroup(
		huh.NewInput().
			Title("tsconfig.json path").
			Description("Path to tsconfig.json for TypeScript indexing.").
			Value(&cfg.TSConfig),
	).WithHideFunc(func() bool {
		return !slices.Contains(cfg.Languages, "ts")
	})

	groupBehavior := huh.NewGroup(
		huh.NewSelect[string]().
			Title("Call graph method").
			Options(
				huh.NewOption("rta — precise, recommended for most projects", "rta"),
				huh.NewOption("cha — fast, conservative (good for large codebases)", "cha"),
				huh.NewOption("ast — fastest, no type info (CI / huge monorepos)", "ast"),
			).
			Value(&cfg.Method),

		huh.NewConfirm().
			Title("Run indexer now?").
			Description("Runs scout index --deps to build the initial index.").
			Value(&cfg.IndexDeps),

		huh.NewConfirm().
			Title("Enable file watcher?").
			Description("Adds --watch to the MCP server so the index updates on save.").
			Value(&cfg.EnableWatch),
	)

	// Shown only when no conventions file exists yet.
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

func printInitSummary(cfg wizardConfig) {
	fmt.Fprintln(os.Stderr, "\nScout will configure the following:")
	fmt.Fprintf(os.Stderr, "  Root:        %s\n", cfg.Root)
	fmt.Fprintf(os.Stderr, "  DB:          %s\n", cfg.DBPath)
	fmt.Fprintf(os.Stderr, "  Languages:   %s\n", strings.Join(cfg.Languages, ", "))
	if cfg.TSConfig != "" {
		fmt.Fprintf(os.Stderr, "  tsconfig:    %s\n", cfg.TSConfig)
	}
	fmt.Fprintf(os.Stderr, "  Method:      %s\n", cfg.Method)
	fmt.Fprintf(os.Stderr, "  Watch:       %v\n", cfg.EnableWatch)
	fmt.Fprintf(os.Stderr, "  Index now:   %v\n", cfg.IndexDeps)
	fmt.Fprintf(os.Stderr, "  Conventions: %v\n", cfg.ScaffoldConventions)
	fmt.Fprintln(os.Stderr)
}

func ensureGitignore(root string, logger interface{ Info(string, ...any); Warn(string, ...any) }) error {
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

func checkConventionsExist(root string) bool {
	for _, name := range []string{"conventions.yaml", "conventions.yml"} {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			return true
		}
	}
	return false
}

// resolveBin finds a binary in PATH, falling back to ~/go/bin/<name>.
func resolveBin(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "go", "bin", name)
}

func writeInitMCPJSON(root, dbPath, scoutBin, tsconfig, tsCommand string, enableWatch bool, logger interface{ Warn(string, ...any); Info(string, ...any) }) error {
	mcpPath := filepath.Join(root, ".mcp.json")

	cfg := mcpFile{MCPServers: map[string]json.RawMessage{}}

	if raw, err := os.ReadFile(mcpPath); err == nil {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			logger.Warn(".mcp.json parse error, overwriting", "err", err)
			cfg = mcpFile{MCPServers: map[string]json.RawMessage{}}
		}
	}

	// Use "scout serve" as the MCP command rather than the legacy "scout-server".
	args := []string{"serve", "--db", dbPath}
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
		Command: scoutBin,
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

func updateCLAUDEMD(root string, logger interface{ Info(string, ...any) }) error {
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

func scaffoldConventions(root string, logger interface{ Info(string, ...any) }) error {
	path := filepath.Join(root, "conventions.yaml")
	if err := os.WriteFile(path, []byte(conventionsStarter), 0o644); err != nil {
		return err
	}
	logger.Info("scaffolded conventions.yaml")
	return nil
}

// runInitIndex execs "scout index" to run the full index with --deps.
func runInitIndex(root, dbPath, tsconfig, tsCommand, exclude, method string, logger interface{ Info(string, ...any); Error(string, ...any) }) error {
	scoutBin := resolveBin("scout")

	args := []string{"index", "--db", dbPath, "--root", root, "--deps", "--method", method}
	if exclude != "" {
		args = append(args, "--exclude", exclude)
	}
	if tsconfig != "" {
		args = append(args, "--tsconfig", tsconfig)
		if tsCommand != "" && tsCommand != "ts-callgraph" {
			args = append(args, "--ts-command", tsCommand)
		}
	}

	logger.Info("running full index", "root", root)
	cmd := exec.Command(scoutBin, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
