package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_MissingFileReturnsZero(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Embedder != nil {
		t.Errorf("missing file should yield nil embedder, got %+v", cfg.Embedder)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{Embedder: DefaultOllamaConfig()}
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Embedder == nil {
		t.Fatal("embedder was nil after round-trip")
	}
	if got.Embedder.Kind != EmbedderOllama {
		t.Errorf("kind: got %q, want %q", got.Embedder.Kind, EmbedderOllama)
	}
	if got.Embedder.Host != DefaultOllamaHost {
		t.Errorf("host: got %q", got.Embedder.Host)
	}
	if got.Embedder.Model != DefaultOllamaModel {
		t.Errorf("model: got %q", got.Embedder.Model)
	}
}

func TestSave_CreatesScoutDir(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, &Config{}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, Dir, FileName)); err != nil {
		t.Fatalf("expected config file at .scout/%s: %v", FileName, err)
	}
}

func TestValidate_RejectsUnknownKind(t *testing.T) {
	err := (&EmbedderConfig{Kind: "voyage", Host: "x", Model: "y"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("got %v, want 'not supported' error", err)
	}
}

func TestValidate_RequiresFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  EmbedderConfig
		want string
	}{
		{"missing kind", EmbedderConfig{Host: "h", Model: "m"}, "kind"},
		{"missing host", EmbedderConfig{Kind: EmbedderOllama, Model: "m"}, "host"},
		{"missing model", EmbedderConfig{Kind: EmbedderOllama, Host: "h"}, "model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want error mentioning %q", err, tc.want)
			}
		})
	}
}

func TestValidate_NilIsOK(t *testing.T) {
	var e *EmbedderConfig
	if err := e.Validate(); err != nil {
		t.Errorf("nil embedder should validate, got %v", err)
	}
}

func TestLoad_RejectsInvalidFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := []byte("embedder:\n  kind: garbage\n  host: h\n  model: m\n")
	if err := os.WriteFile(Path(dir), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected validation error for unknown kind")
	}
}
