// Package embedder talks to embedding services that turn text into vectors.
// Today only Ollama is supported. The package exposes a tiny Client interface
// the indexer and (later) the query engine call into, plus the probe helper
// `scout init` uses before any client exists.
package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is the minimal surface scout's orchestration code depends on. Keeping
// it small makes it trivial to mock in tests and to add a second backend
// without touching callers.
type Client interface {
	// Embed turns a batch of texts into vectors of equal length. The returned
	// slice has the same length as texts; element i is the vector for text i.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Model is the identifier scout stores alongside each vector so it can
	// detect and re-embed when the configured model changes.
	Model() string
}

// OllamaClient implements Client against an Ollama server's /api/embed endpoint.
type OllamaClient struct {
	host  string
	model string
	httpc *http.Client
}

// NewOllamaClient constructs a client. The default 60s timeout covers a batch
// of ~32 inputs through nomic-embed-text on a typical laptop; tune via
// SetTimeout if your model or batch size differs.
func NewOllamaClient(host, model string) *OllamaClient {
	return &OllamaClient{
		host:  strings.TrimRight(host, "/"),
		model: model,
		httpc: &http.Client{Timeout: 60 * time.Second},
	}
}

// SetTimeout overrides the HTTP client timeout. Useful for tests and for
// callers that batch larger.
func (c *OllamaClient) SetTimeout(d time.Duration) {
	c.httpc.Timeout = d
}

// Model returns the embedding model the client was configured with.
func (c *OllamaClient) Model() string { return c.model }

type ollamaEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed sends texts to Ollama's /api/embed endpoint and returns one vector
// per input, in the same order. Errors are surfaced verbatim — the
// orchestrator decides whether to retry, skip the batch, or bail.
func (c *OllamaClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(ollamaEmbedRequest{Model: c.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.host+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embed http %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var res ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("decode embed response: %w", err)
	}
	if len(res.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embed returned %d vectors for %d inputs", len(res.Embeddings), len(texts))
	}
	return res.Embeddings, nil
}

// ProbeResult describes what an Ollama probe found.
type ProbeResult struct {
	// Reachable is true when the host responded to /api/tags.
	Reachable bool
	// ModelInstalled is true when Reachable AND the requested model name
	// (with or without the ":tag" suffix) appears in the host's tag list.
	ModelInstalled bool
	// AvailableModels lists every tag the host reported. Surfaced so the
	// init wizard can suggest the closest already-installed model.
	AvailableModels []string
}

type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// ProbeOllama checks whether an Ollama server is reachable at host and whether
// the requested embedding model is installed. Never errors on plain network
// failure — those are reported via Reachable=false instead so callers can
// degrade gracefully. A non-nil error means something unexpected went wrong
// while parsing a successful response.
func ProbeOllama(ctx context.Context, host, model string) (ProbeResult, error) {
	host = strings.TrimRight(host, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, host+"/api/tags", nil)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("build request: %w", err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Treat any transport-level failure as "not reachable" — no point
		// distinguishing DNS, refused, timeout, etc. at this layer.
		return ProbeResult{}, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ProbeResult{Reachable: true}, nil
	}

	var body ollamaTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ProbeResult{Reachable: true}, fmt.Errorf("decode /api/tags: %w", err)
	}

	tags := make([]string, 0, len(body.Models))
	wanted := strings.TrimSpace(model)
	found := false
	for _, m := range body.Models {
		tags = append(tags, m.Name)
		if matchesModel(m.Name, wanted) {
			found = true
		}
	}
	return ProbeResult{
		Reachable:       true,
		ModelInstalled:  found,
		AvailableModels: tags,
	}, nil
}

// matchesModel matches "nomic-embed-text" against "nomic-embed-text:latest"
// and against the bare name. Ollama always returns names with an explicit
// tag, but users configure scout with the bare name.
func matchesModel(installed, wanted string) bool {
	if installed == wanted {
		return true
	}
	if base, _, ok := strings.Cut(installed, ":"); ok && base == wanted {
		return true
	}
	return false
}
