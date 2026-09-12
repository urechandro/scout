package navigation

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestObserverEvaluatesAndRecordsWithoutEnforcement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "navigation.jsonl")
	recorder, err := NewRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()

	decision, err := (Observer{
		Policy:   Policy{MaxWholeFileLines: 10},
		Recorder: recorder,
	}).Observe(ReadEvent{File: "pkg/large.go", FileType: "source", FileLines: 100})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Proposed != "block" {
		t.Fatalf("proposed = %q, want block", decision.Proposed)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var recorded RecordedDecision
	if err := json.NewDecoder(bufio.NewReader(f)).Decode(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.Event.File != "pkg/large.go" || recorded.Proposed != "block" {
		t.Fatalf("recorded = %+v", recorded)
	}
}

func TestObserverWithoutRecorderIsUsable(t *testing.T) {
	decision, err := (Observer{}).Observe(ReadEvent{File: "pkg/a.go", FileLines: 2})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Class != "whole-source" || decision.Proposed != "allow" {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestObserverReturnsTelemetryErrorWithoutChangingDecision(t *testing.T) {
	recorder := &Recorder{}
	decision, err := (Observer{Recorder: recorder}).Observe(ReadEvent{File: "pkg/a.go", FileLines: 2})
	if err == nil {
		t.Fatal("expected telemetry error")
	}
	if decision.Proposed != "allow" {
		t.Fatalf("proposed = %q, want allow", decision.Proposed)
	}
}
