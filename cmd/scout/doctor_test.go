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
