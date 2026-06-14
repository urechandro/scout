package embedder

import (
	"context"
	"fmt"
	"log"

	"github.com/urechandro/scout/store"
)

// DefaultBatchSize is the number of symbols sent to /api/embed in one HTTP
// call. Sized for typical embedding models on a laptop: large enough to
// amortize HTTP overhead, small enough to keep failures cheap (a bad batch
// only loses ~32 vectors that the next index pass will retry).
const DefaultBatchSize = 32

// Options configures a Run pass.
type Options struct {
	// BatchSize overrides DefaultBatchSize when > 0.
	BatchSize int
	// Limit caps the total number of symbols processed in a single pass.
	// 0 means no cap — process every unembedded symbol. Used by callers that
	// want to amortize embedding cost across multiple runs (e.g. a watcher
	// embedding only the most recently changed files).
	Limit int
	// Logger receives per-batch progress lines. nil uses the std logger.
	Logger Logger
}

// Logger is the minimal logging surface Run depends on. *log.Logger satisfies
// it, as does slog via a small adapter — keeping the dependency loose lets
// callers route progress to wherever they already log.
type Logger interface {
	Printf(format string, args ...any)
}

// Stats summarizes a Run pass.
type Stats struct {
	Considered int // symbols ListUnembedded returned
	Embedded   int // symbols successfully vectorized and written
	Failed     int // symbols in batches the embedder rejected
}

// Run vectorizes every symbol the store lists as unembedded against c.Model()
// and writes the vectors back. Per-batch errors are logged and counted, never
// returned — a misbehaving embedder should not fail the whole index. The only
// returned errors come from the store itself (listing or writing).
func Run(ctx context.Context, s *store.Store, c Client, opts Options) (Stats, error) {
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}

	syms, err := s.ListUnembedded(c.Model(), opts.Limit)
	if err != nil {
		return Stats{}, fmt.Errorf("list unembedded: %w", err)
	}
	stats := Stats{Considered: len(syms)}
	if len(syms) == 0 {
		return stats, nil
	}

	logger.Printf("embedder: %d symbols to vectorize via %s (batch %d)", len(syms), c.Model(), batchSize)

	for start := 0; start < len(syms); start += batchSize {
		end := min(start+batchSize, len(syms))
		chunk := syms[start:end]

		texts := make([]string, len(chunk))
		for i, sym := range chunk {
			texts[i] = EmbeddingText(sym)
		}

		vecs, err := c.Embed(ctx, texts)
		if err != nil {
			logger.Printf("embedder: batch %d-%d failed: %v", start, end, err)
			stats.Failed += len(chunk)
			if ctx.Err() != nil {
				// Stop early on cancellation so the user can interrupt
				// a long pass without waiting for every batch to fail.
				return stats, ctx.Err()
			}
			continue
		}

		for i, vec := range vecs {
			if err := s.UpsertEmbedding(chunk[i].ID, c.Model(), vec); err != nil {
				logger.Printf("embedder: write %s: %v", chunk[i].ID, err)
				stats.Failed++
				continue
			}
			stats.Embedded++
		}
	}

	logger.Printf("embedder: done — %d embedded, %d failed", stats.Embedded, stats.Failed)
	return stats, nil
}

// EmbeddingText is the text scout sends to the embedder for a given symbol.
// Signature carries the structural meaning (name + parameter/return types);
// docstring captures the why when present. Body is deliberately excluded —
// bodies churn far more often than signatures, which would burn embedder
// budget on no-op re-embeds and noise up the vector space.
func EmbeddingText(sym store.Symbol) string {
	if sym.Docstring == "" {
		return sym.Signature
	}
	return sym.Signature + "\n" + sym.Docstring
}
