package navigation

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// TelemetryStats summarizes recorded navigation decisions without exposing
// source contents. Classes are counted by classifier output.
type TelemetryStats struct {
	Total          int            `json:"total"`
	ProposedBlocks int            `json:"proposed_blocks"`
	Denied         int            `json:"denied"`
	Overrides      int            `json:"overrides"`
	Classes        map[string]int `json:"classes"`
}

// SummarizeTelemetry reads newline-delimited navigation telemetry and returns
// aggregate counts. A malformed non-empty line is an error so callers do not
// mistake partial telemetry for a complete report.
func SummarizeTelemetry(r io.Reader) (TelemetryStats, error) {
	stats := TelemetryStats{Classes: make(map[string]int)}
	scanner := bufio.NewScanner(r)
	for line := 1; scanner.Scan(); line++ {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var event RecordedDecision
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return TelemetryStats{}, fmt.Errorf("decode navigation telemetry line %d: %w", line, err)
		}
		stats.Total++
		if event.Proposed == "block" {
			stats.ProposedBlocks++
		}
		if !event.Allowed {
			stats.Denied++
		}
		if event.Overridden {
			stats.Overrides++
		}
		stats.Classes[event.Class]++
	}
	if err := scanner.Err(); err != nil {
		return TelemetryStats{}, fmt.Errorf("read navigation telemetry: %w", err)
	}
	return stats, nil
}
