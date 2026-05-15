# scout — Project Summary

## What We Are Building

A **Model Context Protocol (MCP) server** that gives Claude Code fast,
token-efficient navigation of a Go codebase. Instead of Claude reading files
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
your codebase (.go + .proto files)
        ↓  go/packages (type-checked AST) + proto parser
    indexer + protoindexer
        ↓  upsert
    store (SQLite)
      ├── symbols table (id, kind, signature, docstring, file, lines, body)
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
| `get_relevant_context(query)` | Primary tool. Exact name lookup + FTS + graph expansion + ranking. Returns symbol summaries within a 4k token budget. Includes a `packages` field grouping hits by package with counts and kinds when results span multiple packages. |
| `get_pattern(task)` | Complete vertical slice: proto RPC → request/response messages → Go implementation, with full source bodies. Use before implementing a new RPC. Requires proto indexing; degrades to a single FTS hit otherwise. |
| `get_body(symbol_id)` | Full source of one symbol plus signatures of referenced types/functions (up to 20). Works for any indexed symbol: Go functions, methods, structs, interfaces, **and proto messages, RPCs, enums, services**. Call only when about to read or edit it. |
| `get_flow(symbol_id)` | Full source of a symbol plus caller/callee summaries in one call. Use instead of separate get_body + get_callers + get_callees. |
| `get_impact(symbol_id)` | Full blast radius across layers. Traces proto↔Go name linkage, generated code, callers, implementors, and tests. Use before renaming or changing a type/field. |
| `get_callers(symbol_id)` | Everything that calls this symbol. Falls back to interface/RPC lookup and body-reference heuristics when call graph edges are missing. |
| `get_callees(symbol_id)` | Everything this symbol depends on. Falls back to body-reference extraction. |
| `get_unimplemented(service)` | Diff a proto service against Go server methods. Returns which RPCs are missing or stubbed (`codes.Unimplemented`). Call before adding a new RPC. |
| `get_conventions(topic)` | Look up a documented architectural pattern by topic (e.g. "pagination", "auth", "event handler"). Returns the pattern description, pseudocode structure, and resolved example symbols. Falls back to FTS if no convention matches. |

## Design Decisions

**Summaries by default, bodies on demand.** `get_relevant_context` returns
signatures + docstrings, not full source. This keeps the tool result small.
The model calls `get_body` only for symbols it's about to act on. When it
does, `get_body` also returns a `references` field with summaries (signatures
+ locations) of types and functions referenced in the body — this eliminates
follow-up calls to understand dependencies before editing.

**Exact name lookup before FTS.** The query engine's primary retrieval path is
exact name lookup for compound identifiers (PascalCase/camelCase). FTS is the
fallback for natural language queries. This solved a major precision problem:
FTS with OR semantics returns too many results for broad terms like "service"
or "shipment", drowning the relevant symbols. Exact lookup scores 3.0 vs FTS
position-based ~0-1.0.

**FTS5 only, no vector store.** SQLite FTS5 handles "find rate limiting",
"find auth middleware" well enough for a codebase where you know the naming
conventions. Vector search (LanceDB) can be added later as a second layer for
semantic gap cases — the query engine is designed with this seam in mind.

**CLAUDE.md is required for tool adoption.** Claude Code does not auto-inject
MCP `prompts/get` results, and tool descriptions alone are not enough to make
Claude use scout tools. A project CLAUDE.md with explicit directives ("Always
use scout tools first. Not grep. Not find. Not Read.") is required. The README
includes a template.

**One MCP server, thin wrapper.** The MCP server is ~200 lines of JSON-RPC
over stdio. All the logic lives in the indexer and query engine as plain Go
packages. The server just wires them together.

**Go install, no container.** The indexer and server are installed as plain Go
binaries via `go install`. The target machine needs Go available because
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
│   ├── scout-index/main.go — CLI: run full or incremental index
│   └── scout-server/main.go — CLI: run MCP server over stdio
├── indexer/
│   ├── indexer.go          — Parses Go packages via go/packages, extracts
│   │                         symbols and call edges, writes to store
│   ├── deps.go             — Indexes exported signatures from external
│   │                         dependency packages (--deps flag)
│   └── conventions.go      — Reads conventions.yaml from the indexed project,
│                             upserts entries into the conventions table
├── protoindexer/
│   └── indexer.go          — Parses .proto files, extracts services, RPCs,
│                             messages, and enums, writes to store
├── store/
│   └── store.go            — SQLite schema + CRUD: symbols, FTS, edges
├── query/
│   └── engine.go           — FTS search, graph expansion, ranking, trimming
├── mcp/
│   └── server.go           — JSON-RPC 2.0 over stdio, MCP protocol
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

### protoindexer/indexer.go
- Walks the configured directory for `.proto` files
- Extracts services, RPCs, messages, and enums as symbols
- Uses a line-by-line parser (no protoc dependency)
- Tracks `blockSymIdx` in parse state to update LineEnd when closing braces are
  found — this gives get_body full proto message/enum/service definitions
- Supports `ExcludePaths` to skip generated or vendor directories

### store/store.go
- `symbols` table: primary store, keyed by fully-qualified symbol ID
  e.g. `github.com/einride/core-planning-service/auth.ValidateToken`
- `symbols_fts`: FTS5 virtual table, **standalone** (no `content=` option —
  this caused corruption bugs). Synced manually via delete+insert in UpsertSymbol
- `edges` table: directed graph edges with kind (calls/implements/uses_type)
- `conventions` table: populated from conventions.yaml by the indexer
- Body field stores a line reference `/* file.go:42-67 */` not full source,
  keeping the index small. Full source read from disk via `get_body`.
- `GetByName`, `GetByNameAndKind`: exact name lookups used by Phase 1 retrieval
- `SearchFTSByKinds`: kind-filtered FTS queries (used by get_pattern to search
  RPCs directly instead of getting mixed results)
- `FuzzyGetSymbol`: suffix + name matching for partial/guessed symbol IDs

### query/engine.go
- **Query type detection** (`classifyQuery`): classifies queries upfront as
  `queryPrecise` (single compound/dotted identifier like `CreateShipmentLeg` or
  `grpc.Dial`) or `queryDiscovery` (multi-word natural language like "how does
  auth work"). Precise queries get a 1000-token budget and skip FTS entirely.
  Discovery queries get the full 4000-token budget.
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
  query type (1000 for precise, 4000 for discovery)
- `buildFTSQuery`: strips stop words, ORs remaining terms. Dynamic FTS limit:
  `30 + len(queryTerms)*10`
- Compound identifier utilities: `extractCompoundIdents`, `extractCompoundParts`,
  `isCompoundIdent`, `decomposeIdentifier`
- `extractReferences`: called by `GetBody`, extracts identifiers from the symbol's
  body via `extractCallIdents` (call sites) and `extractTypeIdents` (PascalCase
  type names), looks them up in the symbol table, returns up to 20 summaries.
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

## What Works Well (Dogfooding Results)

Tested on a production Go codebase (~14k symbols, 78% generated). Key findings:

- **`get_pattern`** is the standout tool. Returns a complete vertical slice
  (proto RPC → messages → Go impl) with full source in one call. Saves 1-2
  round trips vs manual exploration. Nails the "add a new RPC following this
  pattern" use case.
- **`get_conventions`** saves 3-4 round trips when implementing cross-cutting
  patterns (pagination, outbox, auth). The structured pseudocode + resolved
  examples give Claude enough context to implement correctly on the first try.
- **`get_relevant_context`** is best for cross-cutting discovery ("who uses this
  pattern?", "where is auth enforced?") — not for surgical single-symbol lookups.
- **Exact name lookup** is the primary retrieval path. When the model includes a
  specific symbol name like `CreateShipmentLeg`, it gets a direct hit (score 3.0)
  instead of wading through FTS noise.

## Known Issues / Next Steps

### High value
- **"Find simplest example" queries** — "which RPC has the fewest dependencies?"
  isn't expressible in the current tool set. The model has to call get_pattern
  on several RPCs and compare manually.

### Housekeeping
- No tests yet.
- Pre-commit hook not tested end-to-end.

## Dependencies

```
golang.org/x/tools v0.26.0   — go/packages for type-checked AST parsing
modernc.org/sqlite v1.30.0   — pure Go SQLite driver (CGO_ENABLED=1 for perf)
```

## Running It

### Install
```sh
go install github.com/urechandro/scout/cmd/scout-index@latest
go install github.com/urechandro/scout/cmd/scout-server@latest
```

### Full index
```sh
scout-index --db /your/project/.scout/index.db --root /your/project
```

### Full index with dependency signatures
```sh
scout-index --db /your/project/.scout/index.db --root /your/project --deps
```

`--deps` indexes exported signatures (no bodies) from external dependency
packages. This makes imported types like `grpc.ClientConn`, `codes.NotFound`,
and SDK types discoverable via FTS and exact name lookup. Proto-generated dep
methods are filtered out to avoid boilerplate bloat.

### Incremental reindex
```sh
scout-index --db /your/project/.scout/index.db --root /your/project \
  --files path/to/changed.go,other.go
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
Add to `.mcp.json` in your project root. Claude Code picks it up automatically.

```json
{
  "mcpServers": {
    "scout": {
      "type": "stdio",
      "command": "scout-server",
      "args": ["--db", "/your/project/.scout/index.db"],
      "env": {}
    }
  }
}
```

If `scout-server` is not on your `$PATH`, use the full path (e.g. `~/go/bin/scout-server`).
