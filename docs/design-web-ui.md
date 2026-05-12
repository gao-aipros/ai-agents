# Web UI for Master Agent — Design & Implementation Plan

## Context

The ai-agents system uses a master agent (Claude Code with orchestrator instructions) to plan complex tasks, delegate sub-tasks to worker agents via a Redis task queue, and aggregate results. Currently everything is CLI-only: the user types requests directly into the master container's interactive Claude Code session. The master agent uses `task.py` as a skill (a tool it can invoke) to interact with Redis — enqueuing tasks, checking statuses, reading results, and managing threads. Workers run `worker.py` to consume tasks from Redis and execute agent CLIs.

We need a web console that:
1. Forwards user requests to the **existing master agent** (the running Claude Code session) — the master remains the sole planner/orchestrator
2. Provides **monitoring** of threads, workers, and tasks by reading Redis state
3. Is implemented in **Go** with an HTMX frontend

The web UI is an **addon**, not a replacement. The master agent still does all planning, tool calling, and delegation. The web UI just gives it a browser-based front door and a real-time dashboard.

### Shared `tasklib` — the foundation

Rather than reimplement Redis logic twice (Go for web UI, Python for `task.py` / `worker.py`), extract a shared **Go `tasklib`** package that all three Go binaries use:

```
tasklib/              # Shared Go library — all Redis CRUD for tasks, threads, workers
  ├─ tasks.go         # enqueue, status, result, list, wait, cancel, requeue-stale
  ├─ threads.go       # create, history, state, update, list, cleanup, unlock
  ├─ workers.go       # worker stats, health, queue depths
  ├─ inbox.go         # write to requests:inbox
  └─ *_test.go        # Unit tests with miniredis

cmd/task/main.go      # Go CLI — drop-in replacement for task.py (master agent skill)
cmd/worker/main.go    # Go worker loop — replacement for worker.py
cmd/webui/main.go     # Go HTTP server + HTMX frontend
```

All three binaries are built from the same `go.mod`. The Go `tasklib` produces the exact same Redis key names, JSON shapes, and behavior as the current Python code. Python `task.py` and `worker.py` are **removed** once the Go equivalents are validated.

## Architecture

```
master (Claude Code session)          webui (Go binary, :8000)
  ├─ plans & delegates                  ├─ writes requests to Redis inbox
  ├─ invokes Go task CLI as skill       ├─ reads Redis via tasklib for monitoring
  │   (cmd/task: enqueue, wait, ...)    ├─ serves REST API (JSON)
  └─ receives requests via:             └─ serves HTMX frontend
      1. interactive CLI (stdin)
      2. Redis inbox (from web UI)     worker (Go binary)
       └─ inbox-reader sidecar           ├─ tasklib: BLMOVE from queue
                                         ├─ exec AGENT_CMD as subprocess
                                         └─ tasklib: write result back to Redis
```

The Go web server has **no LLM client, no tool definitions, no planning logic**. It only:
- Writes incoming user requests to a Redis inbox list (via `tasklib`)
- Reads thread/task/worker state from Redis for display (via `tasklib`)
- Serves REST API + HTMX UI

### Request forwarding

When a user submits a request via the web UI:

1. Web UI writes the request to `requests:inbox` (a Redis list) via `tasklib`
2. An `inbox-reader` sidecar in the master container does `BLPOP` on that list, then writes the request text to the master's stdin (the Claude Code session)
3. The master agent processes the request exactly as if the user typed it — plans, delegates via Go `task` CLI to workers, aggregates results
4. All state (thread messages, task statuses, results) is written to Redis by the master and workers — same Redis keys as today
5. The web UI polls Redis (via its own REST API, backed by `tasklib`) to show live progress to the browser user

This preserves the master agent as the single source of truth for planning. The web UI has zero agency — it cannot create tasks, assign workers, or make decisions.

### What the web UI does NOT do

- Does NOT call the LLM
- Does NOT define tools for the master agent
- Does NOT enqueue tasks, wait on tasks, or cancel tasks directly
- Does NOT update thread state
- Does NOT make planning decisions

## Tech Stack

| Layer | Choice |
|-------|--------|
| Language | Go 1.22+ |
| HTTP router | `chi` (go-chi/chi/v5) |
| Redis client | `go-redis/v9` |
| CLI framework | `cobra` (for `cmd/task`) |
| Testing | `miniredis` for tasklib unit tests |
| Templates | `html/template` (stdlib) |
| Frontend | HTMX via CDN, no JS build step |
| CSS | Minimal hand-written, no framework |

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
func (c *Client) CleanupThread(threadID, workspaceDir string) error
func (c *Client) UnlockThread(threadID string) error

// Workers (read-only — used by cmd/webui and cmd/task for status checks)
func (c *Client) GetWorkerStats() (map[string]*WorkerInfo, error)
func (c *Client) GetWorkerInfo(workerType string) (*WorkerInfo, error)

// Inbox (write — used by cmd/webui)
func (c *Client) PushRequest(threadID, repo, request string) error
```

### Key design rules

- **Byte-for-byte JSON compatibility** with current Python serialization — workers and master must see identical payloads during the transition
- **Same Redis key names** as today (`task:<id>:status`, `thread:<id>:messages`, `tasks:queue:<worker>`, etc.)
- **Tested with `miniredis`** — no real Redis needed in unit tests; full coverage of all CRUD operations including edge cases (NX locks, BLPOP/BLMOVE, TTL expiry)
- **No dependency on `cmd/` packages** — `tasklib` is pure library code

## `cmd/task` — Go CLI (replaces `scripts/task.py`)

Drop-in replacement for the Python CLI. The master agent invokes it with the same arguments:

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

Implementation: `cobra` commands that call `tasklib.Client` methods. ~200 lines of CLI glue. The binary is statically compiled (`CGO_ENABLED=0`) and copied into the master container at `/usr/local/bin/task`.

## `cmd/worker` — Go worker loop (replaces `scripts/worker.py`)

Long-running process that:

1. Connects to Redis via `tasklib`
2. Loops: `BLMOVE tasks:queue:<worker> tasks:processing:<worker>` with timeout
3. Reads thread history and state via `tasklib`
4. Builds a prompt from task instruction + thread context
5. Executes `AGENT_CMD` (from env) as a subprocess with the prompt on stdin
6. Writes result + exit code back via `tasklib` (`task:<id>:result`, `task:<id>:status`)
7. Appends result to thread history via `tasklib`

The worker binary takes a single argument: the worker type (`claude`, `copilot`, or `opencode`). It reads `AGENT_CMD` from the environment (set in the Dockerfile, same as today).

Replaces `scripts/worker.py` (~300 lines of Python → ~300 lines of Go). Statically compiled and copied into each worker Docker image at `/usr/local/bin/worker`.

## `cmd/webui` — HTTP server + HTMX frontend

Entry point for the web UI. Detailed in the sections below.

## Requirements

### 1. Dashboard (Home Page)

**Route:** `GET /`

A single-page overview showing:

| Section | Content |
|---------|---------|
| **Worker status** | 3 cards (Claude, Copilot, OpenCode) showing: online/offline, queue depth, active task count. Polls every 10s. |
| **Active threads** | Table: thread ID, status badge, repo, last updated, task count. Click to drill in. Polls every 5s. |
| **Recent tasks** | Table: task ID, worker type, thread, status, elapsed time. Polls every 5s. |
| **New request** | Form: thread ID (optional), repo (optional), request textarea. Submits to `POST /api/requests`. |

### 2. Thread Detail View

**Route:** `GET /threads/{thread_id}`

| Section | Content |
|---------|---------|
| **State panel** | Current status, repo, PR number, last design, last updated. Read-only in phase 1 (state is managed by the master agent). Polls every 3s when thread is active. |
| **Message history** | Chat-like scrollable timeline of all messages (user → master → worker → result). Color-coded by role. Auto-scrolls to bottom. Polls every 3s when active. |
| **Task list** | All tasks for this thread with status icons, click to expand result |

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
| `POST` | `/api/requests` | Submit a request to the master agent. Body: `{thread_id?, repo?, request}`. Calls `tasklib.PushRequest` to write to `requests:inbox`, appends to thread history. Returns `{thread_id, status: "submitted"}`. |

#### Tasks (read-only)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/tasks` | List tasks via `tasklib.ListTasks`. Query params: `worker`, `status`, `thread_id`, `limit`, `offset`. |
| `GET` | `/api/tasks/{task_id}` | Get task detail via `tasklib.GetTask`. |
| `GET` | `/api/tasks/{task_id}/result` | Get just the result text via `tasklib.GetTaskResult` (supports `?tail=N`). |

#### Threads (read-only for state, but web UI can create threads)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/threads` | Create a new thread via `tasklib.CreateThread`. Body: `{thread_id, repo?}`. |
| `GET` | `/api/threads` | List all threads via `tasklib.ListThreads`. |
| `GET` | `/api/threads/{thread_id}` | Get thread state + recent messages via `tasklib.GetThread`. |
| `GET` | `/api/threads/{thread_id}/history` | Get full message history via `tasklib.GetThreadHistory` (supports `?tail=N`). |

#### Workers (read-only)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/workers` | List all workers via `tasklib.GetWorkerStats`. |
| `GET` | `/api/workers/{worker_type}` | Detail for one worker type via `tasklib.GetWorkerInfo`. |

#### System

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/health` | Health check — Redis connectivity, worker counts. |
| `GET` | `/api/stats` | Aggregate stats: total tasks, success rate, avg task duration, queue depths. |

### 5. Real-Time Updates

HTMX polling with configurable intervals:
- Dashboard task list: 5s
- Thread detail: 3s (when thread is active)
- Worker status cards: 10s

Each poll hits the existing REST endpoint with `HX-Request` header, returning only the HTML partial for that section. No WebSocket needed.

### 6. Inbox Reader Sidecar

The master container needs a lightweight sidecar that bridges the Redis inbox to Claude Code's stdin.

```
master container:
  ├─ inbox-reader (Go binary, built from cmd/inbox-reader or a --sidecar flag on cmd/webui)
  │   └─ BLPOP requests:inbox → write request text to claude stdin
  └─ claude (interactive session, ENTRYPOINT)
```

The sidecar uses `tasklib` to `BLPOP` the inbox list, then writes the request text to the Claude process's stdin. Implemented as a small Go binary (~50 lines) or a mode on the webui binary.

### 7. Authentication

**Phase 1:** No auth — assume the webui runs within a trusted network (same as the rest of the Docker setup).

**Phase 2 (future):** Optional `WEBUI_API_KEY` env var — if set, all `/api/` mutation endpoints (`POST /api/requests`, `POST /api/threads`) require `Authorization: Bearer <key>`. Read endpoints and the UI are public.

### 8. Docker Integration

Updated images (Go binaries replace Python scripts):

| Image | Change |
|-------|--------|
| `master-agent` | Replace `scripts/task.py` with Go `task` binary. Add `inbox-reader` binary. |
| `worker-claude` | Replace `scripts/worker.py` with Go `worker` binary. New `ENTRYPOINT`. |
| `worker-copilot` | (inherits worker changes from base) |
| `worker-opencode` | (inherits worker changes from base) |
| `webui` (new) | Go `webui` binary, templates, static CSS. |

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
  ports:
    - "${WEBUI_PORT:-8000}:8000"
  restart: unless-stopped
```

`master` service is unchanged except the Docker image now contains Go binaries instead of Python scripts.

Dockerfiles — the Go binaries are built once in a multi-stage build and copied into the appropriate images:

- `docker/master-agent/Dockerfile` — copies `task` + `inbox-reader` binaries, removes `scripts/task.py`
- `docker/worker-claude/Dockerfile` — copies `worker` binary, `ENTRYPOINT ["worker", "claude"]`, removes `scripts/worker.py`
- `docker/webui/Dockerfile` — copies `webui` binary + `internal/templates/` + `static/`

### 9. File Structure

```
go.mod                          # Single Go module for the whole repo
go.sum

tasklib/                        # Shared library — all Redis CRUD
  client.go                     # Redis connection, key name helpers, TTL constants
  tasks.go                      # Task CRUD
  threads.go                    # Thread CRUD
  workers.go                    # Worker stats
  inbox.go                      # Write to requests:inbox
  tasks_test.go                 # Unit tests with miniredis
  threads_test.go

cmd/
  task/
    main.go                     # CLI (cobra) — drop-in replacement for task.py
  worker/
    main.go                     # Worker loop — replacement for worker.py
  inbox-reader/
    main.go                     # BLPOP requests:inbox → stdout (plumbed to claude stdin)
  webui/
    main.go                     # HTTP server entry point
    internal/
      api/
        router.go               # Chi router, HTMX detection middleware, error recovery
        requests.go             # POST /api/requests (tasklib.PushRequest)
        tasks.go                # GET /api/tasks handlers (read-only)
        threads.go              # GET/POST /api/threads handlers
        workers.go              # GET /api/workers handlers
        system.go               # GET /api/health, /api/stats handlers
      templates/
        base.html               # Layout shell (nav, HTMX CDN, polling)
        dashboard.html          # Dashboard page
        threads/
          list.html             # Thread list
          detail.html           # Thread detail
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
      style.css                 # Minimal CSS

docker/
  master-agent/
    Dockerfile                  # UPDATED: Go task binary instead of task.py
  worker-claude/
    Dockerfile                  # UPDATED: Go worker binary instead of worker.py
  webui/
    Dockerfile                  # NEW

scripts/                        # REMOVED after Go equivalents validated
  task.py                       # → replaced by cmd/task
  worker.py                     # → replaced by cmd/worker

docker-compose.yml              # MODIFIED: add webui service
```

### 10. Redis Data Model

The Go `tasklib` reads and writes the exact same Redis keys used by `task.py` and `worker.py` today. No migration needed.

One new key is added:

| Key | Type | Purpose |
|-----|------|---------|
| `requests:inbox` | List | Pending web UI requests. The inbox-reader does `BLPOP` on this list. Each value is JSON: `{thread_id, repo, request, submitted_at}`. |

All other keys are unchanged.

## Implementation Plan

### Step 1: Go module scaffold + `tasklib` package

Create `go.mod` at repo root. Dependencies: `go-redis/v9`, `chi/v5`, `cobra`, `miniredis`. Implement `tasklib/` with full CRUD for tasks, threads, workers, and inbox. Cover all operations with `miniredis` unit tests. The tests must pass against the same key names and JSON shapes the Python code produces today.

This is the highest-leverage step — `tasklib` correctness determines correctness of all three binaries.

### Step 2: Go `task` CLI + `worker` binary

Build `cmd/task/main.go` — a `cobra` CLI with the same subcommands and flags as `task.py`. Build `cmd/worker/main.go` — the worker loop. Test side-by-side with the existing Python code against a shared Redis instance. Once validated, update Dockerfiles to use Go binaries and remove `scripts/task.py` and `scripts/worker.py`.

### Step 3: Inbox-reader sidecar

Build `cmd/inbox-reader/main.go` — `BLPOP` on `requests:inbox`, write request text to stdout. Update `docker/master-agent/Dockerfile` to include the binary and plumb its stdout to claude's stdin. Update `docker/master-agent/CLAUDE.md` to tell the master that requests may arrive from the web UI.

### Step 4: Web UI REST API handlers

Build `cmd/webui/internal/api/` — chi router, HTMX detection middleware, all `/api/` endpoints backed by `tasklib`. Each handler checks `HX-Request` header to return either an HTML partial or JSON.

### Step 5: HTML templates and CSS

Build all Go `html/template` files under `cmd/webui/internal/templates/`. Minimal CSS — clean, readable styles, no framework. HTMX attributes for polling (`hx-trigger="every 5s"`) and form submissions.

### Step 6: Dockerize

Multi-stage Dockerfiles for all images. Add `webui` service to `docker-compose.yml`. Update CI (`.github/workflows/build-images.yml`) to build the Go binaries and all images.

### Step 7: End-to-end test

Spin up the full stack. Submit a request via the web UI → verify inbox-reader delivers to master → master plans and delegates via Go `task` CLI → Go workers consume, execute, return results → thread detail page shows real-time progress via HTMX polling. Test REST API with `curl`.

## Verification

1. `go test ./tasklib/...` passes all unit tests with `miniredis`
2. `docker compose up -d` starts all services including webui
3. `curl http://localhost:8000/api/health` returns `{"redis":"ok","workers":{...}}`
4. `curl -X POST http://localhost:8000/api/requests -H 'Content-Type: application/json' -d '{"thread_id":"test","request":"Say hello"}'` writes to the Redis inbox; returns `{"thread_id":"test","status":"submitted"}`
5. The master agent picks up the request via the inbox-reader, processes it using the Go `task` CLI, and results flow back through Redis
6. Browser at `http://localhost:8000` shows dashboard with worker cards, active threads
7. Thread detail page at `/threads/test` shows message history updating in real time via HTMX polling, with master planning messages and worker responses
8. `curl http://localhost:8000/api/tasks` returns JSON matching the UI display
9. The Go `task` CLI produces identical output to `task.py` for the same Redis state: `task status --id <id>`, `task list`, `task thread-history --id <id>`
