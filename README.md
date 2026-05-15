# scout

MCP server for fast, token-efficient navigation of a Go codebase. Gives Claude
Code structured symbol lookup instead of broad file reads.

## Install

Requires Go 1.21+. The target codebase must also have Go available (the indexer
shells out to `go list` for type resolution).

```sh
go install github.com/urechandro/scout/cmd/scout-index@latest
go install github.com/urechandro/scout/cmd/scout-server@latest
```

## Usage

### 1. Index your codebase

```sh
scout-index --db /your/project/.scout/index.db --root /your/project
```

Proto files (`.proto`) are indexed automatically alongside Go. Use `--exclude`
to skip generated or vendored proto directories.

For incremental reindex (e.g. from a pre-commit hook):

```sh
scout-index --db /your/project/.scout/index.db --root /your/project \
  --files path/to/changed.go,other.go
```

### 2. Wire up Claude Code

Add to `.mcp.json` in your project root:

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

Add the following to your project's `CLAUDE.md` so Claude Code uses scout
tools instead of defaulting to grep/find/Read:

```markdown
## Scout — codebase navigation (MCP)

Scout is connected via MCP. **Always use scout tools first** — for any question
about the codebase, any task involving Go or proto code, or any exploration.
Do not use grep, find, or Read to explore the codebase when scout tools are
available.

| Situation | Tool |
|---|---|
| Any question about the codebase ("how does X work?", "where is Y?") | `get_relevant_context` |
| "Follow this pattern" or "add a new RPC" | `get_pattern` |
| Read full source of a specific symbol | `get_body` with symbol ID from a previous result |
| Understand a symbol's callers/callees with full body | `get_flow` |
| Before implementing a pattern (outbox, pagination, auth) | `get_conventions` |
| Read proto files, go.mod, config, non-Go files | `Read` |

- **Start every task or question with a scout tool call.** Not grep. Not find. Not Read.
- Include specific symbol names in queries (e.g. "CreateShipmentLeg" not "create shipment leg").
- Use get_body for cross-package lookups. Use Read for files you already know the path to.
- Before changing a function signature, call get_callers to check blast radius.
```

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

### 4. Document conventions (optional)

`get_conventions` returns documented architectural patterns when a
`conventions.yaml` exists at your project root. Copy the example file and fill
it in:

```sh
cp conventions.example.yaml /your/project/conventions.yaml
```

Each entry documents one pattern — name, search terms, description, pseudocode
structure, and example symbol IDs. The indexer loads it automatically on every
run.

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
