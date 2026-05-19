# Final Design & Execution Plan — System Observability & Debuggability

**Issue**: [#89](https://github.com/noodle05/ai-agents/issues/89)
**Status**: Design review complete. All 4 workers (codex, claude, copilot, opencode) + master reviewed.
**This document**: Master synthesis — resolves every disputed point with rationale.

---

## Bugs Confirmed (verified in code)

| # | Bug | Location | Impact |
|---|-----|----------|--------|
| B1 | `created_at` overwritten by worker | `cmd/worker/main.go:188` writes dequeue time to `created_at` | Enqueue time lost; queue wait time uncomputable |
| B2 | `/api/stats` only sees 50 tasks | `tasklib/tasks.go:330-331` (`limit=0` → 50) | All aggregate stats incomplete at scale |
| B3 | Heartbeat value is literal `"1"` | `tasklib/workers.go:93-94` | Zero data carried; per-instance visibility needs extra round-trips |
| B4 | No correlation ID across services | — | Cannot trace request→thread→task→logs end-to-end |
| B5 | No cancellation audit trail | `tasklib/tasks.go:617` `CancelTask` only sets `cancel="1"` | Cannot tell who cancelled or why |
| B6 | `cmdStatus` duplicates GetTask key slice | `cmd/task/main.go:272` | CLI will silently omit new fields after changes |
| B7 | `DeleteThread` hardcodes key list | `tasklib/threads.go:264-272` | New keys (events, locked_at) will be orphaned |
| B8 | `SetThreadTTL` only refreshes 2 keys | `tasklib/threads.go:278-281` | New keys will outlive thread TTL and leak memory |

---

## Phase 1 — Critical Fixes (bugs + data enrichment)

### 1.1 Fix `created_at` overwrite

**Decision**: Keep `created_at` as enqueue time (no rename). Add `started_at` as a new key.
**Rationale**: Copilot correctly noted that `created_at` is set in TWO places (`Enqueue` at line 129 AND `EnqueueTaskWithKey` at line 251). Renaming both introduces migration risk for zero benefit. The `Task` struct already has a `StartedAt` field (line 26) — it's just never populated.

Changes:
- `tasklib/tasks.go` `Enqueue` (line ~129) and `EnqueueTaskWithKey` (line ~251): **no change** to `created_at` key.
- `cmd/worker/main.go:188`: change `"created_at"` → `"started_at"`. Worker already computes `startedAt` at line 169.
- `tasklib/tasks.go` `GetTask` key slice (line 270): add `"started_at"`.
- `tasklib/tasks.go` `ListTasks` per-task fetch: add `"started_at"`.

### 1.2 Fix `/api/stats` 50-task limit

**Decision**: Switch to Redis atomic counters. Use `stats:task_running` as a counter but enumerate all DECR points.
**Rationale**: `HLEN active_tasks` was proposed as an alternative but it counts all active tasks (including pending), not just running. Atomic counters are simpler and O(1).

Counter keys: `stats:task_total`, `stats:task_done`, `stats:task_failed`, `stats:task_cancelled`, `stats:task_running`, `stats:task_pending`.

INCR/DECR points (every transition):
| Transition | INCR | DECR |
|-----------|------|------|
| Enqueue → pending | `total`, `pending` | — |
| Dequeue → running (worker line ~183) | `running` | `pending` |
| Done (worker line ~307) | `done` | `running` |
| Failed (worker line ~307) | `failed` | `running` |
| Cancelled (worker cancel-check) | `cancelled` | `running` (or `pending` if pre-start) |
| `RequeueStale`: running→pending | `pending` | `running` |

`/api/stats` handler: replace `ListTasks` scan with `MGET` on all counter keys. Add `stats:total_duration_ms` + `stats:completed_count` for O(1) average duration (from opencode's suggestion).

### 1.3 Enrich heartbeat value

**Decision**: Change heartbeat value from `"1"` to JSON. Include `version` field.
**Rationale**: Copilot and opencode both flagged that omitting a version field is a future footgun. It costs 6 bytes. The design originally said no version field — reviewers convinced me otherwise.

```json
{"v":1,"hostname":"...","tasks_processed":42,"current_task_id":"...","queue_depth":3,"uptime_seconds":3600}
```

Backward compat during rollout: `GetWorkerInstances` handles parse failures gracefully — returns zero-value `HeartbeatData` with `Hostname` populated from the key name (parts[2]). Old workers writing `"1"` won't crash the new code.

Changes:
- `tasklib/workers.go`: Add `HeartbeatData` struct with `Version int \`json:"v"\``. Change `UpdateWorkerHeartbeat` signature to accept `HeartbeatData`. Add `GetWorkerInstances(ctx, workerType) ([]HeartbeatData, error)`.
- `cmd/worker/main.go`: Track `tasksProcessed`, `currentTaskID`, `uptimeSeconds` locally. Compute `queueDepth` from `LLEN tasks:queue:<workerType>`. Pass struct to heartbeat.

### 1.4 Add task lifecycle keys

| Key | Set by | When |
|-----|--------|------|
| `task:{id}:worker_hostname` | Worker | On dequeue (line ~185, add to pipeline) |
| `task:{id}:retry_count` | Client | `INCR` in `RequeueStale` / `RequeueTask` |
| `task:{id}:error_message` | Worker | On failure (line ~298) |
| `task:{id}:correlation_id` | Worker | On dequeue, read from thread state |
| `task:{id}:cancelled_by` | Caller | `"user"` (CLI/web), `"timeout"`, `"system"` (cascade) |
| `task:{id}:cancelled_at` | Caller | ISO8601 timestamp at cancel |
| `task:{id}:cancelled_previous_status` | Caller | Status at cancellation time |

**`cancelled_by` API change**: `CancelTask(ctx, taskID string)` → `CancelTask(ctx, taskID, cancelledBy string)`. Breaking change — update the 2 callers (CLI `cmdCancel`, web UI cancel handler).

**`GetTask` key slice** (tasks.go:270): Must include ALL new keys: `"started_at"`, `"worker_hostname"`, `"retry_count"`, `"error_message"`, `"correlation_id"`, `"cancelled_by"`, `"cancelled_at"`, `"cancelled_previous_status"`.

**Fix B6**: Refactor `cmdStatus` (cmd/task/main.go:272) to call `c.GetTask(ctx, id)` instead of inline Redis GETs. Eliminates ~15 lines of duplicate code.

### 1.5 Master healthcheck in docker-compose.yml

```yaml
master:
  healthcheck:
    test: ["CMD", "curl", "-f", "http://localhost:8000/api/health"]
    interval: 10s
    retries: 3
    start_period: 5s
```

---

## Phase 2 — Event System & Diagnostics

### 2.1 Unified event system

Two scopes, one envelope. **Events are best-effort** — log errors, never fail the parent operation.

| Key | Scope | Cap (LTRIM) | TTL |
|-----|-------|-------------|-----|
| `thread:{id}:events` | Per-thread: task lifecycle, lock/unlock, status changes | 1000 | 7d (matches thread TTL) |
| `system:events` | Cross-cutting: worker online/offline | 10000 | 7d |

Envelope:
```json
{
  "event_id": "<uuid>",
  "type": "task_enqueued|task_started|task_completed|task_failed|task_cancelled|task_requeued|lock_acquired|lock_released|thread_status_change|group_complete|worker_online|worker_offline",
  "timestamp": "<ISO8601>",
  "correlation_id": "<uuid>",
  "task_id": "<id or null>",
  "worker_type": "<copilot|claude|opencode|codex|master>",
  "worker_hostname": "<hostname>",
  "detail": {}
}
```

**Implementation detail**: `RPUSH` + `LTRIM` + `EXPIRE` in a single Redis pipeline (not separate calls). Prevents brief over-cap windows.

**Event emission points** (enumerated — not left unspecified):

| Event | Emission point | File:line |
|-------|---------------|-----------|
| `task_enqueued` | `Enqueue` / `EnqueueGroup` / `EnqueueTaskWithKey` (best-effort) | tasks.go |
| `task_started` | Worker dequeue pipeline | cmd/worker/main.go:~183 |
| `task_completed` | Worker completion (exit 0) | cmd/worker/main.go:~307 |
| `task_failed` | Worker completion (exit ≠ 0) | cmd/worker/main.go:~307 |
| `task_cancelled` | Worker cancel-check | cmd/worker/main.go |
| `task_requeued` | `RequeueStale` | tasks.go:~671 |
| `lock_acquired` | `Enqueue` lock SET NX success (line 68) | tasks.go |
| `lock_released` | `Enqueue` error-paths (DEL after failure), `UnlockThread` (threads.go:221), `EnqueueGroup` lock cleanup | tasks.go, threads.go |
| `thread_status_change` | `updateThreadStatus` (tasks.go:681) | tasks.go |
| `group_complete` | `GroupWait` when all tasks terminal | tasks.go |
| `worker_online` | Worker startup heartbeat | cmd/worker/main.go |
| `worker_offline` | Heartbeat TTL expiry detected by `GetWorkerStats` | tasklib/workers.go |

### 2.2 Add `correlation_id` to thread state

`CreateThread` (threads.go:38) generates UUID, stores as `correlation_id` in thread state hash. Worker reads it on dequeue and includes it in every log line (when debug) and every event.

**Clarification**: `correlation_id` is thread-scoped, not request-scoped. One web request may create multiple threads; all tasks in a thread share the same correlation_id. This means you can trace "which thread?" from any task/log, but not "which HTTP request?" — that requires the web layer to inject a request_id, which is out of scope for this issue.

### 2.3 Split `/api/health` from `/api/diagnostics`

`/api/health` — unchanged, lightweight (<5ms): Redis ping, worker counts, active concurrent.

`/api/diagnostics` — new:
- Lock listing (SCAN `thread:*:lock`, read holder + `locked_at`)
- Stale task detection (tasks in `active_tasks` with `running` status > N minutes, default 30)
- Per-worker queue depths (`LLEN tasks:queue:<worker>`)
- Thread counts: total, active, stuck
- Redis memory info (`INFO memory`)
- Key-space summary: count by pattern

**`locked_at` atomicity**: Lock acquisition in `Enqueue` and `EnqueueGroup` must set `locked_at` atomically with `SET NX`. Use a Lua script:

```lua
-- SET lock + locked_at atomically
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'EX', ARGV[2]) then
  redis.call('SET', KEYS[2], ARGV[3])
  return 1
end
return 0
```

This prevents the window where the lock exists but `locked_at` doesn't (crash between two calls).

### 2.4 Adopt `log/slog` with log levels

Replace custom JSON logger in worker and `log.Printf` in web UI with `log/slog`.

**Backward-compat**: Current worker emits `{"level":"info",...}` (lowercase). `slog` defaults to uppercase `"INFO"`. Use `ReplaceAttr` to normalize level to lowercase. Also add a `"ts"` field matching the existing timestamp format:

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
    Level:     logLevel,
    ReplaceAttr: replaceAttr,
})
```

**Access log separation**: Two loggers — `appLogger` (always on) and `accessLogger` (controlled by `--log-access`, default false). When disabled, access logger writes to `io.Discard`.

### 2.5 Per-instance worker detail

New endpoint: `GET /api/workers/{type}/instances`. Returns `[]HeartbeatData` parsed from heartbeat JSON. Data comes from the existing heartbeat `SCAN` in `GetWorkerStats` — no extra Redis round-trips.

**Merge into `GetWorkerStats`**? Codex suggested adding the instances to the existing `WorkerInfo` struct instead of a separate endpoint. **Decision**: separate endpoint. Keeps `GET /api/workers` fast (aggregate only) and lets callers opt into the detailed view only when needed.

### 2.6 `task why --thread X` command

Single JSON blob aggregating:
- Thread status + last update time
- Recent events tail (last 20 from `thread:{id}:events`)
- Lock state: holder task ID, held duration (from `locked_at`)
- Task state summary: counts by status (from `ListTasks` filter + `GetTask` for errors)
- Any stuck tasks (running > N minutes from `active_tasks`)

**`thread:{id}:last_error` removed**: Copilot correctly flagged this as undefined. `task why` derives "last error" from scanning tasks — if any task in the thread is `failed`, read its `error_message` key. No new Redis key needed.

### 2.7 Enrich existing CLI commands

No new subcommands except `task why`. Extend existing:

| Command | New fields |
|---------|-----------|
| `task status --id X` | `started_at`, `enqueued_at`, `worker_hostname`, `retry_count`, `error_message`, `correlation_id` |
| `task list --thread X --verbose` | Per-task timing and retry count |
| `task thread-state --id X` | Task summary, last error, recent events tail |

### 2.8 Fix `DeleteThread` and `SetThreadTTL`

**`DeleteThread`** (threads.go:264): Add `ThreadEventsKey(threadID)` and `ThreadLockedAtKey(threadID)` to the keys slice.

**`SetThreadTTL`** (threads.go:278): Add `ThreadEventsKey(threadID)` to the pipeline.

---

## Phase 3 — Cross-Cutting Visibility

### 3.1 System event log API

`GET /api/events?limit=N&type=X` — reads from `system:events` capped list.

### 3.2 Alerting webhooks (config-driven)

Env var: `ALERT_WEBHOOK_URL`. Triggers: task `failed`, thread stuck, worker heartbeat lost > 60s. Simple POST with `{trigger, thread_id, task_id, message, timestamp}`.

### 3.3 Redis memory monitoring

`INFO memory` exposed in `/api/diagnostics`. Key-space summary via `SCAN` + `TYPE` by pattern.

---

## Out of Scope (separate issues or deferred)

| Item | Reason |
|------|--------|
| Dead letter queue | Task reliability feature. Design after observing failure patterns. |
| Prometheus /metrics endpoint | Valuable but separate. Internal observability must be solid first. ~50 lines once data layer exists. |
| Docker healthchecks for workers | Heartbeat already covers liveness. Restarts can mask bugs. Moon-bridge healthcheck stays (no heartbeat). |
| Docker logging driver config | Ops/deployment concern, not code change. |
| Agent output streaming (`?follow=1`) | Separate feature. `?tail=N` works for post-hoc. |
| `/api/logs/{service}` endpoint | Requires Docker socket (security boundary). Use SSH + `docker logs` — documented in CLAUDE.md. |
| CLI mirror commands (`task worker-list`, `task events`, etc.) | Enrich existing commands instead. |
| `thread:{id}:task_count` cached counter | `SCAN task:*:thread_id:<id>:status` is cheap. Counter adds consistency burden. |

---

## Resolved Disputes

| Dispute | Positions | Resolution | Rationale |
|---------|-----------|------------|-----------|
| Rename `created_at` → `enqueued_at`? | Claude: yes. Copilot: no, keep as-is. | **Keep `created_at` as enqueue time**. Add `started_at` as new key. | Two sites set `created_at`. Rename is unnecessary risk. `Task.StartedAt` already exists on the struct. |
| Heartbeat version field? | Claude: no. Copilot + Opencode: yes. | **Include `"v": 1`**. | 6 bytes. Future consumers (Grafana, monitors) need format detection. Reviewer consensus against original design. |
| `stats:task_running` counter or `HLEN active_tasks`? | Claude: counter. Codex: `HLEN`. | **Counter, with DECR on ALL exit paths**. | `HLEN active_tasks` includes pending tasks, not just running. Counter is precise if DECR is exhaustive. |
| `locked_at` set atomically or sequentially? | Claude: sequential. Copilot: must be atomic. | **Lua script for atomic SET NX + SET**. | Copilot is right. Crash between SET NX and SET leaves lock without timestamp → "held duration" broken. |
| Event emission from `Enqueue` — block or best-effort? | Claude: unspecified. Codex: best-effort or pipeline. | **Best-effort — log error, don't fail enqueue**. | Enqueue latency/reliability > event completeness. Events are debugging aid, not system-critical. |
| Per-instance worker data: separate endpoint or merge? | Claude: separate. Codex: merge into `WorkerInfo`. | **Separate endpoint `GET /api/workers/{type}/instances`**. | Keeps aggregate endpoint fast. Callers opt into detail. |
| `cmdStatus` refactor: call `GetTask` or update duplicate? | Claude: update duplicate. Opencode: call `GetTask`. | **Refactor to call `GetTask`**. | Eliminates drift. 15 lines deleted. CLI output stays in sync with API automatically. |
| `slog` format: uppercase level ok? | Claude: didn't address. Copilot: will break parsers. | **`ReplaceAttr` to normalize level to lowercase**. | Existing log pipelines (Docker json-file, Loki) expect `{"level":"info"}`. |
| `thread:{id}:last_error` — define or remove? | Claude: referenced but never defined. Copilot: flagged. | **Remove. Derive from task scan in `task why`**. | No new key needed. Scan thread tasks, find first `failed`, read its `error_message`. |
| `correlation_id` scope? | Claude: thread-level. Opencode: notes it won't trace HTTP requests. | **Thread-level. Document the limitation**. | Web request ID injection is a separate concern (needs middleware). |

---

## Execution Plan

**Branch**: `feature/observability-debuggability`
**Base**: `main`

### Files changed

| File | Phase | Est. lines | Key changes |
|------|-------|-----------|-------------|
| `tasklib/tasks.go` | 1, 2 | ~140 | Fix stats (counters), add lifecycle keys, CancelTask signature, event emission points, RequeueStale retry_count |
| `tasklib/workers.go` | 1, 2 | ~80 | HeartbeatData struct, UpdateWorkerHeartbeat JSON, GetWorkerInstances, backward-compat parse |
| `tasklib/threads.go` | 1, 2 | ~65 | correlation_id in CreateThread, lock Lua script, DeleteThread/SetThreadTTL fixes, ThreadEventsKey, event push helper |
| `tasklib/client.go` | 1, 2 | ~25 | Export ts() → Timestamp(), SystemEventsKey, event push helper, locked_at key helper |
| `tasklib/tasks_obs_test.go` (new) | 1, 2 | ~350 | Counter increments, heartbeat JSON round-trip, event envelope, GetTask new fields, DeleteThread key cleanup, LTRIM cap, cancellation audit fields |
| `cmd/worker/main.go` | 1, 2 | ~75 | Fix created_at→started_at, add lifecycle keys, event emission, slog migration, heartbeat enrichment |
| `cmd/webui/internal/api/system.go` | 1, 2 | ~80 | Fix stats (counters), /api/diagnostics handler |
| `cmd/webui/internal/api/router.go` | 2 | ~15 | Routes: /api/diagnostics, /api/workers/{type}/instances, /api/threads/{id}/events |
| `cmd/webui/internal/api/workers.go` | 2 | ~50 | Per-instance handler |
| `cmd/webui/main.go` | 2 | ~20 | slog migration, --log-level flag, --log-access flag |
| `cmd/task/main.go` | 2 | ~120 | `task why`, refactor cmdStatus→GetTask, enrich task status/list/thread-state |
| `docker-compose.yml` | 1 | ~6 | Master healthcheck |
| `CLAUDE.md` | 2 | ~40 | Debug instructions section |

**Total**: ~1066 lines across 13 files. No new dependencies.

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

1. Event system: envelope types + RPUSH/LTRIM helpers (client.go + threads.go)
2. Event emission points in tasks.go + worker/main.go + threads.go
3. correlation_id in thread state + worker dequeue (threads.go + worker/main.go)
4. lock Lua script for atomic locked_at (threads.go)
5. `/api/diagnostics` endpoint (system.go + router.go)
6. `/api/workers/{type}/instances` endpoint (workers.go + router.go)
7. `log/slog` migration with ReplaceAttr (worker/main.go + webui/main.go)
8. `task why` command (cmd/task/main.go)
9. Enrich existing CLI output (cmd/task/main.go)
10. Update CLAUDE.md debug section

### Phase 3 (separate PR, next sprint)

1. `GET /api/events` endpoint + system events
2. Alerting webhook dispatch
3. Redis memory monitoring in diagnostics
