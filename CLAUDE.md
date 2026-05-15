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
      └── edges table (from_id, to_id, kind: calls/implements/uses_type)
        ↓  query
    query engine (FTS search → graph expansion → rank → trim to token budget)
        ↓  JSON-RPC over stdio
    MCP server
        ↓  .mcp.json
    Claude Code
```

## Tools Exposed to Claude Code

| Tool | Purpose |
|---|---|
| `get_relevant_context(task)` | Primary tool. FTS search + graph expansion + ranking. Returns symbol summaries within a token budget. Call before making any changes. |
| `get_body(symbol_id)` | Full source of one symbol. Call only when about to read or edit it. |
| `get_callers(symbol_id)` | Everything that calls this symbol. Call before changing a function signature. |
| `get_callees(symbol_id)` | Everything this symbol depends on. |
| `get_conventions(topic)` | Look up a documented architectural pattern by topic (e.g. "pagination", "auth", "event handler"). Returns the pattern description, pseudocode structure, and example symbols. Falls back to FTS symbol search if no documented convention matches. |

## Design Decisions

**Summaries by default, bodies on demand.** `get_relevant_context` returns
signatures + docstrings, not full source. This keeps the tool result small.
The model calls `get_body` only for symbols it's about to act on.

**FTS5 only, no vector store.** SQLite FTS5 handles "find rate limiting",
"find auth middleware" well enough for a codebase where you know the naming
conventions. Vector search (LanceDB) can be added later as a second layer for
semantic gap cases — the query engine is designed with this seam in mind.

**One MCP server, thin wrapper.** The MCP server is ~200 lines of JSON-RPC
over stdio. All the logic lives in the indexer and query engine as plain Go
packages. The server just wires them together.

**Go install, no container.** The indexer and server are installed as plain Go
binaries via `go install`. The target machine needs Go available because
`go/packages` shells out to `go list` to resolve types.

## File Structure

```
scout/
├── cmd/
│   ├── indexer/main.go     — CLI: run full or incremental index
│   └── server/main.go      — CLI: run MCP server over stdio
├── indexer/
│   ├── indexer.go          — Parses Go packages via go/packages, extracts
│   │                         symbols and call edges, writes to store
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

### indexer/conventions.go
- Looks for `conventions.yaml` or `conventions.yml` in the indexed project root
- If absent, silently skips (conventions are optional)
- Parses YAML into `conventionYAML` structs and upserts each into the `conventions` table
- Called automatically by the indexer after Go and proto indexing

### protoindexer/indexer.go
- Walks the configured directory for `.proto` files
- Extracts services, RPCs, messages, and enums as symbols
- Uses a line-by-line parser (no protoc dependency)
- Supports `ExcludePaths` to skip generated or vendor directories

### store/store.go
- `symbols` table: primary store, keyed by fully-qualified symbol ID
  e.g. `github.com/einride/core-planning-service/auth.ValidateToken`
- `symbols_fts`: FTS5 virtual table, **standalone** (no `content=` option —
  this caused corruption bugs). Synced manually via delete+insert in UpsertSymbol
- `edges` table: directed graph edges with kind (calls/implements/uses_type)
- Body field stores a line reference `/* file.go:42-67 */` not full source,
  keeping the index small. Full source read from disk via `get_body`.

### query/engine.go
- `buildFTSQuery`: strips stop words, ORs remaining terms
- `expand`: BFS over call graph from FTS hits, depth 1 by default
  - Callers scored 0.4/depth (higher — blast radius matters)
  - Callees scored 0.3/depth
- `trimToBudget`: greedy fill, ~4 chars per token estimate
- Returns `SymbolSummary` (no body) + `GraphContext` (caller/callee IDs)

### mcp/server.go
- Pure JSON-RPC 2.0 over stdin/stdout, no SDK dependency
- Logs go to stderr only (stdout is reserved for the MCP protocol)
- 4MB scanner buffer for large responses
- Tool schemas embedded directly — no external schema files

## Known Issues / Next Steps

### Vision: task-shaped queries, not symbol lookups

The current tool thinks in symbols (one at a time). What's actually needed is
tools that think in tasks — multi-layer, spanning proto → generated Go → server
implementation → tests in a single query. Prioritised next steps:

### High value
- **`get_unimplemented(service)`** — diff proto service definition against Go
  server struct, return which RPCs are missing or stubbed. Gap-filling tasks
  need to know what doesn't exist yet before they can add it.

### Medium value
- **Cross-layer impact** — "what's affected if I change this proto field?" spans
  proto → generated Go → server code → tests. Current `get_callers` is Go-only.

### Housekeeping
- Build a `scout` CLI (Go, `cmd/cli`) to replace `scripts/reindex.sh`.
  Subcommands: `index`, `browse`, `rebuild`. Flags: `--dir`, `--exclude`.
- No tests yet.
- Pre-commit hook not tested end-to-end.
- `.mcp.json` has placeholder path — needs real project path.

## Dependencies

```
golang.org/x/tools v0.26.0   — go/packages for type-checked AST parsing
modernc.org/sqlite v1.30.0   — pure Go SQLite driver (CGO_ENABLED=1 for perf)
```

## Running It

### Install
```sh
go install github.com/urechandro/scout/cmd/indexer@latest
go install github.com/urechandro/scout/cmd/server@latest
```

### Full index
```sh
indexer --db /your/project/.scout/index.db --root /your/project
```

### Incremental reindex
```sh
indexer --db /your/project/.scout/index.db --root /your/project \
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
      "command": "server",
      "args": ["--db", "/your/project/.scout/index.db"],
      "env": {}
    }
  }
}
```

If `server` is not on your `$PATH`, use the full path (e.g. `~/go/bin/server`).
