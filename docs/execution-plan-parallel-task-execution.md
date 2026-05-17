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
| `tasklib` (Redis keys + structs) | `client.go` | ~10 lines |
| `tasklib` (EnqueueGroup) | `tasks.go` | ~50 lines |
| `tasklib` (WaitTask change) | `tasks.go` | ~5 lines modified |
| `tasklib` (GroupWait) | `tasks.go` | ~60 lines |
| CLI (`--group` flag) | `cmd/task/main.go` | ~10 lines |
| CLI (`group-wait` subcommand) | `cmd/task/main.go` | ~30 lines |
| Tests | `tasklib/tasks_group_test.go` (new) | ~250 lines |
| Docs | `CLAUDE.md` | ~30 lines |

**No worker changes.** Workers process tasks identically — they don't know about groups. Group logic is entirely in the master's enqueue/wait path.

**No web UI changes.** The web UI surfaces tasks and threads using existing keys; group membership is stored in new keys that the UI doesn't query. Group-wait is CLI-only (master automation).

---

## Phase 1 — `tasklib/client.go`: Key helpers + `GroupResult` struct

**Risk**: none. New key constructors, no logic.

Two new Redis key helpers following the existing naming pattern:

- `GroupTasksKey(threadID, label)` → `"thread:<id>:group:<label>:tasks"` — Redis SET of task IDs belonging to a group
- `TaskGroupKey(taskID)` → `"task:<id>:group"` — reverse-lookup: group label a task belongs to

Plus a `GroupResult` struct returned by `GroupWait`:

```go
type GroupResult struct {
    Status string            `json:"status"` // complete | error | cancelled | timeout
    Tasks  map[string]string `json:"tasks"`  // taskID → status
}
```

---

## Phase 2 — `tasklib/tasks.go`: `EnqueueGroup` method

**Risk**: low. New code path, existing `Enqueue` untouched.

A new method patterned closely on `Enqueue` (same payload structure, same queue push, same thread history append, same per-task key initialization), with two behavioral differences:

1. **Lock gate-check instead of hold**: `SET NX` → check result → `DEL` immediately. This gates on the sequential phase being complete (lock must not be held by a running sequential task) while allowing subsequent group enqueues to pass the same gate.

2. **Group membership tracking**: After task initialization, `SADD` the task ID into `thread:<id>:group:<label>:tasks` and `SET` `task:<id>:group` to the group label.

```go
func (c *Client) EnqueueGroup(ctx context.Context, worker, threadID, groupLabel, instruction string) (*Task, error)
```

Returns the same `(*Task, error)` as `Enqueue`. The JSON output from the CLI is identical (`{"task_id": "..."}`).

---

## Phase 3 — `tasklib/tasks.go`: `WaitTask` modification

**Risk**: low. Gated behind an empty-string check; existing sequential path unchanged.

One conditional added in the completion path. After the terminal-status block builds the return `*Task`, before calling `updateThreadStatus` and `DEL lock`:

```go
groupLabel, _ := c.rdb.Get(ctx, TaskKey(taskID, "group")).Result()
if groupLabel == "" {
    c.updateThreadStatus(ctx, threadID, status)
    c.rdb.Del(ctx, ThreadLockKey(threadID))
}
```

For group tasks (`task:<id>:group` is non-empty):
- Skip `updateThreadStatus` — aggregate status is computed later by `GroupWait` once all group tasks complete
- Skip lock release — the lock was already released by `EnqueueGroup`; `DEL` on a nonexistent key is a no-op

For sequential tasks (key is empty or absent): existing behavior, no change.

---

## Phase 4 — `tasklib/tasks.go`: `GroupWait` method

**Risk**: medium. New polling logic, but no Lua scripts, no worker changes, single writer (no race).

```go
func (c *Client) GroupWait(ctx context.Context, threadID, groupLabel string, timeout time.Duration) (*GroupResult, error)
```

Implementation: client-side polling loop.

1. `SMEMBERS thread:<id>:group:<label>:tasks` → get task ID set
2. Every 2s: pipeline `GET task:<id>:status` for each task ID
3. If any status is `"pending"` or `"running"`, continue polling
4. If all terminal:
   - Compute aggregate: all `"done"` → `"complete"`, any `"failed"` → `"error"`, all `"cancelled"` → `"cancelled"`, mixed `"done"` + `"cancelled"` → `"complete"`
   - Call `updateThreadStatus(ctx, threadID, aggregateStatus)` once
   - Return `GroupResult` with status and per-task breakdown
5. If timeout expires: return `GroupResult{Status: "timeout", Tasks: <snapshot>}`

For 4 workers, this is 4 `GET`s every 2s — negligible.

---

## Phase 5 — `cmd/task/main.go`: `--group` flag on `task enqueue`

**Risk**: low. Conditional dispatch, existing path unchanged.

Add a `--group` string flag to the existing `enqueue` subcommand. In `cmdEnqueue`:

- If `--group` is set: call `c.EnqueueGroup(ctx, worker, threadID, groupLabel, instruction)`
- If not set: call `c.Enqueue(ctx, worker, threadID, instruction)` (unchanged path)

The JSON output is `{"task_id": "..."}` in both cases — downstream scripts parsing enqueue output need no changes.

---

## Phase 6 — `cmd/task/main.go`: New `task group-wait` subcommand

**Risk**: low. New subcommand, no existing command changes.

New cobra subcommand:

```
task group-wait --thread <id> --group <label> --timeout <seconds>
```

Flags: `--thread` (required), `--group` (required), `--timeout` (default 600).

Handler calls `c.GroupWait(ctx, threadID, groupLabel, timeout)` and prints JSON result to stdout. Exit codes:

- `0` — all tasks done (`status: "complete"`)
- `1` — any task failed, cancelled, or timeout

---

## Phase 7 — `tasklib/tasks_group_test.go`: Integration tests

**Risk**: low. New test file, uses existing `miniredis` harness from `tasks_test.go`.

11 tests as specified in the design doc:

| # | Test | What it verifies |
|---|------|-----------------|
| 1 | `TestEnqueueGroupFanOut` | 4 `EnqueueGroup` calls on same thread all succeed |
| 2 | `TestEnqueueGroupFailsWhenLocked` | `EnqueueGroup` fails if a sequential task holds the thread lock |
| 3 | `TestGroupWaitAllDone` | `GroupWait` returns `"complete"` when all tasks finish `"done"` |
| 4 | `TestGroupWaitMixedOutcomes` | `GroupWait` returns `"error"` when any task fails |
| 5 | `TestGroupWaitCancelled` | `GroupWait` returns `"cancelled"` when all tasks cancelled |
| 6 | `TestGroupWaitTimeout` | `GroupWait` returns `"timeout"` with per-task breakdown |
| 7 | `TestGroupTasksInSet` | All group task IDs are in the group SET |
| 8 | `TestGroupTaskLabelPersisted` | `task:<id>:group` key is set on each group task |
| 9 | `TestWaitTaskSkipsThreadStatusForGroup` | `WaitTask` on a group task does NOT update thread status |
| 10 | `TestThreadStatusAfterGroupWait` | Thread status set by `GroupWait` matches aggregate outcome |
| 11 | `TestParallelSequentialPhases` | Sequential enqueue after `GroupWait` acquires lock normally |

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
  task wait --id "$tid" --timeout 600
done
```

### 8b. Production workflow: task groups

Replace the sequential enqueue instructions in steps 2 (design review) and 4 (code review) with `--group` + `group-wait`:

```bash
# Master transitions thread status
task thread-update --id $THREAD --status "reviewing"

# Fan out parallel review tasks under a named group
task enqueue --worker copilot  --thread $THREAD --group "design-review" --instruction "..."
task enqueue --worker opencode --thread $THREAD --group "design-review" --instruction "..."
task enqueue --worker codex    --thread $THREAD --group "design-review" --instruction "..."
task enqueue --worker claude   --thread $THREAD --group "design-review" --instruction "..."

# Wait for group to finish
RESULT=$(task group-wait --thread $THREAD --group "design-review" --timeout 600)
```

Sequential phases (implement, revise, merge) continue using default locked enqueue (no `--group`).

---

## Review checklist

- [ ] `EnqueueGroup` lock semantics correct? Gate-check + immediate release, no TTL extension risk
- [ ] `WaitTask` group-task detection correct? Empty-string check on `task:<id>:group` safe?
- [ ] `GroupWait` timeout behavior: does it return partial results on timeout or just an error?
- [ ] Thread status lifecycle: does `group-wait` set the right status for each aggregate outcome?
- [ ] Test coverage: are all 11 tests non-flaky with `miniredis`?
- [ ] `CLAUDE.md` clarity: do the sub-thread and task-group instructions read clearly for an LLM agent?
- [ ] No worker changes needed — confirmed?
- [ ] No web UI changes needed — confirmed?
