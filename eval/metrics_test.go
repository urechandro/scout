package eval

import (
	"sort"
	"strings"
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
	ranked    rankedMetrics
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

type rankedMetrics struct {
	relevant           int
	hitsAt5            int
	hitsAt10           int
	reciprocalRankSum  float64
	queriesWithTargets int
	prohibitedChecks   int
	prohibitedHits     int
}

func (m *retrievalMetrics) recordRanking(f Fixture, result any) {
	resp, ok := result.(*query.ContextResponse)
	if !ok {
		return
	}
	targets := append(append([]string(nil), f.MustInclude...), f.ShouldInclude...)
	if len(targets) > 0 {
		m.ranked.queriesWithTargets++
	}
	bestRank := 0
	for _, target := range targets {
		rank := symbolRank(resp, target)
		m.ranked.relevant++
		if rank > 0 && rank <= 5 {
			m.ranked.hitsAt5++
		}
		if rank > 0 && rank <= 10 {
			m.ranked.hitsAt10++
		}
		if rank > 0 && (bestRank == 0 || rank < bestRank) {
			bestRank = rank
		}
	}
	if bestRank > 0 {
		m.ranked.reciprocalRankSum += 1 / float64(bestRank)
	}
	for _, prohibited := range f.MustNotInclude {
		m.ranked.prohibitedChecks++
		if symbolRank(resp, prohibited) > 0 {
			m.ranked.prohibitedHits++
		}
	}
}

func symbolRank(resp *query.ContextResponse, target string) int {
	for i, symbol := range resp.Symbols {
		if strings.Contains(symbol.ID, target) {
			return i + 1
		}
	}
	return 0
}

func bestTargetRank(f Fixture, result any) int {
	resp, ok := result.(*query.ContextResponse)
	if !ok {
		return 0
	}
	best := 0
	for _, target := range append(append([]string(nil), f.MustInclude...), f.ShouldInclude...) {
		rank := symbolRank(resp, target)
		if rank > 0 && (best == 0 || rank < best) {
			best = rank
		}
	}
	return best
}

type semanticDelta struct {
	compared  int
	improved  int
	worsened  int
	unchanged int
}

func (d *semanticDelta) record(baselineRank, semanticRank int) {
	if baselineRank == 0 && semanticRank == 0 {
		d.unchanged++
	} else if semanticRank > 0 && (baselineRank == 0 || semanticRank < baselineRank) {
		d.improved++
	} else if baselineRank > 0 && (semanticRank == 0 || semanticRank > baselineRank) {
		d.worsened++
	} else {
		d.unchanged++
	}
	d.compared++
}

func (d semanticDelta) log(t *testing.T) {
	t.Helper()
	t.Logf(
		"semantic rank delta: queries=%d improved=%.1f%% worsened=%.1f%% unchanged=%.1f%%",
		d.compared,
		100*ratio(d.improved, d.compared),
		100*ratio(d.worsened, d.compared),
		100*ratio(d.unchanged, d.compared),
	)
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
	if m.ranked.relevant > 0 {
		t.Logf(
			"%s ranked metrics: recall@5=%.3f recall@10=%.3f mrr=%.3f must_not_violation_rate=%.3f",
			label,
			float64(m.ranked.hitsAt5)/float64(m.ranked.relevant),
			float64(m.ranked.hitsAt10)/float64(m.ranked.relevant),
			m.ranked.reciprocalRankSum/float64(m.ranked.queriesWithTargets),
			ratio(m.ranked.prohibitedHits, m.ranked.prohibitedChecks),
		)
	}
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
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

func TestRankedMetrics(t *testing.T) {
	result := &query.ContextResponse{Symbols: []query.SymbolSummary{
		{ID: "example/first.Target"},
		{ID: "example/second.Helper"},
		{ID: "example/generated.Unrelated"},
	}}
	fixture := Fixture{
		MustInclude:    []string{"first.Target"},
		ShouldInclude:  []string{"second.Helper", "missing.Optional"},
		MustNotInclude: []string{"generated.Unrelated"},
	}
	var metrics retrievalMetrics
	metrics.recordRanking(fixture, result)

	if metrics.ranked.relevant != 3 || metrics.ranked.hitsAt5 != 2 || metrics.ranked.hitsAt10 != 2 {
		t.Fatalf("unexpected recall counters: %+v", metrics.ranked)
	}
	if metrics.ranked.reciprocalRankSum != 1 {
		t.Fatalf("reciprocal rank sum = %v, want 1", metrics.ranked.reciprocalRankSum)
	}
	if metrics.ranked.prohibitedHits != 1 || metrics.ranked.prohibitedChecks != 1 {
		t.Fatalf("unexpected prohibited counters: %+v", metrics.ranked)
	}
}

func TestSemanticDelta(t *testing.T) {
	var delta semanticDelta
	delta.record(0, 4)
	delta.record(2, 0)
	delta.record(3, 3)
	if delta.improved != 1 || delta.worsened != 1 || delta.unchanged != 1 {
		t.Fatalf("unexpected delta: %+v", delta)
	}
}
