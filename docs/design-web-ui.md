# Web UI for Master Agent — Design & Implementation Plan

## Context

The ai-agents system uses a master agent (Claude Code with orchestrator instructions) to plan complex tasks, delegate sub-tasks to worker agents via a Redis task queue, and aggregate results. Currently everything is CLI-only: the user types requests directly into the master container's interactive Claude Code session. The master agent uses `task.py` as a skill (a tool it can invoke) to interact with Redis — enqueuing tasks, checking statuses, reading results, and managing threads. Workers run `worker.py` to consume tasks from Redis and execute agent CLIs.

We need a web console that:
1. Forwards user requests to the **existing master agent** (the running Claude Code session) — the master remains the sole planner/orchestrator
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
  ├─ tasks.go         # enqueue, status, result, list, wait, cancel, requeue-stale
  ├─ threads.go       # create, history, state, update, list, unlock
  ├─ workers.go       # worker stats, heartbeat, queue depths
  ├─ inbox.go         # atomic PushRequest (Lua script: inbox write + history append)
  └─ *_test.go        # Unit tests with miniredis (non-blocking CRUD only)

cmd/task/main.go      # Go CLI — drop-in replacement for task.py (master agent skill)
cmd/worker/main.go    # Go worker loop — replacement for worker.py
cmd/webui/main.go     # Go HTTP server + HTMX frontend
```

All three binaries are built from the same `go.mod`. The Go `tasklib` produces the exact same Redis key names, JSON shapes, and behavior as the current Python code. Python `task.py` and `worker.py` are **kept behind an env toggle** (`TASKLIB_BACKEND=python`) for one release cycle as a fallback. After Step 8 (end-to-end validation) passes, `TASKLIB_BACKEND=python` and the Python scripts are removed in the following release. The Go binaries become the sole implementation.

## Architecture

```
master (Claude Code session)          webui (Go binary, :8000)
  ├─ plans & delegates                  ├─ writes requests to Redis inbox
  ├─ invokes Go task CLI as skill       │   (atomic Lua: inbox + thread history)
  │   (cmd/task: enqueue, wait, ...)    ├─ reads Redis via tasklib for monitoring
  └─ receives requests via:             ├─ serves REST API (JSON)
      1. interactive CLI (stdin)        └─ serves HTMX frontend
      2. Redis inbox (from web UI)
       └─ supervisor process           worker (Go binary)
           multiplexes stdin             ├─ tasklib: BLMOVE from queue
           via named pipe                ├─ heartbeat: SETEX worker:<type>:<hostname>:heartbeat
           + mutex lock                  ├─ exec AGENT_CMD as subprocess
                                         └─ tasklib: write result back to Redis
```

The Go web server has **no LLM client, no tool definitions, no planning logic**. It only:
- Writes incoming user requests to a Redis inbox list (via `tasklib.PushRequestAtomic`)
- Reads thread/task/worker state from Redis for display (via `tasklib`)
- Serves REST API + HTMX UI

### Request forwarding

When a user submits a request via the web UI:

1. Web UI receives `POST /api/requests` with `{thread_id?, repo?, request}`.
2. If `thread_id` is omitted, auto-generate one: `web_<unix_seconds>_<random 10-char base36 [0-9a-z]>`. The 10 base36 characters provide ~3.6×10¹⁵ combinations — collision probability is negligible even under heavy concurrent use. `HSETNX` in the Lua script additionally guards against any collision.
3. Execute `tasklib.PushRequestAtomic` — a **Lua script** that atomically:
   - Sets the thread state repo field if not already present (`HEXISTS thread:<id>:current_state gh_repo` → `HSET gh_repo <repo>` if missing; maps REST body `repo` to Redis field `gh_repo` for backward-compat with task.py)
   - Appends the user request to `thread:<id>:messages` as a JSON message with `role: "user"`, `type: "request"`, and `source: "webui"`
   - LPUSHes the request payload onto `requests:inbox`
   All three operations succeed or fail together — no orphan thread shells. A separate `POST /api/threads` endpoint still exists for creating a thread without submitting a request (e.g., pre-configuring repo metadata before the first request).
4. The web UI responds with `{thread_id, status: "submitted"}` and the browser **redirects** to `/threads/{thread_id}`.
5. Meanwhile, the inbox-reader does `LMOVE requests:inbox requests:inbox_processing 0 0` (atomic move), writes the request text to a **named pipe** (`/tmp/master-inbox.fifo`), then `LREM requests:inbox_processing`. On startup, any stranded entries in `requests:inbox_processing` are restored to `requests:inbox`.
6. The **supervisor** writes the request to claude's stdin. The master agent processes it exactly as if the user typed it — plans, delegates via Go `task` CLI to workers, aggregates results.
7. The supervisor watches claude's stdout for the prompt marker. When detected, it writes a `type: "response"` message (with ANSI-stripped output) to `thread:<id>:messages` via `tasklib`.
8. The thread detail page at `/threads/{thread_id}` **polls** every 3s (via HTMX, hitting `GET /api/threads/{thread_id}/history`). When the poll picks up a `type: "response"` message, a styled **response banner** appears at the top of the message timeline showing the master's answer.

**What is kept (persisted in Redis):**
- The user's original request — stored in `thread:<id>:messages` as a `role: "user"` message
- All intermediate master messages (plan, delegate, tool_call) — same list
- The final response — stored as a `role: "master"`, `type: "response"` message by the supervisor
- Thread state (`thread:<id>:current_state`) — status, repo, design, PR number

**What the user sees:**
- After submission, the browser is on `/threads/{thread_id}` showing the message history
- A "Waiting for master..." indicator with elapsed timer is visible while the thread is `planning`
- When the supervisor writes the response, the next poll cycle picks it up and the response banner replaces the waiting indicator
- The full message timeline (user request → master plan → delegated tasks → worker results → master response) is visible on one scrollable page

This preserves the master agent as the single source of truth for planning. The web UI has zero agency — it cannot create tasks, assign workers, or make decisions.

### Response detection (supervisor-managed)

Response completion is detected by the **supervisor**, not by relying on the LLM to self-report. When the supervisor writes a request to claude's stdin, it also monitors claude's **stdout** for the completion signal:

1. After the request text is written to claude stdin, the supervisor watches claude stdout for the **prompt marker** — the string that Claude Code emits when it's ready for the next input (e.g., `"⏵ "` or a configurable regex).
2. When the prompt marker appears on stdout, claude is done responding. The supervisor writes a JSON message to the thread history via `tasklib`:
   ```json
   {"role": "master", "type": "response", "content": "<captured from claude stdout>", "timestamp": "<iso8601>"}
   ```
3. The supervisor captures claude's stdout output during the response window (between request write and prompt marker) and stores it as the response content.
4. If the claude child process exits non-zero before the prompt marker is seen, the supervisor writes an error message with the captured stderr (exit code + last 4KB of stderr). This catches crashes, fatals, and API errors.
5. If no prompt marker is detected and claude hasn't exited within `MASTER_RESPONSE_TIMEOUT` (default 30 minutes, configurable), the supervisor writes an error message:
   ```json
   {"role": "master", "type": "error", "content": "Master agent timed out after 30m", "timestamp": "<iso8601>"}
   ```

The master agent's CLAUDE.md still documents the expected workflow (plan → delegate → aggregate → respond), but the web UI doesn't depend on the LLM correctly formatting a JSON message. The supervisor is the authoritative source for "done / error / timeout."

The web UI's thread detail page polls for messages with `type: "response"` or `type: "error"` and displays them in a styled banner (green for response, red for error). Threads with a pending request and no supervisor-written response yet show a "Waiting for master..." indicator with an elapsed timer.

**Fallback completion signal:** As a backup to prompt-marker detection, the master agent's CLAUDE.md instructs it to `SET thread:<id>:complete 1` as the final step before returning. The supervisor confirms completion via **both** the prompt marker on stdout **and** the presence of this Redis key. If the key appears but no marker is seen, the supervisor still considers the response complete. This dual-signal approach guards against both prompt-marker false positives and false negatives.

**Multi-turn interaction:** Claude Code can ask clarifying questions mid-session. When it does, the supervisor captures the question, the prompt marker triggers response detection, and the web UI shows the question. The user can submit a follow-up to the same thread. If the master hangs waiting for input, `MASTER_RESPONSE_TIMEOUT` handles the timeout.

**ANSI handling:** The supervisor strips carriage returns and escape sequences for clean inline display. For diffs and colored code blocks that lose meaning when stripped, the raw ANSI output is preserved in `<WORKSPACE_DIR>/<thread_id>/.response.raw` alongside the cleaned version. The web UI offers a "View raw output" toggle.

### Thread creation (bootstrap only)

`POST /api/threads` is intentionally allowed as a lightweight bootstrap — the web UI creates a thread shell so it has a thread ID to attach the request to. This is not a planning action; it's equivalent to running `mkdir -p` before writing a file. The master agent still owns all thread state updates (`status`, `design`, `pr_number`, etc.).

### What the web UI does NOT do

- Does NOT call the LLM
- Does NOT define tools for the master agent
- Does NOT enqueue tasks, wait on tasks, or cancel tasks directly
- Does NOT update thread state (beyond creating an empty thread)
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
func (c *Client) GetThreadHistory(threadID string, tail int) ([]Message, error)
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

// Inbox (atomic write — used by cmd/webui)
// PushRequestAtomic wraps three operations in a Lua script for atomicity:
//   1. HEXISTS thread:<id>:current_state gh_repo → if missing, HSET gh_repo <repo>
//      (REST body field "repo" maps to Redis hash field "gh_repo" — backward-compat with task.py)
//   2. RPUSH thread:<id>:messages <user-request-json>
//   3. LPUSH requests:inbox <request-payload-json>
func (c *Client) PushRequestAtomic(threadID, repo, request string) error

// CancelRequest atomically (Lua script):
//   1. Removes the request from requests:inbox and requests:inbox_processing
//   2. Sets thread:<id>:current_state status to "cancelled"
// Used by the web UI. Covers both inbox and processing list (race with LMOVE).
// If the supervisor has already begun processing, it checks thread status before
// writing to claude stdin and will drop the cancelled request.
func (c *Client) CancelRequest(threadID string) error
```

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
2. Starts a background **heartbeat goroutine** that runs `SETEX worker:<type>:<hostname>:heartbeat 30 1` every 10 seconds (30s TTL with 10s refresh = 20s margin for GC pauses and Redis blips; hostname from `os.Hostname()`, read once at startup), independently of task processing. This keeps the heartbeat fresh even during long task executions (up to `TASK_TIMEOUT`, default 30 minutes).
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
| **Actions** | If thread has a pending request (status `planning`), show "Cancel request" button. Submits `POST /api/threads/{id}/cancel`. Only cancels the inbox entry — already-enqueued tasks continue to completion but the master won't process further. |

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
| `POST` | `/api/requests` | Submit a request to the master agent. Body: `{thread_id?, repo?, request}`. `request` capped at 32KB (HTTP 413 if exceeded). Checks `supervisor:busy` (set by supervisor while processing); returns `503` with `Retry-After` if busy. Returns `429` if inbox depth exceeds `MAX_INBOX_DEPTH` (default 50). Auto-generates `thread_id` if omitted (`web_<ts>_<10 base36 [0-9a-z]>`), then calls `tasklib.PushRequestAtomic`. Returns `{thread_id, status: "submitted"}`. Returns `409 Conflict` if the thread already has a pending inbox entry (enforced via `requests:inbox:pending:<thread_id>` sentinel key with TTL in the Lua script). |
| `POST` | `/api/threads/{thread_id}/cancel` | Cancel a pending request. Calls `tasklib.CancelRequest` to remove the request from `requests:inbox` and mark the thread as cancelled. Only works for requests that haven't been picked up by the master yet. |

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
| `DELETE` | `/api/threads/{thread_id}/workspace` | Cleanup thread workspace directory. Validates path within `WORKSPACE_DIR` (rejects `../` traversal with 400). Refuses if `ListTasks(threadID, status="running")` is non-empty (400: "in-flight tasks"). Requires `?confirm=true`. Auth-gated. Does NOT delete Redis thread/task keys (archival). |
| `POST` | `/api/threads/{thread_id}/keep` | Extend Redis TTL for thread keys (7 more days). Prevents auto-expiry for long-lived threads. Auth-gated. |

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

Rate limiting: `POST /api/requests` is rate-limited to 10 requests per minute per client IP via chi middleware. Exceeded returns `429 Too Many Requests`. When deployed behind a reverse proxy (nginx/Caddy), configure trusted `X-Forwarded-For` / `X-Real-IP` headers so the middleware sees real client IPs, not the proxy IP.

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

### 6. Inbox Reader + Supervisor

The master container runs a **supervisor process** (PID 1) that manages all child processes and multiplexes stdin into the Claude Code session:

```
master container (ENTRYPOINT: supervisor):
  supervisor (Go binary, PID 1)
    ├─ connects to Redis via tasklib (for writing response/error messages)
    ├─ spawns inbox-reader as child process (restarts on exit)
    ├─ spawns claude as child process
    ├─ opens named pipe /tmp/master-inbox.fifo (O_RDONLY | O_NONBLOCK)
    ├─ opens /dev/tty for interactive CLI input
    ├─ pipes claude stdout for prompt-marker detection
    ├─ select loop:
    │   ├─ reads from FIFO → acquires stdin-mutex → writes to claude stdin
    │   │   → monitors stdout for prompt marker (MASTER_PROMPT_MARKER regex)
    │   │   → on marker: writes type:"response" to thread via tasklib → releases mutex
    │   │   → on timeout (MASTER_RESPONSE_TIMEOUT, default 30m): writes type:"error"
    │   └─ reads from TTY  → acquires stdin-mutex → writes to claude stdin
    │       → monitors stdout for prompt marker → releases mutex
    └─ signal handling:
        ├─ SIGTERM/SIGINT → forward to claude → wait for exit → cleanup FIFO → exit
        └─ inbox-reader exit → log warning → respawn after 1s backoff

  inbox-reader (Go binary, spawned by supervisor)
    └─ LMOVE requests:inbox requests:inbox_processing → writes to FIFO → LREM requests:inbox_processing
       (reconnect-on-error loop; startup sweep restores stranded processing entries)
```

How it works:

1. `supervisor` spawns both `inbox-reader` and `claude` as child processes. On startup, it checks `/tmp/inbox-reader.pid` for a stale process from a previous (crashed) supervisor, sends `SIGTERM` if found, and removes the PID file before re-creating the FIFO. If `inbox-reader` exits unexpectedly during normal operation, the supervisor respawns it after a 1s backoff.
2. `inbox-reader` runs a reconnect-on-error loop with a **reliable queue pattern**: `LMOVE requests:inbox requests:inbox_processing 0 0` (atomically moves to processing list), writes to the FIFO, then `LREM requests:inbox_processing`. On startup, a sweep restores any stranded `requests:inbox_processing` entries back to `requests:inbox` (covers crash-after-LMOVE).
3. `supervisor` opens the FIFO with `O_NONBLOCK`. On startup, it drains any leftover bytes from the FIFO (a short read loop that discards partial data from a previous crashed instance) before entering the select loop. This prevents a partial newline-delimited message from being mis-parsed as valid. The supervisor creates and opens the FIFO reader end **before** spawning the inbox-reader, so the inbox-reader's `open(WRONLY)` never blocks on `ENXIO`.

**FIFO message framing:** Requests are written as **newline-delimited JSON** (`<json>\n`) with a unique `request_id` field (UUIDv4). The supervisor accumulates reads into a buffer, splitting on `\n` to get complete messages. This handles partial reads (Linux PIPE_BUF is 4KB and payloads can be ~32.5KB — writes larger than PIPE_BUF are non-atomic). The supervisor deduplicates by `request_id` within a 1-hour window — if the startup sweep restores a stranded `requests:inbox_processing` entry that was already partially delivered, the duplicate is silently dropped.
4. `supervisor` does a `select` on FIFO, TTY, and claude stdout file descriptors, with a periodic wake-up (1s timer) to `waitpid(-1, WNOHANG)` to reap **any** zombie child process (PID 1 responsibility — prevents process table exhaustion). This also detects claude exiting mid-request before the prompt marker.
5. Before writing to claude stdin, the supervisor checks `thread:<id>:current_state` via `tasklib` — if the thread status is `"cancelled"`, the request is dropped (a cancel raced with inbox delivery). This closes the race window between `CancelRequest` and the inbox-reader's `LMOVE`. It then monitors claude's stdout for the **prompt marker** — a configurable regex (`MASTER_PROMPT_MARKER`, default matches Claude Code's `"⏵ "` prompt). When the marker appears, the supervisor:
   - Captures the stdout output between request-write and prompt-marker
   - Strips ANSI escape sequences and carriage-return-overwrites (`\r`) from captured output (Claude Code emits spinner animations, color codes, and line-rewrite progress bars)
   - Writes a `type: "response"` message to the thread history via `tasklib` with the cleaned output
   - Releases the mutex
6. If the prompt marker is not detected within `MASTER_RESPONSE_TIMEOUT` (default 30 minutes), the supervisor writes a `type: "error"` message and releases the mutex. This timeout is generous because Claude Code tool calls (e.g., `task.py wait --timeout 300`) can take minutes.
7. To prevent mutex starvation from long interactive CLI sessions, FIFO-side mutex acquisition has a configurable timeout (`MASTER_MUTEX_TIMEOUT`, default 5 minutes). If the mutex can't be acquired within this window, the request is re-queued via `RPUSH` with a `retry_after` field (now + 30s) and an incremented `retry_count`. After `MASTER_RETRY_MAX` (default 3), the request is abandoned and the web UI receives a `410 Gone` via a status update to the thread. The inbox-reader skips entries with `retry_after > now`, `RPUSH`ing them back to the tail (no tight re-pop loop).
8. On `SIGTERM` (Docker stop), the supervisor forwards the signal to claude, waits for graceful exit, then cleans up the FIFO and exits.
9. If the supervisor itself crashes (panic, OOM), both `inbox-reader` and `claude` die with it. Docker restarts the container (`restart: unless-stopped`). Pending inbox entries remain in Redis — they're picked up after restart. The supervisor's startup sweep not only restores stranded `requests:inbox_processing` entries but also **DEL**'s any stale `requests:inbox:pending:*` sentinels for threads that already have a `type: "response"` or `type: "error"` message (leftover from a crash mid-completion). To prevent crash loops from repeatedly `LMOVE`ing new requests, the supervisor tracks consecutive crash count via a `supervisor:restart_count` Redis key (TTL 60s) and skips the processing-list sweep if the count exceeds 3.

The FIFO is created by the Dockerfile:

```dockerfile
RUN mkfifo /tmp/master-inbox.fifo
ENTRYPOINT ["/usr/local/bin/supervisor"]
```

**Readiness probe:** The supervisor serves a minimal HTTP health endpoint on `:9090` (`GET /healthz` returns 200 when the FIFO is open, both child processes are running, and Redis is connected).

**Supervisor env vars:**

| Env var | Default | Purpose |
|---------|---------|---------|
| `MASTER_PROMPT_MARKER` | `⏵ ` (UTF-8: `\xe2\x8f\xb5 `) | Regex for Claude Code's ready-for-input prompt. Only the FIFO-triggered response path performs marker detection; TTY-initiated input (interactive CLI) does not check for markers. |
| `MASTER_RESPONSE_TIMEOUT` | `1800` | Seconds before declaring the master hung (30 min) |
| `MASTER_MUTEX_TIMEOUT` | `300` | Seconds before re-queueing a web request that can't get stdin (5 min) |
| `MASTER_RESPONSE_MIN_ELAPSED` | `2` | Ignore prompt markers within first N seconds (prevents false match on initial output) |
| `MASTER_RESPONSE_QUIET_PERIOD` | `3` | Seconds of no stdout growth before a marker is considered valid (prevents mid-output false match) |
| `MASTER_RESPONSE_MAX_SIZE` | `65536` | Max inline bytes of captured stdout (64KB). Exceeded content is written to `<WORKSPACE_DIR>/<thread_id>/.response.txt` and the thread message contains `{"file": "<path>"}` instead of inline content. The web UI serves the file on demand. |

### 7. Authentication

**Default:** When `WEBUI_API_KEY` is set, **all** `/api/` endpoints require `Authorization: Bearer <key>` — both read and write. The browser UI pages embed the key via a cookie or session token so the polling HTMX requests are authenticated. If `WEBUI_API_KEY` is not set, a warning is logged on startup and all requests proceed without auth (development mode / single-user trusted network).

Thread history, task results, and aggregate stats can contain source code, PR details, and error messages. Exposing these without auth in a shared-network environment is a data leak risk.

To disable auth intentionally in dev, explicitly set `WEBUI_API_KEY=` (empty string).

For production deployments on shared networks, place a reverse proxy (nginx, Caddy) with TLS and auth in front of the web UI. The web UI itself is stateless — multiple replicas can run behind a load balancer.

**CSRF protection:** Mutation endpoints require `Content-Type: application/json` (reject non-JSON with `415`). Since simple cross-origin forms cannot set `Content-Type: application/json`, this is an effective CSRF defense without requiring tokens. Additionally, HTMX sends a custom `HX-Request` header on all AJAX requests — the middleware validates this header is present for mutation endpoints.

### 8. Docker Integration

Updated images (Go binaries replace Python scripts):

| Image | Change |
|-------|--------|
| `master-agent` | Replace `scripts/task.py` with Go `task` binary. Add `supervisor` + `inbox-reader` binaries. Python fallback via `TASKLIB_BACKEND=python`. |
| `worker-claude` | New `ENTRYPOINT ["worker", "claude"]`. Copies Go `worker` binary into `docker/worker-claude/`. |
| `copilot` | Copies Go `worker` binary into existing `docker/copilot/` image. `ENTRYPOINT ["worker", "copilot"]` replaces `worker.py`. |
| `opencode` | Copies Go `worker` binary into existing `docker/opencode/` image. `ENTRYPOINT ["worker", "opencode"]` replaces `worker.py`. |
| `webui` (new) | Go `webui` binary, templates, static CSS, vendored HTMX. Base image: `gcr.io/distroless/static-debian12` (minimal, shell-less — set `DEBUG=true` build arg to use `ai-base` instead for `docker exec` access). |

New `webui` service in `docker-compose.yml`:

```yaml
webui:
  image: ${WEBUI_IMAGE:-webui:latest}
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
    - WORKSPACE_DIR=${WORKSPACE_DIR:-/workspace}
    - TASKLIB_BACKEND=go
  volumes:
    - workspace:/workspace
  ports:
    - "${WEBUI_PORT:-8000}:8000"
  restart: unless-stopped
```

The workspace volume mount enables the web UI to serve oversized response files (`.response.txt`) written by the supervisor.

Master service gains a healthcheck targeting the supervisor's readiness probe:

```yaml
master:
  # ... existing config ...
  healthcheck:
    test: ["CMD", "curl", "-f", "http://localhost:9090/healthz"]
    interval: 10s
    timeout: 2s
    retries: 3
```

`master` and `worker` services also gain `TASKLIB_BACKEND=go` with fallback to `python`. This allows a one-line env change to revert to the Python code if a Go `tasklib` edge case surfaces in production.

Dockerfiles:

- `docker/master-agent/Dockerfile` — copies `task` + `supervisor` + `inbox-reader` binaries; `RUN mkfifo /tmp/master-inbox.fifo`; `ENTRYPOINT ["/usr/local/bin/supervisor"]`
- `docker/worker-claude/Dockerfile` — copies `worker` binary; `ENTRYPOINT ["worker", "claude"]`
- `docker/webui/Dockerfile` — multi-stage: Go build, then `FROM gcr.io/distroless/static-debian12` (or `FROM ai-base:latest` when `DEBUG=true`); copies `webui` binary + `templates/` + `static/` (including vendored `htmx.min.js`)

### 9. File Structure

```
go.mod                          # Single Go module for the whole repo
go.sum

tasklib/                        # Shared library — all Redis CRUD (pure Redis, no filesystem)
  client.go                     # Redis connection, key name helpers, TTL constants
  tasks.go                      # Task CRUD
  threads.go                    # Thread CRUD (no cleanup — that lives in cmd/task)
  workers.go                    # Worker stats + heartbeat
  inbox.go                      # PushRequestAtomic (Lua script)
  lua/
    push_request.lua            # Atomic inbox write + thread history append + dedup
    cancel_request.lua          # Atomic inbox removal + thread status → cancelled
  tasks_test.go                 # Unit tests with miniredis (non-blocking CRUD only)
  threads_test.go
  tasks_integration_test.go     # Integration tests with real Redis (blocking cmds + JSON compat)

cmd/
  task/
    main.go                     # CLI (cobra) — drop-in replacement for task.py (~350 lines)
  worker/
    main.go                     # Worker loop — replacement for worker.py (~300 lines)
  supervisor/
    main.go                     # Stdin multiplexer (FIFO + TTY → claude stdin, mutex-guarded)
  inbox-reader/
    main.go                     # LMOVE requests:inbox → FIFO write → LREM
  webui/
    main.go                     # HTTP server entry point
    internal/
      api/
        router.go               # Chi router, HTMX detection middleware, auth middleware, error recovery
        requests.go             # POST /api/requests (auto-gen thread, tasklib.PushRequestAtomic)
        tasks.go                # GET /api/tasks handlers (read-only)
        threads.go              # GET/POST /api/threads + POST /api/threads/{id}/cancel handlers
        workers.go              # GET /api/workers handlers
        system.go               # GET /api/health, /api/stats handlers
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
    Dockerfile                  # UPDATED: Go binaries + supervisor, mkfifo, Python fallback
  worker-claude/
    Dockerfile                  # UPDATED: Go worker binary, Python fallback
  webui/
    Dockerfile                  # NEW: multi-stage, distroless base

scripts/                        # Kept behind TASKLIB_BACKEND=python toggle for one release cycle
  task.py                       # → replaced by cmd/task when TASKLIB_BACKEND=go
  worker.py                     # → replaced by cmd/worker when TASKLIB_BACKEND=go
  test_json_compat.py           # NEW: side-by-side integration tests (Go vs Python output)

docker-compose.yml              # MODIFIED: add webui service, TASKLIB_BACKEND env on all services
```

### 10. Redis Data Model

The Go `tasklib` reads and writes the exact same Redis keys used by `task.py` and `worker.py` today. No migration needed.

Keys used by tasklib (all pre-existing except those marked NEW):

| Key | Type | Purpose |
|-----|------|---------|
| `active_tasks` | Hash | **Existing.** Currently executing tasks. Keyed by `task_id`, value is JSON task info. Written by `cmd/worker` (HSET on start, HDEL on completion). Read by `task list` and web UI. |
| `requests:inbox` | List (NEW) | Pending web UI requests. Inbox-reader moves to `requests:inbox_processing` atomically via `LMOVE`. |
| `requests:inbox_processing` | List (NEW) | Requests being written to the FIFO. Stranded entries restored to inbox on startup. |
| `requests:inbox:pending:<thread_id>` | String (NEW, SET NX) | Sentinel preventing duplicate inbox entries. Deleted by supervisor when request is fully processed (not solely TTL-based — cleared on completion so follow-ups aren't blocked for 10m). |
| `supervisor:busy` | String (NEW, TTL 1800s) | Set by supervisor while claude is actively processing, refreshed every 60s. Auto-clears on TTL expiry if supervisor crashes. Web UI checks via Lua script that also tests `thread:<id>:complete` — a stale `supervisor:busy` with a completed thread doesn't block. |
| `thread:<id>:complete` | String (NEW) | Set by master agent on completion. Dual-signal with prompt marker for completion detection. |
| `worker:<type>:<hostname>:heartbeat` | String (NEW, SETEX, 30s TTL) | Per-instance worker liveness. Value `1`. Set by background goroutine every 10s (20s margin). Read by `GetWorkerStats` via `SCAN worker:*:heartbeat`. Docker hostname changes on restart → old keys age out via TTL. |

Thread message taxonomy — all messages in `thread:<id>:messages` have `role`, `type`, `content`, and `timestamp` fields:

| `role` | `type` | Meaning | Rendered as |
|--------|--------|---------|-------------|
| `"user"` | `"request"` | Request from web UI or CLI | Plain text bubble |
| `"master"` | `"plan"` | Master agent planning output | Dashed border, muted |
| `"master"` | `"delegate"` | Master agent delegating a task | Dashed border, muted |
| `"master"` | `"tool_call"` | Master agent invoking a tool (task.py) | Dashed border, muted |
| `"master"` | `"response"` | Final response written by supervisor | Green banner |
| `"master"` | `"error"` | Error written by supervisor | Red banner |
| `"worker"` | `"result"` | Worker agent task result (new Go worker) | Solid border, worker color-coded |
| `"claude"` | `"result"` | Legacy: Python worker role (backward-compat) | Same as `worker` |
| `"copilot"` | `"result"` | Legacy: Python worker role (backward-compat) | Same as `worker` |
| `"opencode"` | `"result"` | Legacy: Python worker role (backward-compat) | Same as `worker` |

During the `TASKLIB_BACKEND=python` transition period, both old (`role: "claude"`) and new (`role: "worker"`) messages coexist. The web UI rendering maps all four to the same visual treatment. Unknown types fall back to plain-text rendering.

All other keys are unchanged.

**Thread lifecycle:** `tasklib` preserves the existing TTL behavior from `task.py` (tasks are created with `TTL_TASK`, threads with `TTL_THREAD`). On thread completion (status `complete` or `cancelled`), the web UI handler sets an **additional** 7-day TTL as a safety net — the existing TTLs already provide baseline cleanup. A `POST /api/threads/{id}/keep` endpoint extends the TTL. Expired keys are cleaned up automatically by Redis. A startup goroutine in the web UI finds orphaned threads (picked up by inbox-reader but no response after `MASTER_RESPONSE_TIMEOUT`) and applies TTL to them as well.

## Implementation Plan

### Step 0: Pre-implementation validation

Before any code is written, validate two architectural assumptions:

1. **Prompt marker behavior:** Run Claude Code in a container with a multi-turn task (delegate → wait → result → delegate → aggregate) and capture stdout. Confirm that Claude Code's prompt marker (`⏵ `) only appears at final completion, not between tool invocations. **Also test with non-TTY stdin** (`echo "instruction" | claude` vs `claude` with TTY) — Claude Code may suppress the prompt marker entirely when stdin is a pipe. If so, wrap claude via `unbuffer` or `script -qc` to provide a pseudo-TTY.

2. **Blocking command behavior:** Verify `LMOVE` and `BLMOVE` are available in the target Redis version (6.2+). Verify `SETEX` / `SET ... EX` semantics.

### Deployment milestones

To reduce risk, the implementation is grouped into three deployable milestones. Each milestone ships, stabilizes, and can be rolled back independently via `TASKLIB_BACKEND=python`.

| Milestone | Steps | Deliverables | Revert |
|-----------|-------|-------------|--------|
| M1: tasklib | 1, 2 | Go `tasklib` package + JSON compat tests (validated, not yet used in prod) | N/A (test-only) |
| M2: CLI + worker | 3 | Go `task` + `worker` binaries behind `TASKLIB_BACKEND=go` toggle alongside Python | Set `TASKLIB_BACKEND=python` |
| M3: web UI | 4, 5, 6, 7 | Supervisor + inbox-reader + web UI, full stack | `docker compose down webui` |

### Step 1: Go module scaffold + `tasklib` package

Create `go.mod` at repo root. Dependencies: `go-redis/v9`, `chi/v5`, `cobra`, `miniredis`. Implement `tasklib/` with full CRUD for tasks, threads, workers, heartbeat, and the `PushRequestAtomic` Lua script. Cover all non-blocking operations with `miniredis` unit tests.

This is the highest-leverage step — `tasklib` correctness determines correctness of all three binaries.

### Step 2: Side-by-side JSON compatibility test suite

Write `scripts/test_json_compat.py` — an integration test that:
1. Spins up a real Redis instance
2. Runs each operation through both the Python code and the Go `tasklib` with identical inputs
3. Compares the Redis keys produced byte-for-byte
4. Compares JSON output (stdout) field-for-field

This gates the removal of Python. All tests must pass identically before proceeding to Step 3.

### Step 3: Go `task` CLI + `worker` binary

Build `cmd/task/main.go` — a `cobra` CLI with the same subcommands and flags as `task.py`. Build `cmd/worker/main.go` — the worker loop with heartbeat. Both read `TASKLIB_BACKEND` env var; when set to `python`, they exec the Python scripts instead. Default is `go`. Update Dockerfiles to include Go binaries while keeping Python scripts as fallback.

### Step 4: Supervisor + inbox-reader

Build `cmd/supervisor/main.go` — stdin multiplexer (select loop, mutex, FIFO + TTY → claude stdin). Build `cmd/inbox-reader/main.go` — `BLPOP` on `requests:inbox`, write to `/tmp/master-inbox.fifo`. Update `docker/master-agent/Dockerfile` with `mkfifo` and new `ENTRYPOINT`. Update `docker/master-agent/CLAUDE.md` with:
- Awareness that requests may arrive from the web UI (via the inbox) in addition to interactive CLI
- That completion is detected automatically by the supervisor (master does not need to self-report)

### Step 5: Web UI REST API handlers

Build `cmd/webui/internal/api/` — chi router, auth middleware (bearer token when `WEBUI_API_KEY` is set), HTMX detection middleware, all `/api/` endpoints backed by `tasklib`. Each handler checks `HX-Request` header to return either an HTML partial or JSON. `POST /api/requests` handles auto-generation of `thread_id` when omitted.

### Step 6: HTML templates and CSS

Build all Go `html/template` files under `cmd/webui/internal/templates/`. Minimal CSS with CSS custom properties for theming (`--color-*`, `--spacing-*`). HTMX loaded from `WEBUI_HTMX_SRC` (default `/static/htmx.min.js` — vendored). HTMX attributes for polling (intervals from respective `WEBUI_POLL_*` env vars) and form submissions. Response banner in thread detail for master responses (green for `type: "response"`, red for `type: "error"`).

### Step 7: Dockerize

Multi-stage Dockerfiles for all images. `webui` uses `gcr.io/distroless/static-debian12`. Add `webui` service to `docker-compose.yml`. `TASKLIB_BACKEND=go` on all services. Update CI (`.github/workflows/build-images.yml`) to build the Go binaries and all images.

### Step 8: End-to-end test

Spin up the full stack. Submit a request via the web UI → verify inbox-reader delivers to supervisor FIFO → supervisor writes to claude stdin → master plans and delegates via Go `task` CLI → Go workers consume, execute, return results → supervisor detects prompt marker on claude stdout → supervisor writes `type: "response"` message → thread detail page shows response banner via HTMX polling. Test REST API with `curl`. Verify auth middleware rejects unauthenticated mutations when `WEBUI_API_KEY` is set.

## Verification

1. `go test ./tasklib/...` passes all unit tests with `miniredis`
2. `python3 scripts/test_json_compat.py` passes all byte-for-byte comparisons
3. `docker compose up -d` starts all services including webui
4. `curl http://localhost:8000/api/health` returns `{"redis":"ok","workers":{...}}`
5. `curl -X POST http://localhost:8000/api/requests -H 'Content-Type: application/json' -H 'Authorization: Bearer <key>' -d '{"request":"Say hello"}'` auto-generates a thread_id, atomically writes to inbox + thread history; returns `{"thread_id":"web_<ts>_<abcdefghij>","status":"submitted"}`
6. The master agent picks up the request via inbox-reader → FIFO → supervisor → claude stdin, processes it using the Go `task` CLI, and writes a `type: "response"` message when done
7. Browser at `http://localhost:8000` shows dashboard with worker cards (online/offline from heartbeat), active threads with response indicators
8. Thread detail page shows message history updating in real time via HTMX polling, with a response banner when the master finishes
9. `curl http://localhost:8000/api/tasks` returns JSON matching the UI display
10. The Go `task` CLI produces identical output to `task.py` for the same Redis state
11. Setting `TASKLIB_BACKEND=python` on the master container falls back to `task.py` with identical behavior
