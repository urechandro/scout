package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestWriteStatsEmitsJSON(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "telemetry-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(`{"class":"whole-source","proposed":"block","allowed":false}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeStats(&out, file); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"total":1`) || !strings.Contains(out.String(), `"denied":1`) {
		t.Fatalf("output = %s", out.String())
	}
}
