package navigation

import "testing"

func TestEvaluateObserveNeverBlocks(t *testing.T) {
	d := (Policy{Enforcement: EnforcementObserve}).Evaluate(ReadEvent{File: "pkg/large.go", FileLines: 500})
	if d.Class != "whole-source" || d.Proposed != "block" {
		t.Fatalf("unexpected decision: %+v", d)
	}
}

func TestEvaluateClassifiesReadKinds(t *testing.T) {
	tests := []struct {
		name, file, typ, source, class string
	}{
		{"targeted", "pkg/a.go", "", "", "targeted-source"},
		{"generated", "gen/api.pb.go", "", "", "generated"},
		{"non-source", "docs/design.md", "", "", "non-source"},
		{"binary", "assets/logo", "binary", "", "non-source"},
		{"mcp", "pkg/a.go", "", "mcp", "exempt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := ReadEvent{File: tt.file, FileType: tt.typ, CommandSource: tt.source, FileLines: 500, StartLine: 10, EndLine: 20}
			if tt.name != "targeted" {
				e.StartLine, e.EndLine = 0, 0
			}
			d := (Policy{}).Evaluate(e)
			if d.Class != tt.class {
				t.Fatalf("class = %q, want %q (%+v)", d.Class, tt.class, d)
			}
		})
	}
}

func TestEvaluateUsesRangeLength(t *testing.T) {
	d := (Policy{MaxTargetedLines: 5}).Evaluate(ReadEvent{File: "pkg/a.ts", FileLines: 1000, StartLine: 10, EndLine: 16})
	if d.Proposed != "block" {
		t.Fatalf("proposed = %q, want block", d.Proposed)
	}
}
