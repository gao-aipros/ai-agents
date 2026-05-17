# Design: Parallel Task Execution for Workers

## Problem

Currently, the thread lock (`thread:<id>:lock`, acquired via `SET NX` during `Enqueue`) serializes ALL tasks on a thread. The master must enqueue → wait → enqueue → wait sequentially, even for independent parallel phases:

- **Design review**: Master sends design docs to all 4 workers. With the lock, this runs sequentially — copilot first, then opencode, then codex, then worker-claude.
- **Code review**: Master sends a PR to all non-implementer workers. Same sequential bottleneck.

The lock is necessary for sequential phases (implement → revise → merge) where one worker writes code and the next step depends on its output. But for review phases, workers operate on read-only copies and write to separate review files (`docs/code-review-copilot.md`, `docs/code-review-opencode.md`, etc.) — no conflicts.

## Solution

Introduce **task groups** — a lightweight concept that lets the master fan out independent tasks to multiple workers under a named group label. The group tracks task membership explicitly and computes aggregate status once when all member tasks complete.

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

`∥` = parallel (task group), `→` = sequential (thread lock)

## Design Decisions

This design was revised after review feedback. The original proposal used `--no-lock` + `wait-all` (scan-based discovery). Reviewers identified gaps: thread status races, underspecified discovery, missing error semantics. Three alternative approaches were evaluated:

| Approach | Discovery | Status Race | New Concepts | Verdict |
|----------|-----------|-------------|--------------|---------|
| `--no-lock` + `wait-all` | SCAN (fragile) | Yes | 2 (flag + command) | Rejected |
| Reference-counted lock (INCR/DECR) | SCAN (fragile) | Yes | 0 | Lighter but still SCAN |
| Sub-threads per reviewer | Explicit (thread list) | No | 3+ (parent, child, aggregate) | Overkill for 4 workers |
| **Task groups** | Explicit (Redis set) | No | 2 (--group, group-wait) | **Chosen** |

Task groups were the consensus recommendation across reviews — they solve the discovery and status-race problems with minimal new surface area.

## Implementation

### 1. Task group concept

A task group is a named collection of tasks on the same thread. The group label distinguishes parallel phases (e.g., `"design-review"` vs `"code-review"`).

**Redis keys:**

```
thread:<id>:group:<label>:tasks     → Redis SET of task IDs
thread:<id>:group:<label>:status    → aggregate status (pending → running → complete/error/cancelled)
```

**Lifecycle:**

1. Master enqueues N tasks with `--group <label>` — each enqueue `SADD`s the task ID into the group set and sets group status to `running`
2. Workers process tasks normally (no worker-side changes)
3. When each task completes, the worker's post-processing updates its per-task status as today. A Lua script atomically checks if all tasks in the group are terminal; if so, computes aggregate status and updates the group status key
4. Master calls `task group-wait --thread X --group <label>` which polls the group status key until terminal or timeout

### 2. `tasklib`: Group-aware enqueue and group-wait

**`Enqueue` changes** — add optional `groupLabel` parameter:

```go
func (c *Client) Enqueue(ctx context.Context, worker, threadID, groupLabel, instruction string) (*Task, error) {
    // Generate taskID, acquire thread lock (unchanged)
    // ... existing logic ...

    // If groupLabel is set, add task to group set and init group status
    if groupLabel != "" {
        c.rdb.SAdd(ctx, GroupTasksKey(threadID, groupLabel), taskID)
        c.rdb.Set(ctx, GroupStatusKey(threadID, groupLabel), "running", TTLTask)
    }
    // ... rest unchanged ...
}
```

**`WaitTask` changes** — after task completes and status is set, check group membership:

```go
// After setting task status to terminal:
if groupLabel != "" {
    // Lua script: check if all tasks in group set are terminal.
    // If yes, compute aggregate status (all done → complete, any failed → error, any cancelled → cancelled)
    // and update group status key.
    // If the task was the last one, also compute and set thread status once.
    c.checkAndUpdateGroup(ctx, threadID, groupLabel, taskID)
}
```

**New `GroupWait` method:**

```go
func (c *Client) GroupWait(ctx context.Context, threadID, groupLabel string, timeout time.Duration) (*GroupResult, error)

type GroupResult struct {
    Status   string            // complete | error | cancelled | timeout
    Tasks    map[string]string // taskID → status
    FailedAt string            // set if timeout
}
```

### 3. CLI: `--group` flag and `group-wait` command

```bash
# Sequential (no group — acquires thread lock as before)
task enqueue --worker claude --thread my-thread --instruction "implement..."

# Parallel fan-out with task group
task enqueue --worker copilot   --thread my-thread --group "design-review" --instruction "..."
task enqueue --worker opencode  --thread my-thread --group "design-review" --instruction "..."
task enqueue --worker codex     --thread my-thread --group "design-review" --instruction "..."
task enqueue --worker claude    --thread my-thread --group "design-review" --instruction "..."

# Wait for group to complete
task group-wait --thread my-thread --group "design-review" --timeout 600
```

**`group-wait` output** (JSON):

```json
{
  "status": "complete",
  "tasks": {
    "task-abc-123": "done",
    "task-def-456": "done",
    "task-ghi-789": "done",
    "task-jkl-012": "failed"
  }
}
```

**`group-wait` exit codes:**
- `0` — all tasks done (`status: "complete"`)
- `1` — any task failed, cancelled, or timeout (`status: "error"` / `"cancelled"` / `"timeout"`)

**`group-wait` polling**: Uses `GET group:<label>:status` every 2s. No scanning — the group status key is updated atomically by the completion Lua script. Terminal tasks from earlier phases are irrelevant because they aren't in the group's SET.

**Timeout behavior**: If `group-wait` times out with pending/running tasks, it returns `status: "timeout"` with a per-task breakdown showing which tasks are still pending/running. The master can then decide to cancel pending tasks, requeue-stale, or abort the phase.

### 4. Thread status aggregation

When the last task in a group completes, the Lua script sets thread status once based on aggregate outcome:

| Group outcome | Thread status |
|---------------|---------------|
| All tasks `done` | `complete` |
| Any task `failed` | `error` |
| All tasks `cancelled` | `cancelled` |
| Mixed done + cancelled | `complete` (cancelled tasks excluded from aggregate) |

If a task fails, the remaining tasks in the group continue to completion — `group-wait` doesn't preempt them. The master can inspect per-task statuses in the output to decide whether to retry individual failures or abort the phase.

### 5. Master workflow (CLAUDE.md instructions)

For parallel phases (design review, code review):

```bash
THREAD="my-feature"

# Fan out parallel review tasks under a named group
task enqueue --worker copilot  --thread $THREAD --group "design-review" --instruction "..."
task enqueue --worker opencode --thread $THREAD --group "design-review" --instruction "..."
task enqueue --worker codex    --thread $THREAD --group "design-review" --instruction "..."
task enqueue --worker claude   --thread $THREAD --group "design-review" --instruction "..."

# Wait for group to finish; capture aggregate result
RESULT=$(task group-wait --thread $THREAD --group "design-review" --timeout 600)
if echo "$RESULT" | jq -e '.status == "error"' > /dev/null; then
  # Handle failure: inspect .tasks for which ones failed, retry if needed
  FAILED=$(echo "$RESULT" | jq -r '.tasks | to_entries | map(select(.value != "done")) | .[].key')
  for TID in $FAILED; do
    task enqueue --worker ... --thread $THREAD --group "design-review-retry" --instruction "..."
  done
  task group-wait --thread $THREAD --group "design-review-retry" --timeout 600
fi
```

For sequential phases (implement, revise, merge), continue using default locked enqueue (no `--group`):

```bash
task enqueue --worker claude --thread $THREAD --instruction "implement..."
task wait --id $TASK_ID --timeout 1800
```

**Thread state transitions** the master should set via `task thread-update`:

| Phase | Thread status |
|-------|---------------|
| Design started | `designing` |
| Design review (group-wait) | `reviewing` → `complete` or `error` |
| Implementation | `implementing` |
| Code review (group-wait) | `reviewing` → `complete` or `error` |
| Done | `complete` |

## Rationale

### Why task groups over sub-threads?
Sub-threads (one thread per reviewer) work today with zero code changes — the master creates `thread:my-feature/review-copilot`, `.../review-opencode`, etc., and enqueues normally. This is viable for 4 workers. However, sub-threads scatter state across N thread keys, making it harder to observe "what phase is this feature in?" at a glance. Task groups keep all tasks on one thread, with the group label providing phase-level observability. Sub-threads remain a valid fallback if task groups prove insufficient.

### Why task groups over reference-counted locks?
Reference-counted locks (INCR/DECR on `thread:<id>:lock`) eliminate the `--no-lock` flag but don't solve discovery — the master still needs to find and poll all tasks on a thread. The group SET provides explicit membership, avoiding SCAN-based discovery entirely.

### Why task groups over batch/fan-out primitives?
A single `task enqueue --workers copilot,opencode,codex` that auto-fans-out is ergonomic but hides the per-worker nature of the delegation. Explicit per-worker enqueue with a shared group label keeps the master in control of which workers get which instructions (important when routing reviews away from the implementer).

### Thread history interleaving
During parallel phases, worker outputs are appended to the same thread history list in completion order. This means messages from different workers will be interleaved. This is acceptable because:
- The master reads review results from **files** (`docs/design-review-copilot.md`, etc.), not from thread history. Output files are the correctness barrier.
- Workers read thread history for context, but a revising implementer won't read interleaved review messages — they read the review files directly.
- If debugging is needed, each message in thread history is timestamped and tagged with `role` and `task_id`, making it possible to reconstruct per-worker timelines.

### Safety
- **No lock interference**: Task groups don't touch `thread:<id>:lock`. Sequential phases with locks work exactly as before.
- **No worker changes**: Workers process tasks identically — they don't know about groups. The group logic runs in `WaitTask`'s post-completion path.
- **`task requeue-stale`**: Works identically. Tasks in a group that are requeued remain in the group SET until they complete.
- **Lock TTL (2100s)**: Unchanged. Sequential tasks acquire and release the lock normally. The lock is never held during parallel phases.
- **Crash recovery**: If a worker crashes during a parallel task, the task remains in the group SET as `pending`. `group-wait` will timeout, reporting the stuck task. The master can requeue-stale or cancel and re-enqueue.
- **Accidental parallelism guard**: Tasks with `--group` do NOT skip the thread lock — they still require `SET NX`. This means a group cannot be started while a sequential task holds the lock, and vice versa. Parallel phases are explicitly gated by the master releasing the lock after the previous sequential phase completes.
