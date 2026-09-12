package navigation

import (
	"strings"
	"testing"
)

func TestSummarizeTelemetry(t *testing.T) {
	input := strings.Join([]string{
		`{"class":"whole-source","proposed":"block","allowed":false}`,
		`{"class":"whole-source","proposed":"block","allowed":true,"overridden":true}`,
		`{"class":"targeted-source","proposed":"allow","allowed":true}`,
		"",
	}, "\n")
	got, err := SummarizeTelemetry(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 3 || got.ProposedBlocks != 2 || got.Denied != 1 || got.Overrides != 1 {
		t.Fatalf("stats = %+v", got)
	}
	if got.Classes["whole-source"] != 2 || got.Classes["targeted-source"] != 1 {
		t.Fatalf("classes = %+v", got.Classes)
	}
}

func TestSummarizeTelemetryRejectsMalformedLine(t *testing.T) {
	_, err := SummarizeTelemetry(strings.NewReader("{}\nnot-json\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("error = %v", err)
	}
}
