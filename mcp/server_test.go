package mcp

import (
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urechandro/scout/query"
	"github.com/urechandro/scout/store"
)

// newTestServer builds a Server over an in-memory index seeded with one
// symbol, so handler tests exercise real engine paths.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	sym := store.Symbol{
		ID:        "github.com/acme/svc/internal/auth.ValidateToken",
		Package:   "github.com/acme/svc/internal/auth",
		Name:      "ValidateToken",
		Kind:      "func",
		Signature: "func ValidateToken(tok string) error",
		File:      "/home/u/proj/internal/auth/auth.go",
		LineStart: 10,
		LineEnd:   20,
		Body:      "func ValidateToken(tok string) error { return nil }",
	}
	if err := s.UpsertSymbol(sym); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	e := query.New(s, query.Options{
		ModulePrefix: "github.com/acme/svc",
		RootDir:      "/home/u/proj",
	})
	return New(slog.New(slog.DiscardHandler), e, s)
}

// Both of the bugs found in the 2026-07-12 benchmarks lived in these
// handlers: models guess param names (pattern/id/task), and the old code
// either silently dropped them or leaked raw FTS5 errors. These tests lock
// in the contract: aliases resolve, empty params produce an error that
// names the expected parameter.

func TestSymbolIDHandlers_AliasAndValidation(t *testing.T) {
	srv := newTestServer(t)

	handlers := map[string]func(json.RawMessage) (any, error){
		"get_body":    srv.callGetBody,
		"get_callers": srv.callGetCallers,
		"get_callees": srv.callGetCallees,
		"get_flow":    srv.callGetFlow,
		"get_impact":  srv.callGetImpact,
	}

	for name, h := range handlers {
		t.Run(name+"/symbol_id", func(t *testing.T) {
			if _, err := h(json.RawMessage(`{"symbol_id": "internal/auth.ValidateToken"}`)); err != nil {
				t.Errorf("symbol_id form failed: %v", err)
			}
		})
		t.Run(name+"/id_alias", func(t *testing.T) {
			if _, err := h(json.RawMessage(`{"id": "internal/auth.ValidateToken"}`)); err != nil {
				t.Errorf("id alias failed: %v", err)
			}
		})
		t.Run(name+"/empty", func(t *testing.T) {
			_, err := h(json.RawMessage(`{}`))
			if err == nil {
				t.Fatal("empty args should error")
			}
			if !strings.Contains(err.Error(), "symbol_id") {
				t.Errorf("error should name the expected parameter, got: %v", err)
			}
		})
	}
}

func TestGetRelevantContext_QueryAliasAndValidation(t *testing.T) {
	srv := newTestServer(t)

	for _, arg := range []string{
		`{"query": "ValidateToken"}`,
		`{"task": "ValidateToken"}`, // pre-rename alias
	} {
		res, err := srv.callGetRelevantContext(json.RawMessage(arg))
		if err != nil {
			t.Fatalf("callGetRelevantContext(%s): %v", arg, err)
		}
		// Brief mode renders as plain text with elided IDs.
		text, ok := res.(string)
		if !ok {
			t.Fatalf("brief result should be a rendered string, got %T", res)
		}
		if !strings.Contains(text, "internal/auth.ValidateToken") {
			t.Errorf("rendered text missing elided ID:\n%s", text)
		}
		if strings.Contains(text, "github.com/acme") {
			t.Errorf("rendered text leaked full ID:\n%s", text)
		}
	}

	// Verbose mode returns the struct for JSON rendering.
	res, err := srv.callGetRelevantContext(json.RawMessage(`{"query": "ValidateToken", "verbose": true}`))
	if err != nil {
		t.Fatalf("verbose: %v", err)
	}
	if _, ok := res.(string); ok {
		t.Error("verbose result should not be pre-rendered text")
	}

	_, err = srv.callGetRelevantContext(json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "query") {
		t.Errorf("empty args should error naming \"query\", got: %v", err)
	}
}

func TestGetConventions_TopicAliasesAndValidation(t *testing.T) {
	srv := newTestServer(t)

	for _, arg := range []string{
		`{"topic": "ValidateToken"}`,
		`{"pattern": "ValidateToken"}`,
		`{"query": "ValidateToken"}`,
	} {
		if _, err := srv.callGetConventions(json.RawMessage(arg)); err != nil {
			t.Errorf("callGetConventions(%s): %v", arg, err)
		}
	}

	_, err := srv.callGetConventions(json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "topic") {
		t.Errorf("empty args should error naming \"topic\", got: %v", err)
	}
	// The all-stop-words case must return guidance, never a raw FTS5 error.
	_, err = srv.callGetConventions(json.RawMessage(`{"topic": "the a of"}`))
	if err == nil {
		t.Fatal("stop-word-only topic should error")
	}
	if strings.Contains(err.Error(), "fts5") || strings.Contains(err.Error(), "SQL") {
		t.Errorf("leaked a raw FTS error: %v", err)
	}
}

func TestGetPattern_TaskAliasAndValidation(t *testing.T) {
	srv := newTestServer(t)

	for _, arg := range []string{
		`{"task": "ValidateToken"}`,
		`{"query": "ValidateToken"}`,
	} {
		if _, err := srv.callGetPattern(json.RawMessage(arg)); err != nil {
			t.Errorf("callGetPattern(%s): %v", arg, err)
		}
	}

	_, err := srv.callGetPattern(json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "task") {
		t.Errorf("empty args should error naming \"task\", got: %v", err)
	}
}
