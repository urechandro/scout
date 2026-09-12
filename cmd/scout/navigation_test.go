package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/urechandro/scout/navigation"
)

func TestRunNavigationObserveEmitsDecision(t *testing.T) {
	var out bytes.Buffer
	err := runNavigationObserve(strings.NewReader(`{"file":"pkg/large.go","file_type":"source","file_lines":500}`), &out, navigation.Observer{Policy: navigation.Policy{MaxWholeFileLines: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"proposed":"block"`) {
		t.Fatalf("output = %s", out.String())
	}
}

func TestRunNavigationObserveRejectsInvalidEventWithoutOutput(t *testing.T) {
	var out bytes.Buffer
	err := runNavigationObserve(strings.NewReader(`{"file":""}`), &out, navigation.Observer{})
	if err == nil {
		t.Fatal("expected invalid event error")
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want empty", out.String())
	}
}
