package eval

import (
	"sort"
	"testing"
	"time"

	"github.com/urechandro/scout/query"
)

// retrievalMetrics captures inexpensive, tokenizer-independent baseline data
// for get_relevant_context fixtures. Ranked retrieval metrics belong to the
// next evaluation milestone; these measurements freeze current volume and
// latency without changing retrieval behavior.
type retrievalMetrics struct {
	durations []time.Duration
	results   int
	estTokens int
}

func (m *retrievalMetrics) record(result any, responseBytes int, elapsed time.Duration) {
	resp, ok := result.(*query.ContextResponse)
	if !ok {
		return
	}
	m.durations = append(m.durations, elapsed)
	m.results += len(resp.Symbols)
	m.estTokens += responseBytes / 4
}

func (m *retrievalMetrics) log(t *testing.T, label string) {
	t.Helper()
	if len(m.durations) == 0 {
		t.Logf("%s metrics: queries=0", label)
		return
	}

	sort.Slice(m.durations, func(i, j int) bool {
		return m.durations[i] < m.durations[j]
	})
	queries := len(m.durations)
	t.Logf(
		"%s metrics: queries=%d avg_results=%.2f avg_marshaled_est_tokens=%.2f p50=%s p95=%s",
		label,
		queries,
		float64(m.results)/float64(queries),
		float64(m.estTokens)/float64(queries),
		percentile(m.durations, 50),
		percentile(m.durations, 95),
	)
}

func percentile(values []time.Duration, percent int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := (percent*len(values) + 99) / 100
	if index < 1 {
		index = 1
	}
	return values[index-1]
}
