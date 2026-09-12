package telemetry

import (
	"strings"
	"testing"
)

func TestSummarize(t *testing.T) {
	stats, err := Summarize(strings.NewReader(`{"command":"serve","success":true,"response_bytes":100,"estimated_tokens":25}
{"command":"serve","success":false,"response_bytes":40,"estimated_tokens":10}
`))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 2 || stats.Successful != 1 || stats.Failed != 1 || stats.ResponseBytes != 140 || stats.EstimatedTokens != 35 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestSummarizeRejectsMalformedLine(t *testing.T) {
	if _, err := Summarize(strings.NewReader("not-json\n")); err == nil {
		t.Fatal("expected malformed telemetry error")
	}
}
