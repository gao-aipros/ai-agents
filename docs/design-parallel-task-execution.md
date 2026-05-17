# Design: Parallel Task Execution for Workers

## Problem

Currently, the thread lock (`thread:<id>:lock`, acquired via `SET NX` during `Enqueue`) serializes ALL tasks on a thread. The master must enqueue → wait → enqueue → wait sequentially, even for independent parallel phases:

- **Design review**: Master sends design docs to all 4 workers. With the lock, this runs sequentially — copilot first, then opencode, then codex, then worker-claude.
- **Code review**: Master sends a PR to all non-implementer workers. Same sequential bottleneck.

The lock is necessary for sequential phases (implement → revise → merge) where one worker writes code and the next step depends on its output. But for review phases, workers operate on read-only copies and write to separate review files (`docs/code-review-copilot.md`, `docs/code-review-opencode.md`, etc.) — no conflicts.

## Solution

Add a **parallel enqueue mode** that bypasses the thread lock, allowing the master to fan out independent tasks to multiple workers simultaneously. The master waits for ALL parallel tasks to finish before proceeding to the next sequential phase.

### Workflow with parallel phases

```
Design → [Review: copilot ∥ opencode ∥ codex ∥ worker-claude] → Revise Design
                                                                        ↓
                                                              Implement (worker-claude or codex)
                                                                        ↓
                                                              [Review: 3 non-implementer workers ∥]
                                                                        ↓
                                                              Revise → Re-review → Merge
```

`∥` = parallel, `→` = sequential

## Implementation

### 1. New `EnqueueParallel` method in `tasklib`

Refactor `Enqueue` to extract common logic into a private `enqueue(ctx, worker, threadID, instruction, acquireLock bool)` method. `Enqueue` calls `enqueue(..., true)`, `EnqueueParallel` calls `enqueue(..., false)`.

The only difference: `EnqueueParallel` skips `SET NX` on `thread:<id>:lock`. Everything else — appending to thread history, LPUSH to worker queue, initializing per-task keys — is identical.

```go
func (c *Client) Enqueue(ctx context.Context, worker, threadID, instruction string) (*Task, error) {
    return c.enqueue(ctx, worker, threadID, instruction, true)
}

func (c *Client) EnqueueParallel(ctx context.Context, worker, threadID, instruction string) (*Task, error) {
    return c.enqueue(ctx, worker, threadID, instruction, false)
}
```

### 2. CLI: `--no-lock` flag on `task enqueue`

```bash
# Sequential (default — acquires thread lock)
task enqueue --worker claude --thread my-thread --instruction "implement..."

# Parallel (no lock — fan out to multiple workers)
task enqueue --worker copilot --thread my-thread --instruction "review..." --no-lock
task enqueue --worker opencode --thread my-thread --instruction "review..." --no-lock
```

When `--no-lock` is set, the CLI calls `EnqueueParallel`. Default behavior is unchanged.

### 3. CLI: `task wait-all` command

Convenience command that polls all tasks on a thread until none are in `pending` or `running` status:

```bash
task wait-all --thread my-thread --timeout 600
```

Returns when all tasks are terminal (`done`/`failed`/`cancelled`) or timeout expires. The master can also use `task wait --id <taskID>` per task — both work; `wait-all` is simpler for scripts.

### 4. Master workflow (CLAUDE.md instructions)

For parallel phases:

```bash
# Enqueue parallel review tasks
ID1=$(task enqueue --worker copilot --thread $THREAD --no-lock --instruction "review..." | jq -r .task_id)
ID2=$(task enqueue --worker opencode --thread $THREAD --no-lock --instruction "review..." | jq -r .task_id)
ID3=$(task enqueue --worker codex --thread $THREAD --no-lock --instruction "review..." | jq -r .task_id)

# Wait for all to complete
task wait-all --thread $THREAD --timeout 600
```

For sequential phases, continue using default locked enqueue:

```bash
task enqueue --worker claude --thread $THREAD --instruction "implement..."
task wait --id $(...) --timeout 1800
```

## Rationale

### Why not sub-threads?
Sub-threads would require creating separate Redis keys, thread state, and history for each parallel branch. This adds complexity (tracking parent/child relationships, aggregating results) and the cost isn't justified — parallel review tasks are simple read-and-report operations that don't need thread isolation.

### Why not a batch task primitive?
A batch/fan-out primitive would be a first-class concept in the task system (enqueue once, auto-fan-out to N workers, aggregate results). This is the right long-term solution but premature — the current 4-worker setup is small enough that explicit per-worker enqueue with `--no-lock` is sufficient and more transparent.

### Thread history interleaving
During parallel phases, worker outputs are appended to the same thread history list. Since all workers write to different review files and their messages are timestamped, interleaving is harmless. The master reads all review files after `wait-all` completes — message order in history doesn't matter.

### Safety
- `WaitTask` already handles missing locks gracefully (`DEL` on nonexistent key is a no-op)
- Workers don't interact with the thread lock at all — only the master does
- `task requeue-stale` works identically for both locked and unlocked tasks
- If a worker crashes during a parallel task, the master's `wait-all` will time out — same recovery path as locked tasks
