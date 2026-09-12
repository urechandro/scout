package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteSessionStats(t *testing.T) {
	var out bytes.Buffer
	if err := writeSessionStats(&out, strings.NewReader(`{"command":"serve","success":true,"response_bytes":12}`+"\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"total":1`) || !strings.Contains(out.String(), `"successful":1`) {
		t.Fatalf("output = %s", out.String())
	}
}
