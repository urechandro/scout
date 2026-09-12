package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urechandro/scout/store"
)

func TestRunDoctorWarnsWhenTelemetryIsUnconfigured(t *testing.T) {
	var out bytes.Buffer
	if err := runDoctor(t.TempDir(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"status":"warn"`) {
		t.Fatalf("output = %s", out.String())
	}
}

func TestRunDoctorReportsRepositoryAndSchema(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, ".scout", "index.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	var out bytes.Buffer
	if err := runDoctor(root, &out); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Checks []doctorCheck `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, check := range payload.Checks {
		statuses[check.Name] = check.Status
	}
	if statuses["repository.root"] != "ok" || statuses["database.schema"] != "ok" {
		t.Fatalf("statuses = %#v", statuses)
	}
}

func TestRunDoctorReportsLanguageManifest(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"example"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runDoctor(root, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"name":"language.tooling"`) || !strings.Contains(out.String(), `"status":"ok"`) {
		t.Fatalf("output = %s", out.String())
	}
}

func TestRunDoctorReportsMCPAndClientGuidance(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"mcpServers":{"scout":{"args":["--watch"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Scout\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runDoctor(root, &out); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{`"name":"mcp.config"`, `"name":"client.guidance"`, `"name":"watcher.config"`} {
		if !strings.Contains(out.String(), name) {
			t.Fatalf("missing %s in %s", name, out.String())
		}
	}
}
