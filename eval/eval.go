// Package eval runs fixture-driven retrieval tests against scout's tools.
// The harness asserts that named symbols surface in tool output and that
// responses stay within byte budgets — no LLM involved.
package eval

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/urechandro/scout/query"
)

// Fixture is one golden test case: call one tool with args and assert
// substring presence + byte budget on the JSON-marshaled response.
//
// If WantError is set, the fixture instead expects the tool call to return an
// error whose message contains the substring. Response-shape assertions are
// skipped in that case.
type Fixture struct {
	Name           string         `yaml:"name"`
	Tool           string         `yaml:"tool"`
	Args           map[string]any `yaml:"args"`
	MustInclude    []string       `yaml:"must_include,omitempty"`
	MustNotInclude []string       `yaml:"must_not_include,omitempty"`
	MaxBytes       int            `yaml:"max_bytes,omitempty"`
	WantError      string         `yaml:"want_error,omitempty"`
}

// LoadFixtures parses a YAML fixture file.
func LoadFixtures(path string) ([]Fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var fixtures []Fixture
	if err := yaml.Unmarshal(data, &fixtures); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return fixtures, nil
}

// RunFixture dispatches one fixture to the matching engine method.
func RunFixture(eng *query.Engine, f Fixture) (any, error) {
	switch f.Tool {
	case "get_relevant_context":
		return eng.GetRelevantContext(query.ContextRequest{
			Task:    str(f.Args, "query"),
			Verbose: boolean(f.Args, "verbose"),
		})
	case "get_body":
		return eng.GetBody(str(f.Args, "symbol_id"))
	case "get_flow":
		return eng.GetFlow(str(f.Args, "symbol_id"))
	case "get_callers":
		return eng.GetCallers(str(f.Args, "symbol_id"))
	case "get_callees":
		return eng.GetCallees(str(f.Args, "symbol_id"))
	case "get_impact":
		return eng.GetImpact(str(f.Args, "symbol_id"))
	case "get_pattern":
		return eng.GetPattern(str(f.Args, "task"))
	case "get_simplest_rpc":
		return eng.GetSimplestRPC(str(f.Args, "service"), intArg(f.Args, "limit"))
	case "get_unimplemented":
		return eng.GetUnimplemented(str(f.Args, "service"))
	case "get_conventions":
		return eng.GetConventions(str(f.Args, "topic"))
	case "get_viz":
		return eng.GetViz(str(f.Args, "symbol_id"), str(f.Args, "direction"), intArg(f.Args, "depth"))
	}
	return nil, fmt.Errorf("unknown tool: %s", f.Tool)
}

func str(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func boolean(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

func intArg(m map[string]any, key string) int {
	if v, ok := m[key].(int); ok {
		return v
	}
	return 0
}
