package embedder

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/urechandro/scout/store"
)

// fakeClient lets tests script Embed responses deterministically.
type fakeClient struct {
	model string
	// embed is called for each batch; first arg is the inputs, second is the
	// 1-indexed batch number so a test can fail a specific batch.
	embed func(inputs []string, batchNum int) ([][]float32, error)
	calls int
}

func (f *fakeClient) Model() string { return f.model }
func (f *fakeClient) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	return f.embed(texts, f.calls)
}

func newRunStore(t *testing.T, syms []store.Symbol) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "run.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	for _, sym := range syms {
		if err := s.UpsertSymbol(sym); err != nil {
			t.Fatalf("upsert %s: %v", sym.ID, err)
		}
	}
	return s
}

func sym(id, sig, doc string) store.Symbol {
	return store.Symbol{
		ID: id, Package: "pkg", Name: id, Kind: "func",
		Signature: sig, Docstring: doc, File: "f.go",
	}
}

func TestRun_EmbedsEverything(t *testing.T) {
	syms := []store.Symbol{sym("A", "func A()", ""), sym("B", "func B()", "B does B")}
	s := newRunStore(t, syms)
	c := &fakeClient{
		model: "m",
		embed: func(inputs []string, _ int) ([][]float32, error) {
			out := make([][]float32, len(inputs))
			for i := range inputs {
				out[i] = []float32{float32(i)}
			}
			return out, nil
		},
	}

	stats, err := Run(context.Background(), s, c, Options{BatchSize: 10})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Embedded != 2 || stats.Failed != 0 {
		t.Errorf("stats: %+v", stats)
	}
	n, _ := s.CountEmbeddings("m")
	if n != 2 {
		t.Errorf("count: got %d, want 2", n)
	}
}

func TestRun_BatchesByOptionSize(t *testing.T) {
	var syms []store.Symbol
	for i := range 7 {
		syms = append(syms, sym(fmt.Sprintf("S%d", i), fmt.Sprintf("func S%d()", i), ""))
	}
	s := newRunStore(t, syms)
	var batchSizes []int
	c := &fakeClient{
		model: "m",
		embed: func(inputs []string, _ int) ([][]float32, error) {
			batchSizes = append(batchSizes, len(inputs))
			out := make([][]float32, len(inputs))
			for i := range inputs {
				out[i] = []float32{1}
			}
			return out, nil
		},
	}

	if _, err := Run(context.Background(), s, c, Options{BatchSize: 3}); err != nil {
		t.Fatal(err)
	}
	want := []int{3, 3, 1}
	if fmt.Sprint(batchSizes) != fmt.Sprint(want) {
		t.Errorf("batch sizes: got %v, want %v", batchSizes, want)
	}
}

func TestRun_SkipsBadBatchAndContinues(t *testing.T) {
	var syms []store.Symbol
	for i := range 4 {
		syms = append(syms, sym(fmt.Sprintf("S%d", i), fmt.Sprintf("func S%d()", i), ""))
	}
	s := newRunStore(t, syms)
	c := &fakeClient{
		model: "m",
		embed: func(inputs []string, batchNum int) ([][]float32, error) {
			if batchNum == 1 {
				return nil, errors.New("first batch tantrum")
			}
			out := make([][]float32, len(inputs))
			for i := range inputs {
				out[i] = []float32{1}
			}
			return out, nil
		},
	}

	stats, err := Run(context.Background(), s, c, Options{BatchSize: 2})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Failed != 2 || stats.Embedded != 2 {
		t.Errorf("stats: %+v", stats)
	}
}

func TestRun_LimitCapsWork(t *testing.T) {
	var syms []store.Symbol
	for i := range 10 {
		syms = append(syms, sym(fmt.Sprintf("S%d", i), fmt.Sprintf("func S%d()", i), ""))
	}
	s := newRunStore(t, syms)
	c := &fakeClient{
		model: "m",
		embed: func(inputs []string, _ int) ([][]float32, error) {
			out := make([][]float32, len(inputs))
			for i := range inputs {
				out[i] = []float32{1}
			}
			return out, nil
		},
	}

	stats, err := Run(context.Background(), s, c, Options{Limit: 3, BatchSize: 10})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Considered != 3 || stats.Embedded != 3 {
		t.Errorf("stats: %+v", stats)
	}
}

func TestRun_EmptyIsNoop(t *testing.T) {
	s := newRunStore(t, nil)
	c := &fakeClient{model: "m", embed: func([]string, int) ([][]float32, error) {
		t.Fatal("client should not be called when no symbols are unembedded")
		return nil, nil
	}}
	stats, err := Run(context.Background(), s, c, Options{})
	if err != nil || stats.Considered != 0 {
		t.Errorf("expected no-op, got stats=%+v err=%v", stats, err)
	}
}

func TestEmbeddingText(t *testing.T) {
	if got := EmbeddingText(store.Symbol{Signature: "func A()"}); got != "func A()" {
		t.Errorf("no doc: %q", got)
	}
	if got := EmbeddingText(store.Symbol{Signature: "func A()", Docstring: "does A"}); got != "func A()\ndoes A" {
		t.Errorf("with doc: %q", got)
	}
}
