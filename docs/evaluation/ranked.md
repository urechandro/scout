# Ranked retrieval evaluation

Status: initial benchmark

Date: 2026-09-11

Corpus: Scout repository plus `eval/testdata/planning.proto`

## Fixture contract

Retrieval fixtures support these ranked expectations:

- `must_include`: required targets that contribute to ranked metrics;
- `should_include`: optional relevant targets that contribute to ranked metrics;
- `must_not_include`: prohibited targets used for the violation rate;
- `max_rank`: required upper rank bounds, keyed by symbol ID substring.

Ranks are one-based positions in `ContextResponse.Symbols`. A target matches
when its fixture value is contained in a returned symbol ID.

## Metrics

- Recall@5 and Recall@10 divide relevant targets found within the cutoff by all
  `must_include` and `should_include` targets.
- Mean reciprocal rank uses the first relevant result for each query. A query
  with no relevant result contributes zero.
- Must-not-include violation rate divides returned prohibited targets by all
  prohibited-target checks.
- Semantic rank delta compares the best relevant rank for the same fixture with
  and without vector retrieval. A newly found target or a lower rank is an
  improvement; a lost target or a higher rank is a regression.

## Initial results

The exact/FTS general fixture set reports:

```text
Recall@5:  0.750
Recall@10: 0.750
MRR:       0.733
Must-not-include violation rate: 0.000
```

The 15 meaning-based fixtures report:

```text
FTS-only Recall@5:  0.067
FTS-only Recall@10: 0.067
FTS-only MRR:       0.055

Semantic Recall@5:  0.067
Semantic Recall@10: 0.133
Semantic MRR:       0.063
Semantic rank improved:  33.3%
Semantic rank worsened:  53.3%
Semantic rank unchanged: 13.3%
```

The semantic hit rate remains 11/15 (73%) versus 9/15 (60%) for FTS-only.
The four known semantic misses remain `slogPrintfAdapter`, `ListUnembedded`,
`GetImpact`, and `dedup`. The semantic test therefore exits nonzero, preserving
the existing fail-closed behavior.

These results show that hit rate alone overstates semantic quality. Milestone
1 ranking changes should preserve Recall@10 while reducing the semantic rank
regression percentage.
