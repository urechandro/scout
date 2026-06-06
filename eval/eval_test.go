package eval

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/urechandro/scout/indexer"
	"github.com/urechandro/scout/query"
	"github.com/urechandro/scout/store"
)

var (
	setupOnce sync.Once
	setupErr  error
	setupEng  *query.Engine
)

// setupIndex builds an index of scout's own source into a temp DB once per
// test binary invocation. The engine is shared across all golden fixtures.
func setupIndex(t *testing.T) *query.Engine {
	t.Helper()
	setupOnce.Do(func() {
		repoRoot, err := filepath.Abs("..")
		if err != nil {
			setupErr = err
			return
		}
		dbPath := filepath.Join(t.TempDir(), "golden.db")
		s, err := store.New(dbPath)
		if err != nil {
			setupErr = err
			return
		}
		idx := indexer.New(indexer.Config{
			Dir:              repoRoot,
			Patterns:         []string{"./..."},
			ExcludeGenerated: true,
			CallGraph:        indexer.CallGraphAST,
		}, s)
		if err := idx.Run(); err != nil {
			setupErr = err
			return
		}
		setupEng = query.New(s)
	})
	if setupErr != nil {
		t.Fatalf("index scout repo: %v", setupErr)
	}
	return setupEng
}

func TestGoldenFixtures(t *testing.T) {
	if testing.Short() {
		t.Skip("golden fixtures index scout's source — skipping under -short")
	}

	eng := setupIndex(t)

	fixtures, err := LoadFixtures("fixtures.yaml")
	if err != nil {
		t.Fatalf("load fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("fixtures.yaml has no entries")
	}

	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			result, err := RunFixture(eng, f)

			if f.WantError != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got success", f.WantError)
				}
				if !strings.Contains(err.Error(), f.WantError) {
					t.Errorf("error %q does not contain %q", err.Error(), f.WantError)
				}
				t.Logf("tool=%s err=%q", f.Tool, err.Error())
				return
			}

			if err != nil {
				t.Fatalf("%s: %v", f.Tool, err)
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal: %v", err)
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
