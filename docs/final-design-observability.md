# Final Design — System Observability & Debuggability

Based on [issue #89](https://github.com/noodle05/ai-agents/issues/89) and review comments from all four workers.

---

## Problem Statement

When something goes wrong with the master agent or a worker, there is no centralized way to answer:

- "Did the task enqueue properly?"
- "Why did a thread get stuck?"
- "What caused a request to be cancelled?"
- "Which worker handled this task, and on which host?"
- "How long did the task wait in queue vs. actually execute?"
- "Has this task been retried? How many times?"

The system relies on ad-hoc Redis inspection and stdout logs scattered across containers.

---

## Current State

### API Endpoints

| Endpoint | What it does |
|----------|-------------|
| `GET /api/health` | Redis ping + aggregate worker counts + active concurrent |
| `GET /api/stats` | Aggregate task counts by status, success rate, avg duration, queue depths |
| `GET /api/workers` | Worker list (online/instances/active) |
| `GET /api/workers/{worker_type}` | Single worker type aggregate |
| `GET /api/tasks` | Task list with filtering |
| `GET /api/tasks/{id}` | Single task detail |
| `GET /api/tasks/{id}/result` | Task result with optional `?tail=N` |
| `GET /api/threads` | List all threads |
| `GET /api/threads/{id}` | Thread state |
| `GET /api/threads/{id}/history` | Thread message history |

### CLI Commands

`task enqueue`, `task status`, `task result`, `task list`, `task wait`, `task cancel`, `task requeue-stale`, `task unlock`, `task thread-create`, `task thread-state`, `task thread-update`, `task thread-list`, `task thread-history`, `task thread-cleanup`, `task group-wait`

### Redis Key Patterns

| Pattern | Type | TTL | Set by |
|---------|------|-----|--------|
| `task:{id}:status` | STRING | 24h | Enqueue + Worker |
| `task:{id}:worker` | STRING | 24h | Enqueue |
| `task:{id}:thread_id` | STRING | 24h | Enqueue |
| `task:{id}:description` | STRING | 24h | Enqueue |
| `task:{id}:result` | STRING | 24h | Worker |
| `task:{id}:exit_code` | STRING | 24h | Worker |
| `task:{id}:created_at` | STRING | 24h | Enqueue, **overwritten by Worker** |
| `task:{id}:completed_at` | STRING | 24h | Worker |
| `task:{id}:cancel` | STRING | 24h | CancelTask |
| `tasks:queue:{worker}` | LIST | — | Enqueue |
| `tasks:processing:{worker}` | LIST | — | Worker (BLMOVE) |
| `thread:{id}:current_state` | HASH | 7d | CreateThread |
| `thread:{id}:messages` | LIST | 7d | Enqueue + Worker |
| `thread:{id}:lock` | STRING | 9300s | Enqueue |
| `thread:{id}:running` | STRING | 9300s | Request handler |
| `thread:{id}:complete` | STRING | 7d | — |
| `thread:{id}:session_id` | STRING | 7d | — |
| `thread:{id}:last_activity` | STRING | 7d | — |
| `thread:{id}:group:{label}:tasks` | SET | 7d | EnqueueGroup |
| `worker:{type}:{hostname}:heartbeat` | STRING | 30s | Worker heartbeat |

### Logging

- **Worker**: Hand-rolled JSON-line logger (`cmd/worker/main.go:380-395`), writes to stdout. Levels: `info`, `warn`.
- **WebUI**: stdlib `log.Printf` with `[webui]` / `[request]` prefix, writes to stderr.
- **task CLI**: `fmt.Println` / `fmt.Fprintf(os.Stderr, ...)`.

---

## Bugs Found During Review

These are fixed in Phase 1 before any new features:

| # | Bug | Location | Impact |
|---|-----|----------|--------|
| B1 | `created_at` overwritten by worker | `cmd/worker/main.go:188` | Enqueue time lost; queue wait time cannot be computed |
| B2 | `/api/stats` scans only 50 tasks | `tasklib/tasks.go:330-331` (`limit=0` → 50) | All aggregate stats incomplete |
| B3 | Heartbeat value is literal `"1"` | `tasklib/workers.go:93` | Zero data carried; per-instance visibility requires extra Redis round-trips |
| B4 | No correlation ID | — | Cannot trace request → thread → task → execution end-to-end |
| B5 | No cancellation audit trail | `tasklib/tasks.go:617` `CancelTask` | Cannot tell who cancelled or why |
| B6 | `StartedAt` field on `Task` struct never read from Redis | `tasklib/tasks.go:269-270` keys slice | Struct field exists but is dead code |
| B7 | `cmdStatus` duplicates GetTask key slice | `cmd/task/main.go:272` | CLI will silently omit new fields after changes |
| B8 | `DeleteThread` hardcodes key list | `tasklib/threads.go:264-272` | New keys (events, locked_at) will be orphaned |
| B9 | `SetThreadTTL` only refreshes 2 keys | `tasklib/threads.go:278-281` | New keys will outlive thread TTL and leak memory |

---

## Resolved Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Event storage | Capped lists (RPUSH + LTRIM) | Matches existing `thread:{id}:messages` pattern. Redis Streams add consumer group complexity the system doesn't need. |
| `/api/health` vs `/api/diagnostics` | Split into two endpoints | `/api/health` stays lightweight (<5ms) for Docker healthchecks. `/api/diagnostics` does the expensive scans (locks, stale tasks, key-space). |
| Structured logging | `log/slog` (Go stdlib) | Replaces hand-rolled worker logger + webui `log.Printf`. JSON handler, log levels, structured fields — all from the standard library. |
| Heartbeat enrichment | Worker writes JSON into heartbeat value | The master already SCANs heartbeat keys. Encoding `hostname`, `tasks_processed`, `current_task_id`, `queue_depth` in the value avoids N extra Redis GETs per stats poll. |
| Per-instance worker detail | New endpoint `GET /api/workers/{type}/instances` | Keeps `GET /api/workers` as aggregate summary (HTMX templates unaffected). Fed by heartbeat JSON values. |
| CLI commands | Extend existing commands, add only `task why` | `task status --id X` gains new fields. `task thread-state --id X` gains event tail. `task list --thread X` gains `--verbose`. No 6 new subcommands. |
| Atomic counters vs Prometheus | Redis atomic counters first | `/api/stats` reads Redis counters directly. Persistent across restarts. Prometheus endpoint in Phase 3 reads from same Redis keys. |
| `heartbeat_version` field | Skip | No consumer parses the heartbeat value today. `json.Unmarshal` failure is sufficient format-change signal. Add a version if/when needed. |
| Event duplication | Thread events stay scoped, no copy to system:events | `GET /api/events` reads `system:events` only. `GET /api/threads/{id}/events` reads thread events. No dual-write complexity. |
| `stats:task_running` counter | Computed from `active_tasks` hash size, not INCR/DECR | Avoids drift on worker crash (no deferred cleanup). Matches existing `GetWorkerStats` pattern. |

---

## Phase 1 — Critical Fixes (immediate, low effort, high impact)

### 1.1 Fix `created_at` overwrite (B1, B6)

Split the single overwritten key into three:

| Key | Set by | When | Overwritten on retry? |
|-----|--------|------|-----------------------|
| `task:{id}:enqueued_at` | `Enqueue` / `EnqueueGroup` | Task creation | Never |
| `task:{id}:started_at` | Worker `processOneTask` | First dequeue only | Never |
| `task:{id}:last_started_at` | Worker `processOneTask` | Every dequeue (including retries) | Yes |

**Files to change:**

- **`tasklib/tasks.go`** — `Enqueue` and `EnqueueGroup`: rename `"created_at"` → `"enqueued_at"` in pipeline SETs. Add `StartedAt` / `EnqueuedAt` fields to returned `Task`. Update `GetTask` keys slice to include `"enqueued_at"`, `"started_at"`, `"last_started_at"`. Wire `StartedAt` in the switch.
- **`cmd/worker/main.go:188`** — Pipeline: replace `"created_at"` with `"started_at"` (SETNX, skip if already exists — preserves first dequeue time across retries) and add `"last_started_at"` (always SET).
- **`tasklib/tasks.go:650`** `RequeueStale` — update to read `"last_started_at"` instead of `"created_at"` for staleness detection.
- **`cmd/webui/internal/api/system.go:41`** `/api/stats` — update duration calculation to use `enqueued_at` → `completed_at` for wall-clock time, `started_at` → `completed_at` for processing time.

### 1.2 Fix `/api/stats` 50-task limit (B2)

Switch from `ListTasks` scan to atomic Redis counters. New keys:

| Key | Type | Semantics |
|-----|------|-----------|
| `stats:task_total` | STRING (INCR) | Total tasks ever enqueued |
| `stats:task_done` | STRING (INCR) | Tasks completed successfully |
| `stats:task_failed` | STRING (INCR) | Tasks that failed |
| `stats:task_cancelled` | STRING (INCR) | Tasks cancelled |

`stats:task_running` and `stats:task_pending` are computed:
- `stats:task_running` = size of `active_tasks` hash
- `stats:task_pending` = sum of `LLEN tasks:queue:{worker}` across all worker types

**Files to change:**

- **`tasklib/tasks.go`** — `Enqueue` and `EnqueueGroup`: add `INCR stats:task_total` (with `TTLTask` expiry) to the pipeline.
- **`cmd/worker/main.go`** — `processOneTask`: after setting terminal status, `INCR stats:task_done` / `stats:task_failed` / `stats:task_cancelled`.
- **`cmd/webui/internal/api/system.go`** — Rewrite `stats()` to read counters via `MGET` + compute running/pending from `HLen active_tasks` + `LLEN` queue keys. Remove the `ListTasks` call entirely.
- **`tasklib/tasks.go:617`** `CancelTask` — `INCR stats:task_cancelled`.

### 1.3 Enrich heartbeat value (B3)

Change `UpdateWorkerHeartbeat` (`tasklib/workers.go:93`) from writing `"1"` to writing a JSON object:

```json
{
  "hostname": "worker-claude-abc123",
  "tasks_processed": 42,
  "current_task_id": "uuid-or-null",
  "queue_depth": 3,
  "uptime_seconds": 3600
}
```

**Files to change:**

- **`tasklib/workers.go`** — `UpdateWorkerHeartbeat`: accept additional fields, marshal JSON, `SETEX` with 30s TTL.
- **`tasklib/workers.go`** — Add `WorkerInstance` struct: `{Hostname, LastHeartbeat, Uptime, TasksProcessed, CurrentTaskID, QueueDepth, Online}`. Add `GetWorkerInstances(ctx, workerType) ([]WorkerInstance, error)` that parses heartbeat JSON values from the existing SCAN.
- **`cmd/worker/main.go`** — Track `tasksProcessed` counter, pass to heartbeat. Compute `queueDepth` from `LLEN` on its own queue key during the BLMOVE loop. Track `startTime` for uptime.

### 1.4 Add task lifecycle keys

New Redis keys, all with `TTLTask` (24h):

| Key | Set by | When |
|-----|--------|------|
| `task:{id}:worker_hostname` | Worker | On dequeue |
| `task:{id}:retry_count` | `RequeueStale` / Worker | INCR on requeue |
| `task:{id}:error_message` | Worker | On failure (dedicated key, no `[FAILED]` prefix) |
| `task:{id}:correlation_id` | Worker | Copied from thread state on dequeue |
| `task:{id}:cancelled_by` | CancelTask / Worker | `"user"` \| `"timeout"` \| `"system"` |
| `task:{id}:cancelled_at` | Worker | ISO8601 when cancelled |
| `task:{id}:cancelled_previous_status` | Worker | Status at cancellation time |

**Files to change:**

- **`cmd/worker/main.go`** — `processOneTask`: add `worker_hostname`, `correlation_id` to the pipeline at line 183-189. On failure: set `error_message` key separately. On cancellation (line 240-245): add `cancelled_by`, `cancelled_at`, `cancelled_previous_status`.
- **`tasklib/tasks.go`** — `GetTask`: add new keys to the read pipeline and struct.
- **`tasklib/tasks.go`** — `CancelTask`: accept a `cancelledBy` parameter (`"user"` | `"timeout"` | `"system"`), set `task:{id}:cancelled_by`.
- **`cmd/task/main.go`** — `task cancel` command: pass `"user"` as cancelledBy.
- **`cmd/webui/internal/api/requests.go`** — thread cancel handler: pass `"user"` as cancelledBy.

### 1.5 Master healthcheck in docker-compose.yml

**File to change:** `docker-compose.yml` — add to the `master` service:

```yaml
healthcheck:
  test: ["CMD", "curl", "-f", "http://localhost:8000/api/health"]
  interval: 10s
  retries: 3
  start_period: 5s
```

---

## Phase 2 — Event System & Diagnostics (this sprint)

### 2.1 Add `correlation_id` to thread state

Generate a UUID at thread creation and store it in the `thread:{id}:current_state` hash.

**Files to change:**

- **`tasklib/threads.go`** — `CreateThread`: generate `correlation_id` via `NewUUID()`, add to the HSet mapping. Add `CorrelationID` field to `Thread` struct. `GetThread` reads it.
- **`tasklib/threads.go`** — `DeleteThread`: include `thread:{id}:events`, `thread:{id}:locked_at` keys in the deletion list. (Fixes B8.)
- **`tasklib/threads.go`** — `SetThreadTTL`: add `thread:{id}:events` to the pipeline. (Fixes B9.)
- **`cmd/worker/main.go`** — `processOneTask`: read `correlation_id` from thread state, include in task keys and log lines.

**Scope:** `correlation_id` is thread-scoped, not request-scoped. One web request may create multiple threads; all tasks in a thread share the same `correlation_id`. This means you can trace "which thread?" from any task/log, but not "which HTTP request?" — that requires the web layer to inject a request_id, which is out of scope for this issue.

### 2.2 Unified event system

Two scopes, one envelope. Capped lists with LTRIM after each push.

| Key | Scope | Cap (LTRIM) | TTL |
|-----|-------|-------------|-----|
| `thread:{id}:events` | Per-thread: task lifecycle, lock/unlock, status changes | 1000 | 7d |
| `system:events` | Cross-cutting: worker online/offline | 10000 | 7d |

**Event envelope** (every event):
```json
{
  "event_id": "<uuid>",
  "type": "task_enqueued|task_started|task_completed|task_failed|task_cancelled|lock_acquired|lock_released|thread_status_change|worker_online|worker_offline",
  "timestamp": "<ISO8601>",
  "correlation_id": "<thread correlation_id or null>",
  "task_id": "<task id or null>",
  "worker_type": "<copilot|claude|opencode|codex|master>",
  "worker_hostname": "<hostname>",
  "detail": {}
}
```

**Typed `detail` payloads per event type:**

| Event type | detail fields |
|------------|---------------|
| `task_enqueued` | `queue_depth_after` |
| `task_started` | *(empty)* |
| `task_completed` | `exit_code`, `duration_ms` |
| `task_failed` | `exit_code`, `error_message` |
| `task_cancelled` | `cancelled_by`, `previous_status` |
| `lock_acquired` | `holder_task_id` |
| `lock_released` | `holder_task_id`, `held_duration_ms` |
| `thread_status_change` | `from`, `to` |
| `worker_online` | `worker_type`, `hostname` |
| `worker_offline` | `worker_type`, `hostname` |

**Files to change:**

- **`tasklib/events.go`** (new) — `PushEvent(ctx, key, event)`, `GetEvents(ctx, key, limit int)`, `Event` struct, typed detail structs. LTRIM after each RPUSH.
- **`tasklib/tasks.go`** — `Enqueue` / `EnqueueGroup`: push `task_enqueued` event to `thread:{id}:events`.
- **`cmd/worker/main.go`** — `processOneTask`: push `task_started`, `task_completed`/`task_failed`/`task_cancelled` events.
- **`tasklib/client.go`** — `AcquireThreadLock` (internal to Enqueue): push `lock_acquired`/`lock_released` events.
- **`cmd/worker/main.go`** — Heartbeat goroutine: on first successful heartbeat, push `worker_online` to `system:events`. On graceful shutdown, push `worker_offline`.

**New API endpoints:**
- `GET /api/threads/{id}/events?limit=50` — reads from `thread:{id}:events`
- `GET /api/events?limit=50&type=worker_online` — reads from `system:events`

**Event emission points** (exact locations — not left unspecified):

| Event | Emission point | File:line |
|-------|---------------|-----------|
| `task_enqueued` | `Enqueue` / `EnqueueGroup` (best-effort) | `tasklib/tasks.go` |
| `task_started` | Worker dequeue pipeline | `cmd/worker/main.go:~183` |
| `task_completed` | Worker completion (exit 0) | `cmd/worker/main.go:~307` |
| `task_failed` | Worker completion (exit ≠ 0) | `cmd/worker/main.go:~307` |
| `task_cancelled` | Worker cancel-check | `cmd/worker/main.go:~240` |
| `task_requeued` | `RequeueStale` | `tasklib/tasks.go:~671` |
| `lock_acquired` | `Enqueue` SET NX success | `tasklib/tasks.go:68` |
| `lock_released` | `Enqueue` error-paths, `UnlockThread`, `EnqueueGroup` DEL | `tasklib/tasks.go`, `threads.go` |
| `thread_status_change` | `updateThreadStatus` | `tasklib/tasks.go:681` |
| `group_complete` | `GroupWait` when all tasks terminal | `tasklib/tasks.go` |
| `worker_online` | Worker startup first heartbeat | `cmd/worker/main.go` |
| `worker_offline` | Heartbeat TTL expiry detected by `GetWorkerStats` | `tasklib/workers.go` |

**Events are best-effort** — log errors, never fail the parent operation. Enqueue latency/reliability > event completeness.

**New CLI:**
- No new subcommands. `task thread-state --id X` includes last 20 events. `task why --thread X` includes event tail.

### 2.3 Split `/api/health` from `/api/diagnostics`

**`/api/health`** — unchanged, stays lightweight for Docker healthchecks:
- Redis ping
- Worker aggregate counts (`GetWorkerStats`)
- Active concurrent requests

**`/api/diagnostics`** (new):
- Lock listing: which threads are locked, by whom, for how long (reads `thread:{id}:locked_at`, new key)
- Stale task detection: tasks in `active_tasks` with `started_at` > N minutes ago (configurable via `?stale_threshold=30`, default 30)
- Per-worker queue depths (from `LLEN`)
- Thread counts: total, active, stuck
- Redis memory info (`INFO memory` via raw Redis command)
- Key-space summary: count by pattern (SCAN with COUNT)

**New key:** `thread:{id}:locked_at` — ISO8601 timestamp set atomically with the lock.

**Atomicity:** Lock acquisition in `Enqueue` and `EnqueueGroup` must set `locked_at` atomically with `SET NX`. Use a Lua script to prevent the window where the lock exists but `locked_at` doesn't (crash between two Redis calls):

```lua
-- SET lock + locked_at atomically
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'EX', ARGV[2]) then
  redis.call('SET', KEYS[2], ARGV[3])
  return 1
end
return 0
```

**Files to change:**

- **`tasklib/client.go`** — `Enqueue` / `EnqueueGroup`: use Lua script for atomic lock + `locked_at`.
- **`tasklib/client.go`** — `UnlockThread` / lock release paths: delete `thread:{id}:locked_at`.
- **`cmd/webui/internal/api/diagnostics.go`** (new) — `GET /api/diagnostics` handler.
- **`cmd/webui/internal/api/router.go`** — register route.

### 2.4 Adopt `log/slog`

Replace the custom `logger` struct in `cmd/worker/main.go:380-395` and bare `log.Printf` in `cmd/webui/` with `log/slog` (Go stdlib since 1.21).

**Standard log format** (JSON handler):
```json
{"ts":"2026-05-19T10:30:00Z","level":"info","msg":"task dequeued","worker":"claude","task_id":"...","thread_id":"...","correlation_id":"..."}
```

**Backward compat:** Current worker emits `{"level":"info",...}` (lowercase). `slog` defaults to uppercase `"INFO"`. Use `ReplaceAttr` to normalize level to lowercase and match existing field names:

```go
replaceAttr := func(groups []string, a slog.Attr) slog.Attr {
    if a.Key == slog.LevelKey {
        return slog.String("level", strings.ToLower(a.Value.String()))
    }
    if a.Key == slog.TimeKey {
        return slog.String("ts", a.Value.Time().UTC().Format(time.RFC3339))
    }
    return a
}
handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
    Level:       logLevel,
    ReplaceAttr: replaceAttr,
})
```

**Access log separation:** Two loggers — `appLogger` (application events, always writes to stdout) and `accessLogger` (HTTP requests, controlled by `--log-access` flag, default `false`). When disabled, access logger writes to `io.Discard`. This keeps application logs clean by default while allowing HTTP request logging when needed for debugging.

**Log level control:** `--log-level` flag (debug/info/warn/error). Default: `info`. In debug mode, every log line includes `correlation_id`, `task_id`, `thread_id`, `worker_type`.

**Files to change:**

- **`cmd/worker/main.go`** — Delete `logger` struct. Use `slog.NewJSONHandler`. Wire `--log-level` flag.
- **`cmd/webui/main.go`** — Switch from `log.Printf` to `slog`.
- **`cmd/webui/internal/api/*.go`** — Replace `log.Printf` calls.
- **`cmd/webui/internal/request/handler.go`** — Replace custom `logger`.

### 2.5 Per-instance worker detail endpoint

**New endpoint:** `GET /api/workers/{type}/instances`

Returns per-hostname data parsed from heartbeat JSON values. The existing `GetWorkerStats` SCAN already finds all heartbeat keys — `GetWorkerInstances` extends this to parse the value instead of just checking TTL.

**Files to change:**

- **`tasklib/workers.go`** — Add `WorkerInstance` struct. Add `GetWorkerInstances(ctx, workerType) ([]WorkerInstance, error)`.
- **`cmd/webui/internal/api/workers.go`** — Add handler for `GET /api/workers/{type}/instances`.
- **`cmd/webui/internal/api/router.go`** — Register route.

### 2.6 `task why --thread X` command

Aggregates into a single JSON blob:
- Thread status + last update time
- Last error message + timestamp (derived from scanning thread tasks — find first `failed` task, read its `error_message` key. No new Redis key needed.)
- Lock state: holder task ID, held duration (from `locked_at`)
- Task state summary: counts by status
- Recent events tail (last 20 from `thread:{id}:events`)
- Any stuck tasks (running > N minutes)

**Files to change:**

- **`tasklib/threads.go`** — Add `GetThreadDiagnostics(ctx, threadID) (ThreadDiagnostics, error)`.
- **`cmd/task/main.go`** — Add `task why` subcommand.

### 2.7 Enrich existing CLI commands

No new subcommands beyond `task why`. Extend existing ones:

| Command | New fields |
|---------|-----------|
| `task status --id X` | `enqueued_at`, `started_at`, `last_started_at`, `worker_hostname`, `retry_count`, `error_message`, `correlation_id`, `cancelled_by`, `cancelled_at` |
| `task list --thread X --verbose` | Per-task timing and retry count |
| `task thread-state --id X` | Task summary, last error, recent events tail (last 20) |

**Files to change:**

- **`tasklib/tasks.go`** — `GetTask` reads all new keys (already in scope from 1.4).
- **`cmd/task/main.go`** — Update output formatting in `task status`, `task list`, `task thread-state`.
- **`cmd/task/main.go`** — Refactor `cmdStatus` to call `client.GetTask(ctx, id)` instead of inline Redis GETs. Eliminates ~15 lines of duplicate code and ensures CLI output stays in sync with API automatically. (Fixes B7.)

---

## Phase 3 — Cross-Cutting Visibility (next sprint)

### 3.1 System event log API

- `GET /api/events?limit=50&type=worker_online` — reads from `system:events` capped list.
- `task events --limit 50 --type X` — CLI equivalent.

### 3.2 Alerting webhooks (config-driven, optional)

- Env var: `ALERT_WEBHOOK_URL`.
- `WEBHOOK_SECRET` for HMAC signature header.
- Triggers (opt-in via env vars `ALERT_ON_FAILED=true`, etc.):
  - Task enters `failed` state
  - Thread stuck (task running > N minutes)
  - Worker heartbeat lost > 60s
- Simple POST with JSON payload describing the event.

**Implementation:** In `cmd/worker/main.go`, after setting task status to `failed`, check `ALERT_WEBHOOK_URL` and fire-and-forget POST. In the master (WebUI), a background goroutine polls for stale tasks and lost heartbeats, firing webhooks on threshold breach.

### 3.3 Prometheus metrics endpoint (deferred from "Future" to "Phase 3")

Add `GET /metrics` with `prometheus/client_golang`:

- `tasks_total{worker,status}` — counter (read from `stats:task_*` Redis keys)
- `task_duration_seconds{worker}` — histogram (computed by worker, stored in Redis or in-memory)
- `task_queue_wait_seconds{worker}` — histogram (computed from `enqueued_at` → `started_at`)
- `threads_active` — gauge (count of non-terminal threads)
- `workers_online{type}` — gauge (from heartbeat SCAN)
- `queue_depth{worker}` — gauge (from Redis LLEN)

The metrics endpoint reads from Redis atomic counters and heartbeat data — no dual-write needed. Histogram data is stored in-memory (the WebUI process accumulates it), with the understanding that it resets on restart.

---

## Rejected Ideas

| Idea | Reason for rejection |
|------|---------------------|
| `thread:{id}:task_count` cached counter | Drift risk. Counting tasks per thread is cheap via `ListTasks` filter. Skip unless threads routinely have 1000+ tasks. |
| Dead letter queue | Task reliability feature, not observability. Design after observing failure patterns with the new tooling. |
| Docker healthchecks for workers | If a worker is hung, its heartbeat stops — `GetWorkerStats` already surfaces that. Container restarts from failed healthchecks can mask bugs. |
| 6 new CLI subcommands | Extend existing commands instead. `task status` gains fields, `task thread-state` gains event tail, `task list` gains `--verbose`. |
| Redis Streams for events | Consumer groups, ACK tracking, message ID semantics add complexity with no benefit for internal debugging. Capped lists match the existing `thread:{id}:messages` pattern. |
| `heartbeat_version` field | No consumer parses the value today. Add when needed. |
| Event duplication across scopes | Thread events stay on `thread:{id}:events`. System events on `system:events`. No dual-write. |
| `stats:task_running` as INCR/DECR counter | Crash-induced drift. Compute from `active_tasks` hash size instead. |

---

## Execution Plan

**Branch**: `feature/observability-debuggability`
**Base**: `main`
**Total**: ~1100 lines across 15 files. No new dependencies (besides `prometheus/client_golang` in Phase 3).

### Files changed with line estimates

| File | Phase | Est. lines | Key changes |
|------|-------|-----------|-------------|
| `tasklib/tasks.go` | 1, 2 | ~140 | Fix stats (counters), add lifecycle keys, CancelTask signature, event emission, RequeueStale retry_count |
| `tasklib/workers.go` | 1, 2 | ~80 | HeartbeatData struct, UpdateWorkerHeartbeat JSON, GetWorkerInstances, backward-compat parse |
| `tasklib/threads.go` | 1, 2 | ~65 | correlation_id in CreateThread, lock Lua script, DeleteThread/SetThreadTTL fixes, ThreadEventsKey |
| `tasklib/client.go` | 1, 2 | ~25 | ThreadLockedAtKey helper, SystemEventsKey, event push helper |
| `tasklib/events.go` (new) | 2 | ~50 | Event structs, PushEvent, GetEvents |
| `tasklib/tasks_obs_test.go` (new) | 1, 2 | ~350 | Counter increments, heartbeat JSON round-trip, event envelope, GetTask new fields, cancellation audit fields |
| `cmd/worker/main.go` | 1, 2 | ~75 | Fix created_at→started_at, lifecycle keys, event emission, slog migration, heartbeat enrichment |
| `cmd/webui/internal/api/system.go` | 1 | ~50 | Fix stats (counters) |
| `cmd/webui/internal/api/diagnostics.go` (new) | 2 | ~60 | /api/diagnostics handler |
| `cmd/webui/internal/api/workers.go` | 2 | ~50 | Per-instance handler |
| `cmd/webui/internal/api/events.go` (new) | 2, 3 | ~40 | Event endpoint handlers |
| `cmd/webui/internal/api/router.go` | 2 | ~15 | Routes: /api/diagnostics, /api/workers/{type}/instances, /api/threads/{id}/events |
| `cmd/webui/main.go` | 2 | ~20 | slog migration, --log-level flag, --log-access flag |
| `cmd/webui/internal/request/handler.go` | 2 | ~10 | slog migration |
| `cmd/task/main.go` | 2 | ~120 | task why, refactor cmdStatus→GetTask, enrich status/list/thread-state |
| `docker-compose.yml` | 1 | ~6 | Master healthcheck |

### Phase 1 implementation order (sequential within phase)

1. B1: Fix `created_at` overwrite (tasks.go + worker/main.go)
2. B2: Fix `/api/stats` with atomic counters (tasks.go + system.go)
3. B3: Enrich heartbeat value (workers.go + worker/main.go)
4. B4/B5: Add task lifecycle keys + correlation_id (tasks.go + threads.go + worker/main.go)
5. B6: Refactor cmdStatus to call GetTask (cmd/task/main.go)
6. B7/B8: Fix DeleteThread + SetThreadTTL (threads.go)
7. Master healthcheck (docker-compose.yml)
8. Write `tasklib/tasks_obs_test.go`

### Phase 2 implementation order

1. Event system: envelope types + RPUSH/LTRIM helpers (client.go + events.go)
2. Event emission points in tasks.go + worker/main.go
3. correlation_id in thread state + worker dequeue (threads.go + worker/main.go)
4. lock Lua script for atomic locked_at (client.go)
5. `/api/diagnostics` endpoint (system.go + router.go)
6. `/api/workers/{type}/instances` endpoint (workers.go + router.go)
7. `log/slog` migration with ReplaceAttr (worker/main.go + webui/main.go)
8. `task why` command (cmd/task/main.go)
9. Enrich existing CLI output (cmd/task/main.go)

### Phase 3 (separate PR, next sprint)

1. `GET /api/events` endpoint + system events
2. Alerting webhook dispatch
3. Redis memory monitoring in diagnostics
4. Prometheus `/metrics` endpoint

---

## Debugging Guide (for CLAUDE.md)

After implementation, add the following to `CLAUDE.md`:

### Quick diagnostic commands

```bash
# One-shot thread diagnosis
task why --thread <id>

# Task lifecycle detail
task status --id <task-id>

# Thread status with recent events
task thread-state --id <thread-id>

# System-wide event tail
task events --limit 50

# Per-worker instance detail
curl -H "Authorization: Bearer $WEBUI_API_KEY" http://localhost:8000/api/workers/claude/instances
```

### Common debugging workflows

**"Why is this thread stuck?"**
```bash
task why --thread <id>
# Look at: lock_state, stuck_tasks, recent_events
```

**"Did the task actually start?"**
```bash
task status --id <task-id>
# Check: enqueued_at (when it was created), started_at (when worker picked it up),
# last_started_at (most recent attempt if retried), retry_count
```

**"Which worker ran this and where?"**
```bash
task status --id <task-id>
# Check: worker_hostname field
```

**"What happened across the system in the last hour?"**
```bash
curl "http://localhost:8000/api/events?limit=100"
```

**"Is anything broken right now?"**
```bash
curl http://localhost:8000/api/diagnostics
# Check: stale_tasks, locks, queue_depths, redis_memory
```
