package embedder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeOllama_HostDown(t *testing.T) {
	// Use a port nothing is listening on. Probe should report Reachable=false
	// without erroring.
	res, err := ProbeOllama(context.Background(), "http://127.0.0.1:1", "nomic-embed-text")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Reachable {
		t.Errorf("expected unreachable, got %+v", res)
	}
}

func TestProbeOllama_ModelInstalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3:latest"},{"name":"nomic-embed-text:latest"}]}`))
	}))
	defer srv.Close()

	res, err := ProbeOllama(context.Background(), srv.URL, "nomic-embed-text")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !res.Reachable {
		t.Errorf("expected reachable, got %+v", res)
	}
	if !res.ModelInstalled {
		t.Errorf("expected ModelInstalled=true, got %+v", res)
	}
	if len(res.AvailableModels) != 2 {
		t.Errorf("expected 2 tags, got %v", res.AvailableModels)
	}
}

func TestProbeOllama_ModelMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3:latest"}]}`))
	}))
	defer srv.Close()

	res, err := ProbeOllama(context.Background(), srv.URL, "nomic-embed-text")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !res.Reachable || res.ModelInstalled {
		t.Errorf("expected reachable + missing, got %+v", res)
	}
}

func TestOllamaClient_Embed_RoundTrip(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		_, _ = w.Write([]byte(`{"embeddings":[[0.1,0.2],[0.3,0.4]]}`))
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "nomic-embed-text")
	out, err := c.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if gotPath != "/api/embed" {
		t.Errorf("path: got %q, want /api/embed", gotPath)
	}
	if gotBody == "" || !contains(gotBody, "nomic-embed-text") || !contains(gotBody, `"a"`) {
		t.Errorf("body did not include model + inputs: %q", gotBody)
	}
	if len(out) != 2 || out[0][0] != 0.1 || out[1][1] != 0.4 {
		t.Errorf("unexpected vectors: %+v", out)
	}
}

func TestOllamaClient_Embed_EmptyInput(t *testing.T) {
	c := NewOllamaClient("http://unused", "m")
	got, err := c.Embed(context.Background(), nil)
	if err != nil || got != nil {
		t.Errorf("empty input: got %v, err=%v; want nil,nil", got, err)
	}
}

func TestOllamaClient_Embed_CountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"embeddings":[[1.0]]}`))
	}))
	defer srv.Close()
	c := NewOllamaClient(srv.URL, "m")
	_, err := c.Embed(context.Background(), []string{"a", "b"})
	if err == nil {
		t.Fatal("expected count mismatch error")
	}
}

func TestOllamaClient_Embed_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewOllamaClient(srv.URL, "m")
	_, err := c.Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("expected error on non-200")
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestMatchesModel(t *testing.T) {
	cases := []struct {
		installed, wanted string
		want              bool
	}{
		{"nomic-embed-text:latest", "nomic-embed-text", true},
		{"nomic-embed-text", "nomic-embed-text", true},
		{"nomic-embed-text:v1", "nomic-embed-text", true},
		{"llama3:latest", "nomic-embed-text", false},
		{"nomic-embed-code:latest", "nomic-embed-text", false},
	}
	for _, tc := range cases {
		got := matchesModel(tc.installed, tc.wanted)
		if got != tc.want {
			t.Errorf("matchesModel(%q,%q) = %v, want %v", tc.installed, tc.wanted, got, tc.want)
		}
	}
}
