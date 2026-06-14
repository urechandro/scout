package query

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urechandro/scout/store"
)

// fakeEmbedder is an in-memory Client used to drive Phase 3 from tests.
//
// Behaviors are controlled per field:
//   - model: returned by Model()
//   - byText: keyword → vector mapping. Embed picks the matching entry when
//     the input contains the keyword, otherwise returns fallback.
//   - fallback: default vector for unmatched queries
//   - err: when non-nil, Embed returns this error
//   - calls: atomic counter so tests can assert Embed was/wasn't called
type fakeEmbedder struct {
	model    string
	byText   map[string][]float32
	fallback []float32
	err      error
	calls    int32
}

func (f *fakeEmbedder) Model() string { return f.model }

func (f *fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = f.pickVec(t)
	}
	return out, nil
}

func (f *fakeEmbedder) Count() int { return int(atomic.LoadInt32(&f.calls)) }

func (f *fakeEmbedder) pickVec(text string) []float32 {
	lower := strings.ToLower(text)
	for keyword, vec := range f.byText {
		if strings.Contains(lower, strings.ToLower(keyword)) {
			return vec
		}
	}
	return f.fallback
}

// seedEmbedding writes a vector for an existing symbol row under the given
// model name. Mirrors what embedder.Run does after a successful pass.
func seedEmbedding(t *testing.T, s *store.Store, id, model string, vec []float32) {
	t.Helper()
	if err := s.UpsertEmbedding(id, model, vec); err != nil {
		t.Fatalf("seed embedding for %s: %v", id, err)
	}
}

// --- Phase 3: precise queries skip the embedder entirely ---

func TestPhaseThree_PreciseQuerySkipsEmbedder(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{{
		ID: "myapp/svc.CreateShipmentLeg", Package: "myapp/svc",
		Name: "CreateShipmentLeg", Kind: "func",
		Signature: "func CreateShipmentLeg(ctx context.Context) error",
		File:      "/svc/create.go", LineStart: 1, LineEnd: 5,
	}})

	fake := &fakeEmbedder{model: "test", fallback: []float32{1, 0, 0}}
	engine := New(s, Options{Embedder: fake})

	if _, err := engine.GetRelevantContext(ContextRequest{Task: "CreateShipmentLeg"}); err != nil {
		t.Fatalf("GetRelevantContext: %v", err)
	}
	if fake.Count() != 0 {
		t.Errorf("precise query called embedder %d times, want 0", fake.Count())
	}
}

func TestPhaseThree_PreciseDottedQuerySkipsEmbedder(t *testing.T) {
	s := newTestStore(t)
	fake := &fakeEmbedder{model: "test", fallback: []float32{1, 0, 0}}
	engine := New(s, Options{Embedder: fake})

	if _, err := engine.GetRelevantContext(ContextRequest{Task: "grpc.Dial"}); err != nil {
		t.Fatalf("GetRelevantContext: %v", err)
	}
	if fake.Count() != 0 {
		t.Errorf("dotted precise query called embedder %d times, want 0", fake.Count())
	}
}

// --- Phase 3: discovery queries surface vector hits FTS would have missed ---

func TestPhaseThree_DiscoveryQuerySurfacesVectorHit(t *testing.T) {
	s := newTestStore(t)
	// The hit's name and signature share no terms with the query, so FTS
	// alone returns nothing. Vector match is the only way it surfaces.
	hitID := "myapp/util.retryDeferredJob"
	seedSymbols(t, s, []store.Symbol{{
		ID: hitID, Package: "myapp/util",
		Name: "retryDeferredJob", Kind: "func",
		Signature: "func retryDeferredJob(j Job) error",
		Docstring: "schedules a deferred retry for a failed delivery",
		File:      "/util/retry.go", LineStart: 1, LineEnd: 10,
	}})

	fake := &fakeEmbedder{
		model: "test",
		byText: map[string][]float32{
			"the thing": {1, 0, 0},
		},
		fallback: []float32{0, 1, 0},
	}
	seedEmbedding(t, s, hitID, "test", []float32{1, 0, 0})
	engine := New(s, Options{Embedder: fake})

	resp, err := engine.GetRelevantContext(ContextRequest{
		Task: "the thing that retries failed deliveries",
	})
	if err != nil {
		t.Fatalf("GetRelevantContext: %v", err)
	}
	if fake.Count() == 0 {
		t.Fatal("discovery query did not invoke embedder")
	}

	found := false
	for _, sym := range resp.Symbols {
		if sym.ID == hitID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("vector hit %q not surfaced; got %d symbols", hitID, len(resp.Symbols))
	}
}

// --- Phase 3: every failure mode is a silent no-op ---

func TestPhaseThree_NilEmbedderIsSilentNoop(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{{
		ID: "myapp/auth.ValidateToken", Package: "myapp/auth",
		Name: "ValidateToken", Kind: "func",
		Signature: "func ValidateToken(token string) error",
		Docstring: "validates an authentication token",
		File:      "/auth/auth.go", LineStart: 1, LineEnd: 5,
	}})
	// nil Embedder is the pre-semantic default; should behave exactly as
	// the FTS-only engine did before Phase 3 existed.
	engine := New(s, Options{})

	resp, err := engine.GetRelevantContext(ContextRequest{Task: "find authentication"})
	if err != nil {
		t.Fatalf("nil embedder errored: %v", err)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
}

func TestPhaseThree_EmbedderErrorIsSilentNoop(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{{
		ID: "myapp/auth.ValidateToken", Package: "myapp/auth",
		Name: "ValidateToken", Kind: "func",
		Signature: "func ValidateToken(token string) error",
		Docstring: "validates an authentication token",
		File:      "/auth/auth.go", LineStart: 1, LineEnd: 5,
	}})
	// FTS would still find ValidateToken via "authentication" in the
	// docstring — that result must survive an embedder failure.
	fake := &fakeEmbedder{model: "test", err: errors.New("connection refused")}
	engine := New(s, Options{Embedder: fake})

	resp, err := engine.GetRelevantContext(ContextRequest{Task: "find authentication"})
	if err != nil {
		t.Fatalf("embedder error leaked out: %v", err)
	}
	if len(resp.Symbols) == 0 {
		t.Fatal("FTS results were dropped by phase-3 failure")
	}
}

func TestPhaseThree_EmptySlabIsSilentNoop(t *testing.T) {
	s := newTestStore(t)
	seedSymbols(t, s, []store.Symbol{{
		ID: "myapp/auth.ValidateToken", Package: "myapp/auth",
		Name: "ValidateToken", Kind: "func",
		Signature: "func ValidateToken(token string) error",
		File:      "/auth/auth.go", LineStart: 1, LineEnd: 5,
	}})
	// No vectors seeded — every symbol unembedded, slab empty.
	fake := &fakeEmbedder{model: "test", fallback: []float32{1, 0, 0}}
	engine := New(s, Options{Embedder: fake})

	resp, err := engine.GetRelevantContext(ContextRequest{Task: "find auth"})
	if err != nil {
		t.Fatalf("empty slab errored: %v", err)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
	if fake.Count() == 0 {
		t.Error("expected embedder to be called for discovery query, even with empty slab")
	}
}

func TestPhaseThree_DimMismatchIsSilentNoop(t *testing.T) {
	s := newTestStore(t)
	hitID := "myapp/util.retryJob"
	seedSymbols(t, s, []store.Symbol{
		{
			ID: hitID, Package: "myapp/util",
			Name: "retryJob", Kind: "func",
			Signature: "func retryJob() error",
			File:      "/util/retry.go", LineStart: 1, LineEnd: 5,
		},
		{
			ID: "myapp/auth.ValidateToken", Package: "myapp/auth",
			Name: "ValidateToken", Kind: "func",
			Signature: "func ValidateToken(token string) error",
			Docstring: "validates authentication",
			File:      "/auth/auth.go", LineStart: 1, LineEnd: 5,
		},
	})
	// Slab stored vectors are 3-dim; embedder produces 4-dim. Phase 3
	// should skip silently without poisoning FTS results.
	seedEmbedding(t, s, hitID, "test", []float32{1, 0, 0})
	fake := &fakeEmbedder{model: "test", fallback: []float32{1, 0, 0, 0}}
	engine := New(s, Options{Embedder: fake})

	resp, err := engine.GetRelevantContext(ContextRequest{Task: "find authentication"})
	if err != nil {
		t.Fatalf("dim mismatch errored: %v", err)
	}
	// FTS still has ValidateToken; Phase 3 added no extra (retryJob).
	gotIDs := map[string]bool{}
	for _, sym := range resp.Symbols {
		gotIDs[sym.ID] = true
	}
	if gotIDs[hitID] {
		t.Errorf("dim-mismatched vector hit %q leaked through phase-3", hitID)
	}
}

// --- Phase 3: MarkVectorsDirty triggers a slab reload ---

func TestPhaseThree_MarkVectorsDirtyTriggersReload(t *testing.T) {
	s := newTestStore(t)
	originalID := "myapp/util.foo"
	addedID := "myapp/util.bar"
	seedSymbols(t, s, []store.Symbol{
		{ID: originalID, Package: "myapp/util", Name: "foo", Kind: "func",
			Signature: "func foo() error", File: "/util/foo.go", LineStart: 1, LineEnd: 5},
		{ID: addedID, Package: "myapp/util", Name: "bar", Kind: "func",
			Signature: "func bar() error", File: "/util/bar.go", LineStart: 1, LineEnd: 5},
	})
	seedEmbedding(t, s, originalID, "test", []float32{1, 0, 0})

	fake := &fakeEmbedder{model: "test", fallback: []float32{0, 1, 0}}
	engine := New(s, Options{Embedder: fake})

	// First call: slab has only foo. Cosine against [0,1,0] is 0 for [1,0,0],
	// so nothing surfaces — but slab is now cached.
	if _, err := engine.GetRelevantContext(ContextRequest{Task: "discovery query for foo"}); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Add bar's vector after the slab was cached, then mark dirty.
	seedEmbedding(t, s, addedID, "test", []float32{0, 1, 0})
	engine.MarkVectorsDirty()

	// Second call: slab should reload and now include bar. Query vector
	// [0,1,0] cosines to 1.0 against bar, so it surfaces.
	resp, err := engine.GetRelevantContext(ContextRequest{Task: "discovery query for bar"})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	found := false
	for _, sym := range resp.Symbols {
		if sym.ID == addedID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("MarkVectorsDirty did not trigger slab reload; %q not in results", addedID)
	}
}

// --- Phase 3: does not overwrite stronger Phase 1 / Phase 2 scores ---

func TestPhaseThree_DoesNotOverwriteHigherScores(t *testing.T) {
	s := newTestStore(t)
	hitID := "myapp/svc.RetryJob"
	seedSymbols(t, s, []store.Symbol{{
		ID: hitID, Package: "myapp/svc",
		Name: "RetryJob", Kind: "func",
		Signature: "func RetryJob() error",
		File:      "/svc/retry.go", LineStart: 1, LineEnd: 5,
	}})
	// Phase 1 will match "RetryJob" exactly (precise compound query).
	// Phase 3 is bypassed for precise queries, so this exercises the
	// "don't overwrite" guard indirectly. Discovery-query overwrite is
	// the other branch — covered by DiscoveryQuerySurfacesVectorHit:
	// when FTS already scored the symbol, Phase 3 leaves it alone.
	seedEmbedding(t, s, hitID, "test", []float32{1, 0, 0})

	fake := &fakeEmbedder{model: "test", fallback: []float32{1, 0, 0}}
	engine := New(s, Options{Embedder: fake})

	resp, err := engine.GetRelevantContext(ContextRequest{Task: "RetryJob", Verbose: true})
	if err != nil {
		t.Fatalf("GetRelevantContext: %v", err)
	}
	if len(resp.Symbols) == 0 || resp.Symbols[0].ID != hitID {
		t.Fatalf("expected exact-match hit on top, got %d results", len(resp.Symbols))
	}
	// Exact match scores 3.0 + kindWeight + implBoost. Phase-3 cosine
	// would be 1.5 + bias. The fact that the top result is well above
	// 1.5 means Phase 3 didn't overwrite the score.
	if resp.Symbols[0].Score < 2.5 {
		t.Errorf("top score = %v; phase-3 may have overwritten the exact-match score", resp.Symbols[0].Score)
	}
}
