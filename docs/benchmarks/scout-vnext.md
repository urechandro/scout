# Scout vNext controlled benchmark

This benchmark compares a baseline navigation workflow with Scout-assisted
navigation using the same repository, task set, and client model.

## Procedure

1. Use a clean checkout and a fixed set of representative code-navigation
   tasks. Do not change the task wording between runs.
2. Run the baseline and Scout workflows separately. Keep semantic retrieval
   disabled unless the experiment specifically measures Ollama.
3. Record one session telemetry event per Scout call and summarize it:

   ```sh
   scout session-stats --path .scout/session.jsonl
   ```

4. Record task success, correctness, total calls, response bytes, estimated
   tokens, and p50/p95 latency for each run.

## Interpretation

Report context reduction only when task correctness does not materially regress.
The vNext decision target is at least 50% less broad source context, no more
than 5% degradation in task success/correctness, and no severe p95 latency
regression. Include the task list, Scout version, repository revision, and
configuration so the result is reproducible.

The repository's deterministic fixture metrics can be exercised with:

```sh
go test ./eval -run 'Test(RankedMetrics|SemanticDelta)$' -v
```
