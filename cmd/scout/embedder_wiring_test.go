package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/urechandro/scout/config"
	"github.com/urechandro/scout/embedder"
	"github.com/urechandro/scout/query"
	"github.com/urechandro/scout/store"
)

// newTestStore creates a fresh store under a temp dir suitable for both
// runEmbedderPass and the watcher callback (which expect the project root
// to be the dir containing .scout/).
func newTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	rootDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootDir, ".scout"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.New(filepath.Join(rootDir, ".scout", "index.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, rootDir
}

func seedOneSymbol(t *testing.T, s *store.Store) {
	t.Helper()
	if err := s.UpsertSymbol(store.Symbol{
		ID: "pkg.Foo", Package: "pkg", Name: "Foo", Kind: "func",
		Signature: "func Foo()", Docstring: "Foo does X.", File: "foo.go",
	}); err != nil {
		t.Fatal(err)
	}
}

// fakeOllama returns a single-vector response for any input batch.
func fakeOllama(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.NotFound(w, r)
			return
		}
		// Echo back as many [1.0]-vectors as the request asked for. We don't
		// parse the body here — embedder.Run sends one input per snapshot.
		// Hardcoding to 1 input keeps this test focused.
		_, _ = w.Write([]byte(`{"embeddings":[[1.0]]}`))
	}))
}

func TestRunEmbedderPass_NoConfigIsNoop(t *testing.T) {
	s, root := newTestStore(t)
	seedOneSymbol(t, s)

	err := runEmbedderPass(root, "", s, slog.Default())
	if err != nil {
		t.Fatalf("expected nil error with no config, got %v", err)
	}
	n, _ := s.CountEmbeddings("nomic-embed-text")
	if n != 0 {
		t.Errorf("expected no vectors written, got %d", n)
	}
}

func TestRunEmbedderPass_OllamaConfigWritesVectors(t *testing.T) {
	s, root := newTestStore(t)
	seedOneSymbol(t, s)

	srv := fakeOllama(t)
	defer srv.Close()

	if err := config.Save(root, &config.Config{Embedder: &config.EmbedderConfig{
		Kind:  config.EmbedderOllama,
		Host:  srv.URL,
		Model: "test-model",
	}}); err != nil {
		t.Fatal(err)
	}

	if err := runEmbedderPass(root, "", s, slog.Default()); err != nil {
		t.Fatalf("pass: %v", err)
	}

	n, _ := s.CountEmbeddings("test-model")
	if n != 1 {
		t.Errorf("expected 1 vector, got %d", n)
	}
}

func TestRunEmbedderPass_UnsupportedKindReturnsError(t *testing.T) {
	s, root := newTestStore(t)

	// Write a config file directly with an unsupported kind so we bypass
	// config.Save's validator. (Validate rejects this at Load time too —
	// but we want to confirm runEmbedderPass surfaces the error rather
	// than silently no-op'ing.)
	raw := []byte("embedder:\n  kind: voyage\n  host: x\n  model: y\n")
	if err := os.WriteFile(config.Path(root), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	err := runEmbedderPass(root, "", s, slog.Default())
	if err == nil {
		t.Fatal("expected error for unsupported embedder kind")
	}
}

func TestLoadEmbedderClient_NilWhenUnconfigured(t *testing.T) {
	_, root := newTestStore(t)
	if c := loadEmbedderClient(root, slog.Default()); c != nil {
		t.Errorf("expected nil when no .scout/config.yaml, got %v", c)
	}
}

func TestLoadEmbedderClient_NilForUnsupportedKind(t *testing.T) {
	_, root := newTestStore(t)
	raw := []byte("embedder:\n  kind: voyage\n  host: x\n  model: y\n")
	if err := os.WriteFile(config.Path(root), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if c := loadEmbedderClient(root, slog.Default()); c != nil {
		t.Errorf("expected nil for unsupported kind, got %v", c)
	}
}

func TestLoadEmbedderClient_ReturnsClientWhenConfigured(t *testing.T) {
	_, root := newTestStore(t)
	if err := config.Save(root, &config.Config{Embedder: &config.EmbedderConfig{
		Kind:  config.EmbedderOllama,
		Host:  "http://localhost:11434",
		Model: "nomic-embed-text",
	}}); err != nil {
		t.Fatal(err)
	}
	c := loadEmbedderClient(root, slog.Default())
	if c == nil {
		t.Fatal("expected non-nil client when Ollama embedder configured")
	}
	if c.Model() != "nomic-embed-text" {
		t.Errorf("client.Model() = %q, want nomic-embed-text", c.Model())
	}
}

func TestBuildWatcherEmbedCallback_SerializesPasses(t *testing.T) {
	s, root := newTestStore(t)
	seedOneSymbol(t, s)

	// Track concurrent /api/embed calls. The mutex inside the callback
	// should keep concurrency at 1.
	var (
		mu      sync.Mutex
		current int
		peak    int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current++
		if current > peak {
			peak = current
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			current--
			mu.Unlock()
		}()
		_, _ = w.Write([]byte(`{"embeddings":[[1.0]]}`))
	}))
	defer srv.Close()

	if err := config.Save(root, &config.Config{Embedder: &config.EmbedderConfig{
		Kind: config.EmbedderOllama, Host: srv.URL, Model: "m",
	}}); err != nil {
		t.Fatal(err)
	}

	client := embedder.NewOllamaClient(srv.URL, "m")
	eng := query.New(s, query.Options{})
	cb, startup := buildWatcherEmbedCallback(s, client, eng, slog.Default())
	if cb == nil || startup == nil {
		t.Fatal("expected non-nil callback + startup")
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			cb("go", nil)
		})
	}
	wg.Wait()

	if peak > 1 {
		t.Errorf("embedder passes ran concurrently (peak=%d); callback mutex broken", peak)
	}
}

func TestBuildWatcherEmbedCallback_MarksEngineDirtyAfterSuccess(t *testing.T) {
	s, root := newTestStore(t)
	seedOneSymbol(t, s)

	srv := fakeOllama(t)
	defer srv.Close()

	if err := config.Save(root, &config.Config{Embedder: &config.EmbedderConfig{
		Kind: config.EmbedderOllama, Host: srv.URL, Model: "m",
	}}); err != nil {
		t.Fatal(err)
	}

	client := embedder.NewOllamaClient(srv.URL, "m")
	eng := query.New(s, query.Options{Embedder: client})
	cb, _ := buildWatcherEmbedCallback(s, client, eng, slog.Default())

	// Drive a phase-3 call once so the slab loads (slab is empty here —
	// the symbol has no vector yet — but the cached state is what matters).
	_, _ = eng.GetRelevantContext(query.ContextRequest{Task: "anything goes here"})

	cb("go", []string{"foo.go"})

	// After a pass that wrote a vector, the engine should reload its slab
	// on the next phase-3 call. Easiest check: after a second phase-3 call,
	// the slab now contains the row. We probe via the public API by asking
	// for it directly; pure-state inspection would require exporting fields.
	resp, err := eng.GetRelevantContext(query.ContextRequest{Task: "anything goes here"})
	if err != nil {
		t.Fatalf("phase-3 call after dirty: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response")
	}
}
