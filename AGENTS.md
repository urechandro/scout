# Repository guidelines for agents

These instructions apply to the entire repository.

## Branch workflow

- Do not implement new features directly on `main`.
- Start each feature, fix, refactor, or other material change on a dedicated
  branch based on the latest local `main`.
- Before creating a branch, verify that the worktree is clean and that switching
  branches will not disturb user-owned changes.
- Do not fetch, pull, rebase, merge, or otherwise change remote state unless the
  user asks for it or the task explicitly requires it.
- Use short, descriptive branch names with a category prefix, for example:
  - `feat/codex-integration`
  - `fix/vector-refresh`
  - `refactor/agent-init`
  - `docs/retrieval-baseline`
- Keep unrelated work on separate branches and in separate commits.
- Never discard or overwrite existing user changes to obtain a clean branch.

When the user explicitly requests work on the current branch, follow that
request. Small documentation-only or administrative changes may remain on the
current non-`main` branch when they are clearly part of its purpose.

## Commits

- Use Conventional Commits for every commit:

  ```text
  <type>[optional scope]: <imperative summary>
  ```

- Use lowercase types. Common types are:
  - `feat`: new user-visible behavior;
  - `fix`: bug fix;
  - `refactor`: structural change without a behavior change;
  - `test`: test-only change;
  - `docs`: documentation-only change;
  - `chore`: maintenance or tooling;
  - `perf`: performance improvement;
  - `ci`: continuous-integration configuration.
- Keep the summary concise, imperative, and free of a trailing period.
- Add a scope only when it makes the affected area clearer, such as
  `feat(init): add codex agent selection`.
- Use a body when the reason, migration impact, or non-obvious tradeoff matters.
- Mark breaking changes with `!` and explain them in a `BREAKING CHANGE:` footer.
- Do not amend, squash, rebase, force-push, or rewrite existing commits unless
  the user explicitly asks.
- Commit only files that belong to the requested change. Inspect the staged diff
  before committing.

## Implementation discipline

- Read `docs/plans/scout-vnext.md` before implementing Scout vNext work.
- Respect its milestone gates: do not use later integration or enforcement work
  to compensate for an unmet retrieval-quality gate.
- Prefer small, reviewable changes with tests over broad rewrites.
- Preserve Scout's deterministic exact/FTS fallback when optional semantic
  retrieval is unavailable.
- Keep core indexing and retrieval agent-neutral. Put Codex- or Claude-specific
  behavior behind integration-specific rendering or configuration.
- Preserve manual `scout index` and `scout serve` workflows.
- Preserve unrelated configuration and user-authored content when updating
  managed files such as `AGENTS.md`, `CLAUDE.md`, or MCP settings.

## Verification

- Run `git diff --check` for every change.
- Run the narrowest relevant tests while developing, then run `go test ./...`
  before declaring a code change complete.
- Retrieval or ranking changes must include before/after evaluation results.
- Initialization changes must test fresh setup, existing user content, and
  repeated/idempotent setup.
- If a test cannot run because of sandboxing, missing services, unavailable
  models, or another environmental constraint, report that explicitly. Never
  describe an unrun test as passing.
- Do not make semantic-search tests depend on Ollama unless they are explicitly
  opt-in or use a deterministic test double.

## Worktree safety

- Treat pre-existing modified and untracked files as user-owned.
- Do not use destructive Git commands to remove changes.
- Before committing, review `git status --short` and the staged diff.
- After committing, report the branch name, commit hash, tests run, and any
  remaining worktree changes or verification limitations.
