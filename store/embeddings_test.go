package store

import (
	"math"
	"testing"
)

func TestEncodeDecodeEmbedding_RoundTrip(t *testing.T) {
	in := []float32{0, 1, -1, 0.5, math.MaxFloat32, math.SmallestNonzeroFloat32}
	out := DecodeEmbedding(EncodeEmbedding(in))
	if len(out) != len(in) {
		t.Fatalf("length mismatch: got %d, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("index %d: got %v, want %v", i, out[i], in[i])
		}
	}
}

func TestDecodeEmbedding_Empty(t *testing.T) {
	if v := DecodeEmbedding(nil); v != nil {
		t.Errorf("nil blob: got %v, want nil", v)
	}
	if v := DecodeEmbedding([]byte{}); v != nil {
		t.Errorf("empty blob: got %v, want nil", v)
	}
}

func TestUpsertEmbedding_AndLoad(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []Symbol{
		{ID: "pkg.Foo", Package: "pkg", Name: "Foo", Kind: "func", Signature: "func Foo()", File: "foo.go"},
		{ID: "pkg.Bar", Package: "pkg", Name: "Bar", Kind: "func", Signature: "func Bar()", File: "foo.go"},
	})

	vec := []float32{0.1, 0.2, 0.3}
	if err := s.UpsertEmbedding("pkg.Foo", "nomic-embed-text", vec); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	rows, err := s.LoadEmbeddings("nomic-embed-text")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "pkg.Foo" {
		t.Fatalf("unexpected load result: %+v", rows)
	}
	got := rows[0].Vector
	if len(got) != len(vec) {
		t.Fatalf("dim mismatch: got %d, want %d", len(got), len(vec))
	}
	for i := range vec {
		if got[i] != vec[i] {
			t.Errorf("index %d: got %v, want %v", i, got[i], vec[i])
		}
	}
}

func TestLoadEmbeddings_FiltersByModel(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []Symbol{
		{ID: "pkg.A", Package: "pkg", Name: "A", Kind: "func", Signature: "func A()", File: "a.go"},
		{ID: "pkg.B", Package: "pkg", Name: "B", Kind: "func", Signature: "func B()", File: "b.go"},
	})

	if err := s.UpsertEmbedding("pkg.A", "model-v1", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertEmbedding("pkg.B", "model-v2", []float32{0, 1}); err != nil {
		t.Fatal(err)
	}

	v1, _ := s.LoadEmbeddings("model-v1")
	v2, _ := s.LoadEmbeddings("model-v2")
	if len(v1) != 1 || v1[0].ID != "pkg.A" {
		t.Errorf("model-v1: %+v", v1)
	}
	if len(v2) != 1 || v2[0].ID != "pkg.B" {
		t.Errorf("model-v2: %+v", v2)
	}
}

func TestListUnembedded_SkipsCurrentModel(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []Symbol{
		{ID: "pkg.A", Package: "pkg", Name: "A", Kind: "func", Signature: "func A()", File: "a.go"},
		{ID: "pkg.B", Package: "pkg", Name: "B", Kind: "func", Signature: "func B()", File: "b.go"},
		{ID: "pkg.C", Package: "pkg", Name: "C", Kind: "func", Signature: "func C()", File: "c.go"},
	})
	if err := s.UpsertEmbedding("pkg.A", "current", []float32{1}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertEmbedding("pkg.B", "stale-model", []float32{1}); err != nil {
		t.Fatal(err)
	}

	missing, err := s.ListUnembedded("current", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]bool{}
	for _, sym := range missing {
		got[sym.ID] = true
	}
	if got["pkg.A"] {
		t.Errorf("pkg.A should be skipped (already embedded with current model)")
	}
	if !got["pkg.B"] {
		t.Errorf("pkg.B should be listed (stale model)")
	}
	if !got["pkg.C"] {
		t.Errorf("pkg.C should be listed (never embedded)")
	}
}

func TestCountEmbeddings(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []Symbol{
		{ID: "pkg.A", Package: "pkg", Name: "A", Kind: "func", Signature: "func A()", File: "a.go"},
		{ID: "pkg.B", Package: "pkg", Name: "B", Kind: "func", Signature: "func B()", File: "b.go"},
	})
	if err := s.UpsertEmbedding("pkg.A", "m", []float32{1}); err != nil {
		t.Fatal(err)
	}

	n, err := s.CountEmbeddings("m")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("got %d, want 1", n)
	}
}

func TestUpsertSymbol_PreservesEmbeddingOnNoOp(t *testing.T) {
	s := newTestStore(t)
	sym := Symbol{
		ID: "pkg.Foo", Package: "pkg", Name: "Foo", Kind: "func",
		Signature: "func Foo()", Docstring: "Foo does X.", File: "foo.go",
	}
	seedSymbols(t, s, []Symbol{sym})
	if err := s.UpsertEmbedding(sym.ID, "m", []float32{1, 2}); err != nil {
		t.Fatal(err)
	}

	// Re-upsert with identical name/signature/docstring (line numbers and
	// body may change, but those don't affect the vector).
	sym.LineStart = 99
	sym.Body = "func Foo() { /* new body */ }"
	if err := s.UpsertSymbol(sym); err != nil {
		t.Fatal(err)
	}

	rows, _ := s.LoadEmbeddings("m")
	if len(rows) != 1 || rows[0].Vector[0] != 1 || rows[0].Vector[1] != 2 {
		t.Errorf("expected embedding preserved, got %+v", rows)
	}
}

func TestUpsertSymbol_InvalidatesEmbeddingOnTextChange(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Symbol)
	}{
		{"signature changed", func(s *Symbol) { s.Signature = "func Foo(x int)" }},
		{"docstring changed", func(s *Symbol) { s.Docstring = "Foo does Y now." }},
		{"name changed", func(s *Symbol) { s.Name = "Bar" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			sym := Symbol{
				ID: "pkg.Foo", Package: "pkg", Name: "Foo", Kind: "func",
				Signature: "func Foo()", Docstring: "Foo does X.", File: "foo.go",
			}
			seedSymbols(t, s, []Symbol{sym})
			if err := s.UpsertEmbedding(sym.ID, "m", []float32{1, 2}); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&sym)
			if err := s.UpsertSymbol(sym); err != nil {
				t.Fatal(err)
			}
			rows, _ := s.LoadEmbeddings("m")
			if len(rows) != 0 {
				t.Errorf("expected embedding cleared, got %+v", rows)
			}
			missing, _ := s.ListUnembedded("m", 0)
			found := false
			for _, m := range missing {
				if m.ID == "pkg.Foo" {
					found = true
				}
			}
			if !found {
				t.Errorf("expected pkg.Foo in unembedded list")
			}
		})
	}
}

func TestSnapshotRestore_RestoresOnlyUnchanged(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []Symbol{
		{ID: "pkg.A", Package: "pkg", Name: "A", Kind: "func", Signature: "func A()", Docstring: "A.", File: "a.go"},
		{ID: "pkg.B", Package: "pkg", Name: "B", Kind: "func", Signature: "func B()", Docstring: "B.", File: "b.go"},
		{ID: "pkg.C", Package: "pkg", Name: "C", Kind: "func", Signature: "func C()", Docstring: "C.", File: "c.go"},
	})
	for _, id := range []string{"pkg.A", "pkg.B", "pkg.C"} {
		if err := s.UpsertEmbedding(id, "m", []float32{1, 2}); err != nil {
			t.Fatal(err)
		}
	}

	snaps, err := s.SnapshotEmbeddings()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snaps) != 3 {
		t.Fatalf("snapshot count: got %d, want 3", len(snaps))
	}

	if err := s.ResetIndex(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	// Re-seed: A unchanged, B has a new docstring, C removed entirely.
	seedSymbols(t, s, []Symbol{
		{ID: "pkg.A", Package: "pkg", Name: "A", Kind: "func", Signature: "func A()", Docstring: "A.", File: "a.go"},
		{ID: "pkg.B", Package: "pkg", Name: "B", Kind: "func", Signature: "func B()", Docstring: "B does Y.", File: "b.go"},
	})

	restored, err := s.RestoreEmbeddings(snaps)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored != 1 {
		t.Errorf("restored: got %d, want 1 (only A unchanged)", restored)
	}

	rows, _ := s.LoadEmbeddings("m")
	if len(rows) != 1 || rows[0].ID != "pkg.A" {
		t.Errorf("expected only pkg.A embedded, got %+v", rows)
	}

	missing, _ := s.ListUnembedded("m", 0)
	got := map[string]bool{}
	for _, m := range missing {
		got[m.ID] = true
	}
	if !got["pkg.B"] {
		t.Errorf("pkg.B should be unembedded after docstring change")
	}
	if got["pkg.A"] {
		t.Errorf("pkg.A should not be in unembedded list")
	}
}

func TestRestoreEmbeddings_EmptyIsNoop(t *testing.T) {
	s := newTestStore(t)
	n, err := s.RestoreEmbeddings(nil)
	if err != nil || n != 0 {
		t.Errorf("got n=%d err=%v, want 0,nil", n, err)
	}
}

func TestMigrate_AddsColumnsToExistingDB(t *testing.T) {
	// Simulate an older DB without the embedding columns by hand-creating
	// the legacy schema, then opening with New (which should ALTER it).
	dir := t.TempDir()
	path := dir + "/legacy.db"

	// Build a legacy DB by opening the modern store and dropping our columns.
	s, err := New(path)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := s.db.Exec(`
		DROP TABLE symbols;
		CREATE TABLE symbols (
			id         TEXT PRIMARY KEY,
			package    TEXT NOT NULL,
			name       TEXT NOT NULL,
			kind       TEXT NOT NULL,
			signature  TEXT NOT NULL,
			docstring  TEXT NOT NULL DEFAULT '',
			file       TEXT NOT NULL,
			line_start INTEGER NOT NULL,
			line_end   INTEGER NOT NULL,
			body       TEXT NOT NULL DEFAULT ''
		);
	`); err != nil {
		t.Fatalf("legacy reset: %v", err)
	}
	s.Close()

	// Re-open: migrate() should add the embedding columns.
	s2, err := New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	seedSymbols(t, s2, []Symbol{{ID: "pkg.A", Package: "pkg", Name: "A", Kind: "func", Signature: "func A()", File: "a.go"}})
	if err := s2.UpsertEmbedding("pkg.A", "m", []float32{1, 2, 3}); err != nil {
		t.Fatalf("upsert embedding after migrate: %v", err)
	}
	rows, err := s2.LoadEmbeddings("m")
	if err != nil || len(rows) != 1 {
		t.Fatalf("load after migrate: rows=%v err=%v", rows, err)
	}
}
