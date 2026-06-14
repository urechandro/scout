// Package config owns scout's persisted, user-facing configuration:
// the contents of <root>/.scout/config.yaml. CLI flags and the MCP server
// read this file to discover optional features the user enabled at init time
// (currently: the semantic-search embedder).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// FileName is the on-disk name of the config file under .scout/.
const FileName = "config.yaml"

// Dir is the conventional .scout/ directory that holds the index and config.
const Dir = ".scout"

// EmbedderKind enumerates the supported embedding backends.
type EmbedderKind string

const (
	// EmbedderOllama uses a locally-running Ollama server to generate vectors.
	EmbedderOllama EmbedderKind = "ollama"
)

// DefaultOllamaHost is the address Ollama listens on out of the box.
const DefaultOllamaHost = "http://localhost:11434"

// DefaultOllamaModel is a 274MB, 768-dim general-purpose embedding model.
// Code-specific alternatives exist (nomic-embed-code, bge-code) but are
// >1.5GB and not worth defaulting users into.
const DefaultOllamaModel = "nomic-embed-text"

// Config is the root of .scout/config.yaml. All fields are optional so the
// file can grow without breaking older scout binaries.
type Config struct {
	// Embedder configures the optional semantic-search retrieval layer.
	// Nil means semantic search is disabled — scout falls back to exact
	// name lookup + FTS only, the pre-semantic behavior.
	Embedder *EmbedderConfig `yaml:"embedder,omitempty"`
}

// EmbedderConfig points scout at an embedding service.
type EmbedderConfig struct {
	Kind  EmbedderKind `yaml:"kind"`
	Host  string       `yaml:"host"`
	Model string       `yaml:"model"`
}

// DefaultOllamaConfig returns the recommended starter config when a user
// opts into Ollama-backed semantic search.
func DefaultOllamaConfig() *EmbedderConfig {
	return &EmbedderConfig{
		Kind:  EmbedderOllama,
		Host:  DefaultOllamaHost,
		Model: DefaultOllamaModel,
	}
}

// Validate returns an error if the embedder block is structurally invalid.
// It does NOT verify the embedder is reachable — that is a runtime concern.
func (e *EmbedderConfig) Validate() error {
	if e == nil {
		return nil
	}
	switch e.Kind {
	case EmbedderOllama:
		// ok
	case "":
		return errors.New("embedder.kind is required")
	default:
		return fmt.Errorf("embedder.kind %q is not supported", e.Kind)
	}
	if e.Host == "" {
		return errors.New("embedder.host is required")
	}
	if e.Model == "" {
		return errors.New("embedder.model is required")
	}
	return nil
}

// Path returns the absolute config file path for a given project root.
func Path(rootDir string) string {
	return filepath.Join(rootDir, Dir, FileName)
}

// Load reads .scout/config.yaml from rootDir. A missing file is not an error
// — it simply returns a zero Config (no embedder configured).
func Load(rootDir string) (*Config, error) {
	path := Path(rootDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.Embedder.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// Save writes cfg to .scout/config.yaml under rootDir, creating the directory
// if it doesn't yet exist. Safe to call repeatedly — overwrites in place.
func Save(rootDir string, cfg *Config) error {
	if err := cfg.Embedder.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(rootDir, Dir), 0o755); err != nil {
		return fmt.Errorf("create %s/: %w", Dir, err)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	path := Path(rootDir)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
