# Retrieval baseline

Status: frozen; retrieval-quality gate not yet accepted

Measured: 2026-09-11, Europe/Stockholm

Retrieval source revision: `9271a8e591aa5b58c3705f28395e74050df75baa`

This artifact freezes Scout's retrieval behavior before vNext ranking or agent
integration changes. The evaluation instrumentation added alongside this file
does not change retrieval logic or ranking constants.

## Environment

| Component | Value |
|---|---|
| Platform | macOS arm64 |
| Go | `go1.27.1 darwin/arm64` |
| Ollama client | `0.33.3` |
| Ollama host | `http://127.0.0.1:11434` |
| Embedding model | `nomic-embed-text` |
| Embedded symbols | 533 |
| General fixtures | 14 |
| Semantic fixtures | 15 |

Run the baseline with:

```sh
./scripts/eval-baseline
```

The runner stores Go caches beneath `${SCOUT_BASELINE_CACHE}` or a temporary
directory instead of writing to the user's home directory. To include the live
semantic evaluation:

```sh
SCOUT_OLLAMA_HOST=http://127.0.0.1:11434 \
SCOUT_OLLAMA_MODEL=nomic-embed-text \
./scripts/eval-baseline
```

Ollama and the named model must already be available. Semantic evaluation is
explicitly skipped when either variable is absent.

## Test status

The default complete suite passed:

```text
ok  github.com/urechandro/scout/cmd/scout
ok  github.com/urechandro/scout/config
ok  github.com/urechandro/scout/embedder
ok  github.com/urechandro/scout/eval
ok  github.com/urechandro/scout/indexer
ok  github.com/urechandro/scout/mcp
ok  github.com/urechandro/scout/protoindexer
ok  github.com/urechandro/scout/query
ok  github.com/urechandro/scout/store
?   github.com/urechandro/scout/tsindexer [no test files]
```

The opt-in live semantic evaluation completed end to end but failed four of its
15 expectations. This is a retrieval-quality failure, not an infrastructure
failure: Ollama embedded all 533 symbols with zero failed embeddings, then Phase
3 executed for every discovery query.

## Retrieval results

| Metric | Exact/FTS | Semantic Phase 3 |
|---|---:|---:|
| Semantic-fixture hits | 9/15 (60%) | 11/15 (73%) |
| Average returned symbols | 39.73 | 39.27 |
| Average marshaled estimate | 2,674 tokens | 2,674 tokens |
| Query latency p50 | 2.83 ms | 15.22 ms |
| Query latency p95 | 5.19 ms | 18.64 ms |

The token estimate is `len(json.Marshal(engineResponse)) / 4`. It describes the
evaluation representation, not the MCP server's compact text rendering and not
provider-tokenizer parity. Milestone 1 must add response-path token accounting.

The five general `get_relevant_context` fixtures returned an average of 25.60
symbols, with an average marshaled estimate of 1,730 tokens, p50 latency of 0.55
ms, and p95 latency of 2.58 ms. All 14 general tool fixtures passed their
presence, absence, error, and byte-limit expectations.

Recall@5, Recall@10, MRR, and a general false-positive rate are not available
from the current substring-based fixture schema. Adding ranked expectations is
Milestone 1 work; reporting fabricated ranked metrics here would make the
baseline misleading.

## Semantic delta

Phase 3 recovered five targets missed by exact/FTS retrieval:

- `indexer.Watcher` (`file_watcher`);
- `indexer.LinkProtoToGo` (`proto_link`);
- `query.topKCosine` (`topk_picker`);
- `embedder.ProbeOllama` (`health_check`);
- `query.Engine.expand` (`graph_walk`).

It displaced three targets that exact/FTS retrieval returned:

- `cmd/scout.slogPrintfAdapter` (`log_bridge`);
- `query.Engine.GetImpact` (`blast_radius`);
- `query.dedup` (`duplicate_killer`).

`store.Store.ListUnembedded` (`unembedded_picker`) was missed in both modes.
The remaining semantic fixtures passed in both modes.

The semantic layer therefore adds five semantic-only wins but causes three
regressions, for a net improvement of two fixtures. This is useful but not yet
trustworthy enough to pass the retrieval-quality gate.

## Current capability inventory

| Capability | Current behavior |
|---|---|
| Go indexing | Uses `go/packages`; supports AST, CHA, and RTA call-graph modes and optional dependency signatures. |
| TypeScript indexing | Invokes `ts-callgraph` when a tsconfig is configured; watcher changes trigger a debounced full TS reindex. |
| Proto indexing | Indexes services, RPCs, messages, fields, and enums, then links proto names to Go symbols. |
| Exact lookup | Extracts compound identifiers and performs direct name lookup with deterministic scoring. |
| FTS retrieval | Uses SQLite FTS5 for discovery queries, with source/generated partitioning and ranking. |
| Semantic retrieval | Optional Ollama query embedding plus in-memory cosine scan for discovery queries only. |
| Graph expansion | Expands retrieved candidates one hop by default through stored relationships. |
| Token budgeting | Defaults to 600 estimated tokens for precise queries and 2,000 for discovery, using an approximate four-characters-per-token trim. |
| Incremental reindex | `scout reindex` and Go/proto watcher paths update affected files; full indexing resets stale data while restoring unchanged embeddings. |
| Watcher | Performs immediate light Go/proto updates, debounced type-aware/TS work, proto-Go relinking, embedding backlog refresh, and vector-slab invalidation. |
| `get_relevant_context` | Runs exact lookup, discovery FTS, optional Phase 3 vectors, graph expansion, ranking, deduplication, and budget trimming. |
| `get_body` | Returns exact indexed symbol source plus summaries of referenced symbols. |
| `get_flow` | Combines exact body, caller summaries, and callee summaries. |
| `get_callers` | Uses graph edges with interface/RPC and body-reference fallbacks. |
| `get_callees` | Uses graph edges with body-reference fallback. |
| `get_impact` | Builds a cross-layer blast radius including proto, generated code, implementations, callers, and tests. |
| `get_pattern` | Returns a proto-to-implementation slice when possible and falls back to an implementation search. |
| `get_unimplemented` | Compares proto RPCs with Go server methods and detects missing or explicitly unimplemented handlers. |
| `get_conventions` | Resolves indexed project conventions by topic, with FTS fallback. |
| MCP lifecycle | `scout serve` opens the SQLite store, configures the optional embedder and watcher, then serves JSON-RPC over stdio. |
| Claude initialization | `scout init` creates `.scout`, updates `.gitignore`, writes `.mcp.json`, inserts a managed `CLAUDE.md` block, optionally configures semantic search and conventions, and indexes the repository. |

## Known limitations and next gate

- The semantic fixture result is worse than the previously hand-recorded
  `13/15`; fixture comments have been corrected to the measured `11/15`.
- Three exact/FTS hits are displaced when vectors compete for the token budget.
- The present eval schema cannot say where a target ranked or compute Recall@K
  and MRR.
- The serialized-response token estimate overstates the compact MCP rendering
  and must not be used as a product claim.
- Latencies are local single-run measurements and are not statistically robust
  across machines.

Gate A is frozen but not accepted for downstream Codex work. The next change
must remain within retrieval evaluation and quality: add ranked fixtures,
reproduce the three displacement regressions, and tune only against aggregate
before/after evidence. Agent-neutral initialization and Codex integration remain
blocked.
