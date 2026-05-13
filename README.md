# scout

MCP server for fast, token-efficient navigation of a Go codebase. Gives Claude
Code structured symbol lookup instead of broad file reads.

## Install

Requires Go 1.21+. The target codebase must also have Go available (the indexer
shells out to `go list` for type resolution).

```sh
go install github.com/urechandro/scout/cmd/indexer@latest
go install github.com/urechandro/scout/cmd/server@latest
```

## Usage

### 1. Index your codebase

```sh
indexer --db /your/project/.scout/index.db --root /your/project
```

For incremental reindex (e.g. from a pre-commit hook):

```sh
indexer --db /your/project/.scout/index.db --root /your/project \
  --files path/to/changed.go,other.go
```

### 2. Wire up Claude Code

Add to `.claude/settings.json` in your project root:

```json
{
  "mcpServers": {
    "scout": {
      "command": "server",
      "args": ["--db", "/your/project/.scout/index.db"]
    }
  }
}
```

If `server` is not on your `$PATH`, use the full path (e.g. `~/go/bin/server`).

### 3. Inspect the index (optional)

Uses [Datasette](https://datasette.io/) for a browsable UI at http://localhost:8001:

```sh
datasette /your/project/.scout/index.db --host 0.0.0.0 --port 8001
```

Or query directly with `sqlite3`:

```sh
sqlite3 /your/project/.scout/index.db
SELECT COUNT(*) FROM symbols;
SELECT id, kind, signature FROM symbols LIMIT 20;
SELECT COUNT(*) FROM edges;
```

## Tools exposed to Claude Code

| Tool | Purpose |
|---|---|
| `get_relevant_context(query)` | Primary tool. FTS search + graph expansion + ranking. Returns symbol summaries within a token budget. |
| `get_body(symbol_id)` | Full source of one symbol. Call only when about to read or edit it. |
| `get_callers(symbol_id)` | Everything that calls this symbol. Useful before changing a signature. |
| `get_callees(symbol_id)` | Everything this symbol depends on. |
| `get_flow(symbol_id)` | Full source of a symbol plus caller/callee summaries in one call. Use instead of separate get_body + get_callers + get_callees. |
| `get_pattern(task)` | A complete vertical slice (proto RPC → request/response messages → Go implementation) with full source bodies. Requires proto indexing; degrades to a single FTS hit otherwise. Use before implementing a new RPC. |
| `get_conventions(topic)` | How a cross-cutting pattern is used across the codebase (e.g. "pagination", "error handling", "outbox"). |
