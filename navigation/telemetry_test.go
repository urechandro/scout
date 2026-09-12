package navigation

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecorderAppendsPrivacySafeJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".scout", "navigation.jsonl")
	r, err := NewRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	d := Decision{Event: ReadEvent{File: "pkg/auth.go", FileType: "source", FileLines: 500}, Class: "whole-source", Proposed: "block", Reason: "too large"}
	if err := r.Record(d); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Record(d); err == nil {
		t.Fatal("expected recording after close to fail")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("telemetry permissions = %o, want 600", got)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := bufio.NewScanner(f)
	if !s.Scan() {
		t.Fatal("expected one telemetry event")
	}
	var got RecordedDecision
	if err := json.Unmarshal(s.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got.Event.File != d.Event.File || got.Proposed != d.Proposed || got.Timestamp.IsZero() || time.Since(got.Timestamp) < 0 {
		t.Fatalf("recorded decision = %+v", got)
	}
	if s.Scan() {
		t.Fatal("expected exactly one telemetry event")
	}
}

func TestRecorderAppendsExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "navigation.jsonl")
	if err := os.WriteFile(path, []byte("prior\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Record(Decision{Class: "exempt"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) <= len("prior\n") {
		t.Fatal("expected event to append")
	}
}
