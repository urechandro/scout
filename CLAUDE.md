# scout — Project Summary

## What We Are Building

A **Model Context Protocol (MCP) server** that gives Claude Code fast,
token-efficient navigation of a Go and TypeScript codebase. Instead of Claude reading files
blindly and burning tokens on irrelevant code, it calls tools that return only
the symbols relevant to the task at hand.

## The Core Problem Being Solved

Claude Code's default behaviour when given a task is to read broadly and hope —
opening files, grepping around, reading things that turn out to be irrelevant.
On a large codebase this is slow and wasteful. The goal is to convert
orientation cost from O(files read) to O(query).

## Mental Model

Context window as economics, not storage:
- **Hot** — what the model is reasoning about right now (2-4k tokens)
- **Warm** — what it might need next, retrievable on demand via tools
- **Cold** — everything else, never touches context directly

The MCP server is the boundary between warm and cold. It promotes the right
things at the right time via pull-based navigation: the model reads summaries
to orient, fetches full source only when it's about to act.

## Architecture

```
your codebase (.go + .proto + .ts/.tsx files)
        ↓  go/packages (type-checked AST) + proto parser + ts-callgraph
    indexer + protoindexer + tsindexer
        ↓  upsert
    store (SQLite)
      ├── symbols table (id, kind, signature, docstring, file, lines, body,
      │                  embedding BLOB, embedding_model TEXT)
      ├── symbols_fts (FTS5 virtual table for text search)
      ├── edges table (from_id, to_id, kind: calls/implements/uses_type)
      └── conventions table (from conventions.yaml)
        ↓  query
    query engine (exact name lookup → FTS → graph expansion → rank → dedup → trim)
        ↓  JSON-RPC over stdio
    MCP server
        ↓  .mcp.json + CLAUDE.md
    Claude Code
```

## Tools Exposed to Claude Code

| Tool | Purpose |
|---|---|
| `get_relevant_context(query)` | Primary tool. Exact name lookup + FTS + graph expansion + ranking. Returns compact pointers (id, kind, signature, file:line) by default — pass `verbose=true` for docstrings/scores. Budget: 600 tokens (precise), 2000 (discovery). Includes a `packages` field grouping hits by package when results span multiple packages. |
| `get_pattern(task)` | Complete vertical slice: proto RPC → request/response messages → Go implementation, with full source bodies. Use before implementing a new RPC. Requires proto indexing; degrades to a single FTS hit otherwise. |
| `get_body(symbol_id)` | Full source of one symbol plus signatures of referenced types/functions (up to 10). Works for any indexed symbol: Go functions, methods, structs, interfaces, **and proto messages, RPCs, enums, services**. Call only when about to read or edit it. |
| `get_flow(symbol_id)` | Full source of a symbol plus caller/callee summaries in one call. Use instead of separate get_body + get_callers + get_callees. |
| `get_impact(symbol_id)` | Full blast radius across layers. Traces proto↔Go name linkage, generated code, callers, implementors, and tests. Use before renaming or changing a type/field. |
| `get_callers(symbol_id)` | Everything that calls this symbol. Falls back to interface/RPC lookup and body-reference heuristics when call graph edges are missing. |
| `get_callees(symbol_id)` | Everything this symbol depends on. Falls back to body-reference extraction. |
| `get_simplest_rpc(service, limit)` | Find the RPCs with the fewest direct callees in their Go implementation, ranked ascending. Returns full vertical slices (proto → messages → Go impl with bodies). Use to pick the cleanest existing example to copy when adding a new RPC. Stubs (zero callees) excluded. `service` is an optional substring filter. |
| `get_unimplemented(service)` | Diff a proto service against Go server methods. Returns which RPCs are missing or stubbed (`codes.Unimplemented`). Call before adding a new RPC. |
| `get_conventions(topic)` | Look up a documented architectural pattern by topic (e.g. "pagination", "auth", "event handler"). Returns the pattern description, pseudocode structure, and resolved example symbols. Falls back to FTS if no convention matches. |

## Design Decisions

**Brief pointers by default, bodies on demand.** `get_relevant_context` returns
compact pointers (id, kind, signature, file:line) — no docstrings or scores
unless `verbose=true`. This enforces the warm→hot boundary: the model orients
with pointers, then promotes to hot via `get_body` only for symbols it's about
to act on. `get_body` returns a `references` field with up to 10 summaries of
referenced types/functions — enough to understand dependencies without extra calls.

**Session boundary rule: `/new` when the next question requires source lines.**
Exploration queries (`get_relevant_context`, `get_impact`, `get_callers`) return
signatures and pointers — cheap, safe to chain in one session. The moment the
next question can only be answered by reading source lines (`get_body`, `get_flow`,
`get_pattern`), that is the trigger: write a 2-3 sentence decision summary, open
a new session, and paste it in. Carrying exploration context into implementation
turns multiplies re-read cost on every subsequent token.

**Display elision: project-relative IDs and paths, text rendering for brief
results.** Symbol IDs and file paths dominate the byte cost of pointer-shaped
responses (measured on core-planning-service: avg ID 112 chars, avg path 113
chars, most of it constant prefix). At render time the engine strips the
module prefix from IDs (`github.com/acme/svc/internal/auth.ValidateToken` →
`internal/auth.ValidateToken`) and makes file paths root-relative. Display-only:
the store always holds full IDs; elided IDs are accepted back as inputs
(`expandID` re-adds the prefix, `FuzzyGetSymbol` suffix-match covers the rest).
Root-package IDs (`github.com/acme/svc.Foo`) are NOT elided — stripping to
`.Foo` would be ambiguous. Elision happens at engine exit points (after
dedup/expand, which need full IDs for store lookups), and in
`GetRelevantContext` specifically *before* `trimToBudget` so the budget is
charged for rendered lengths. `scout serve` resolves the project root from
`--root`, falling back to `--watch`, then the parent of the db's `.scout/`
dir; the module prefix comes from the root's `go.mod`. No root resolved → no
elision (old behavior). The same resolved root now also loads
`.scout/config.yaml` for the embedder, so serve without `--watch` still gets
Phase 3.

The MCP server renders brief `get_relevant_context` results as plain text
(one line per symbol: kind, id, signature, file:line) instead of JSON —
repeated keys and indentation were pure overhead. Everything else renders as
compact `json.Marshal` (never `MarshalIndent`). `trimToBudget`'s cost model
matches these formats (~20 chars/symbol overhead for text lines, ~110 for
verbose JSON), so the 600/2000-token budgets now reflect actual output size —
previously real payloads ran ~45% over budget from serialization overhead.

**Exact name lookup before FTS.** The query engine's primary retrieval path is
exact name lookup for compound identifiers (PascalCase/camelCase). FTS is the
fallback for natural language queries. This solved a major precision problem:
FTS with OR semantics returns too many results for broad terms like "service"
or "shipment", drowning the relevant symbols. Exact lookup scores 3.0 vs FTS
position-based ~0-1.0.

**FTS5 plus optional semantic layer.** SQLite FTS5 handles "find rate limiting",
"find auth middleware" well enough for a codebase where you know the naming
conventions. For cases where the model knows what a thing does but not its name
("the thing that retries failed deliveries"), an opt-in semantic retrieval layer
is being added behind `get_relevant_context`. Vectors are produced by a
locally-running Ollama server (no API keys, no network) and stored as BLOBs on
the existing `symbols` table — no separate vector DB, no CGO dep. Off by
default; enabled via the `scout init` prompt, persisted to `.scout/config.yaml`.

**Status of the semantic layer:** schema, config, init prompt, indexer
integration, embedding preservation across `ResetIndex`, watcher-driven
re-embedding, and query-time vector retrieval are all wired.
`scout index` snapshots vectors before the reset and restores them after the
reindex for symbols whose `name+signature+docstring` survived unchanged — so
a full re-index only embeds new or text-changed symbols. `scout serve
--watch` drains any embedding backlog in a background startup pass, then
re-embeds invalidated symbols after each debounced Go / proto / TS reindex
via a `WatcherConfig.PostReindex` callback, which also calls
`engine.MarkVectorsDirty` so the next semantic query reloads its slab.

### Query Phase 3 (semantic retrieval)

**Where it lives.** `query/engine.go`. The two-phase pipeline is now three:
- Phase 1: exact name lookup (compound identifiers) — unchanged
- Phase 2: FTS — discovery queries only — unchanged
- Phase 3: vector cosine — discovery queries only — new

Precise queries (single compound/dotted identifier) **skip Phase 3
entirely.** They are deterministic by design; fuzzy semantic matches would
add noise without value. An empty Phase 1 still means "this symbol doesn't
exist."

**Engine wiring.** `query.New(s, opts)` takes a `query.Options` struct with
an optional `Embedder embedder.Client`. Nil disables Phase 3 silently —
the pre-semantic behavior. `cmd/scout/serve.go` constructs the client once
via `loadEmbedderClient` and passes it to both the query engine and the
watcher's `buildWatcherEmbedCallback`, so the slab cache and the
re-embedding pass share the same model configuration.

**Vector slab.** On first Phase-3 invocation the engine calls
`store.LoadEmbeddings(model)` and caches the result as a flat slab in
memory. ~14k symbols × 768 dims × 4 bytes = ~42MB. Brute-force cosine
over the slab is ~50ms — fast enough for interactive queries, no need
for an ANN index until the corpus crosses ~100k symbols.

**Slab invalidation.** The watcher's `PostReindex` callback signals the
engine that vectors changed (via a setter on the engine, or a shared
`*atomic.Bool` "dirty" flag). Next Phase-3 call reloads the slab before
scoring. Concurrency: the watcher's callback already serializes embedder
passes, so the slab is reloaded at most once per debounced reindex.

**Per-query embedding.** Each discovery query incurs one HTTP round-trip
to Ollama (~50–200ms on a laptop) to embed the query string. The result
is **not** cached across queries — the eval corpus is small enough that
hit rate would be low, and an LRU adds complexity. Revisit if profiles
show this dominating latency.

**Scoring integration.** Cosine similarity sits in [-1, 1]. Mapped to the
existing FTS/exact-match scoring scale:
- `vectorScore = cosine × 1.5` (puts top hits near FTS's ~1.0 range)
- Then the existing `kindWeight`, `implBoost`, `generatedPenalty` apply
- Top-K cap (heap-based, K = ~30) keeps the scoring pool manageable
- Existing `dedup()` and `prioritizeSource` passes apply unchanged

**Failure modes — all silent skips, never error.**
- No embedder configured → Phase 3 doesn't run (today's behavior)
- Embedder unreachable at query time → log once, return FTS-only results
- Stored vector dim ≠ query embedding dim → log once, skip the slab,
  return FTS-only (signals a model change without a reindex)
- Slab empty → skip Phase 3; queries succeed via FTS

**Recall benchmark.** `eval/semantic_fixtures.yaml` holds meaning-based
fixtures (e.g. "the component that re-runs the indexer when files on disk
change"). `TestSemanticFixtures` runs them against an Ollama-backed engine
when `SCOUT_OLLAMA_HOST` and `SCOUT_OLLAMA_MODEL` are set; otherwise the
test is skipped so CI without Ollama stays green. The FTS-only baseline
remains `eval/fixtures.yaml` + `TestGoldenFixtures`. Compare must_include
hit rate across the two to measure the recall delta the layer provides.

**Not in Phase 3 scope.** Vector quantization (8-bit packed BLOBs to cut
slab size 4x). ANN index (HNSW, IVF). Hybrid score fusion algorithms
(RRF, weighted ranking). These are layers we add later if recall is
worth the complexity.

**CLAUDE.md is required for tool adoption.** Claude Code does not auto-inject
MCP `prompts/get` results, and tool descriptions alone are not enough to make
Claude use scout tools. A project CLAUDE.md with explicit directives ("Always
use scout tools first. Not grep. Not find. Not Read.") is required. The README
includes a template.

**One MCP server, thin wrapper.** The MCP server is ~200 lines of JSON-RPC
over stdio. All the logic lives in the indexer and query engine as plain Go
packages. The server just wires them together.

**Single binary, subcommand CLI.** Everything ships as one `scout` binary with
subcommands: `scout init`, `scout index`, `scout reindex`, `scout serve`. One
`go install` installs the full tool. The target machine needs Go available because
`go/packages` shells out to `go list` to resolve types.

**Dependency signatures on demand.** With `--deps`, the indexer walks
`pkg.Imports` for each target package, collects unique external `*types.Package`
values (skipping stdlib and target module), and extracts exported symbols from
their `Scope()`. Only signatures are stored (no bodies). Methods on
proto-generated dep types are filtered out to avoid boilerplate bloat. This
keeps the agent in scout's fast path when it encounters imported types like
`grpc.ClientConn` or `codes.NotFound`.

## File Structure

```
scout/
├── cmd/
│   └── scout/
│       ├── main.go   — Umbrella CLI: dispatches to subcommands
│       ├── index.go  — scout index / scout reindex subcommands
│       ├── serve.go  — scout serve subcommand (MCP server)
│       └── init.go   — scout init subcommand (TUI setup wizard)
├── indexer/
│   ├── indexer.go          — Parses Go packages via go/packages, extracts
│   │                         symbols and call edges, writes to store
│   ├── deps.go             — Indexes exported signatures from external
│   │                         dependency packages (--deps flag)
│   ├── conventions.go      — Reads conventions.yaml from the indexed project,
│   │                         upserts entries into the conventions table
│   ├── light.go            — Fast AST-only reindex (~50ms): updates symbols
│   │                         and line numbers without type-checking
│   └── watcher.go          — fsnotify file watcher with hybrid reindex:
│                             light immediately, full after debounce
├── protoindexer/
│   └── indexer.go          — Parses .proto files, extracts services, RPCs,
│                             messages, and enums, writes to store
├── tsindexer/
│   └── indexer.go          — Shells out to ts-callgraph, parses JSON output,
│                             writes TypeScript symbols and edges to store
├── store/
│   ├── store.go            — SQLite schema + CRUD: symbols, FTS, edges
│   └── embeddings.go       — Vector BLOB codec + per-model upsert/load/list
├── query/
│   └── engine.go           — FTS search, graph expansion, ranking, trimming
├── mcp/
│   └── server.go           — JSON-RPC 2.0 over stdio, MCP protocol
├── config/
│   └── config.go           — Read/write .scout/config.yaml (embedder block)
├── embedder/
│   ├── ollama.go           — Ollama client: probe + /api/embed batch call
│   └── run.go              — Orchestration: list unembedded, batch, write back
├── scripts/
│   └── pre-commit          — Git hook for incremental reindex on commit
├── .mcp.json               — Wires MCP server into Claude Code
└── go.mod                  — module github.com/urechandro/scout
```

## Key Implementation Details

### indexer/indexer.go
- Uses `golang.org/x/tools/go/packages` with `NeedSyntax | NeedTypes |
  NeedTypesInfo` load mode for full type resolution
- Extracts functions, methods, types, structs, interfaces per file
- Resolves call edges by walking `*ast.CallExpr` nodes and looking up
  `pkg.TypesInfo.Uses[ident]`
- Skips packages with load errors (logs warning, continues) — important for
  large codebases where some packages may fail to resolve
- Incremental mode: `RunFiles([]string)` deletes stale symbols for given files
  then re-parses only affected packages

### indexer/deps.go
- Walks `pkg.Imports` for each loaded target package, collects unique external
  `*types.Package` values (skips stdlib and target module)
- Extracts exported symbols from `types.Package.Scope()`: funcs, types
  (struct/interface/type), consts, vars — signatures only, no bodies
- For non-proto named types, also indexes exported methods via `Named.NumMethods()`
- Proto-generated dep types (detected via `isProtoGenerated` on file path) skip
  method indexing to avoid boilerplate bloat (Reset, ProtoMessage, etc.)
- File paths point to Go module cache (`~/go/pkg/mod/...`)
- Uses same `qualifiedID` format as call graph edges, so existing edges
  from target code to dep functions now resolve to real symbols
- `detectModulePath` reads `go.mod` to determine target module prefix

### indexer/conventions.go
- Looks for `conventions.yaml` or `conventions.yml` in the indexed project root
- If absent, silently skips (conventions are optional)
- Parses YAML into `conventionYAML` structs and upserts each into the `conventions` table
- Called automatically by the indexer after Go and proto indexing

### indexer/light.go
- `RunFilesLight(files)`: fast AST-only reindex (~50ms per file including DB ops)
- Parses with `go/parser` — no `go/packages`, no type-checking
- Builds symbol IDs in the same `pkg/path.Type.Method` format as the full indexer
- `resolvePackagePath`: walks up to find `go.mod`, computes package path from
  relative directory — avoids needing `go/packages` for package resolution
- Extracts AST-level call edges: same-package calls by name, cross-package via
  store lookup (`GetByName`). Accurate for unambiguous names, skips ambiguous
- Bodies read from disk (same as full indexer) for non-generated files
- Used by the watcher for immediate on-save updates

### indexer/watcher.go
- `Watcher`: fsnotify-based file watcher with hybrid two-phase reindex
- Phase 1 (immediate): `RunFilesLight` on save — fixes line numbers in ~50ms
- Phase 2 (debounced): `RunFiles` after configurable delay (default 2s) — restores
  accurate call edges and type information
- Watches recursively, auto-adds new directories, skips hidden/vendor/node_modules
- Triggers on `.go` and `.proto` file writes/creates
- Go files: two-phase (light immediate + full debounced)
- Proto files: immediate single-phase reindex (parser is fast, no type-checking)
- TS/TSX files: debounced-only full reindex (TS compiler needs whole program)
- `.d.ts` files are ignored
- Debounce batches rapid saves into a single reindex per language
- Started as a goroutine in the MCP server via `--watch` flag

### protoindexer/indexer.go
- Walks the configured directory for `.proto` files
- Extracts services, RPCs, messages, and enums as symbols
- Uses a line-by-line parser (no protoc dependency)
- Tracks `blockSymIdx` in parse state to update LineEnd when closing braces are
  found — this gives get_body full proto message/enum/service definitions
- `RunFiles(files)`: incremental reindex for specific proto files, used by watcher
- Supports `ExcludePaths` to skip generated or vendor directories

### tsindexer/indexer.go
- Shells out to `ts-callgraph` CLI (or `node /path/to/cli.js`) via `exec.Command`
- Parses JSON stdout into Go structs matching ts-callgraph's `Output` type
- Symbol kinds from TS: "func", "method", "class", "interface", "type", "enum", "const"
- Edge kinds: "calls", "implements", "uses_type" — same as Go indexer
- `Run()`: full reindex — clears stale TS files not in output, upserts all symbols/edges
- `RunFiles()`: delegates to `Run()` because the TS compiler needs the full program
- `Config.Command` + `Config.CommandArgs` allow `node /path/to/cli.js` invocation
- Requires `ts-callgraph` npm package (github.com/urechandro/ts-callgraph)

### store/store.go
- `symbols` table: primary store, keyed by fully-qualified symbol ID
  e.g. `github.com/einride/core-planning-service/auth.ValidateToken`
- `symbols_fts`: FTS5 virtual table, **standalone** (no `content=` option —
  this caused corruption bugs). Synced manually via delete+insert in UpsertSymbol
- `edges` table: directed graph edges with kind (calls/implements/uses_type)
- `conventions` table: populated from conventions.yaml by the indexer
- Body field stores a line reference `/* file.go:42-67 */` not full source,
  keeping the index small. Full source read from disk via `get_body`.
- `embedding` BLOB + `embedding_model` TEXT columns hold the optional
  per-symbol vector. Nullable: rows without a vector are simply skipped at
  query time. `migrate()` adds them in place on existing DBs via a tolerant
  `addColumnIfMissing` helper (SQLite has no `ADD COLUMN IF NOT EXISTS`).
- `GetByName`, `GetByNameAndKind`: exact name lookups used by Phase 1 retrieval
- `SearchFTSByKinds`: kind-filtered FTS queries (used by get_pattern to search
  RPCs directly instead of getting mixed results)
- `FuzzyGetSymbol`: suffix + name matching for partial/guessed symbol IDs

### store/embeddings.go
- `EncodeEmbedding` / `DecodeEmbedding`: little-endian float32 pack/unpack
  for the BLOB column. No header — length derived from `len(blob)/4`.
- `UpsertEmbedding(id, model, vec)`: writes vector + source model. Does not
  create the symbol row; callers must `UpsertSymbol` first.
- `LoadEmbeddings(model)`: returns every `(id, vector)` for the named model.
  The query engine loads this once into an in-memory slab and brute-forces
  cosine against it; 14k × 768-dim × 4 bytes = ~43MB resident, ~50ms scan.
- `ListUnembedded(model, limit)`: symbols whose stored embedding is missing
  or was produced by a different model. The indexer uses this to backfill
  vectors incrementally instead of re-embedding the whole corpus on every
  model change.
- `CountEmbeddings(model)`: status counter for tests and CLI output.
- Model-name filter on every query is deliberate: it prevents accidentally
  comparing vectors across embedding model versions, which would silently
  return garbage relevance scores.

### config/config.go
- Owns `.scout/config.yaml`. Schema is a single optional `Embedder` block
  (`kind`, `host`, `model`). All fields have `omitempty` so the file can
  grow without breaking older binaries.
- `Load(rootDir)` returns a zero Config when the file is absent — callers
  do not need to special-case "no embedder configured".
- `Save(rootDir, cfg)` creates `.scout/` if needed and writes atomically
  enough for this use case (single-process init).
- `Validate()` rejects unknown `kind` values and missing fields. Called from
  both Load and Save so a hand-edited file is caught at open time.
- `DefaultOllamaConfig()` is the recommended starter (`nomic-embed-text`
  via `http://localhost:11434`), used by the `scout init` wizard.

### embedder/ollama.go
- Exposes a `Client` interface (`Embed`, `Model`) the orchestrator and (later)
  the query engine call into. Tiny on purpose so a second backend slots in
  without touching callers.
- `OllamaClient.Embed` POSTs to `/api/embed` with `{"model": ..., "input":
  [...]}` and returns vectors in input order. Validates that the response
  vector count matches the input count — Ollama has been known to silently
  truncate on overflow, which would silently corrupt the index.
- `ProbeOllama` is a standalone helper (not on the client) because it runs
  during `scout init` before any client is configured. Probe hits `/api/tags`
  with a 3s timeout, treats transport errors as `Reachable=false` rather
  than returning an error.
- `matchesModel` matches a bare model name (`nomic-embed-text`) against
  Ollama's tagged names (`nomic-embed-text:latest`). Users configure the
  bare form; Ollama always reports the tagged form.

### embedder/run.go
- `Run(ctx, store, client, opts)` is the embedder pass invoked by
  `scout index`. Loads `store.ListUnembedded(model)`, batches by
  `Options.BatchSize` (default 32), pushes each batch through the client,
  writes vectors back via `store.UpsertEmbedding`.
- **Per-batch fault tolerance:** an embedder error logs and skips the batch,
  bumping `Stats.Failed`, but the pass continues. Rationale: with 14k symbols
  and a flaky network, an all-or-nothing pass would leave a corpus with zero
  vectors. Skipped rows stay unembedded, so the next `scout index` retries
  them automatically.
- `EmbeddingText(sym)` produces the string fed to the embedder: signature
  (always) + `\n` + docstring (when present). Body is deliberately excluded
  — bodies churn far more often than signatures, and including them would
  burn embedder budget on no-op re-embeds plus noise up the vector space.
- `Options.Limit` caps the pass for callers that want to amortize cost
  (e.g. a watcher embedding only recent changes — not wired yet).

### query/engine.go
- **Query type detection** (`classifyQuery`): classifies queries upfront as
  `queryPrecise` (single compound/dotted identifier like `CreateShipmentLeg` or
  `grpc.Dial`) or `queryDiscovery` (multi-word natural language like "how does
  auth work"). Precise queries get a 600-token budget and skip FTS entirely.
  Discovery queries get a 2000-token budget.
- **Two-phase retrieval** in `GetRelevantContext`:
  - Phase 1: exact name lookup for compound identifiers (`extractCompoundIdents`
    detects PascalCase/camelCase). Score 3.0 + kindWeight + implBoost + generatedPenalty
  - Phase 2: FTS fallback for discovery queries only. Precise queries never
    fall through to FTS — if Phase 1 misses, the response is empty, which
    clearly signals "this symbol doesn't exist." Fetches 3× the FTS limit, partitions into source vs
    generated, caps generated at 5 (or unlimited if no source hits), then
    scores. Position-based score + nameMatchBonus + termCoverage + kindWeight +
    implBoost + generatedPenalty
- **Scoring pipeline**: multi-signal, additive
  - `kindWeight`: methods +0.3, funcs +0.2, interfaces +0.1, structs -0.3
  - `nameMatchBonus`: +1.0 for func/method, +0.7 interface, +0.3 others (skipped
    when name frequency ≥ 3 to suppress boilerplate like Validate)
  - `termCoverage`: quadratic reward for matching multiple query terms
    (`coverage² × 1.5`). Uses decomposed compound parts for scoring only (not
    added to FTS query — that broadened results)
  - `implBoost`: +0.5 for methods/funcs in svc/server/service packages
  - `generatedPenalty`: -0.6 for .pb.go and .pb.gw.go files
- **Dedup** (`dedup()`): two passes
  1. Gen-path dedup: same name+kind across multiple /gen/ directories → keep
     backend copy only. Critical for codebases with multiple generated copies
  2. Name dedup: when 3+ symbols share a name (e.g. Validate on every resource
     type), keep highest-scored, penalize by group size (−0.15 per extra)
- `getPatternSlices`: exact RPC name lookup first, FTS by kind as fallback.
  Builds a `PatternSlice` (proto RPC → request/response messages → Go impl)
  with full bodies for get_pattern, summaries-only for conventions
- `expand`: BFS over call graph from seeds, depth 1 by default
  - Callers scored 0.4/depth, callees 0.3/depth
- `buildPackageSummary`: groups returned symbols by directory, strips common path
  prefix for short relative paths, returns per-package hit count + kind breakdown.
  Omitted when results are single-symbol or single-package
- `prioritizeSource`: reorders ranked results so source symbols come before
  generated ones, preserving score order within each group
- `trimToBudget`: greedy fill, ~4 chars per token estimate, budget varies by
  query type (600 for precise, 2000 for discovery). Uses smaller cost estimate
  in brief mode (no docstring/why in calculation)
- `buildFTSQuery`: strips stop words, ORs remaining terms. Dynamic FTS limit:
  `30 + len(queryTerms)*10`
- Compound identifier utilities: `extractCompoundIdents`, `extractCompoundParts`,
  `isCompoundIdent`, `decomposeIdentifier`
- `extractReferences`: called by `GetBody`, extracts identifiers from the symbol's
  body via `extractCallIdents` (call sites) and `extractTypeIdents` (PascalCase
  type names), looks them up in the symbol table, returns up to 10 summaries.
  Skips self. Filters generated symbols (`.pb.go`, `.pb.gw.go`, `/gen/` paths)
  via `isGenerated` to avoid flooding results with gRPC stubs
- **`GetImpact`**: cross-layer blast radius analysis in 6 phases:
  1. Same-name lookup across layers (proto↔Go name linkage via `GetByName`)
  2. RPC request/response message discovery (via `parseRPCMessages`)
  3. Interface/RPC implementation chain (`GetImplements`)
  4. Reverse: implementors of interfaces/RPCs (`GetImplementors`)
  5. Callers of all discovered non-generated Go symbols (`GetCallers`)
  6. Body-reference fallback (`GetCallersFromBody`)
  Results classified into layers (proto/generated/implementation/test) by
  `classifyLayer` using file extension heuristics

### mcp/server.go
- Pure JSON-RPC 2.0 over stdin/stdout, no SDK dependency
- Logs go to stderr only (stdout is reserved for the MCP protocol)
- 4MB scanner buffer for large responses
- Tool schemas embedded directly — no external schema files

## Benchmark Results

Tested on a production Go codebase (~14k symbols, 78% generated) vs Opus 4.6
without scout (grep/find/Read only). Three task types, all using Opus:

| Task type | No Scout | With Scout | Savings |
|---|---|---|---|
| Broad discovery ("how is auth enforced?") | $8.61, 49 turns | $1.73, 7 turns | **80% cost, 86% turns** |
| Implementation planning ("implement ArchiveShipmentLeg") | $6.18, 29 turns | $0.86, 3 turns | **86% cost, 90% turns** |
| Targeted tracing ("validation before UpdateShipmentLeg") | $1.73, 13 turns | $1.53, 5 turns | **12% cost, 62% turns** |

Key findings:
- **Brief mode is critical.** Compact pointers prevent context accumulation
  across turns. Without brief, targeted queries were 2.4x *worse* than no-scout.
- **`get_pattern`** is the standout tool. One call replaces 10+ file reads.
- **CLAUDE.md discipline matters as much as the tool implementation.** The
  "Only if" gating on playbook step 2 prevents autopilot exploration — model
  stops after step 1 when it has enough context.
- **"Search for the analog, not the new thing"** — when implementing something
  new, call `get_pattern` on the closest existing RPC, not the non-existent one.

## Known Issues / Next Steps

### Housekeeping
- Pre-commit hook not tested end-to-end.
- Store and query engine have tests; indexer and MCP server do not yet.

## Dependencies

```
golang.org/x/tools v0.26.0   — go/packages for type-checked AST parsing
modernc.org/sqlite v1.30.0   — pure Go SQLite driver (CGO_ENABLED=1 for perf)
ts-callgraph (npm)           — TypeScript symbol/edge extraction (optional, for TS indexing)
```

## Running It

### Install
```sh
go install github.com/urechandro/scout/cmd/scout@latest
```

### Bootstrap a new project
```sh
# Interactive TUI wizard: sets up .scout/, .mcp.json, CLAUDE.md block,
# optional conventions.yaml, optional semantic search (Ollama), then
# runs a full index.
scout init
```

Flags (all optional — TUI prompts for them interactively):
- `--root` — project root (default: `.`)
- `--db` — database path (default: `<root>/.scout/index.db`)
- `--tsconfig` — tsconfig.json for TypeScript (auto-detected)
- `--ts-command` — ts-callgraph binary (default: `ts-callgraph`)
- `--exclude` — comma-separated package substrings to skip
- `--skip-index` — write config files only, skip the indexer
- `--yes` / `-y` — non-interactive, accept all defaults (CI-safe; semantic
  search stays off in this mode — opting in requires the TUI)

Re-running `scout init` is safe: it merges `.mcp.json`, replaces the
`<!-- scout -->` block in CLAUDE.md idempotently, skips `conventions.yaml`
if it already exists, and overwrites `.scout/config.yaml` with the latest
wizard answers.

### Optional: semantic search

The wizard's final prompt asks whether to enable semantic search via Ollama.
Saying yes writes an `embedder` block to `.scout/config.yaml`:

```yaml
embedder:
  kind: ollama
  host: http://localhost:11434
  model: nomic-embed-text
```

The wizard then probes `<host>/api/tags` and prints a warning if Ollama is
not running or the model is not pulled. Init does not fail on a bad probe —
the user can `ollama pull nomic-embed-text` afterwards.

Saying no writes no `embedder` block; scout queries continue using
exact-name + FTS only.

### Full index
```sh
scout index --db /your/project/.scout/index.db --root /your/project
scout index --db /your/project/.scout/index.db --root /your/project --deps
```

`--deps` indexes exported signatures from external dependency packages.

### Incremental reindex
```sh
scout reindex --db /your/project/.scout/index.db \
  --files path/to/changed.go,other.go
```

### Full index with TypeScript
```sh
scout index --db /your/project/.scout/index.db --root /your/project \
  --tsconfig /your/project/tsconfig.json \
  --ts-command "node /path/to/ts-callgraph/dist/cli.js"
```

### Inspect the index
```sh
sqlite3 /your/project/.scout/index.db
SELECT COUNT(*) FROM symbols;
SELECT id, kind, signature FROM symbols LIMIT 20;
SELECT COUNT(*) FROM edges;
```

### Browse the index (Datasette)
```sh
datasette /your/project/.scout/index.db --host 0.0.0.0 --port 8001
```
Then open http://localhost:8001.

### Document conventions (optional)

Copy the example file into your project root and fill it in:

```sh
cp /path/to/scout/conventions.example.yaml /your/project/conventions.yaml
```

Each entry documents one architectural pattern:

```yaml
- name: my-pattern          # unique slug
  terms:                    # search terms that trigger this convention
    - pattern name
    - related keywords
  description: |            # what the pattern is and WHY it exists
    ...
  structure: |              # pseudocode showing the repeating shape
    ...
  examples:                 # symbol IDs (suffix is fine, fuzzy-matched)
    - pkg.TypeName.MethodName
```

The indexer loads `conventions.yaml` automatically on every run. Claude calls
`get_conventions("topic")` to retrieve the pattern before implementing it.

### Wire up Claude Code
`scout init` generates `.mcp.json` automatically. To wire it up manually:

```json
{
  "mcpServers": {
    "scout": {
      "type": "stdio",
      "command": "scout",
      "args": ["serve", "--db", "/your/project/.scout/index.db",
               "--watch", "/your/project"],
      "env": {}
    }
  }
}
```

For TypeScript projects, add `--tsconfig` and optionally `--ts-command`:
```json
{
  "mcpServers": {
    "scout": {
      "type": "stdio",
      "command": "scout",
      "args": ["serve", "--db", "/your/project/.scout/index.db",
               "--watch", "/your/project",
               "--tsconfig", "/your/project/tsconfig.json",
               "--ts-command", "node /path/to/ts-callgraph/dist/cli.js"],
      "env": {}
    }
  }
}
```

The `--watch` flag enables live reindexing: when a `.go` file is saved, the
server updates symbols and line numbers immediately (~50ms via AST-only parse),
then runs a full type-checked reindex after a 2s debounce (~1.5-2s) to restore
accurate call edges. When `--tsconfig` is set, `.ts`/`.tsx` saves trigger a
debounced full reindex via ts-callgraph. Without `--watch`, the index is static.

If `scout-server` is not on your `$PATH`, use the full path (e.g. `~/go/bin/scout-server`).
