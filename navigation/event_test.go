package navigation

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestObserveJSONDecodesAndRecordsEvent(t *testing.T) {
	recorder, err := NewRecorder(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()

	payload, err := json.Marshal(ReadEvent{File: "pkg/auth.go", FileType: "source", FileLines: 400, CommandSource: "read"})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := (Observer{Policy: Policy{MaxWholeFileLines: 100}, Recorder: recorder}).ObserveJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Class != "whole-source" || decision.Proposed != "block" {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestDecodeReadEventRejectsMalformedAndTrailingInput(t *testing.T) {
	for _, payload := range []string{"", "{", `{"file":"pkg/a.go"}{"file":"pkg/b.go"}`, `{"file":""}`} {
		t.Run(payload, func(t *testing.T) {
			if _, err := DecodeReadEvent([]byte(payload)); err == nil {
				t.Fatal("expected decode error")
			}
		})
	}
}

func TestObserveJSONDoesNotRecordInvalidInput(t *testing.T) {
	recorder, err := NewRecorder(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	if _, err := (Observer{Recorder: recorder}).ObserveJSON([]byte(`{"file":""}`)); err == nil || !strings.Contains(err.Error(), "file is required") {
		t.Fatalf("error = %v", err)
	}
	if got := recorder.file; got == nil {
		t.Fatal("recorder unexpectedly closed")
	}
}
