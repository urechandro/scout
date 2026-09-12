package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type initTestLogger struct{}

func (initTestLogger) Info(string, ...any) {}

func TestUpdateCodexConfigPreservesContentAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	codexDir := filepath.Join(root, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(codexDir, "config.toml")
	original := "approval_policy = \"on-request\"\n\n[mcp_servers.other]\ncommand = \"other\"\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := updateCodexConfig(root, filepath.Join(root, ".scout", "index.db"), "/opt/scout\"bin", "tsconfig.json", "node /tmp/ts-callgraph.js", true, initTestLogger{}); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(first)
	if !strings.HasPrefix(content, original) {
		t.Fatalf("user content was not preserved:\n%s", content)
	}
	for _, want := range []string{
		"[mcp_servers.scout]",
		`args = ["serve", "--db", "` + filepath.Join(root, ".scout", "index.db") + `", "--watch", "` + root + `", "--tsconfig", "tsconfig.json", "--ts-command", "node /tmp/ts-callgraph.js"]`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("config missing %q:\n%s", want, content)
		}
	}

	if err := updateCodexConfig(root, filepath.Join(root, ".scout", "index.db"), "/opt/scout\"bin", "tsconfig.json", "node /tmp/ts-callgraph.js", true, initTestLogger{}); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != content {
		t.Fatalf("repeated initialization changed config\nfirst:\n%s\nsecond:\n%s", content, second)
	}
	if strings.Count(string(second), "[mcp_servers.scout]") != 1 {
		t.Fatal("duplicate Scout server entries")
	}
}

func TestUpdateCodexConfigCreatesProjectConfig(t *testing.T) {
	root := t.TempDir()
	if err := updateCodexConfig(root, "/tmp/index.db", "scout", "", "ts-callgraph", false, initTestLogger{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "[mcp_servers.scout]") || !strings.Contains(content, `args = ["serve", "--db", "/tmp/index.db"]`) {
		t.Fatalf("unexpected generated config:\n%s", content)
	}
}

func TestUpdateAgentGuidanceMigratesLegacyMarkers(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "CLAUDE.md")
	original := "# Keep this\n\n" + legacyScoutStart + "\nold guidance\n" + legacyScoutEnd + "\n\n# Keep this too\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := updateAgentGuidance(root, agentClaude, initTestLogger{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if strings.Contains(content, "old guidance") || strings.Contains(content, legacyScoutStart) {
		t.Fatalf("legacy block was not migrated:\n%s", content)
	}
	if !strings.Contains(content, claudeMDBlock) || !strings.Contains(content, "# Keep this too") {
		t.Fatalf("managed or user content missing:\n%s", content)
	}
}
