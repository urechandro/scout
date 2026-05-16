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

### When to use each tool

| Situation | Tool |
|---|---|
| Any question about the codebase ("how does X work?", "where is Y?", "who calls Z?") | `get_relevant_context` |
| "Follow this pattern" or "add a new RPC" | `get_pattern` |
| Read full source of a specific symbol | `get_body` with symbol ID from a previous result |
| Understand a symbol's callers/callees with full body | `get_flow` |
| Before renaming, changing a type, or modifying a signature | `get_impact` |
| Before adding a new RPC (check what's already missing) | `get_unimplemented` |
| Before implementing a pattern (outbox, pagination, auth) | `get_conventions` |
| Read proto files, go.mod, config, non-Go files | `Read` |

### Playbooks by task type

Each playbook is 2 rounds max. **After each round, stop and ask: "Do I know enough to answer?" If yes, deliver immediately. If you cannot name a specific gap that would change your answer, you know enough.**

**Add new RPC / follow existing pattern:**
1. `get_unimplemented` to confirm it doesn't exist, then `get_pattern` with the closest existing analog (NOT the new RPC name)
2. *Only if* combining patterns from two RPCs: `get_pattern` on the second analog. Do NOT use `get_relevant_context` to search for the new thing — it doesn't exist yet.

**Explore unfamiliar area ("how does X work?"):**
1. `get_relevant_context` with domain terms
2. *Only if* you need source: `get_body` or `get_flow` on 1-2 key symbols

**Rename / change signature / refactor:**
1. `get_impact` — gives full blast radius across proto, generated, impl, and test layers in one call
2. *Only if* you need the source: `get_body` on the function itself

**Implement a cross-cutting pattern for the first time:**
1. `get_conventions` with the pattern name
2. *Only if* examples are unclear: `get_body` on 1-2 examples from results

**When a Scout call returns "not found":** stop. Do NOT fall back to grep/find/Read to chase it down. Either:
- The symbol is an external dependency → describe it from its signature and move on.
- The symbol doesn't exist → tell the user.

Do not explore out of anxiety. Extra rounds add tokens without changing the deliverable.

### Rules

- **Start every task or question with a scout tool call.** Not grep. Not find. Not Read.
- Include specific symbol names in queries when you have them (e.g. "CreateShipmentLeg" not "create shipment leg").
- **One `get_relevant_context` per task.** Never call it more than once. If the first call didn't return what you need, use `get_body`/`get_flow`/`get_callers` on a symbol ID from the results — don't retry with rephrased queries.
- **Follow-up chain: stay in scout.** After get_relevant_context returns symbol IDs:
  - Need source? → `get_body` (NOT Read)
  - Need callers? → `get_callers` (NOT grep)
  - Need callees? → `get_callees` (NOT grep)
  - Need full call context? → `get_flow`
  - Need blast radius? → `get_impact`
- **NEVER use Read on .go or .proto files.** This is absolute. Use `get_body` with the symbol ID instead. `Read` is ONLY for non-code files (go.mod, yaml, config, markdown, Makefile).
- **NEVER use Bash (grep/find/cat) on .go or .proto files.** Use scout tools instead.
- **Do not chase into external dependencies.** If a symbol comes from an external package (e.g. `saga-toolbox`, `grpc`), its signature from `get_body` is enough. Do NOT grep/find/Read in the Go module cache (`~/go/pkg/mod/`). Describe external calls by their signature and move on.
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
| `get_relevant_context(query)` | Primary tool. Exact name lookup + FTS + graph expansion + ranking. Returns symbol summaries within a token budget. |
| `get_pattern(task)` | Complete vertical slice: proto RPC → request/response messages → Go implementation, with full source bodies. Use before implementing a new RPC. |
| `get_body(symbol_id)` | Full source of one symbol plus signatures of referenced types/functions. Call only when about to read or edit it. |
| `get_flow(symbol_id)` | Full source of a symbol plus caller/callee summaries in one call. Use instead of separate get_body + get_callers + get_callees. |
| `get_impact(symbol_id)` | Full blast radius across layers. Traces proto↔Go name linkage, generated code, callers, implementors, and tests. Use before renaming or changing a type/field. |
| `get_callers(symbol_id)` | Everything that calls this symbol. Falls back to interface/RPC lookup and body-reference heuristics when call graph edges are missing. |
| `get_callees(symbol_id)` | Everything this symbol depends on. Falls back to body-reference extraction. |
| `get_unimplemented(service)` | Diff a proto service against Go server methods. Returns which RPCs are missing or stubbed. Call before adding a new RPC. |
| `get_conventions(topic)` | Look up a documented architectural pattern by topic (e.g. "pagination", "auth", "outbox"). Returns the pattern description, pseudocode structure, and resolved example symbols. |
