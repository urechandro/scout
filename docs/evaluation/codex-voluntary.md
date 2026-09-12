# Voluntary Codex navigation benchmark

This benchmark measures whether Codex uses Scout when merely instructed to do
so. It does not block reads or change retrieval behavior. The fixed task list is
`eval/codex_tasks.yaml` (5 explanation, 5 bug-localization, 4
implementation-pattern, 3 refactor, and 3 cross-layer tasks).

## Protocol

Run every task in a fresh session and in the same repository revision, using:

1. **control** — Codex without Scout MCP or Scout guidance;
2. **scout** — `scout init --agent codex --yes`, with Scout MCP and `AGENTS.md`.

Keep model, sandbox, approval policy, and task wording constant. Do not count
the setup/indexing session as a task. If a task needs a targeted source read to
edit code, allow it in both modes.

## Record

Store one JSON or CSV row per task with:

| Field | Meaning |
| --- | --- |
| `task_id`, `mode`, `repo`, `commit` | Reproduction identity |
| `correct` | Human-rated task correctness (yes/no) |
| `scout_calls` | Scout MCP calls, by tool where possible |
| `normal_reads` | Normal file/tool reads |
| `whole_file_reads` | Reads of complete source files |
| `shell_searches` | grep/find/ripgrep-style searches |
| `estimated_source_tokens` | Approximate source context returned to Codex |
| `elapsed_seconds` | Time to completion |
| `retries` | Failed/repeated attempts |
| `notes` | Short quality or latency observations |

Do not record prompts, raw source, or secrets. Human ratings should use the
same correctness rubric for both modes: the requested explanation is accurate,
the proposed change is buildable and scoped, or the diagnosed location is
actionable. Report aggregate correctness, broad reads, source tokens, latency,
and Scout call counts; do not claim provider cost savings from estimates.

## Decision

Compare Scout with control by task category and overall. This slice is
successful if MCP connection and guidance are reliable, Scout contributes to
successful tasks, and broad reads decline without a material correctness or
latency regression. Keep raw measurements and notable failures with the
benchmark artifact before designing any observe-only hook.
