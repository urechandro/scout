// Package navigation classifies file reads for Scout's optional navigation
// telemetry. It never performs I/O and observe mode never blocks a read.
package navigation

import (
	"path/filepath"
	"strings"
)

const (
	EnforcementObserve = "observe"
	EnforcementBlock   = "block"
	DefaultWholeLines  = 350
	DefaultTargetLines = 200
	BlockGuidance      = "Broad source read blocked by Scout. Use get_relevant_context for discovery, then get_body or get_flow for exact symbols. A targeted source range remains available for editing context."
)

// ReadEvent is the structured representation of one attempted file read.
type ReadEvent struct {
	File          string `json:"file"`
	FileType      string `json:"file_type"`
	FileLines     int    `json:"file_lines"`
	StartLine     int    `json:"start_line,omitempty"`
	EndLine       int    `json:"end_line,omitempty"`
	CommandSource string `json:"command_source,omitempty"`
	Override      bool   `json:"override,omitempty"`
}

// Decision records the proposed policy outcome and why it was chosen.
type Decision struct {
	Event      ReadEvent `json:"event"`
	Class      string    `json:"class"`
	Proposed   string    `json:"proposed"`
	Allowed    bool      `json:"allowed"`
	Overridden bool      `json:"overridden,omitempty"`
	Reason     string    `json:"reason"`
	Guidance   string    `json:"guidance,omitempty"`
}

// Policy is a read classifier configuration.
type Policy struct {
	Enforcement       string
	MaxWholeFileLines int
	MaxTargetedLines  int
}

func (p Policy) withDefaults() Policy {
	if p.Enforcement == "" {
		p.Enforcement = EnforcementObserve
	}
	if p.MaxWholeFileLines <= 0 {
		p.MaxWholeFileLines = DefaultWholeLines
	}
	if p.MaxTargetedLines <= 0 {
		p.MaxTargetedLines = DefaultTargetLines
	}
	return p
}

// Evaluate classifies a read. Proposed may be "allow", "observe", or
// "block"; this package does not execute the proposed action.
func (p Policy) Evaluate(event ReadEvent) Decision {
	p = p.withDefaults()
	d := Decision{Event: event, Proposed: "allow", Allowed: true}
	if isExempt(event) {
		d.Class, d.Reason = "exempt", "Scout or hook-generated operation"
		return d
	}
	if generated(event) {
		d.Class, d.Reason = "generated", "Generated source is exempt from broad-read policy"
		return d
	}
	if !source(event) {
		d.Class, d.Reason = "non-source", "Non-source files are outside navigation policy"
		return d
	}
	targeted := event.StartLine > 0 || event.EndLine > 0
	lines := event.FileLines
	if targeted {
		d.Class = "targeted-source"
		if event.EndLine >= event.StartLine && event.StartLine > 0 {
			lines = event.EndLine - event.StartLine + 1
		}
		if lines > p.MaxTargetedLines {
			d.Proposed, d.Reason = "block", "Targeted source range exceeds configured line limit"
			d.applyEnforcement(p, event.Override)
			d.setGuidance()
		}
		return d
	}
	d.Class = "whole-source"
	if lines > p.MaxWholeFileLines {
		d.Proposed, d.Reason = "block", "Whole source file exceeds configured line limit"
		d.applyEnforcement(p, event.Override)
		d.setGuidance()
	}
	return d
}

func (d *Decision) setGuidance() {
	if !d.Allowed {
		d.Guidance = BlockGuidance
	}
}

func (d *Decision) applyEnforcement(p Policy, override bool) {
	d.Allowed = p.Enforcement != EnforcementBlock || override
	d.Overridden = override && p.Enforcement == EnforcementBlock
	d.setGuidance()
}

func isExempt(e ReadEvent) bool {
	s := strings.ToLower(e.CommandSource)
	return strings.Contains(s, "scout") || strings.Contains(s, "hook") || strings.Contains(s, "mcp")
}

func generated(e ReadEvent) bool {
	p := strings.ToLower(filepath.ToSlash(e.File))
	b := strings.ToLower(filepath.Base(e.File))
	return strings.Contains(p, "/generated/") || strings.Contains(p, "/gen/") || strings.HasSuffix(b, ".pb.go") || strings.HasSuffix(b, "_generated.go")
}

func source(e ReadEvent) bool {
	if strings.EqualFold(e.FileType, "binary") || strings.EqualFold(e.FileType, "asset") {
		return false
	}
	switch strings.ToLower(filepath.Ext(e.File)) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".proto", ".rs", ".py", ".java", ".c", ".cc", ".cpp", ".h", ".hpp", ".cs", ".rb", ".swift", ".kt":
		return true
	default:
		return strings.EqualFold(e.FileType, "source")
	}
}
