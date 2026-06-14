package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urechandro/scout/embedder"
	"github.com/urechandro/scout/indexer"
	"github.com/urechandro/scout/query"
	"github.com/urechandro/scout/store"
)

// TestSemanticFixtures runs the meaning-based fixtures with an Ollama-backed
// engine. It is skipped unless both env vars are set, because (a) it needs a
// live Ollama server reachable on the network and (b) the embedder pass adds
// several seconds to test runtime.
//
// To run locally:
//
//	ollama serve
//	ollama pull nomic-embed-text
//	SCOUT_OLLAMA_HOST=http://localhost:11434 \
//	SCOUT_OLLAMA_MODEL=nomic-embed-text \
//	  go test ./eval -run TestSemanticFixtures -v
//
// The test fails closed on must_include misses so the recall delta against
// the FTS-only baseline (TestGoldenFixtures) is visible at a glance.
func TestSemanticFixtures(t *testing.T) {
	host := os.Getenv("SCOUT_OLLAMA_HOST")
	model := os.Getenv("SCOUT_OLLAMA_MODEL")
	if host == "" || model == "" {
		t.Skip("SCOUT_OLLAMA_HOST and SCOUT_OLLAMA_MODEL not set — skipping semantic fixtures")
	}
	if testing.Short() {
		t.Skip("semantic fixtures index scout's source — skipping under -short")
	}

	// Quick reachability + model-installed probe so a misconfigured run
	// fails with a clear message instead of N per-fixture timeouts.
	probe, err := embedder.ProbeOllama(context.Background(), host, model)
	if err != nil {
		t.Fatalf("probe ollama: %v", err)
	}
	if !probe.Reachable {
		t.Fatalf("ollama not reachable at %s — start the server or unset SCOUT_OLLAMA_HOST", host)
	}
	if !probe.ModelInstalled {
		t.Fatalf("model %q not installed in ollama — run `ollama pull %s`", model, model)
	}

	eng := setupSemanticIndex(t, host, model)

	fixtures, err := LoadFixtures("semantic_fixtures.yaml")
	if err != nil {
		t.Fatalf("load semantic fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("semantic_fixtures.yaml has no entries")
	}

	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			result, err := RunFixture(eng, f)
			if err != nil {
				t.Fatalf("%s: %v", f.Tool, err)
			}
			data, mErr := json.Marshal(result)
			if mErr != nil {
				t.Fatalf("marshal: %v", mErr)
			}
			t.Logf("tool=%s bytes=%d", f.Tool, len(data))
			if f.MaxBytes > 0 && len(data) > f.MaxBytes {
				t.Errorf("response %d bytes exceeds max_bytes %d", len(data), f.MaxBytes)
			}
			for _, want := range f.MustInclude {
				if !bytes.Contains(data, []byte(want)) {
					t.Errorf("must_include not in response: %q", want)
				}
			}
			for _, unwanted := range f.MustNotInclude {
				if bytes.Contains(data, []byte(unwanted)) {
					t.Errorf("must_not_include in response: %q", unwanted)
				}
			}
		})
	}
}

// setupSemanticIndex builds an index of scout's own source, runs the embedder
// pass against the configured Ollama model, and returns an engine with
// Phase 3 wired up. Distinct from setupIndex (FTS-only) so the two test
// flavors don't fight over the singleton.
func setupSemanticIndex(t *testing.T, host, model string) *query.Engine {
	t.Helper()
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "semantic.db")
	s, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	idx := indexer.New(indexer.Config{
		Dir:              repoRoot,
		Patterns:         []string{"./..."},
		ExcludeGenerated: true,
		CallGraph:        indexer.CallGraphAST,
	}, s)
	if err := idx.Run(); err != nil {
		t.Fatalf("index: %v", err)
	}

	client := embedder.NewOllamaClient(host, model)
	// Use a generous batch size and an explicit context with no per-test
	// timeout — embedding the whole scout corpus through nomic-embed-text
	// on a laptop takes ~30s.
	stats, err := embedder.Run(context.Background(), s, client, embedder.Options{
		BatchSize: 64,
		Logger:    testLogger{t},
	})
	if err != nil {
		t.Fatalf("embedder run: %v", err)
	}
	if stats.Embedded == 0 {
		t.Fatalf("embedder pass produced 0 vectors (considered=%d failed=%d) — check model %q",
			stats.Considered, stats.Failed, model)
	}
	t.Logf("embedded %d symbols (failed=%d)", stats.Embedded, stats.Failed)

	return query.New(s, query.Options{Embedder: client})
}

// testLogger routes embedder.Run progress lines through testing.T.Logf so
// failures show what the pass was doing when something blew up.
type testLogger struct{ t *testing.T }

func (l testLogger) Printf(format string, args ...any) {
	l.t.Helper()
	l.t.Logf(strings.TrimSpace(format), args...)
}
