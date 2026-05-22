# Execution Plan: Parallel Task Execution (Task Groups)

**Branch**: `feature/parallel-task-execution`  
**Base**: `main`  
**Design doc**: [docs/design-parallel-task-execution.md](docs/design-parallel-task-execution.md)

## Summary

This feature introduces **task groups** — a lightweight Redis-native concept that lets the master fan out independent review tasks to multiple workers in parallel, replacing the current thread-lock serialization for design-review and code-review phases.

The thread lock still serializes sequential phases (implement → revise → merge). For parallel review phases, workers operate on read-only copies and write to separate review files — no conflicts, no reason to serialize.

## Scope

| Layer | Files changed | New code |
|-------|--------------|----------|
| `tasklib` (Redis keys + structs) | `client.go` | ~15 lines |
| `tasklib` (EnqueueGroup) | `tasks.go` | ~60 lines |
| `tasklib` (WaitTask change) | `tasks.go` | ~10 lines modified |
| `tasklib` (GroupWait) | `tasks.go` | ~70 lines |
| CLI (`--group` flag) | `cmd/task/main.go` | ~15 lines |
| CLI (`group-wait` subcommand) | `cmd/task/main.go` | ~30 lines |
| Tests | `tasklib/tasks_group_test.go` (new) | ~300 lines |
| Docs | `CLAUDE.md` | ~30 lines |

**No worker changes.** Workers process tasks identically — they don't know about groups. Group logic is entirely in the master's enqueue/wait path.

**No web UI changes.** The web UI surfaces tasks and threads using existing keys; group membership is stored in new keys that the UI doesn't query. Group-wait is CLI-only (master automation).

---

## Phase 1 — `tasklib/client.go`: Key helpers + `GroupResult` struct

**Risk**: none. New key constructors, no logic.

One new Redis key helper following the existing naming pattern:

- `GroupTasksKey(threadID, label)` → `"thread:<id>:group:<label>:tasks"` — Redis SET of task IDs belonging to a group

The per-task reverse-lookup uses the existing `TaskKey(taskID, "group")` → `"task:<id>:group"` — no new exported function needed. This follows the existing `TaskKey(taskID, "<field>")` pattern used for `status`, `type`, etc.

Plus a `GroupResult` struct returned by `GroupWait`:

```go
type GroupResult struct {
    ThreadID string            `json:"thread_id"`
    Label    string            `json:"label"`
    Status   string            `json:"status"` // complete | error | cancelled | timeout
    Tasks    map[string]string `json:"tasks"`  // taskID → status
}
```

`ThreadID` and `Label` make the output self-contained — the CLI consumer doesn't need to correlate with the command that produced it.

---

## Phase 2 — `tasklib/tasks.go`: `EnqueueGroup` method

**Risk**: low. New code path, existing `Enqueue` untouched.

A new method patterned closely on `Enqueue` (same payload structure, same queue push, same thread history append, same per-task key initialization), with these behavioral differences:

1. **Group label validation**: Before any Redis operations, validate that `groupLabel` contains no `:` or whitespace characters (which would produce malformed Redis keys). Return an error if invalid.

2. **Lock gate-check instead of hold**: `SET NX` → check result → `DEL` immediately. This gates on the sequential phase being complete (lock must not be held by a running sequential task) while allowing subsequent group enqueues to pass the same gate.

   Use a **short TTL** (10s) on the gate-check lock, not `LockTTL` (9300s). The lock is held for ~1ms; if `DEL` fails due to a transient network error, a 10s TTL prevents blocking all sequential enqueues for 155 minutes. Recovery path: `task unlock --thread <id>` for the rare case where the process crashes between `SET NX` and `DEL`.

3. **Lock gate race**: The master transitions the thread status to `"reviewing"` **before** fanning out group tasks (see Phase 8b). Between `EnqueueGroup` releasing the lock (immediate `DEL`) and the next `EnqueueGroup` call, a sequential `Enqueue` could theoretically acquire the lock. This can't happen in practice because the master is the sole enqueuer and runs commands sequentially. Document this assumption in `CLAUDE.md`.

4. **Group membership tracking**: Set `task:<id>:group` and `SADD` into `thread:<id>:group:<label>:tasks` **before** pushing the task to the worker queue. If either Redis write fails, the task is never dequeued by workers — no "lost" task risk. Only push to the queue after both group keys are set.

5. **Duplicate group membership prevention**: Before setting `task:<id>:group`, check if the key already exists and is non-empty. If the task is already in a group, return an error to prevent stranding the first group's `GroupWait`.

6. **TTLs on new keys**: After `SADD`, set `Expire(ctx, GroupTasksKey(threadID, label), TTLThread)` (7d) on the group SET. Use `Set(ctx, TaskKey(taskID, "group"), label, TTLTask)` (24h) for the per-task group key. This matches the existing TTL strategy: task keys 24h, thread keys 7d.

```go
func (c *Client) EnqueueGroup(ctx context.Context, worker, threadID, groupLabel, instruction string) (*Task, error)
```

Returns the same `(*Task, error)` as `Enqueue`. The JSON output from the CLI is identical (`{"task_id": "..."}`).

---

## Phase 3 — `tasklib/tasks.go`: `WaitTask` modification

**Risk**: low. Gated behind an empty-string check; existing sequential path unchanged.

The shared gate applied on **all three exit paths** (completion, timeout, context cancellation):

```go
groupLabel, _ := c.rdb.Get(ctx, TaskKey(taskID, "group")).Result()
if groupLabel == "" {
    if threadID != "" {
        c.rdb.Del(ctx, ThreadLockKey(threadID))
    }
    c.updateThreadStatus(ctx, threadID, status)
}
```

For group tasks (`task:<id>:group` is non-empty):
- Skip `updateThreadStatus` — aggregate status is computed later by `GroupWait` once all group tasks complete
- Skip lock release — the lock was already released by `EnqueueGroup`; if a sequential task subsequently acquired the lock, releasing it here would be incorrect

For sequential tasks (key is empty or absent): existing behavior, no change.

**Why all three paths need the gate**: Without gating the timeout and cancellation paths, a `WaitTask` on a group task that times out (or is cancelled) would unconditionally `DEL` the thread lock. If a sequential task was enqueued after the group fan-out (e.g., by a retry script), that sequential task's lock would be incorrectly released.

---

## Phase 4 — `tasklib/tasks.go`: `GroupWait` method

**Risk**: medium. New polling logic, but no Lua scripts, no worker changes, single writer (no race).

```go
func (c *Client) GroupWait(ctx context.Context, threadID, groupLabel string, timeout time.Duration) (*GroupResult, error)
```

Implementation: client-side polling loop.

1. `SMEMBERS thread:<id>:group:<label>:tasks` → get task ID set
2. If `len(taskIDs) == 0`, return error: `"group '<label>' not found or has no tasks"` — prevents vacuous "complete" result from an empty set
3. Every 2s: pipeline `GET task:<id>:status` for each task ID
4. If any status is `"pending"` or `"running"`, continue polling
5. If all terminal, compute aggregate:
   - **Any `"failed"` → `"error"`** (takes highest priority, overrides all other statuses — includes `failed` + `cancelled` combination)
   - All `"done"` → `"complete"`
   - All `"cancelled"` → `"cancelled"`
   - Mixed `"done"` + `"cancelled"` → `"complete"` (the master should inspect `.tasks` to distinguish all-succeeded from some-cancelled)
   - Call `updateThreadStatus(ctx, threadID, aggregateStatus)` once
   - Return `(*GroupResult, nil)` with status and per-task breakdown
6. If timeout expires: return `(*GroupResult, nil)` with `Status: "timeout"` and per-task snapshot. **Do not** call `updateThreadStatus` — some tasks may still be running, so the thread is not in a terminal state. The CLI handler uses `result.Status` to decide the exit code.

**Timeout return convention**: timeout returns `(result, nil)`, not `(nil, error)`. The caller gets the task snapshot. The CLI handler (Phase 6) decides exit code based on `result.Status`.

For 4 workers, this is 4 `GET`s every 2s — negligible.

---

## Phase 5 — `cmd/task/main.go`: `--group` flag on `task enqueue`

**Risk**: low. Conditional dispatch, existing path unchanged.

Add a `--group` string flag to the existing `enqueue` subcommand. In `cmdEnqueue`:

- Validate that the `--group` value contains no `:` or whitespace characters. Return an error before any API call if invalid.
- If `--group` is set and valid: call `c.EnqueueGroup(ctx, worker, threadID, groupLabel, instruction)`
- If not set: call `c.Enqueue(ctx, worker, threadID, instruction)` (unchanged path)

The JSON output is `{"task_id": "..."}` in both cases — downstream scripts parsing enqueue output need no changes.

---

## Phase 6 — `cmd/task/main.go`: New `task group-wait` subcommand

**Risk**: low. New subcommand, no existing command changes.

New cobra subcommand:

```
task group-wait --thread <id> --group <label> --timeout <seconds>
```

Flags: `--thread` (required), `--group` (required), `--timeout` (default 2100).

Handler calls `c.GroupWait(ctx, threadID, groupLabel, timeout)` and prints JSON result to stdout. Exit codes based on `result.Status`:

- `"complete"` → exit 0
- `"error"`, `"cancelled"`, `"timeout"` → exit 1
- Go-level error (e.g., group not found) → exit 1, print error to stderr

---

## Phase 7 — `tasklib/tasks_group_test.go`: Integration tests

**Risk**: low. New test file, uses existing `miniredis` harness from `tasks_test.go`.

16 tests:

| # | Test | What it verifies |
|---|------|-----------------|
| 1 | `TestEnqueueGroupFanOut` | 4 `EnqueueGroup` calls on same thread all succeed |
| 2 | `TestEnqueueGroupFailsWhenLocked` | `EnqueueGroup` fails if a sequential task holds the thread lock |
| 3 | `TestEnqueueGroupRaceBetweenCalls` | Two concurrent `EnqueueGroup` calls don't race on the gate-check lock |
| 4 | `TestEnqueueGroupDuplicateGroup` | `EnqueueGroup` returns error if task is already in a group |
| 5 | `TestEnqueueGroupInvalidLabel` | `EnqueueGroup` returns error for labels with `:` or whitespace |
| 6 | `TestGroupWaitAllDone` | `GroupWait` returns `"complete"` when all tasks finish `"done"` |
| 7 | `TestGroupWaitMixedOutcomes` | `GroupWait` returns `"error"` when any task fails (including failed+cancelled) |
| 8 | `TestGroupWaitCancelled` | `GroupWait` returns `"cancelled"` when all tasks cancelled |
| 9 | `TestGroupWaitTimeout` | `GroupWait` returns `"timeout"` with per-task breakdown; thread status is NOT updated |
| 10 | `TestGroupWaitEmptyGroup` | `GroupWait` returns error for nonexistent group label (not vacuously `"complete"`) |
| 11 | `TestGroupTasksInSet` | All group task IDs are in the group SET |
| 12 | `TestGroupTaskLabelPersisted` | `task:<id>:group` key is set on each group task with TTL |
| 13 | `TestGroupKeysHaveTTLs` | Group SET and per-task group key have non-zero TTLs |
| 14 | `TestWaitTaskSkipsThreadStatusAndLockForGroup` | `WaitTask` on a group task does NOT update thread status or release lock — on all three exit paths (completion, timeout, cancellation) |
| 15 | `TestThreadStatusAfterGroupWait` | Thread status set by `GroupWait` matches aggregate outcome |
| 16 | `TestParallelSequentialPhases` | Sequential enqueue after `GroupWait` acquires lock normally |

All existing tests must continue to pass. `go test ./tasklib/...` runs both old and new tests.

---

## Phase 8 — `CLAUDE.md`: Document parallel workflow

**Risk**: none. Doc-only change.

Two additions to the "Agent Roles and Workflow" section:

### 8a. V1 interim: sub-threads (zero code changes)

Document the sub-thread pattern from section 6 of the design doc. This works today and provides immediate parallelism while task groups are implemented:

```bash
# Create a sub-thread per reviewer
task thread-create --id "$THREAD-review-copilot"
task thread-create --id "$THREAD-review-opencode"
task thread-create --id "$THREAD-review-codex"
task thread-create --id "$THREAD-review-claude"

# Fan out — each sub-thread has its own lock, naturally parallel
T1=$(task enqueue --worker copilot  --thread "$THREAD-review-copilot"  --instruction "..." | jq -r .task_id)
# ... etc

# Wait for all
for tid in "$T1" "$T2" "$T3" "$T4"; do
  task wait --id "$tid" --timeout 2100
done
```

### 8b. Production workflow: task groups

Replace the sequential enqueue instructions in steps 2 (design review) and 4 (code review) with `--group` + `group-wait`:

```bash
# Master transitions thread status to "reviewing" BEFORE fanning out
# This signals that the thread is in a parallel-safe phase
task thread-update --id $THREAD --status "reviewing"

# Fan out parallel review tasks under a named group
task enqueue --worker copilot  --thread $THREAD --group "design-review" --instruction "..."
task enqueue --worker opencode --thread $THREAD --group "design-review" --instruction "..."
task enqueue --worker codex    --thread $THREAD --group "design-review" --instruction "..."
task enqueue --worker claude   --thread $THREAD --group "design-review" --instruction "..."

# Wait for group to finish
RESULT=$(task group-wait --thread $THREAD --group "design-review" --timeout 2100)
STATUS=$(echo "$RESULT" | jq -r .status)

# Inspect .tasks to distinguish all-succeeded from some-cancelled when status is "complete"
```

**Constraint**: The master must only call `EnqueueGroup` after transitioning the thread to `"reviewing"` (or another parallel-safe status). Sequential phases (implement, revise, merge) continue using default locked enqueue (no `--group`). The master is the sole enqueuer and runs commands sequentially, so no race exists between `EnqueueGroup` calls.

---

## Review checklist

- [x] `EnqueueGroup` lock semantics correct? Gate-check + immediate release with short TTL (10s). `task unlock` recovery documented.
- [x] `WaitTask` group-task detection correct? Empty-string check gated on **all three** exit paths (completion, timeout, cancellation).
- [x] `GroupWait` timeout behavior: returns `(result, nil)` with per-task snapshot. Does not call `updateThreadStatus`.
- [x] Thread status lifecycle: `group-wait` sets status once after all tasks complete. Aggregate priority: any `"failed"` → `"error"` (highest).
- [x] Test coverage: 16 tests covering fan-out, lock gating, all terminal states, timeout, empty group, invalid labels, duplicate group, TTLs, lock-release gating on all exit paths, thread status lifecycle, and sequential-after-group.
- [x] `CLAUDE.md` clarity: sub-thread and task-group instructions read clearly for an LLM agent. Thread-status gate documented.
- [x] No worker changes needed — confirmed. Group logic is entirely in the master path.
- [x] No web UI changes needed — confirmed. Group keys are not queried by the web UI.
