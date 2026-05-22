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
task:<id>:group                     → group label this task belongs to (allows reverse lookup)
```

No separate group status key — aggregate status is computed client-side by `group-wait`.

**Lifecycle:**

1. Master enqueues N tasks with `--group <label>` — each enqueue acquires the thread lock as a gate-check (`SET NX`), then immediately releases it (`DEL`) so subsequent group enqueues succeed
2. Each enqueue `SADD`s the task ID into the group SET and stores the group label on the task via `task:<id>:group`
3. Workers process tasks normally (no worker-side changes — workers don't know about groups)
4. `WaitTask` for group tasks suppresses the thread status update (the lock `DEL` is a no-op since the lock was already released)
5. Master calls `task group-wait --thread X --group <label>`, which polls each task in the group SET, computes aggregate status client-side, and updates thread status once when all tasks are terminal

### 2. `tasklib`: Group-aware enqueue and group-wait

No changes to the `Enqueue` signature — callers are untouched. A separate `EnqueueGroup` method handles groups, avoiding breakage of existing call sites in `cmd/task`, tests, and web UI.

**`EnqueueGroup` method:**

```go
func (c *Client) EnqueueGroup(ctx context.Context, worker, threadID, groupLabel, instruction string) (*Task, error)
```

**Lock semantics:** Acquire-and-immediately-release. This gates on the sequential phase being complete (lock must not be held by a running sequential task) while allowing multiple group enqueues:

```go
func (c *Client) EnqueueGroup(ctx context.Context, worker, threadID, groupLabel, instruction string) (*Task, error) {
    taskID, _ := NewUUID()
    now := ts()

    // Gate: fail if thread is locked by a sequential task
    lockKey := ThreadLockKey(threadID)
    ok, _ := c.rdb.SetNX(ctx, lockKey, taskID, LockTTL).Result()
    if !ok {
        return nil, fmt.Errorf("thread locked — finish sequential phase before starting group")
    }
    c.rdb.Del(ctx, lockKey) // release immediately; group-wait handles completion gating

    // Append instruction to thread history, enqueue to worker queue, init task keys
    // ... (same as Enqueue) ...

    // Add to group SET and tag the task
    c.rdb.SAdd(ctx, GroupTasksKey(threadID, groupLabel), taskID)
    c.rdb.Expire(ctx, GroupTasksKey(threadID, groupLabel), TTLTask)
    c.rdb.Set(ctx, TaskKey(taskID, "group"), groupLabel, TTLTask)

    return task, nil
}
```

**`WaitTask` changes — suppress thread status update for group tasks:**

In `WaitTask` (tasks.go:291), the completion path currently does:

```go
c.updateThreadStatus(ctx, threadID, status)   // ← skip for group tasks
c.rdb.Del(ctx, ThreadLockKey(threadID))        // ← already deleted, no-op
```

For group tasks, `updateThreadStatus` is skipped. The thread status is set once by `group-wait` after all tasks complete. Detection: read `task:<id>:group` — if non-empty, this is a group task.

```go
// In WaitTask's completion path:
groupLabel, _ := c.rdb.Get(ctx, TaskKey(taskID, "group")).Result()
if groupLabel == "" {
    c.updateThreadStatus(ctx, threadID, status) // sequential task
    c.rdb.Del(ctx, ThreadLockKey(threadID))
}
// Group tasks: thread status handled by group-wait
```

**`WaitTask` lock release:** `DEL thread:<id>:lock` is safe for group tasks — the lock was already deleted by `EnqueueGroup`, so `DEL` on a non-existent key is a no-op.

**New `GroupWait` method:**

```go
type GroupResult struct {
    Status   string            `json:"status"`   // complete | error | cancelled | timeout
    Tasks    map[string]string `json:"tasks"`    // taskID → status
}

func (c *Client) GroupWait(ctx context.Context, threadID, groupLabel string, timeout time.Duration) (*GroupResult, error)
```

**`GroupWait` implementation** — client-side polling (no Lua script, no worker changes):

```
1. SMEMBERS thread:<id>:group:<label>:tasks → get task ID set
2. Every 2s:
   a. Pipeline GET task:<id>:status for each task ID
   b. If any status is "pending" or "running", continue polling
   c. If all terminal:
      - Compute aggregate: all "done" → "complete", any "failed" → "error", any "cancelled" → "cancelled"
      - Set thread status via updateThreadStatus(ctx, threadID, aggregateStatus)
      - Return GroupResult with status and per-task breakdown
3. If timeout expires:
   - Return GroupResult{Status: "timeout", Tasks: <current snapshot>}
```

**Why client-side polling over Lua script:** For 4 workers, polling is O(4) GETs every 2s — negligible. A Lua script only matters if multiple concurrent writers update a group status key, but `group-wait` is the sole writer — there's no race. This keeps workers unchanged (no Lua script in worker completion path) and avoids the trigger-gap where a Lua script inside `WaitTask` would never fire because the master calls `group-wait` instead of `wait`.

### 3. CLI: `--group` flag and `group-wait` command

```bash
# Sequential (no group — acquires and holds thread lock)
task enqueue --worker claude --thread my-thread --instruction "implement..."

# Parallel fan-out with task group
task enqueue --worker copilot   --thread my-thread --group "design-review" --instruction "..."
task enqueue --worker opencode  --thread my-thread --group "design-review" --instruction "..."
task enqueue --worker codex     --thread my-thread --group "design-review" --instruction "..."
task enqueue --worker claude    --thread my-thread --group "design-review" --instruction "..."

# Wait for group to complete
task group-wait --thread my-thread --group "design-review" --timeout 1200
```

When `--group` is set, the CLI calls `EnqueueGroup` instead of `Enqueue`. The JSON output is identical (`{"task_id": "..."}`) — downstream scripts that parse the output need no changes.

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

**Timeout behavior**: The `--timeout` on `group-wait` is a _group-level_ deadline — the maximum wall-clock time to wait for all tasks in the group. Individual tasks have their own per-task timeout (passed via `task enqueue --timeout N`). A per-task timeout of 900s (15 min) with a group timeout of 1200s (20 min) means all tasks get 900s each; the group waits up to 1200s for the slowest. This is independent of `LockTTL` (7500s) since the lock is not held during parallel phases.

### 4. Thread status aggregation

Thread status is set once by `group-wait` when all tasks complete, based on aggregate outcome:

| Group outcome | Thread status |
|---------------|---------------|
| All tasks `done` | `complete` |
| Any task `failed` | `error` |
| All tasks `cancelled` | `cancelled` |
| Mixed done + cancelled | `complete` (cancelled tasks excluded) |

If a task fails, the remaining tasks in the group continue to completion — `group-wait` doesn't preempt them. The master inspects per-task statuses in the output to decide whether to retry individual failures or abort the phase.

**Thread state lifecycle** — the master sets thread status at phase boundaries via `task thread-update`:

| Phase | Thread status |
|-------|---------------|
| Design started | `designing` |
| Design review (during group-wait) | `reviewing` |
| Design review complete | `complete` or `error` (set by group-wait) |
| Implementation | `implementing` |
| Code review (during group-wait) | `reviewing` |
| Code review complete | `complete` or `error` (set by group-wait) |
| Done | `complete` |

### 5. Master workflow (CLAUDE.md instructions)

For parallel phases (design review, code review):

```bash
THREAD="my-feature"

# Master transitions thread status
task thread-update --id $THREAD --status "reviewing"

# Fan out parallel review tasks under a named group
task enqueue --worker copilot  --thread $THREAD --group "design-review" --instruction "..."
task enqueue --worker opencode --thread $THREAD --group "design-review" --instruction "..."
task enqueue --worker codex    --thread $THREAD --group "design-review" --instruction "..."
task enqueue --worker claude   --thread $THREAD --group "design-review" --instruction "..."

# Wait for group to finish; capture aggregate result
RESULT=$(task group-wait --thread $THREAD --group "design-review" --timeout 1200)
if echo "$RESULT" | jq -e '.status == "error"' > /dev/null; then
  # Handle failure: inspect .tasks for which ones failed
  FAILED=$(echo "$RESULT" | jq -r '.tasks | to_entries | map(select(.value != "done")) | .[].key')
  for TID in $FAILED; do
    WORKER=$(task status --id "$TID" | jq -r .worker)
    task enqueue --worker "$WORKER" --thread $THREAD --group "design-review-retry" --instruction "..."
  done
  task group-wait --thread $THREAD --group "design-review-retry" --timeout 1200
fi
```

For sequential phases (implement, revise, merge), continue using default locked enqueue (no `--group`):

```bash
task enqueue --worker claude --thread $THREAD --instruction "implement..."
task wait --id $TASK_ID --timeout 1200
```

### 6. V1 Interim: Sub-Threads (Zero Code Changes)

Sub-threads per reviewer work today with zero code changes and provide immediate parallelism while task groups are implemented. This is the recommended interim step:

```bash
THREAD="my-feature"

# Create a sub-thread per reviewer (no new CLI, no new Redis keys)
task thread-create --id "$THREAD-review-copilot"
task thread-create --id "$THREAD-review-opencode"
task thread-create --id "$THREAD-review-codex"
task thread-create --id "$THREAD-review-claude"

# Fan out — each sub-thread has its own lock, naturally parallel
T1=$(task enqueue --worker copilot  --thread "$THREAD-review-copilot"  --instruction "..." | jq -r .task_id)
T2=$(task enqueue --worker opencode --thread "$THREAD-review-opencode" --instruction "..." | jq -r .task_id)
T3=$(task enqueue --worker codex    --thread "$THREAD-review-codex"    --instruction "..." | jq -r .task_id)
T4=$(task enqueue --worker claude   --thread "$THREAD-review-claude"   --instruction "..." | jq -r .task_id)

# Wait for all
for tid in "$T1" "$T2" "$T3" "$T4"; do
  task wait --id "$tid" --timeout 1200
done
```

**Advantages over waiting for task groups:**
- Ships today — zero code changes to `tasklib`, CLI, Redis schema, or workers
- Per-reviewer thread isolation: clean history, independent debugging
- Per-reviewer status: each thread shows its reviewer's status unambiguously
- Recovery: if copilot fails, retry just that sub-thread

This should be documented in CLAUDE.md immediately while task groups are implemented.

## Test Plan

Integration tests (new file `tasklib/tasks_group_test.go`):

| Test | What it verifies |
|------|-----------------|
| `TestEnqueueGroupFanOut` | 4 `EnqueueGroup` calls on same thread all succeed (lock gate-check + immediate release) |
| `TestEnqueueGroupFailsWhenLocked` | `EnqueueGroup` fails if a sequential task holds the thread lock |
| `TestGroupWaitAllDone` | `GroupWait` returns `status: complete` when all tasks finish with `done` |
| `TestGroupWaitMixedOutcomes` | `GroupWait` returns `status: error` when any task fails |
| `TestGroupWaitCancelled` | `GroupWait` returns `status: cancelled` when all tasks cancelled |
| `TestGroupWaitTimeout` | `GroupWait` returns `status: timeout` with per-task breakdown |
| `TestGroupTasksInSet` | All group task IDs are in the `thread:<id>:group:<label>:tasks` SET |
| `TestGroupTaskLabelPersisted` | `task:<id>:group` key is set on each group task |
| `TestWaitTaskSkipsThreadStatusForGroup` | `WaitTask` on a group task does NOT update thread status |
| `TestThreadStatusAfterGroupWait` | Thread status set by `GroupWait` matches aggregate outcome |
| `TestParallelSequentialPhases` | Sequential enqueue after group-wait acquires lock normally |

## Rationale

### Why task groups over sub-threads?
Sub-threads (one thread per reviewer) work today with zero code changes. However, they scatter state across N thread keys, making it harder to observe "what phase is this feature in?" at a glance. Task groups keep all tasks on one thread, with the group label providing phase-level observability. **Sub-threads are recommended as the v1 interim** (section 6 above) while task groups are implemented.

### Why task groups over reference-counted locks?
Reference-counted locks (INCR/DECR on `thread:<id>:lock`) eliminate the flag but don't solve discovery — the master still needs to find and poll all tasks on a thread. The group SET provides explicit membership, avoiding SCAN-based discovery entirely.

### Why task groups over batch/fan-out primitives?
A single `task enqueue --workers copilot,opencode,codex` that auto-fans-out is ergonomic but hides the per-worker nature of the delegation. Explicit per-worker enqueue with a shared group label keeps the master in control of which workers get which instructions (important when routing reviews away from the implementer).

### Thread history interleaving
During parallel phases, worker outputs are appended to the same thread history list in completion order. This is acceptable because:
- The master reads review results from **files** (`docs/design-review-copilot.md`, etc.), not from thread history. Output files are the correctness barrier.
- Workers read thread history for context, but a revising implementer won't read interleaved review messages — they read the review files directly.
- Each message in thread history is timestamped and tagged with `role` and `task_id` (set by `Enqueue`/`EnqueueGroup` in `metadata`), making it possible to reconstruct per-worker timelines.
- `group-wait` writes a single summary message to thread history on completion (e.g., `"Group 'design-review' complete: 3 done, 1 failed"`), giving the master a clean phase-boundary entry point.

### Safety
- **Lock gate-check**: `EnqueueGroup` does `SET NX` → `DEL` as a gate, preventing group creation while a sequential task holds the lock. Once the gate passes, the lock is immediately released so subsequent group enqueues succeed.
- **No worker changes**: Workers process tasks identically — they don't know about groups. Group logic is entirely in `EnqueueGroup`, `WaitTask` (suppress status update), and `GroupWait`.
- **`task requeue-stale`**: Works identically. Tasks in a group that are requeued remain in the group SET until they complete.
- **Lock TTL (7500s)**: Unchanged for sequential tasks. During parallel phases, the lock is not held — the gate-check releases it immediately.
- **Crash recovery**: If a worker crashes during a parallel task, the task remains in the group SET as `pending`. `GroupWait` will timeout, reporting the stuck task. The master can requeue-stale or cancel and re-enqueue.
- **Thread status correctness**: `WaitTask` suppresses `updateThreadStatus` for group tasks. Thread status is set exactly once by `GroupWait` after all tasks are terminal, based on aggregate outcome. No race condition.
