# Scout vNext implementation plan

Status: proposed  
Baseline commit: `7cd22c8`  
Last reviewed: 2026-09-11

## Objective

Turn Scout from a Claude-oriented MCP server into an agent-neutral,
token-efficient code-navigation layer for Codex, Claude Code, and other MCP
clients.

Scout should find the smallest useful slice of a repository before the coding
agent reads source. Exact lookup, full-text search, optional local embeddings,
graph expansion, ranking, and token-bounded rendering remain the core. A local
generative model is explicitly out of scope unless later measurements establish
a concrete gap that deterministic retrieval cannot address economically.

## Product invariants

- Exact source remains available whenever it is needed for editing or debugging.
- Precise symbol queries are deterministic and do not receive semantic
  broadening by default.
- Embedding failures degrade to exact lookup and FTS; they never fail a query.
- Scout remains useful as a standalone MCP server without initialization magic.
- Agent-specific configuration is separated from indexing and retrieval.
- Enforcement is optional, starts in observe-only mode, and always has a safe
  escape path.
- Token savings are reported as estimates unless provider tokenizer data is
  available.
- Retrieval quality and task correctness take precedence over token reduction.

## Current state

The following was verified at baseline commit `7cd22c8`:

- Go, TypeScript, and proto indexers are present.
- Exact lookup, FTS, graph expansion, ranking, and token trimming are implemented
  in `query/engine.go`.
- Query-time vector retrieval is implemented and is limited to discovery
  queries. It has focused tests in `query/phase3_test.go`.
- Embeddings are stored in SQLite, filtered by model, preserved across unchanged
  reindexes, invalidated when embedding text changes, and refreshed by the
  watcher.
- The evaluation harness contains 14 general fixtures and 15 semantic fixtures.
  It currently checks substring presence and byte limits but does not calculate
  ranked retrieval metrics.
- `README.md` incorrectly says query-time vector retrieval is not active.
- Initialization writes Claude-specific `.mcp.json` and `CLAUDE.md` content.
- Initialization output and idempotency do not have direct test coverage.
- MCP responses log a four-characters-per-token estimate, but retrieval does not
  expose complete pre-trim/post-trim diagnostics.
- There is no `query --explain`, `stats`, `doctor`, Codex configuration renderer,
  or navigation-enforcement hook.

The initial `go test ./...` baseline attempt was blocked by the execution
environment because Go attempted to write its cache under the user directory.
Baseline automation must set writable cache paths explicitly and must not record
a passing baseline until the complete suite has actually run.

## Delivery strategy

Work proceeds through hard gates. A later milestone must not compensate for an
earlier failure: hooks cannot compensate for weak retrieval, and a local model
cannot compensate for an unreliable deterministic pipeline.

Each milestone should normally be delivered as a separate pull request. Every
retrieval-changing pull request must include before/after evaluation output.

## Milestone 0: freeze the baseline

Goal: establish a reproducible description of current behavior before changing
ranking or integration behavior.

### Deliverables

- Correct the semantic-search status in `README.md`.
- Add `docs/evaluation/baseline.md` containing:
  - full commit SHA;
  - Go version;
  - Ollama version, host, and embedding model when enabled;
  - fixture-set identity;
  - exact/FTS and semantic results;
  - latency and response-size measurements;
  - known misses and false positives.
- Add a reproducible baseline command, preferably `scripts/eval-baseline`, that
  sets writable `GOCACHE`, `GOMODCACHE`, and temporary paths when needed.
- Document current behavior for all indexers, incremental reindexing, watcher
  lifecycle, MCP lifecycle, retrieval tools, token budgeting, and Claude init.
- Run and record:

  ```text
  go test ./...
  general fixture count
  semantic fixture count
  exact/FTS hit rate
  semantic hit rate
  average result count
  average estimated response tokens
  p50 query latency
  p95 query latency
  known failures
  ```

### Gate A: baseline frozen

- The complete test suite passes.
- Retrieval behavior is documented from code, not inferred from the README.
- Semantic retrieval is verified end to end.
- Results and known failures are committed.
- Re-running the baseline command produces comparable output.

If retrieval is nondeterministic or the suite is unstable, stop and repair that
before continuing.

## Milestone 1: retrieval quality and observability

Goal: make retrieval trustworthy enough to be an agent's primary navigation
mechanism.

### 1.1 Formalize query classes

- Preserve two explicit classes:
  - precise: a single compound or dotted identifier;
  - discovery: domain language or a natural-language question.
- For precise queries, run exact lookup first, use the smaller default budget,
  keep deterministic ordering, and treat an empty result as meaningful.
- For discovery queries, combine identifier extraction, FTS, vectors when
  configured, graph expansion, ranking, and budget trimming.
- Add boundary fixtures for punctuation, snake_case, dotted identifiers,
  lowercase names, multiple symbols, and stop-word-only queries.

### 1.2 Upgrade the evaluation harness

Extend fixture expectations to support ranked evaluation:

```yaml
query: "where do charging penalties get calculated"
must_include:
  - package.Symbol
should_include:
  - package.Helper
must_not_include:
  - generated.Unrelated
max_rank:
  package.Symbol: 5
```

Calculate at least:

- Recall@5 and Recall@10;
- mean reciprocal rank;
- must-not-include violation rate;
- average and percentile result counts;
- percentage of discovery queries improved by semantic retrieval;
- percentage made worse by semantic retrieval;
- p50 and p95 latency;
- estimated response tokens.

Add fixtures for exact names, domain language, vague intent, duplicate names,
generated-code traps, interfaces versus implementations, tests versus production
code, proto-to-Go linkage, and TypeScript naming boundaries.

### 1.3 Harden vector retrieval

Add or complete tests for:

- no embedder configured;
- Ollama unavailable or returning an error;
- query-embedding timeout;
- model change and dimension mismatch;
- stale and partially populated vectors;
- watcher invalidation and slab refresh;
- concurrent queries during refresh;
- repeated queries without unnecessary reloads;
- empty semantic results;
- the same candidate appearing through exact, FTS, vector, and graph sources;
- representative large repositories.

All vector failures must preserve deterministic exact/FTS behavior. Model and
dimension problems must be visible in diagnostics.

Implementation note (2026-09-12): query embedding failures and timeouts remain
silent no-ops, while malformed NaN/Inf vectors are rejected before cosine
scoring. Slab rows are copied on load to isolate concurrent refreshes from
scoring. Coverage includes unavailable embedders, empty slabs, dimension
mismatches, dirty-slab reloads, malformed vectors, and timeout behavior. The
remaining concurrency and large-repository cases are tracked as follow-up
fixtures before Gate B is declared complete.

### 1.4 Add retrieval explanations

Add a structured internal trace and expose it through one of:

```sh
scout query --explain "how is authentication wired"
```

or a debug-only MCP response mode. The trace should report:

- query class and effective budget;
- candidate counts per retrieval phase;
- unique additions and duplicates per phase;
- counts after expansion and ranking;
- estimated tokens before and after trimming;
- symbols considered, returned, and truncated;
- source evidence and score components for the top results;
- total and semantic latency.

Normal MCP output must remain compact.

Implementation note (2026-09-12): `query.ContextRequest.Explain` and the MCP
`explain` argument now expose an opt-in `RetrievalTrace` with query class,
budget, per-phase candidate counts, expansion, trimming, and latency. Brief
responses remain unchanged unless explanations are requested.

### Gate B: retrieval trustworthy

- Precise-query regression count is zero.
- Discovery Recall@10 is at least the frozen baseline.
- Semantic-only wins and semantic regressions are documented.
- Vector failure-path tests pass.
- Token-budget behavior is observable.
- p95 latency is acceptable for interactive use.

If semantic retrieval does not materially improve the fixture set, keep it
optional and continue with exact lookup, FTS, and graph retrieval.

## Milestone 2: agent-neutral initialization

Goal: separate repository initialization from client-specific integration
without introducing an unnecessary plugin framework.

### Deliverables

- Add an `agentKind` or similarly small abstraction with `claude`, `codex`, and
  `none` values.
- Split initialization into:
  - repository and index setup;
  - MCP configuration rendering;
  - navigation-policy rendering.
- Support:

  ```sh
  scout init --agent claude --yes
  scout init --agent codex --yes
  scout init --agent none --yes
  ```

- Add the agent choice to the interactive wizard.
- Generate Claude and Codex guidance from a shared core policy.
- Use explicit managed markers:

  ```markdown
  <!-- scout:start -->
  ...
  <!-- scout:end -->
  ```

- Preserve manual `scout index` and `scout serve` workflows.
- Add tests covering fresh files, existing unrelated content, replacement of an
  existing Scout block, malformed client config, repeated initialization, and
  every agent mode.

### Gate C: agent-neutral Scout

- Claude initialization preserves existing supported behavior.
- MCP-only initialization works without writing agent files.
- Repeated initialization is idempotent.
- Unrelated client configuration and instruction content are preserved.
- Retrieval and indexing contain no Codex-specific assumptions.

## Milestone 3: Codex MCP integration

Goal: determine whether reliable tools plus instructions are sufficient before
adding enforcement.

### Deliverables

- Verify the currently supported Codex MCP configuration format at implementation
  time; do not freeze an assumed format in advance.
- Have `scout init --agent codex` update only Scout-owned configuration.
- Create or update `AGENTS.md` while preserving unrelated content.
- Render a light navigation policy that recommends:
  - `get_relevant_context` for discovery;
  - `get_body` for exact implementation;
  - `get_flow` for local control-flow context;
  - `get_impact` before signature or structural changes;
  - `get_pattern` when following an existing implementation pattern.
- Do not prohibit normal reads in this milestone.
- Test custom binary paths, watchers, TypeScript arguments, existing Codex
  configuration, malformed input, and repeated initialization.

### Evaluation

Run representative explanation, bug-localization, small-implementation,
refactor, and cross-layer tasks in at least Scout itself and one external test
repository. Record Scout calls, normal reads, whole-file reads, shell searches,
estimated context, completion quality, and latency. Compare with Scout disabled.

### Gate D: voluntary Codex usage

- Codex connects to Scout through MCP reliably.
- Scout results contribute to successful solutions.
- Broad reads decline materially or their persistence is understood.
- There is no significant correctness or latency regression.

If MCP use itself is unreliable, stop and fix configuration, instructions, or
retrieval. Do not compensate with hooks.

## Milestone 4: observe-only navigation hook

Goal: learn actual Codex read behavior before enforcing restrictions.

### Deliverables

- Define a structured read event containing file, file type, file length,
  requested range, command/tool source, proposed decision, and reason.
- Add configurable thresholds such as:

  ```yaml
  navigation:
    enforcement: observe
    max_whole_file_lines: 350
    max_targeted_read_lines: 200
  ```

- Classify whole-file source reads, targeted ranges, generated files, non-source
  files, and binary/assets separately.
- Prefer structured tool inputs where Codex exposes them. Treat shell parsing as
  fallback infrastructure.
- Record telemetry locally without prompts or raw source by default.
- Explicitly exempt Scout commands, MCP operations, indexing, and hook-generated
  work to prevent recursion.

### Gate E: observed behavior understood

- Observe mode does not break representative tasks.
- Logged decisions accurately identify broad reads.
- The observed command/tool corpus is sufficient to design conservative blocking.

If behavior is not understood, collect more traces instead of expanding a
speculative shell parser.

## Milestone 5: conservative enforcement

Goal: redirect unnecessarily broad source reads while preserving editability.

### Deliverables

- Add `navigation.enforcement: block` using the observed classifier.
- Allow targeted ranges and known non-source files.
- Usually block whole generated files and oversized whole source files.
- Return actionable guidance when blocking:

  ```text
  Broad source read blocked by Scout.
  Use get_relevant_context for discovery, then get_body or get_flow for exact
  symbols. A targeted source range remains available for editing context.
  ```

- Provide a deliberate, measurable override mechanism.
- Add loop-prevention, targeted-read, override, and end-to-end task tests.

### Gate F: enforcement preserves quality

- Broad reads are blocked as configured.
- Targeted reads and exact symbol bodies remain available.
- Scout remains reachable after a block.
- No recursive hook loop is possible.
- Representative task success does not materially decline.

If the agent thrashes or abandons tasks, improve retrieval, guidance, and escape
paths rather than adding restrictions.

## Milestone 6: telemetry and controlled benchmark

Goal: establish whether Scout saves context without reducing task quality.

### Deliverables

- Add local session telemetry for Scout calls:
  - timestamp, tool, query class, phase counts, result counts;
  - estimated returned tokens and latency;
  - whether semantic retrieval ran and its latency.
- Add enforcement telemetry:
  - file and line counts;
  - requested range;
  - estimated source tokens avoided;
  - allowed, observed, blocked, or overridden outcome.
- Add `scout stats` with session and aggregate summaries.
- Do not record prompts or raw source unless explicitly enabled.
- Do not make provider-cost claims without actual provider token and pricing data.

### Benchmark matrix

Use at least five repositories:

1. Scout;
2. a medium Go service;
3. a larger Go service;
4. a TypeScript project;
5. a mixed Go/proto project.

Use at least 20 fixed tasks:

- five explanation/navigation;
- five bug-localization;
- four implementation-pattern;
- three refactor/blast-radius;
- three cross-layer.

Run every task in three modes:

1. control: Codex without Scout;
2. Scout: Codex plus Scout instructions;
3. enforced: Codex plus Scout and blocking.

Measure task correctness, estimated remote source context, broad reads, time to
completion, tool calls, Scout and semantic latency, retries, targeted reads,
blocked reads, and overrides. Use a written human-rating rubric.

### Gate G: evidence of value

Target:

- at least 50% reduction in broad source context;
- no more than 5% degradation in task success or correctness;
- no severe p95 latency regression.

The threshold is a decision aid, not permission to optimize context at the
expense of correctness.

## Milestone 7: hardening and productization

Goal: make proven behavior safe to install, operate, and upgrade.

### Deliverables

- Add `scout doctor` checks for the binary, repository root, database and schema,
  language tooling, Ollama and model, MCP/client configuration, hook state, and
  watcher state.
- Add explicit configuration and managed-section migrations.
- Make upgrades preserve user configuration and unrelated client settings.
- Detect obsolete hook and marker formats.
- Rewrite the README around agent-neutral code navigation, with separate Codex,
  Claude Code, and manual MCP sections.
- Document semantic retrieval according to its measured implementation.

### Gate H: vNext ready

- Standalone retrieval is reliable and evaluated.
- Claude integration remains supported.
- Codex integration works in representative repositories.
- Enforcement is optional and safe.
- Telemetry demonstrates material context reduction without meaningful quality
  loss.
- Exact source access remains available.
- Install and upgrade operations are idempotent.
- Documentation matches implementation.

## Local-model decision

Do not add a local generative worker as part of the milestones above. After the
controlled benchmark, classify remaining gaps such as poor reranking, oversized
symbol bodies, weak cross-file relationship compression, or repetitive code
generation.

Prototype exactly one role only if a measured gap justifies it. Preferred order:

1. rerank the deterministic top candidates;
2. summarize an unusually large, already-selected symbol while preserving exact
   body access;
3. generate repetitive code from an explicit reference and specification, with
   Codex reviewing the diff.

Possible valid conclusion: no local model is needed.

## Suggested pull-request sequence

1. `docs: freeze retrieval baseline`
2. `test: add ranked retrieval evaluation`
3. `feat: explain retrieval decisions`
4. `test: harden semantic failure paths`
5. `refactor: separate agent initialization`
6. `feat: add codex integration`
7. `test: benchmark voluntary codex navigation`
8. `feat: observe broad source reads`
9. `feat: enforce scout-first navigation`
10. `feat: report navigation statistics`
11. `docs: publish controlled benchmark`
12. `feat: add scout doctor`
13. `docs: document scout vnext`

Ranking changes should be isolated from evaluation-harness changes so metric
movement is attributable. Hook enforcement should not share a pull request with
Codex MCP setup.

## Immediate next action

Implement Milestone 0 only:

1. make the Go test/eval commands reproducible in restricted environments;
2. run the complete test suite;
3. run exact/FTS and semantic evaluations;
4. verify Phase 3 end to end;
5. record hit rate, ranked metrics where currently possible, latency, response
   size, and known regressions;
6. correct the README semantic status;
7. commit the frozen baseline artifact.

Do not begin ranking changes or Codex integration until Gate A is satisfied.
