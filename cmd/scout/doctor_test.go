package main

import (
	"bytes"
	"strings"
	"testing"
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
