# Web UI for Master Agent — Design & Implementation Plan

## Context

The ai-agents system uses a master agent (Claude Code with orchestrator instructions) to plan complex tasks, delegate sub-tasks to worker agents via a Redis task queue, and aggregate results. Currently everything is CLI-only: the user types requests directly into the master container's interactive Claude Code session. The master agent uses `task.py` as a skill (a tool it can invoke) to interact with Redis — enqueuing tasks, checking statuses, reading results, and managing threads. Workers run `worker.py` to consume tasks from Redis and execute agent CLIs.

We need a web console that:
1. Forwards user requests by spawning `claude -p` as a one-shot subprocess per request — the master agent remains the sole planner/orchestrator
2. Provides **monitoring** of threads, workers, and tasks by reading Redis state
3. Is implemented in **Go** with an HTMX frontend

The web UI is an **addon**, not a replacement. The master agent still does all planning, tool calling, and delegation. The web UI just gives it a browser-based front door and a real-time dashboard.

### Why Go? (previously Python)

The original `task.py` and `worker.py` were Python because: simple to write, no build step, and `redis-py` was already available in `ai-base`. For the web UI, Go is a better fit:
- **Single static binary** — no Python runtime, no `redis-py`, no `uvicorn`/`fastapi`/`jinja2` in the image. The `webui` Docker image is ~15MB vs ~200MB+ for a Python equivalent.
- **Shared `tasklib`** — Go's cross-compilation means the same `tasklib` code compiles into all three binaries (`task`, `worker`, `webui`) and into multi-arch Docker images (`linux/amd64`, `linux/arm64`) with zero runtime dependencies.
- **Task/worker as static binaries too** — once `tasklib` exists, rewriting `task.py` and `worker.py` as Go CLIs is a small incremental step that eliminates Python from the worker and master images entirely.

### Terminology: "thread"

Throughout this design, a **thread** is a master agent conversation session — one user request, the master's planning, all worker tasks spawned for that request, and the final response. It is identified by a `thread_id` and tracked in Redis as `thread:<id>:current_state` and `thread:<id>:messages`. Workers do not have threads; they consume individual tasks from Redis queues, each task referencing the parent `thread_id`.

### Shared `tasklib` — the foundation

Rather than reimplement Redis logic twice (Go for web UI, Python for `task.py` / `worker.py`), extract a shared **Go `tasklib`** package that all three Go binaries use:

```
tasklib/              # Shared Go library — all Redis CRUD for tasks, threads, workers
  ├─ client.go        # Redis connection, key name helpers, TTL constants
  ├─ tasks.go         # enqueue, status, result, list, wait, cancel, requeue-stale
  ├─ threads.go       # create, history, state, update, list, lock, unlock
  ├─ workers.go       # worker stats, heartbeat, queue depths
  ├─ request.go       # (replaces inbox.go) AcquireRequestLock, ReleaseRequestLock, session ID, CancelRequest
  ├─ uuid.go          # UUID generation for thread IDs and request IDs
  └─ *_test.go        # Unit tests with miniredis (non-blocking CRUD only)

cmd/task/main.go      # Go CLI — drop-in replacement for task.py (master agent skill)
cmd/worker/main.go    # Go worker loop — replacement for worker.py
cmd/webui/main.go     # Go HTTP server + HTMX frontend
```

All three binaries are built from the same `go.mod`. The Go `tasklib` produces the exact same Redis key names, JSON shapes, and behavior as the current Python code. Python `task.py` and `worker.py` are **kept behind an env toggle** (`TASKLIB_BACKEND=python`) for one release cycle as a fallback. After Step 8 (end-to-end validation) passes, `TASKLIB_BACKEND=python` and the Python scripts are removed in the following release. The Go binaries become the sole implementation.

## Architecture

```
                          webui (Go binary, :8000)
                            ├─ receives POST /api/requests
                            ├─ acquires thread lock (SET NX thread:<id>:running)
                            ├─ spawns: claude -p --session-id <uuid>
                            │     --output-format stream-json --verbose
                            │     "user request"
                            ├─ parses stdout JSON lines
                            │     → detects {"type":"result"} = completion
                            ├─ writes response to thread:<id>:messages
                            ├─ releases thread lock
                            ├─ reads Redis via tasklib for monitoring
                            ├─ serves REST API (JSON)
                            └─ serves HTMX frontend

claude -p (one-shot per request)
  ├─ plans & delegates
  ├─ invokes Go task CLI as tool
  │   (cmd/task: enqueue, wait, ...)
  ├─ task wait blocks within -p tool loop
  └─ exits when done (session persists via --session-id)

worker (Go binary)
  ├─ tasklib: BLMOVE from queue
  ├─ heartbeat: SETEX worker:<type>:<hostname>:heartbeat
  ├─ exec AGENT_CMD as subprocess
  └─ tasklib: write result back to Redis
```

The Go web server has **no LLM client, no tool definitions, no planning logic**. It only:
- Spawns `claude -p` as a subprocess per user request (one-shot invocation)
- Reads thread/task/worker state from Redis for display (via `tasklib`)
- Serves REST API + HTMX UI

### Request forwarding

When a user submits a request via the web UI:

1. Web UI receives `POST /api/requests` with `{thread_id?, repo?, request}`.
2. If `thread_id` is omitted, auto-generate one: `web_<unix_seconds>_<random 10-char base36 [0-9a-z]>`. The 10 base36 characters provide ~3.6×10¹⁵ combinations — collision probability is negligible even under heavy concurrent use.
3. The handler writes the thread shell (repo, initial state) and the user request as a `role: "user"`, `type: "request"` message to Redis via `tasklib`. A separate `POST /api/threads` endpoint exists for bootstrap-only thread creation (shell without a request). The session UUID is NOT generated at thread creation — it is generated on the first request (step 5).
4. The handler acquires a per-thread lock (`SET NX thread:<id>:running <request_id>` with TTL `REQUEST_TIMEOUT + 5 minutes` (35 min — Redis TTL is longer than the Go context timeout to prevent lock expiry before subprocess kill)). If the lock is already held, the handler returns `409 Conflict` — only one `claude -p` invocation per thread at a time.
5. The handler checks `thread:<id>:session_id`. If absent (first request for this thread), it generates a new session UUID (`uuid.NewV4()`) and stores it. If present (follow-up request), it reads the existing UUID. It then spawns `claude -p` as a subprocess in a **background goroutine**:
   ```
   claude --dangerously-skip-permissions --bare -p \
     --session-id <session_uuid> \
     --output-format stream-json --verbose \
     "<user request>"
   ```
6. The handler **returns immediately** with `{thread_id, status: "submitted"}` and the browser **redirects** to `/threads/{thread_id}`. The `claude -p` subprocess runs asynchronously.
7. In the background goroutine, the handler reads claude's stdout line by line, parsing each as JSON. It watches for the **completion message**: `{"type":"result","subtype":"success","is_error":false}`. On detection, the handler extracts the `result` field, sets `thread:<id>:complete 1` (Redis, with 7-day TTL), and writes a `role: "master"`, `type: "response"` message to `thread:<id>:messages` via `tasklib`.
8. If the claude subprocess exits non-zero before the completion message, the handler writes an error message: `{"role": "master", "type": "error", "content": "Master agent failed: <exit code> — <last stderr>"}`. If the subprocess exceeds `REQUEST_TIMEOUT`, it is killed (SIGTERM, then SIGKILL after 10s) and an error message is written.
9. The background goroutine releases the thread lock (`DEL thread:<id>:running`).
10. The thread detail page at `/threads/{thread_id}` **polls** every 3s (via HTMX, hitting `GET /api/threads/{thread_id}/history`). When the poll picks up a `type: "response"` message, a styled **response banner** appears at the top of the message timeline showing the master's answer.

**What is kept (persisted in Redis):**
- The user's original request — stored in `thread:<id>:messages` as a `role: "user"` message
- All intermediate master messages (plan, delegate, tool_call) — written by the master agent via `tasklib` during the `-p` invocation
- The final response — stored as a `role: "master"`, `type: "response"` message by the handler after the subprocess completes
- Thread state (`thread:<id>:current_state`) — status, repo, design, PR number
- Thread session ID (`thread:<id>:session_id`) — Claude session UUID for `--resume` on follow-up requests

**What the user sees:**
- After submission, the browser is on `/threads/{thread_id}` showing the message history
- A "Waiting for master..." indicator with elapsed timer is visible while `thread:<id>:running` exists and no `type: "response"` message has been written
- When the handler writes the response, the next poll cycle picks it up and the response banner replaces the waiting indicator
- The full message timeline (user request → master plan → delegated tasks → worker results → master response) is visible on one scrollable page

This preserves the master agent as the single source of truth for planning. The web UI has zero agency — it cannot create tasks, assign workers, or make decisions.

### Response detection (handler-managed)

Response completion is detected by the web UI handler, which parses the `claude -p --output-format stream-json` stdout:

1. `claude -p` with `--output-format stream-json --verbose` emits one JSON object per line on stdout. Messages include `{"type":"system","subtype":"init"}`, `{"type":"assistant","message":{...}}`, and the terminal `{"type":"result","subtype":"success","is_error":false}`.
2. The handler reads stdout line by line, JSON-decoding each. It watches for the **result message**: `type: "result"` with `subtype: "success"` or `subtype: "error_during_execution"`. The `is_error` field and `stop_reason` field provide explicit success/failure signals.
3. On `subtype: "success"`, the handler extracts the `result` field (the final response text) and writes it to `thread:<id>:messages` as a `role: "master"`, `type: "response"` message. The assistant messages (`type: "assistant"`) contain the intermediate output (thinking, tool calls, text) and are written to thread history by default for live progress display (planning, delegating, tool call steps appear in real time in the message timeline). The frontend appends them to the thread detail page as they arrive via polling.
4. If the claude subprocess exits non-zero before the result message is seen, the handler writes an error message with the captured stderr (exit code + last 4KB of stderr).
5. If the subprocess doesn't complete within `REQUEST_TIMEOUT` (default 30 minutes), the handler sends SIGTERM, waits 10s, then SIGKILL. It writes an error message:
   ```json
   {"role": "master", "type": "error", "content": "Master agent timed out after 30m", "timestamp": "<iso8601>"}
   ```

The web UI's thread detail page polls for messages with `type: "response"` or `type: "error"` and displays them in a styled banner (green for response, red for error). Threads with a pending `thread:<id>:running` lock and no response message yet show a "Waiting for master..." indicator with an elapsed timer.

**Multi-turn interaction:** Follow-up requests to the same thread use `claude -p --resume <session_uuid>`. Claude Code restores the full conversation context from the persisted session file. The handler uses the session UUID stored in `thread:<id>:session_id`.

**Session storage:** `~/.claude/projects/<workspace>/` stores session JSON files. This directory is on a shared Docker volume so sessions persist across `claude -p` invocations within the same container. Session cleanup uses a Redis-backed approach: each thread's `thread:<id>:last_activity` key is updated on every request. A periodic background goroutine queries Redis for threads with `last_activity` older than 24h and deletes their session files via `os.Remove`. This avoids O(N) filesystem scans — only threads tracked in Redis are checked.

### Thread creation (bootstrap only)

`POST /api/threads` is intentionally allowed as a lightweight bootstrap — the web UI creates a thread shell so it has a thread ID to attach the request to. This is not a planning action; it's equivalent to running `mkdir -p` before writing a file. The master agent still owns all thread state updates (`status`, `design`, `pr_number`, etc.).

### What the web UI does NOT do

- Does NOT call the LLM directly (it spawns `claude -p` as an opaque subprocess)
- Does NOT define tools for the master agent
- Does NOT enqueue tasks, wait on tasks, or cancel tasks directly
- Does NOT update thread state (beyond creating an empty thread and writing response/error messages)
- Does NOT make planning decisions

## Tech Stack

| Layer | Choice |
|-------|--------|
| Language | Go 1.26.3 (pinned in `go.mod` with `toolchain go1.26.3` and `golang:1.26.3-trixie` build images) |
| HTTP router | `chi` (go-chi/chi/v5) |
| Redis client | `go-redis/v9` |
| CLI framework | `cobra` (for `cmd/task`) |
| Testing (unit) | `miniredis` — non-blocking CRUD operations only |
| Testing (integration) | Real Redis — blocking commands (`BLPOP`, `BLMOVE`) + JSON compatibility |
| Templates | `html/template` (stdlib) |
| Frontend | HTMX 2.0.x (~15KB, vendored in `static/htmx.min.js` with version comment), no JS build step. Source URL overridable via `WEBUI_HTMX_SRC` env var (pin to a specific version for air-gapped deployments). |
| CSS | Minimal hand-written stylesheet using CSS custom properties for theming (`WEBUI_THEME: light/dark`) |

No `anthropic-sdk-go` — the Go server doesn't talk to any LLM.

## `tasklib` Package

The shared Go package that all three binaries import. It is the single source of truth for the Redis data model.

### API surface

```go
package tasklib

// Client wraps *redis.Client and provides all task/thread/worker operations.
type Client struct { ... }

func NewClient(rdb *redis.Client) *Client

// Tasks (read + write — used by cmd/task, cmd/worker, and cmd/webui)
func (c *Client) Enqueue(worker, threadID, instruction string) (*Task, error)
func (c *Client) GetTask(taskID string) (*Task, error)
func (c *Client) GetTaskResult(taskID string, tail int) (string, error)
func (c *Client) ListTasks(worker, status, threadID string, limit, offset int) ([]*Task, error)
// WaitTask polls until task is done/failed/cancelled or timeout expires.
// On timeout, releases the thread lock (same as Python finally block in cmd_wait).
// The caller of Enqueue retains lock ownership; WaitTask only releases on timeout
// to prevent permanent lock-stuck threads. The caller should still UnlockThread
// in a defer (no-op if WaitTask already released on timeout — UnlockThread is
// idempotent: DEL thread:<id>:lock is safe to call twice).
func (c *Client) WaitTask(taskID, threadID string, timeout time.Duration) (*Task, error)
func (c *Client) CancelTask(taskID string) error
func (c *Client) RequeueStale(worker string, olderThan time.Duration) ([]string, error)

// Threads (read + write — used by all three binaries)
func (c *Client) CreateThread(threadID, repo string) (*Thread, error)
func (c *Client) GetThread(threadID string) (*Thread, error)
func (c *Client) ListThreads() ([]*Thread, error)
func (c *Client) GetThreadHistory(threadID string, offset, limit int) ([]Message, error)
func (c *Client) UpdateThread(threadID string, fields map[string]string) error
func (c *Client) LockThread(threadID, taskID string, ttl time.Duration) (bool, error) // SET NX thread:<id>:lock
func (c *Client) UnlockThread(threadID string) error

// Threads — filesystem operation lives in cmd/task, not tasklib
// (tasklib is a pure-Redis library; workspace cleanup requires filesystem access)

// Active tasks hash (backward-compat with existing active_tasks hash in worker.py/task.py)
func (c *Client) SetActiveTask(taskID string, info TaskInfo) error     // HSET active_tasks <task_id> <json>
func (c *Client) RemoveActiveTask(taskID string) error                 // HDEL active_tasks <task_id>
func (c *Client) GetActiveTasks() (map[string]*TaskInfo, error)         // HGETALL active_tasks

// Workers (read-only for webui; write-only for heartbeat from cmd/worker)
func (c *Client) GetWorkerStats() (map[string]*WorkerInfo, error)
func (c *Client) GetWorkerInfo(workerType string) (*WorkerInfo, error)
func (c *Client) UpdateWorkerHeartbeat(workerType, hostname string) error  // SETEX worker:<type>:<hostname>:heartbeat 30 1

// Request execution (used by cmd/webui — spawn claude -p per request)
// AcquireRequestLock sets thread:<id>:running with TTL, NX semantics.
// Returns true if lock acquired, false if thread already has a running request.
// Distinct from LockThread (thread:<id>:lock) which serializes task enqueue within a thread.
func (c *Client) AcquireRequestLock(threadID, requestID string, ttl time.Duration) (bool, error)
// ReleaseRequestLock deletes the thread:<id>:running lock key.
func (c *Client) ReleaseRequestLock(threadID string) error

// Session tracking (used by cmd/webui — persist claude session UUID per thread)
func (c *Client) SetThreadSessionID(threadID, sessionID string) error
func (c *Client) GetThreadSessionID(threadID string) (string, error)

// CancelRequest sets thread:<id>:current_state status to "cancelled".
// The handler immediately calls cancel() on the subprocess context (SIGTERM, 10s grace, SIGKILL).
// The background goroutine detects cancellation, writes an error message, and releases the lock.
func (c *Client) CancelRequest(threadID string) error```

### Key design rules

- **Byte-for-byte JSON compatibility** with current Python serialization — workers and master must see identical payloads during the transition. Validated by a side-by-side integration test suite that runs both Go and Python against the same Redis, compares output for every operation.
- **Same Redis key names** as today (`task:<id>:status`, `thread:<id>:messages`, `tasks:queue:<worker>`, etc.).
- **`miniredis` scope**: Unit tests cover all non-blocking CRUD operations (enqueue, status, result, list, thread CRUD, etc.) and require no real Redis. Blocking commands (`BLPOP`, `BLMOVE`, `WaitTask` polling loop) are tested in a separate integration test suite against a real Redis instance.
- **No dependency on `cmd/` packages** — `tasklib` is pure library code.
- **No filesystem operations** — `tasklib` is a pure-Redis library. Workspace cleanup (`thread-cleanup`) lives in `cmd/task` where it can access the workspace directory.
- **`WORKSPACE_DIR` env var** — default `/workspace`. Read directly by `cmd/task` (for `thread-cleanup`) and `cmd/worker` (for reading/writing files in the thread workspace). `tasklib` does NOT reference this — it is a pure-Redis library. All containers that mount the workspace volume set this env var.

### Worker heartbeat

Heartbeat keys use per-instance keys from day one: `worker:<type>:<hostname>:heartbeat` (hostname from `os.Hostname()`, fallback to container hostname). `GetWorkerStats()` aggregates via `SCAN worker:*:heartbeat` and reports counts per type:

```json
{"claude": {"instances": 3, "online": 2, "total_active": 5}, ...}
```

This supports horizontal worker scaling immediately — no migration needed later. The `SCAN` cost is negligible for typical instance counts (1–10).

| Key | Type | TTL | Purpose |
|-----|------|-----|---------|
| `worker:<type>:<hostname>:heartbeat` | String | 30s | Set by background goroutine every 10s with value `1`. 20s margin covers GC pauses and Redis blips. If absent or expired, that instance is offline. | 

**Orphaned heartbeat cleanup:** Docker restarts change container hostnames, leaving stale keys from previous instances. `GetWorkerStats()` filters `SCAN` results: keys without a TTL (or with TTL < 0) are skipped. Additionally, a periodic cleanup removes keys whose value contains a different instance ID than the current generation.

`GetWorkerStats()` reports `online: true` when either the heartbeat key for that specific hostname exists OR the `active_tasks` hash contains entries that were written by that same hostname (each `active_tasks` entry value includes `"worker_hostname"`). This prevents a crashed instance's stale `active_tasks` entries from showing a different instance as online. A worker running a long task (up to 30 minutes) keeps its `active_tasks` entries fresh while the heartbeat TTL is only 30s (refreshed every 10s, 20s margin).

**Stale active_tasks cleanup:** A crashed worker leaves orphaned entries in `active_tasks`. A periodic cleanup goroutine in `cmd/webui` (which is always running, unlike a potentially-crashed worker) HDELs entries where the task status key (`task:<id>:status`) is missing or has been `done`/`failed`/`cancelled` for more than 60 seconds. This prevents permanently-false online indicators from crashed workers.

## `cmd/task` — Go CLI (replaces `scripts/task.py`)

Drop-in replacement for the Python CLI. The master agent invokes it with the **same arguments** — flag names mirror `task.py` exactly so the master's CLAUDE.md instructions don't need rewriting:

```
task enqueue --worker claude --thread my-thread --instruction "fix the bug"
task status --id <task_id>
task result --id <task_id> --tail 100
task list --worker claude --status done --limit 50
task wait --id <task_id> --timeout 300
task cancel --id <task_id>
task requeue-stale --worker claude --older-than 600
task unlock --thread <thread_id>
task thread-create --id <thread_id> --repo owner/repo
task thread-history --id <thread_id> --tail 50
task thread-state --id <thread_id>
task thread-update --id <thread_id> --status complete --design "..."
task thread-list
task thread-cleanup --id <thread_id>
```

Implementation: `cobra` commands that call `tasklib.Client` methods. `enqueue` acquires a thread-level lock via `LockThread(threadID, taskID, LOCK_TTL)` with `SET NX` before enqueuing — identical to the Python `task.py` behavior. `wait` releases the lock on completion via `UnlockThread`. `thread-cleanup` calls `os.RemoveAll` on the workspace path after validating it's within `WORKSPACE_DIR` (rejects `../` traversal). The workspace directory is read from the `WORKSPACE_DIR` env var (default `/workspace`).

**Stdout format compatibility:** The Go CLI must replicate the exact stdout output format of `task.py` for each command. The master agent parses stdout programmatically. Note that `task.py list` prints a human-readable table (not JSON), while `enqueue` returns `{"task_id": "..."}` and `status` returns a multi-field JSON object. All these formats are defined by the current Python code and must be matched byte-for-byte. The JSON compatibility test suite (Step 2) covers this.

Estimated ~350 lines of CLI glue (cobra dispatcher + output formatting + the one filesystem operation). The binary is statically compiled (`CGO_ENABLED=0`) and copied into the master container at `/usr/local/bin/task`.

## `cmd/worker` — Go worker loop (replaces `scripts/worker.py`)

Long-running process that:

1. Connects to Redis via `tasklib`
2. Starts a background **heartbeat goroutine** that runs `SETEX worker:<type>:<hostname>:heartbeat 30 1` every 10 seconds (30s TTL, 20s margin). The goroutine also monitors the active subprocess: if `exec(AGENT_CMD)` exceeds `TASK_TIMEOUT` without producing output, the heartbeat stops (deliberately allowing the worker to appear offline, signaling a stuck task to operators)., independently of task processing. This keeps the heartbeat fresh even during long task executions (up to `TASK_TIMEOUT`, default 30 minutes).
3. Main loop:
   - `BLMOVE tasks:queue:<worker> tasks:processing:<worker> RIGHT LEFT` with 5s timeout (direction matches `worker.py`: `src="RIGHT"`, `dest="LEFT"`)
   - If no task, continue
4. `HSET active_tasks <task_id> <task_info_json>` — mark as active (also serves as a secondary liveness signal: a worker with entries in `active_tasks` is alive regardless of heartbeat)
5. Reads thread history and state via `tasklib`
6. Copies `~/.claude/CLAUDE.md` (or `~/CLAUDE.md`) and `~/AGENTS.md` into the thread workspace (`<WORKSPACE_DIR>/<thread_id>/`) — same behavior as current `worker.py` at lines 111–120
7. Builds a prompt from task instruction + thread context (supports `history_window` field in task payload, default from `HISTORY_WINDOW` env var — backward-compat with current `worker.py`)
8. Executes `AGENT_CMD` (from env) as a subprocess with the prompt on stdin
9. Writes result + exit code back via `tasklib` (`task:<id>:result`, `task:<id>:status`)
10. `HDEL active_tasks <task_id>` — unmark
11. Appends result to thread history via `tasklib`

The worker binary takes a single argument: the worker type (`claude`, `copilot`, or `opencode`). It reads `AGENT_CMD` from the environment (set in the Dockerfile, same as today).

Estimated ~300 lines of Go. Statically compiled and copied into each worker Docker image at `/usr/local/bin/worker`.

## `cmd/webui` — HTTP server + HTMX frontend

Entry point for the web UI. Detailed in the sections below.

## Requirements

### 1. Dashboard (Home Page)

**Route:** `GET /`

A single-page overview showing:

| Section | Content |
|---------|---------|
| **Worker status** | 3 cards (Claude, Copilot, OpenCode) showing: online/offline (from heartbeat), queue depth, active task count. Polls every 5s. |
| **Active threads** | Table: thread ID, status badge, repo, last updated, task count, **response indicator** ("Waiting..." / "Ready for review"). Click to drill in. Polls every 5s. |
| **Recent tasks** | Table: task ID, worker type, thread, status, elapsed time. Polls every 5s. |
| **New request** | Form: thread ID (optional), repo (optional), request textarea. Submits to `POST /api/requests`. |

### 2. Thread Detail View

**Route:** `GET /threads/{thread_id}`

| Section | Content |
|---------|---------|
| **Response banner** | When the master has written a `type: "response"` message, show a styled banner with the master's summary. Configured via `WEBUI_POLL_THREAD_DETAIL` (default 3s when active). |
| **State panel** | Current status, repo, PR number, last design, last updated. Read-only (state is managed by the master agent). |
| **Message history** | Chat-like scrollable timeline of all messages (user → master → worker → result). Color-coded by role. Auto-scrolls to bottom. |
| **Task list** | All tasks for this thread with status icons, click to expand result |
| **Actions** | If thread has a pending request (`thread:<id>:running` exists), show "Cancel request" button. Submits `POST /api/threads/{id}/cancel`. Immediately cancels the subprocess context (SIGTERM → SIGKILL). The background goroutine writes an error message and releases the lock. Already-enqueued tasks continue to completion. |
| **Reset session** | "Clear session" button deletes the thread's Claude session file and removes `thread:<id>:session_id`. The next request generates a fresh session UUID, effectively starting a new conversation while preserving the thread's tasks, workspace, and state. Useful when the conversation context needs a reset without losing thread history. Submits `POST /api/threads/{id}/reset-session`. |

### 3. Task Management

**Route:** `GET /tasks`

| Section | Content |
|---------|---------|
| **Filters** | By worker type, status, thread — via query params |
| **Task table** | ID, worker, thread, status, created, completed. Paginated. |
| **Task detail** (`GET /tasks/{task_id}`) | Full payload: instruction, full result (with tail toggle), exit code, timestamps, worker |

All read-only. Task mutation (enqueue, cancel, re-queue) happens through the master agent when it processes a user request.

### 4. REST API Endpoints

All endpoints return JSON. Every endpoint also serves HTMX partials when the `HX-Request` header is present.

#### Requests (forwarding to master)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/requests` | Submit a request to the master agent. Body: `{thread_id?, repo?, request}`. `request` capped at 32KB (HTTP 413 if exceeded). Acquires per-thread lock (`SET NX thread:<id>:running`) — only one active request per thread. Returns `409 Conflict` if thread is already processing. Returns `503` with `Retry-After` if global concurrency limit reached (`MAX_CONCURRENT_REQUESTS`, default 5). Auto-generates `thread_id` if omitted (`web_<ts>_<10 base36 [0-9a-z]>`). Spawns `claude -p --session-id <uuid> --output-format stream-json` as subprocess. Returns `{thread_id, status: "submitted"}`. |
| `POST` | `/api/threads/{thread_id}/cancel` | Cancel a pending or running request. Sets thread status to `cancelled` and immediately calls `cancel()` on the subprocess context (SIGTERM, 10s grace, SIGKILL). Does NOT wait for the full `REQUEST_TIMEOUT`. The background goroutine detects context cancellation, writes an error message, and releases the lock. |

#### Tasks (read-only)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/tasks` | List tasks via `tasklib.ListTasks`. Query params: `worker`, `status`, `thread_id`, `limit`, `offset`. |
| `GET` | `/api/tasks/{task_id}` | Get task detail via `tasklib.GetTask`. |
| `GET` | `/api/tasks/{task_id}/result` | Get just the result text via `tasklib.GetTaskResult` (supports `?tail=N`). |

#### Threads

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/threads` | Create an empty thread via `tasklib.CreateThread`. Body: `{thread_id, repo?}`. Returns `409 Conflict` if thread already exists. Allowed as a lightweight bootstrap (the master still owns all state updates). |
| `GET` | `/api/threads` | List all threads via `tasklib.ListThreads`. |
| `GET` | `/api/threads/{thread_id}` | Get thread state + recent messages via `tasklib.GetThread`. |
| `GET` | `/api/threads/{thread_id}/history` | Get full message history via `tasklib.GetThreadHistory`. Supports `?tail=N` and offset-based pagination (`?offset=M&limit=N`). |
| `DELETE` | `/api/threads/{thread_id}/workspace` | Cleanup thread workspace directory. Sets `thread:<id>:deleting` sentinel (SET NX, TTL 60s), then checks `ListTasks(threadID, status="running")`. Refuses if non-empty (400). Workers check the sentinel before writing results — skip write if deleting. Requires `?confirm=true`. Auth-gated. Does NOT delete Redis keys. |
| `POST` | `/api/threads/{thread_id}/keep` | Extend Redis TTL for thread keys (7 more days). Prevents auto-expiry for long-lived threads. Auth-gated. |
| `POST` | `/api/threads/{thread_id}/reset-session` | Delete the thread's Claude session file (`os.Remove` on the session JSON) and clear `thread:<id>:session_id` in Redis. The next request generates a fresh session UUID, starting a new conversation while preserving all thread state, tasks, and workspace. Auth-gated. |

#### Workers (read-only)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/workers` | List all workers via `tasklib.GetWorkerStats`. Includes `online` field derived from heartbeat key. |
| `GET` | `/api/workers/{worker_type}` | Detail for one worker type via `tasklib.GetWorkerInfo`. |

#### System

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/health` | Health check — Redis connectivity, worker counts. |
| `GET` | `/api/stats` | Aggregate stats: total tasks, success rate, avg task duration, queue depths. |

Rate limiting: `POST /api/requests` is rate-limited to 10 requests per minute per client IP via chi middleware. `POST /api/threads` is rate-limited to 30 creations per minute (to prevent flooding). Exceeded returns `429 Too Many Requests`. When deployed behind a reverse proxy (nginx/Caddy), configure trusted `X-Forwarded-For` / `X-Real-IP` headers so the middleware sees real client IPs, not the proxy IP.

### 5. Real-Time Updates

HTMX polling with configurable intervals (each has its own env var, defaults below):

| Section | Default | Env var |
|---------|---------|---------|
| Dashboard task list | 5s | `WEBUI_POLL_DASHBOARD` |
| Dashboard thread list | 5s | `WEBUI_POLL_DASHBOARD` |
| Thread detail (active) | 3s | `WEBUI_POLL_THREAD_DETAIL` |
| Worker status cards | 5s | `WEBUI_POLL_WORKERS` |

Each poll hits the existing REST endpoint with `HX-Request` header, returning only the HTML partial for that section. No WebSocket needed. The thread detail page can also return dynamic `hx-trigger` attributes based on thread state: active threads poll faster (2s), idle threads poll slower (10s).

**Scaling:** For deployments with many concurrent users, the thread detail page can optionally use Redis Pub/Sub (`tasklib.SubscribeToThread(threadID)`) to receive real-time updates instead of polling. The active thread detail endpoint supports a `?stream=true` query param that returns a Server-Sent Events (SSE) stream of thread messages. Polling remains the default for simplicity.

### 6. Request Handler (claude -p subprocess)

The web UI handler spawns `claude -p` as a one-shot subprocess per user request. No persistent Claude process, no FIFO, no stdin multiplexing.

```
POST /api/requests {thread_id?, repo?, request}
       │
       ▼
  handler (Go, in cmd/webui)
       │
       ├─ 1. Create thread if new (tasklib.CreateThread)
       ├─ 2. Write user request to thread:<id>:messages
       ├─ 3. Acquire request lock: SET NX thread:<id>:running <request_id>
       │      (TTL = REQUEST_TIMEOUT + 5 min, i.e. 35 min — provides a 5-min margin
       │       over the Go context timeout to prevent lock expiry before process kill)
       │      → 409 Conflict if already locked
       ├─ 4. Generate/store session UUID (thread:<id>:session_id)
       ├─ 5. Spawn subprocess in background goroutine:
       │      claude --dangerously-skip-permissions --bare -p \
       │        --session-id <session_uuid> \
       │        --output-format stream-json --verbose \
       │        "<user request>"
       ├─ 6. Return {thread_id, status: "submitted"} immediately
       │      (browser redirects to /threads/{thread_id})
       │
       └─ [async goroutine continues below]

  background goroutine (with defer recover):
       │      (panic recovery: logs error, releases lock, kills subprocess, writes error to thread)
       │
       ├─ 7. Check thread:<id>:current_state status → abort if "cancelled"
       ├─ 8. Read stdout line-by-line, JSON-decode each
       │      → {"type":"assistant"} — intermediate: write to thread history for live progress display
       │      → {"type":"result","subtype":"success"} — DONE
       │      → {"type":"result","subtype":"error_during_execution"} — FAILED
       ├─ 9. On completion: SET thread:<id>:complete 1 (7-day TTL), write role:"master" type:"response" to thread:<id>:messages
       ├─ 10. On error/timeout/cancelled: write role:"master" type:"error" to thread:<id>:messages
       ├─ 11. Release request lock: DEL thread:<id>:running
       └─ 12. Update thread state status
```

**Missing session fallback:** If `thread:<id>:session_id` exists in Redis but the corresponding session JSON file is missing from `~/.claude/` (deleted by TTL cleanup or volume wipe), `claude -p --resume <uuid>` will error. The handler detects this by checking stderr for "No conversation found" and falls back to generating a fresh session UUID + `--session-id`, logging a warning. The thread state and message history in Redis are unaffected.

**Session persistence:**

- First request for a thread: generate a new session UUID (`uuid.NewV4()`), store in `thread:<id>:session_id` (Redis), pass `--session-id <uuid>` to `claude -p`.
- Follow-up requests: read `thread:<id>:session_id` from Redis, pass `--resume <uuid>` to `claude -p` instead of `--session-id`.
- `~/.claude/projects/<workspace>/` is on a shared Docker volume — session files persist across `-p` invocations within the same container.
- Session files are cleaned up via a Redis-backed periodic scan (threads with `thread:<id>:last_activity` > 24h old), not immediately on completion, so `--resume` follow-ups still work.

**Concurrency:**

- Only one `claude -p` invocation per thread at a time, enforced by `SET NX thread:<id>:running`.
- Different threads can run concurrently (multiple `claude -p` subprocesses managed by the Go HTTP server).
- Global concurrency limit via `MAX_CONCURRENT_REQUESTS` (default 5) — beyond this, `POST /api/requests` returns `503 Service Unavailable` with `Retry-After`. There is intentionally no Redis-backed request queue — the `503 + Retry-After` pattern is the documented backpressure mechanism. The HTMX frontend handles 503 via an `htmx:responseError` event listener that retries after the `Retry-After` delay (exponential backoff: 5s, 10s, 20s). Alternatively, the form submission can poll for thread creation success via a redirect-based approach.

**Lock taxonomy — two distinct Redis locks:**

| Lock | Redis key | Purpose | Scope |
|------|-----------|---------|-------|
| Task lock | `thread:<id>:lock` | Serializes `task enqueue` within a thread. Only one task can be enqueued per thread at a time (acquired by `cmd/task enqueue`, released by `cmd/task wait` on completion or `cmd/task unlock` on timeout). Prevents concurrent worker tasks from racing on the same thread's state/workspace. Holding the lock across the full task lifecycle (enqueue → wait → result) means new tasks on the same thread block until the current one completes — intentional: sequential execution preserves causal ordering of worker results within a thread. | Per-thread, per-task |
| Request lock | `thread:<id>:running` | Prevents concurrent `claude -p` invocations for the same thread. Acquired by the web UI handler before spawning, released by the background goroutine on completion/error/timeout. | Per-thread, per-request |

**`--dangerously-skip-permissions` justification:** This flag bypasses Claude Code's interactive permission prompts (file reads, bash commands, network access). It is safe because: (a) the `claude -p` subprocess runs inside a Docker container with bounded filesystem access (`/workspace` volume only), (b) the container has no network exposure beyond Redis and the GitHub API (both intentional), and (c) the master orchestrator operates on code checked out within the container, not the host filesystem.

**Timeout and cleanup:**

- `REQUEST_TIMEOUT` (default 30 minutes) — handler kills the subprocess (SIGTERM, 10s grace, SIGKILL) and writes an error message. Complex orchestrator workflows (plan → delegate → `task wait` → delegate → aggregate) can legitimately approach this limit. Tune per-deployment via the env var. As a future enhancement, the timeout could be refreshed when `claude -p` produces intermediate output (e.g., `type: "assistant"` messages).
- `REQUEST_SHUTDOWN_GRACE` (default 60s) — on server shutdown, in-flight subprocesses are given this grace period before being killed.

**Shell safety:** The user request (up to 32KB) is passed to `claude -p` as a CLI argument. Go's `exec.Command` uses `execve` directly — no shell is involved, so shell metacharacters in the request text are harmless. Do NOT use `sh -c` or any shell-based invocation.

**Handler env vars:**

| Env var | Default | Purpose |
|---------|---------|---------|
| `REQUEST_TIMEOUT` | `1800` | Seconds before killing a stuck `claude -p` (30 min) |
| `MAX_CONCURRENT_REQUESTS` | `5` | Max concurrent `claude -p` subprocesses |
| `REQUEST_SHUTDOWN_GRACE` | `60` | Seconds to wait for in-flight requests on shutdown |
| `CLAUDE_PATH` | `/usr/local/bin/claude` | Path to claude binary |
| `CLAUDE_SESSIONS_DIR` | `/home/agent/.claude` | Shared volume path for session persistence |

### 7. Authentication

**Default:** When `WEBUI_API_KEY` is set, **all** `/api/` endpoints require `Authorization: Bearer <key>` — both read and write. The browser UI pages embed the key via a cookie or session token so the polling HTMX requests are authenticated. If `WEBUI_API_KEY` is not set, a warning is logged on startup and all requests proceed without auth (development mode / single-user trusted network).

Thread history, task results, and aggregate stats can contain source code, PR details, and error messages. Exposing these without auth in a shared-network environment is a data leak risk.

To disable auth intentionally in dev, explicitly set `WEBUI_API_KEY=` (empty string). The compose default is `${WEBUI_API_KEY:-CHANGE_ME}`, which forces explicit configuration — no accidental open deployments.

For production deployments on shared networks, place a reverse proxy (nginx, Caddy) with TLS and auth in front of the web UI. The web UI runs in the master-agent container alongside `claude` — since `claude -p` subprocesses and session files are local to each container, the master-agent is intentionally a single-instance service (not horizontally scaled). If higher web throughput is needed, a dedicated lightweight HTTP frontend can be placed in front of the web UI for static asset serving and request buffering.

**CSRF protection:** Mutation endpoints require `Content-Type: application/json` (reject non-JSON with `415`). Since simple cross-origin forms cannot set `Content-Type: application/json`, this is an effective CSRF defense without requiring tokens. HTMX sends a custom `HX-Request` header on all AJAX requests which serves as an additional browser-side signal. The middleware accepts requests with **either** `Content-Type: application/json` **or** the `HX-Request` header — API clients (curl, scripts) use the former, browser HTMX uses the latter.

### 8. Docker Integration

Updated images (Go binaries replace Python scripts):

| Image | Change |
|-------|--------|
| `master-agent` | Copies `webui` + `task` binaries. `ENTRYPOINT ["webui"]` — the web UI server spawns `claude -p` as a subprocess within the same container. `FROM claude-code` so the `claude` binary is present. **Breaking change:** Interactive CLI access (`docker exec -it master claude`) is removed — the web UI (or `curl POST /api/requests`) is the sole entry point. |
| `worker-claude` | `ENTRYPOINT ["worker", "claude"]`. Copies Go `worker` binary into `docker/worker-claude/`. |
| `copilot` | Copies Go `worker` binary into existing `docker/copilot/` image. `ENTRYPOINT ["worker", "copilot"]` replaces `worker.py`. |
| `opencode` | Copies Go `worker` binary into existing `docker/opencode/` image. `ENTRYPOINT ["worker", "opencode"]` replaces `worker.py`. |

Updated `master` service in `docker-compose.yml`:

```yaml
master:
  image: ${MASTER_AGENT_IMAGE:-master-agent:latest}
  depends_on:
    redis:
      condition: service_healthy
  environment:
    - REDIS_HOST=redis
    - REDIS_PORT=6379
    - WEBUI_PORT=8000
    - WEBUI_API_KEY=${WEBUI_API_KEY:-}
    - WEBUI_POLL_DASHBOARD=5
    - WEBUI_POLL_THREAD_DETAIL=3
    - WEBUI_POLL_WORKERS=5
    - WEBUI_HTMX_SRC=/static/htmx.min.js
    - WEBUI_THEME=${WEBUI_THEME:-light}
    - WORKSPACE_DIR=/workspace
    - CLAUDE_PATH=/usr/local/bin/claude
    - CLAUDE_SESSIONS_DIR=/home/agent/.claude
    - REQUEST_TIMEOUT=1800
    - MAX_CONCURRENT_REQUESTS=5
    - ANTHROPIC_AUTH_TOKEN=${ANTHROPIC_AUTH_TOKEN}
    - GH_TOKEN=${GH_TOKEN}
    - GITHUB_TOKEN=${GITHUB_TOKEN}
  volumes:
    - workspace:/workspace
    - claude_sessions:/home/agent/.claude
  ports:
    - "${WEBUI_PORT:-8000}:8000"
  restart: unless-stopped
```

**New volume `claude_sessions`** — persists Claude session files (`~/.claude/projects/`) across `claude -p` invocations, enabling `--resume` for multi-turn threads. Mounted on the `master` container where both `webui` and `claude -p` run.

Dockerfiles:

- `docker/master-agent/Dockerfile` — `FROM claude-code:latest`; copies `webui` binary (Go build), `task` binary, `templates/`, `static/`; `ENTRYPOINT ["/usr/local/bin/webui"]`
- `docker/worker-claude/Dockerfile` — copies `worker` binary; `ENTRYPOINT ["worker", "claude"]`

### 9. File Structure

```
go.mod                          # Single Go module for the whole repo
go.sum

tasklib/                        # Shared library — all Redis CRUD (pure Redis, no filesystem)
  client.go                     # Redis connection, key name helpers, TTL constants
  tasks.go                      # Task CRUD
  threads.go                    # Thread CRUD (no cleanup — that lives in cmd/task)
  workers.go                    # Worker stats + heartbeat
  uuid.go                       # UUID generation for thread/request IDs
  request.go                    # (replaces inbox.go) AcquireRequestLock, ReleaseRequestLock, session ID methods, CancelRequest
  tasks_test.go                 # Unit tests with miniredis (non-blocking CRUD only)
  threads_test.go
  tasks_integration_test.go     # Integration tests with real Redis (blocking cmds + JSON compat)

cmd/
  task/
    main.go                     # CLI (cobra) — drop-in replacement for task.py (~350 lines)
  worker/
    main.go                     # Worker loop — replacement for worker.py (~300 lines)
  webui/
    main.go                     # HTTP server entry point
    internal/
      api/
        router.go               # Chi router, HTMX detection middleware, auth middleware, error recovery
        requests.go             # POST /api/requests (thread lock, spawn claude -p, parse stream-json)
        tasks.go                # GET /api/tasks handlers (read-only)
        threads.go              # GET/POST /api/threads + POST /api/threads/{id}/cancel handlers
        workers.go              # GET /api/workers handlers
        system.go               # GET /api/health, /api/stats handlers
      request/
        handler.go              # claude -p subprocess manager (spawn, stdout parse, timeout)
      templates/
        base.html               # Layout shell (nav, vendored HTMX, polling config)
        dashboard.html          # Dashboard page
        threads/
          list.html             # Thread list
          detail.html           # Thread detail (with response banner)
          _state.html           # HTMX partial: state panel
          _history.html         # HTMX partial: message timeline
        tasks/
          list.html             # Task list with filters
          detail.html           # Task detail
          _table.html           # HTMX partial: task table rows
        workers/
          _cards.html           # HTMX partial: worker status cards
        requests/
          _form.html            # HTMX partial: request form + submission response
    static/
      style.css                 # Minimal CSS with custom properties for theming
      htmx.min.js               # Vendored HTMX (~15KB)

docker/
  master-agent/
    Dockerfile                  # FROM claude-code; copies webui + task binaries + templates/ + static/
  worker-claude/
    Dockerfile                  # Go worker binary

scripts/                        # Python originals (reference for JSON compat tests)
  task.py
  worker.py
  test_json_compat.py           # Side-by-side integration tests (Go vs Python output)

docker-compose.yml              # MODIFIED: master service runs webui, claude_sessions volume
```

### 10. Redis Data Model

The Go `tasklib` reads and writes the exact same Redis keys used by `task.py` and `worker.py` today. No migration needed.

Keys used by tasklib (all pre-existing except those marked NEW):

| Key | Type | Purpose |
|-----|------|---------|
| `active_tasks` | Hash | **Existing.** Currently executing tasks. Keyed by `task_id`, value is JSON task info. Written by `cmd/worker` (HSET on start, HDEL on completion). Read by `task list` and web UI. |
| `thread:<id>:running` | String (NEW, SET NX) | Per-thread lock preventing concurrent `claude -p` invocations. Value is the request ID. TTL = `REQUEST_TIMEOUT` (default 1800s). Released by handler on completion/error/timeout. Web UI checks this key to show "Waiting for master..." indicator. |
| `thread:<id>:complete` | String (NEW, 7-day TTL) | Set by web UI handler when `type: "result"` is received. Provides a fast check for the dashboard ("Ready for review" indicator) without parsing full message history. TTL aligns with the thread lifecycle — expires alongside thread keys. |
| `thread:<id>:session_id` | String (NEW, 7-day TTL) | Claude session UUID for `--session-id` / `--resume`. Generated on first request, reused for follow-ups. TTL aligns with thread lifecycle. |
| `thread:<id>:last_activity` | String (NEW, 7-day TTL) | Unix timestamp updated on every request. Used by the session cleanup goroutine to find threads inactive > 24h. TTL aligns with thread lifecycle. |
| `worker:<type>:<hostname>:heartbeat` | String (NEW, SETEX, 30s TTL) | Per-instance worker liveness. Value `1`. Set by background goroutine every 10s (20s margin). Read by `GetWorkerStats` via `SCAN worker:*:heartbeat`. Docker hostname changes on restart → old keys age out via TTL. |

Stream-json to thread message mapping:

The background goroutine translates `claude -p` stream-json output into Redis thread messages:

| stream-json content | Thread message `type` | Rule |
|---------------------|----------------------|------|
| `{"type":"assistant","content":[{"type":"text",...}]}` (text-only, no tool_use) | `"plan"` | Text content without tool calls → planning/thinking output |
| `{"type":"assistant","content":[...,{"type":"tool_use",...}]}` (contains tool_use block) | `"tool_call"` | Contains a tool invocation (Bash, Read, Edit) |
| `{"type":"result","subtype":"success"}` | `"response"` | Final completion — extract `result` field |
| `{"type":"result","subtype":"error_during_execution"}` | `"error"` | Claude-level error |

The `"delegate"` type is set by the master agent via `task enqueue` (the task instruction is written to thread history with `type: "delegate"` as a side effect of enqueue), not derived from stream-json. All messages have `role: "master"` except worker results which have `role: "worker"`.

Thread message taxonomy — all messages in `thread:<id>:messages` have `role`, `type`, `content`, and `timestamp` fields:

| `role` | `type` | Meaning | Rendered as |
|--------|--------|---------|-------------|
| `"user"` | `"request"` | Request from web UI or CLI | Plain text bubble |
| `"master"` | `"plan"` | Master agent planning output | Dashed border, muted |
| `"master"` | `"delegate"` | Master agent delegating a task | Dashed border, muted |
| `"master"` | `"tool_call"` | Master agent invoking the Go task CLI | Dashed border, muted |
| `"master"` | `"response"` | Final response written by web UI handler | Green banner |
| `"master"` | `"error"` | Error written by web UI handler | Red banner |
| `"worker"` | `"result"` | Worker agent task result | Solid border, worker color-coded. `worker_type` field (`"claude"`/`"copilot"`/`"opencode"`) preserved in message JSON for filtering. |
| `"claude"` | `"result"` | Legacy: Python worker role (backward-compat) | Same as `worker` |
| `"copilot"` | `"result"` | Legacy: Python worker role (backward-compat) | Same as `worker` |
| `"opencode"` | `"result"` | Legacy: Python worker role (backward-compat) | Same as `worker` |

During the `TASKLIB_BACKEND=python` transition period, both old (`role: "claude"`) and new (`role: "worker"`) messages coexist. The web UI rendering maps all four to the same visual treatment. Unknown types fall back to plain-text rendering.

All other keys are unchanged.

**Thread lifecycle:** `tasklib` preserves the existing TTL behavior from `task.py` (tasks are created with `TTL_TASK`, threads with `TTL_THREAD`). On thread completion (status `complete` or `cancelled`), the web UI handler sets an **additional** 7-day TTL as a safety net — the existing TTLs already provide baseline cleanup. A `POST /api/threads/{id}/keep` endpoint extends the TTL. Expired keys are cleaned up automatically by Redis. A startup goroutine in the web UI scans for orphaned `thread:<id>:running` keys (left behind after a server crash). Since no subprocess PID exists for these keys, the startup sweep immediately reaps any `thread:<id>:running` key whose request ID doesn't correspond to a running `claude -p` subprocess — no need to wait for the full `REQUEST_TIMEOUT` TTL. It then writes an error message to the thread and marks the thread timed out.

## Implementation Plan

### Step 0: Pre-implementation validation

Validated 2026-05-12 against `ghcr.io/noodle05/claude-code:latest` (v2.1.126). Full findings in `docs/design-web-ui-revision-notes.md`.

1. **`claude -p --output-format stream-json`** — Validated. `-p` mode supports complex multi-step tool use (`num_turns: 3+`) within a single invocation. `stream-json` produces unambiguous `{"type":"result","subtype":"success"}` completion messages. `--session-id` + `--resume` + shared `~/.claude` volume provides session persistence across invocations. This approach replaces the originally-planned FIFO/supervisor/prompt-marker architecture.

2. **Redis blocking commands** — Validated. Redis 7-alpine supports `LMOVE`, `BLMOVE` (since 6.2), `SETEX`, `SET...EX`. `go-redis/v9` is the client library.

### Deployment milestones

To reduce risk, the implementation is grouped into three deployable milestones. Each milestone ships, stabilizes, and can be rolled back independently via `TASKLIB_BACKEND=python`.

| Milestone | Steps | Deliverables | Revert |
|-----------|-------|-------------|--------|
| M1: tasklib | 1, 2 | Go `tasklib` package + JSON compat tests (validated, not yet used in prod) | N/A (test-only) |
| M2: CLI + worker | 3 | Go `task` + `worker` binaries behind `TASKLIB_BACKEND=go` toggle alongside Python | Set `TASKLIB_BACKEND=python` |
| M3: web UI | 4, 5, 6, 7 | Request handler + web UI, full stack | Revert master-agent image |

### Step 1: Go module scaffold + `tasklib` package

> **Status: DONE** (PRs #15, #16). Scope: `tasks.go`, `threads.go`, `workers.go`, `client.go`, `uuid.go`, unit tests with `miniredis`.

**Post-revision work needed:** The current `tasklib/inbox.go` implements the old FIFO-based `PushRequestAtomic` and `CancelRequest`. This file must be replaced with the new request-execution methods:

- `AcquireRequestLock(threadID, requestID, ttl)` / `ReleaseRequestLock(threadID)` — request-level lock (`thread:<id>:running`)
- `SetThreadSessionID(threadID, sessionID)` / `GetThreadSessionID(threadID)` — session UUID persistence
- `CancelRequest(threadID)` — simplified: sets thread status to `cancelled` only (no inbox list manipulation)

Remove `tasklib/lua/push_request.lua` and `tasklib/lua/cancel_request.lua` (FIFO-era Lua scripts). Add `miniredis` unit tests for the new methods.

### Step 2: Side-by-side JSON compatibility test suite

> **Status: DONE** (PR #16). Covers all task/worker CRUD operations. No changes needed — the new request-execution methods have no Python equivalent to test against.

### Step 3: Go `task` CLI + `worker` binary

> **Status: task CLI DONE** (PR #16), **worker NOT STARTED**. `cmd/task/main.go` is a `cobra` CLI with the same subcommands and flags as `task.py`. `cmd/worker/main.go` — the worker loop with heartbeat — still needs to be built. No changes needed to the `task` CLI itself — it is invoked by `claude -p` exactly as before; the new tasklib methods are called directly by `cmd/webui`, not through the CLI.

### Step 4: Request handler (claude -p subprocess manager)

Build `cmd/webui/internal/request/handler.go` — Go subprocess manager that:
- Spawns `claude --dangerously-skip-permissions --bare -p --session-id <uuid> --output-format stream-json --verbose "<prompt>"` in a background goroutine
- Returns immediately after spawn (handler is non-blocking)
- Reads claude stdout line-by-line in the background goroutine, JSON-decodes each
- Detects `{"type":"result","subtype":"success"}` as completion → sets `thread:<id>:complete 1`
- Handles timeout via `context.WithTimeout` (SIGTERM, 10s grace, SIGKILL)
- Writes response/error messages to `thread:<id>:messages` via `tasklib`
- Manages request lock (`AcquireRequestLock` / `ReleaseRequestLock` — `thread:<id>:running`)
- Manages session UUID (`SetThreadSessionID` / `GetThreadSessionID`) for `--resume` on follow-ups
- Global concurrency cap via semaphore channel (`MAX_CONCURRENT_REQUESTS`, default 5)

Add `claude_sessions` volume to `docker-compose.yml` for session persistence. Update `docker/master-agent/CLAUDE.md` to document the `-p`-based workflow and the `--resume` follow-up path.

### Step 5: Web UI REST API handlers

Build `cmd/webui/internal/api/` — chi router, auth middleware (bearer token when `WEBUI_API_KEY` is set), HTMX detection middleware, all `/api/` endpoints backed by `tasklib`. Each handler checks `HX-Request` header to return either an HTML partial or JSON. `POST /api/requests` handles auto-generation of `thread_id` when omitted, acquires thread lock, and spawns `claude -p` via the request handler.

### Step 6: HTML templates and CSS

Build all Go `html/template` files under `cmd/webui/internal/templates/`. Minimal CSS with CSS custom properties for theming (`--color-*`, `--spacing-*`). HTMX loaded from `WEBUI_HTMX_SRC` (default `/static/htmx.min.js` — vendored). HTMX attributes for polling (intervals from respective `WEBUI_POLL_*` env vars) and form submissions. Response banner in thread detail for master responses (green for `type: "response"`, red for `type: "error"`).

### Step 7: Dockerize

Multi-stage Dockerfiles for all images. `master-agent` uses `FROM claude-code:latest` and copies `webui` + `task` binaries and templates/static. Add `claude_sessions` volume to `docker-compose.yml`. Update CI (`.github/workflows/build-images.yml`) to build the Go binaries and all images.

### Step 8: End-to-end test

Spin up the full stack. Submit a request via the web UI → handler acquires thread lock → handler spawns `claude -p --session-id <uuid> --output-format stream-json` → master plans and delegates via Go `task` CLI → Go workers consume, execute, return results → handler detects `{"type":"result"}` on stdout → handler writes `type: "response"` message → handler releases thread lock → thread detail page shows response banner via HTMX polling. Test follow-up request via `--resume` to verify session persistence. Test REST API with `curl`. Verify auth middleware rejects unauthenticated mutations when `WEBUI_API_KEY` is set.

## Verification

1. `go test ./tasklib/...` passes all unit tests with `miniredis`
2. `python3 scripts/test_json_compat.py` passes all byte-for-byte comparisons
3. `docker compose up -d` starts all services (master serves the web UI on port 8000)
4. `curl http://localhost:8000/api/health` returns `{"redis":"ok","workers":{...}}`
5. `curl -X POST http://localhost:8000/api/requests -H 'Content-Type: application/json' -H 'Authorization: Bearer <key>' -d '{"request":"Say hello"}'` auto-generates a thread_id, spawns `claude -p`, and returns `{"thread_id":"web_<ts>_<abcdefghij>","status":"submitted"}`
6. The handler detects `{"type":"result"}` on claude stdout and writes a `type: "response"` message to thread history
7. Browser at `http://localhost:8000` shows dashboard with worker cards (online/offline from heartbeat), active threads with response indicators
8. Thread detail page shows message history updating in real time via HTMX polling, with a response banner when the master finishes
9. `curl http://localhost:8000/api/tasks` returns JSON matching the UI display
10. The Go `task` CLI produces identical output to `task.py` for the same Redis state
11. Follow-up request to the same thread uses `--resume <session_uuid>` and preserves conversation context
