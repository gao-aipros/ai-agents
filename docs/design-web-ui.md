# Web UI for Master Agent — Design & Implementation Plan

## Context

The ai-agents system uses a master agent (Claude Code with orchestrator instructions) to plan complex tasks, delegate sub-tasks to worker agents via a Redis task queue, and aggregate results. Currently everything is CLI-only: the user types requests directly into the master container's interactive Claude Code session. The master agent uses `task.py` as a skill (a tool it can invoke) to interact with Redis — enqueuing tasks, checking statuses, reading results, and managing threads. Workers run `worker.py` to consume tasks from Redis and execute agent CLIs.

We need a web console that:
1. Forwards user requests to the **existing master agent** (the running Claude Code session) — the master remains the sole planner/orchestrator
2. Provides **monitoring** of threads, workers, and tasks by reading Redis state
3. Is implemented in **Go** with an HTMX frontend

The web UI is an **addon**, not a replacement. The master agent still does all planning, tool calling, and delegation. The web UI just gives it a browser-based front door and a real-time dashboard.

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
           via named pipe                ├─ heartbeat: SETEX worker:<type>:heartbeat
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
   - Creates the thread state hash if it doesn't exist (`HSETNX thread:<id>:current_state repo <repo>`)
   - Appends the user request to `thread:<id>:messages` as a JSON message with `role: "user"` and `source: "webui"`
   - LPUSHes the request payload onto `requests:inbox`
   All three operations succeed or fail together — no orphan thread shells. A separate `POST /api/threads` endpoint still exists for creating a thread without submitting a request (e.g., pre-configuring repo metadata before the first request).
4. The web UI responds with `{thread_id, status: "submitted"}` and the browser **redirects** to `/threads/{thread_id}`.
5. Meanwhile, the inbox-reader does `BLPOP` on `requests:inbox`, writes the request text to a **named pipe** (`/tmp/master-inbox.fifo`).
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
4. If no prompt marker is detected within `MASTER_RESPONSE_TIMEOUT` (default 30 minutes, configurable), the supervisor writes an error message:
   ```json
   {"role": "master", "type": "error", "content": "Master agent timed out after 30m", "timestamp": "<iso8601>"}
   ```

The master agent's CLAUDE.md still documents the expected workflow (plan → delegate → aggregate → respond), but the web UI doesn't depend on the LLM correctly formatting a JSON message. The supervisor is the authoritative source for "done / error / timeout."

The web UI's thread detail page polls for messages with `type: "response"` or `type: "error"` and displays them in a styled banner (green for response, red for error). Threads with a pending request and no supervisor-written response yet show a "Waiting for master..." indicator with an elapsed timer.

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
| Language | Go 1.22 (pinned in `go.mod` and `golang:1.22-bookworm` build images) |
| HTTP router | `chi` (go-chi/chi/v5) |
| Redis client | `go-redis/v9` |
| CLI framework | `cobra` (for `cmd/task`) |
| Testing (unit) | `miniredis` — non-blocking CRUD operations only |
| Testing (integration) | Real Redis — blocking commands (`BLPOP`, `BLMOVE`) + JSON compatibility |
| Templates | `html/template` (stdlib) |
| Frontend | HTMX (~15KB, vendored in `static/htmx.min.js`), no JS build step. Source URL overridable via `WEBUI_HTMX_SRC` env var for air-gapped deployments. |
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
func (c *Client) WaitTask(taskID string, timeout time.Duration) (*Task, error)
func (c *Client) CancelTask(taskID string) error
func (c *Client) RequeueStale(worker string, olderThan time.Duration) ([]string, error)

// Threads (read + write — used by all three binaries)
func (c *Client) CreateThread(threadID, repo string) (*Thread, error)
func (c *Client) GetThread(threadID string) (*Thread, error)
func (c *Client) ListThreads() ([]*Thread, error)
func (c *Client) GetThreadHistory(threadID string, tail int) ([]Message, error)
func (c *Client) UpdateThread(threadID string, fields map[string]string) error
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
func (c *Client) UpdateWorkerHeartbeat(workerType string) error  // SETEX worker:<type>:heartbeat 15

// Inbox (atomic write — used by cmd/webui)
// PushRequestAtomic wraps three operations in a Lua script for atomicity:
//   1. HSETNX thread:<id>:current_state repo <repo> (creates thread if not exists;
//      if thread already exists with a different repo, the existing repo is kept)
//   2. RPUSH thread:<id>:messages <user-request-json>
//   3. LPUSH requests:inbox <request-payload-json>
func (c *Client) PushRequestAtomic(threadID, repo, request string) error

// CancelRequest atomically (Lua script):
//   1. Removes the request from requests:inbox
//   2. Sets thread:<id>:current_state status to "cancelled"
// Used by the web UI. Only works for requests still in the inbox (not yet picked up).
// For requests already being processed, the thread status update signals the master
// to abort gracefully (master checks status early in its processing loop).
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

Workers update a heartbeat key in their poll loop:

| Key | Type | TTL | Purpose |
|-----|------|-----|---------|
| `worker:<type>:heartbeat` | String | 15s | Set by `cmd/worker` via `SETEX` on each poll iteration. The web UI checks for this key to determine online/offline status. If absent or expired, the worker is offline. |

`GetWorkerStats()` reports `online: true` when either the heartbeat key exists OR the `active_tasks` hash contains entries for this worker type. A worker running a long task (up to 30 minutes) keeps its `active_tasks` entries fresh while the heartbeat TTL is only 15s — the `active_tasks` check provides a secondary liveness signal that prevents false offline indicators during long-running work.

The heartbeat key is per worker **type** (not per instance). Multiple replicas of the same worker type (e.g., 3 `worker-claude` containers) will race on the same key. For now, assume single-instance per worker type. Multi-instance support (instance-specific keys like `worker:<type>:<hostname>:heartbeat` with `SCAN`-based aggregation) is deferred to a future iteration.

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

Implementation: `cobra` commands that call `tasklib.Client` methods. `thread-cleanup` additionally calls `os.RemoveAll` on `<workspaceDir>/<thread_id>/` — this is the one command that touches the filesystem, and it lives in `cmd/task`, not `tasklib`. The workspace directory is read from the `WORKSPACE_DIR` env var (default `/workspace`).

**Stdout format compatibility:** The Go CLI must replicate the exact stdout output format of `task.py` for each command. The master agent parses stdout programmatically. Note that `task.py list` prints a human-readable table (not JSON), while `enqueue` returns `{"task_id": "..."}` and `status` returns a multi-field JSON object. All these formats are defined by the current Python code and must be matched byte-for-byte. The JSON compatibility test suite (Step 2) covers this.

Estimated ~350 lines of CLI glue (cobra dispatcher + output formatting + the one filesystem operation). The binary is statically compiled (`CGO_ENABLED=0`) and copied into the master container at `/usr/local/bin/task`.

## `cmd/worker` — Go worker loop (replaces `scripts/worker.py`)

Long-running process that:

1. Connects to Redis via `tasklib`
2. Starts a background **heartbeat goroutine** that runs `SETEX worker:<type>:heartbeat 15 "alive"` every 10 seconds, independently of task processing. This keeps the heartbeat fresh even during long task executions (up to `TASK_TIMEOUT`, default 30 minutes).
3. Main loop:
   - `BLMOVE tasks:queue:<worker> tasks:processing:<worker>` with 5s timeout
   - If no task, continue
4. `HSET active_tasks <task_id> <task_info_json>` — mark as active (also serves as a secondary liveness signal: a worker with entries in `active_tasks` is alive regardless of heartbeat)
5. Reads thread history and state via `tasklib`
6. Builds a prompt from task instruction + thread context
7. Executes `AGENT_CMD` (from env) as a subprocess with the prompt on stdin
8. Writes result + exit code back via `tasklib` (`task:<id>:result`, `task:<id>:status`)
9. `HDEL active_tasks <task_id>` — unmark
10. Appends result to thread history via `tasklib`

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
| **Response banner** | When the master has written a `type: "response"` message, show a styled banner with the master's summary. Configured via `WEBUI_POLL_INTERVAL` (default 3s when active). |
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
| `POST` | `/api/requests` | Submit a request to the master agent. Body: `{thread_id?, repo?, request}`. `request` capped at 32KB (HTTP 413 if exceeded). Returns `429 Too Many Requests` if `requests:inbox` length exceeds `MAX_INBOX_DEPTH` (default 50). Auto-generates `thread_id` if omitted (`web_<ts>_<10 base36 chars>`), then calls `tasklib.PushRequestAtomic`. Returns `{thread_id, status: "submitted"}`. Returns `409 Conflict` if the thread already has a pending inbox entry. |
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
| `DELETE` | `/api/threads/{thread_id}/workspace` | Cleanup thread workspace directory. Validates that the resolved path is within `WORKSPACE_DIR` (rejects `../` traversal with 400). Requires `?confirm=true` query param. Auth-gated. |

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
    └─ BLPOP requests:inbox → writes request text to /tmp/master-inbox.fifo
       (reconnect-on-error loop, same pattern as worker.py)
```

How it works:

1. `supervisor` spawns both `inbox-reader` and `claude` as child processes. On startup, it checks `/tmp/inbox-reader.pid` for a stale process from a previous (crashed) supervisor, sends `SIGTERM` if found, and removes the PID file before re-creating the FIFO. If `inbox-reader` exits unexpectedly during normal operation, the supervisor respawns it after a 1s backoff.
2. `inbox-reader` runs a reconnect-on-error loop: `BLPOP requests:inbox 0` wrapped in a retry that reconnects to Redis on `ConnectionError`, identical to `worker.py`'s behavior today.
3. `supervisor` opens the FIFO with `O_NONBLOCK` so that a stale partial write from a previous (crashed) supervisor instance doesn't block the new supervisor on startup. The supervisor creates and opens the FIFO reader end **before** spawning the inbox-reader, so the inbox-reader's `open(WRONLY)` never blocks on `ENXIO`.

**FIFO message framing:** Requests are written as **newline-delimited JSON** (`<json>\n`). The supervisor accumulates reads into a buffer, splitting on `\n` to get complete messages. This handles partial reads (Linux PIPE_BUF is 4KB and payloads can be ~32.5KB — writes larger than PIPE_BUF are non-atomic).
4. `supervisor` does a `select` on the FIFO, TTY, and claude stdout file descriptors.
5. Before writing to claude stdin, the supervisor checks `thread:<id>:current_state` via `tasklib` — if the thread status is `"cancelled"`, the request is dropped (a cancel raced with inbox delivery). This closes the race window between `CancelRequest` and the inbox-reader's `BLPOP`. It then monitors claude's stdout for the **prompt marker** — a configurable regex (`MASTER_PROMPT_MARKER`, default matches Claude Code's `"⏵ "` prompt). When the marker appears, the supervisor:
   - Captures the stdout output between request-write and prompt-marker
   - Strips ANSI escape sequences and carriage-return-overwrites (`\r`) from captured output (Claude Code emits spinner animations, color codes, and line-rewrite progress bars)
   - Writes a `type: "response"` message to the thread history via `tasklib` with the cleaned output
   - Releases the mutex
6. If the prompt marker is not detected within `MASTER_RESPONSE_TIMEOUT` (default 30 minutes), the supervisor writes a `type: "error"` message and releases the mutex. This timeout is generous because Claude Code tool calls (e.g., `task.py wait --timeout 300`) can take minutes.
7. To prevent mutex starvation from long interactive CLI sessions, FIFO-side mutex acquisition has a configurable timeout (`MASTER_MUTEX_TIMEOUT`, default 5 minutes). If the mutex can't be acquired within this window, the request is re-queued via `RPUSH` back onto `requests:inbox` (tail of the queue — maintains FIFO ordering) with a `retry_after` field (now + 30s). The inbox-reader `BLPOP`s the item, checks `retry_after`: if in the future, it `RPUSH`es the item back to the tail and `BLPOP`s again (no tight re-pop loop).
8. On `SIGTERM` (Docker stop), the supervisor forwards the signal to claude, waits for graceful exit, then cleans up the FIFO and exits.
9. If the supervisor itself crashes (panic, OOM), both `inbox-reader` and `claude` die with it. Docker restarts the container (`restart: unless-stopped`). Pending inbox entries remain in Redis — they're picked up after restart. Stale partial writes in the FIFO are handled by newline framing (incomplete lines without `\n` are discarded on next read start).

The FIFO is created by the Dockerfile:

```dockerfile
RUN mkfifo /tmp/master-inbox.fifo
ENTRYPOINT ["/usr/local/bin/supervisor"]
```

**Readiness probe:** The supervisor serves a minimal HTTP health endpoint on `:9090` (`GET /healthz` returns 200 when the FIFO is open, both child processes are running, and Redis is connected).

**Supervisor env vars:**

| Env var | Default | Purpose |
|---------|---------|---------|
| `MASTER_PROMPT_MARKER` | `⏵ ` | Regex for Claude Code's ready-for-input prompt |
| `MASTER_RESPONSE_TIMEOUT` | `1800` | Seconds before declaring the master hung (30 min) |
| `MASTER_MUTEX_TIMEOUT` | `300` | Seconds before re-queueing a web request that can't get stdin (5 min) |
| `MASTER_RESPONSE_MIN_ELAPSED` | `2` | Ignore prompt markers within first N seconds (prevents false match on initial output) |
| `MASTER_RESPONSE_QUIET_PERIOD` | `3` | Seconds of no stdout growth before a marker is considered valid (prevents mid-output false match) |
| `MASTER_RESPONSE_MAX_SIZE` | `65536` | Max bytes of captured stdout (64KB). Exceeded content is truncated with `...[truncated]` |

### 7. Authentication

**Default:** When `WEBUI_API_KEY` is set, **all** `/api/` endpoints require `Authorization: Bearer <key>` — both read and write. The browser UI pages embed the key via a cookie or session token so the polling HTMX requests are authenticated. If `WEBUI_API_KEY` is not set, a warning is logged on startup and all requests proceed without auth (development mode / single-user trusted network).

Thread history, task results, and aggregate stats can contain source code, PR details, and error messages. Exposing these without auth in a shared-network environment is a data leak risk.

To disable auth intentionally in dev, explicitly set `WEBUI_API_KEY=` (empty string).

For production deployments on shared networks, place a reverse proxy (nginx, Caddy) with TLS and auth in front of the web UI. The web UI itself is stateless — multiple replicas can run behind a load balancer.

### 8. Docker Integration

Updated images (Go binaries replace Python scripts):

| Image | Change |
|-------|--------|
| `master-agent` | Replace `scripts/task.py` with Go `task` binary. Add `supervisor` + `inbox-reader` binaries. Python fallback via `TASKLIB_BACKEND=python`. |
| `worker-claude` | Replace `scripts/worker.py` with Go `worker` binary. New `ENTRYPOINT ["worker", "claude"]`. Python fallback via `TASKLIB_BACKEND=python`. |
| `worker-copilot` | (inherits worker changes from base) |
| `worker-opencode` | (inherits worker changes from base) |
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
  ports:
    - "${WEBUI_PORT:-8000}:8000"
  restart: unless-stopped
```

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
    push_request.lua            # Atomic inbox write + thread history append
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
    main.go                     # BLPOP requests:inbox → write to named pipe
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
| `requests:inbox` | List (NEW) | Pending web UI requests. The inbox-reader does `BLPOP` on this list. Each value is JSON: `{thread_id, repo, request, submitted_at}`. |
| `worker:<type>:heartbeat` | String (NEW, SETEX, 15s TTL) | Worker liveness. Set by `cmd/worker` on each poll iteration. Read by `GetWorkerStats` to determine online/offline. |

Thread message taxonomy — all messages in `thread:<id>:messages` have `role`, `type`, `content`, and `timestamp` fields:

| `role` | `type` | Meaning | Rendered as |
|--------|--------|---------|-------------|
| `"user"` | `"request"` | Request from web UI or CLI | Plain text bubble |
| `"master"` | `"plan"` | Master agent planning output | Dashed border, muted |
| `"master"` | `"delegate"` | Master agent delegating a task | Dashed border, muted |
| `"master"` | `"tool_call"` | Master agent invoking a tool (task.py) | Dashed border, muted |
| `"master"` | `"response"` | Final response written by supervisor | Green banner |
| `"master"` | `"error"` | Error written by supervisor | Red banner |
| `"worker"` | `"result"` | Worker agent task result | Solid border, worker color-coded |

The web UI renders each type appropriately in the message timeline from day one. Unknown types fall back to plain-text rendering.

All other keys are unchanged.

## Implementation Plan

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
