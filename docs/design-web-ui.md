# Web UI for Master Agent — Design & Implementation Plan

## Context

The ai-agents system uses a master agent (Claude Code with orchestrator instructions) to plan complex tasks, delegate sub-tasks to worker agents via a Redis task queue, and aggregate results. Currently everything is CLI-only: `task.py` for task/thread management and an interactive `master` container for orchestration.

We need a web console that:
1. Is the primary UI for the **master agent** — users submit requests, the master plans and delegates, results appear in the UI
2. Provides **management/monitoring** of threads, workers, and tasks
3. Is implemented in **Go** with an HTMX frontend

## Architecture

Add a new **`webui`** service to `docker-compose.yml`. The Go web server embeds the master agent (LLM client + tool use), talks directly to Redis, and serves both a REST API and HTMX-powered browser UI on port 8000.

```
webui (FROM ai-base, Go binary)
  ├─ connects to Redis (read/write)
  ├─ calls DeepSeek API (Anthropic-compatible) for master agent planning
  ├─ serves REST API on :8000
  └─ serves HTMX frontend on :8000
```

The existing `master` container becomes optional — the Go server **is** the master agent interface. If the `master` container is kept, it serves as a CLI fallback.

### How the master agent works in Go

The Go server loads the master orchestrator system prompt (adapted from `docker/master-agent/CLAUDE.md`). When a user submits a request:

1. A thread is created (or reused), status set to `planning`
2. A goroutine runs the agent loop:
   - Send system prompt + thread history + user request to DeepSeek LLM (Anthropic Messages API with tool use)
   - LLM responds with tool calls (`enqueue_task`, `wait_task`, `get_result`, etc.) or a final text response
   - Tool calls execute against Redis — workers pick up tasks, results flow back
   - Each tool call and its result are appended to thread history, visible in the UI via HTMX polling
   - When the LLM produces a final response, thread status is updated
3. The UI shows live progress by polling thread state and message history

All `/api/` endpoints are proper REST (JSON in/out) and can be called by scripts, CI, or external tools. The same endpoints power the HTMX UI when the `HX-Request` header is present.

## Tech Stack

| Layer | Choice |
|-------|--------|
| Language | Go 1.22+ |
| HTTP router | `chi` (go-chi/chi/v5) |
| Redis client | `go-redis/v9` |
| LLM client | `anthropic-sdk-go` (configured for DeepSeek base URL `https://api.deepseek.com/anthropic`) |
| Templates | `html/template` (stdlib) |
| Frontend | HTMX via CDN, no JS build step |
| CSS | Minimal hand-written, no framework |

## Requirements

### 1. Dashboard (Home Page)

**Route:** `GET /`

A single-page overview showing:

| Section | Content |
|---------|---------|
| **Worker status** | 3 cards (Claude, Copilot, OpenCode) showing: online/offline, queue depth, active task count. Polls every 10s. |
| **Active threads** | Table: thread ID, status badge, repo, last updated, task count. Click to drill in. Polls every 5s. |
| **Recent tasks** | Table: task ID, worker type, thread, status, elapsed time. Polls every 5s. |
| **New request** | Form: thread ID, optional repo, request textarea. Submits to `POST /api/master/request`. |

### 2. Thread Detail View

**Route:** `GET /threads/{thread_id}`

| Section | Content |
|---------|---------|
| **State panel** | Current status, repo, PR number, last design, last updated — inline-editable via HTMX PUT. Polls every 3s when thread is active. |
| **Message history** | Chat-like scrollable timeline of all messages (user → master → worker → result). Color-coded by role. Auto-scrolls to bottom. Polls every 3s when active. |
| **Task list** | All tasks for this thread with status icons, click to expand result |
| **Actions** | Submit follow-up request (pre-filled thread), update status, cleanup workspace, unlock |

### 3. Task Management

**Route:** `GET /tasks`

| Section | Content |
|---------|---------|
| **Filters** | By worker type, status, thread — via query params |
| **Task table** | ID, worker, thread, status, created, completed. Paginated. |
| **Task detail** (`GET /tasks/{task_id}`) | Full payload: instruction, full result (with tail toggle), exit code, timestamps, worker |
| **Actions** | Cancel, re-queue stale |

### 4. REST API Endpoints

All endpoints return JSON. Every endpoint also serves HTMX partials when the `HX-Request` header is present.

#### Tasks

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/tasks` | Enqueue a new task. Body: `{worker, thread_id, instruction}`. Returns task object. |
| `GET` | `/api/tasks` | List tasks. Query params: `worker`, `status`, `thread_id`, `limit`, `offset`. |
| `GET` | `/api/tasks/{task_id}` | Get task detail including full result. |
| `GET` | `/api/tasks/{task_id}/result` | Get just the result text (supports `?tail=N`). |
| `POST` | `/api/tasks/{task_id}/cancel` | Cancel a running task. |
| `POST` | `/api/tasks/requeue-stale` | Requeue stale tasks. Body: `{worker?, older_than?}`. |

#### Threads

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/threads` | Create a new thread. Body: `{thread_id, repo?}`. |
| `GET` | `/api/threads` | List all threads with status summaries. |
| `GET` | `/api/threads/{thread_id}` | Get thread state + recent messages. |
| `GET` | `/api/threads/{thread_id}/history` | Get full message history (supports `?tail=N`). |
| `PUT` | `/api/threads/{thread_id}` | Update thread state. Body: `{status?, design?, pr_number?}`. |
| `DELETE` | `/api/threads/{thread_id}` | Cleanup thread (delete workspace dir). |
| `POST` | `/api/threads/{thread_id}/unlock` | Force-release a stale thread lock. |

#### Workers

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/workers` | List all workers with queue depth, active tasks, health. |
| `GET` | `/api/workers/{worker_type}` | Detail for one worker type. |

#### Master Agent

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/master/request` | Submit a request to the master agent. Body: `{thread_id?, repo?, request}`. Returns `{thread_id, status: "planning"}`. The master agent runs asynchronously: it plans, delegates to workers, waits for results, and aggregates. Progress is visible via thread history. |
| `GET` | `/api/master/request/{thread_id}` | Get current planning status for a thread. Returns `{status, last_activity}`. |

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

### 6. Master Agent Tools

The Go server defines these tools for the LLM (adapted from `task.py` commands):

| Tool | Description | Maps to |
|------|-------------|---------|
| `enqueue_task` | Push a task onto a worker queue | `task.py enqueue` |
| `wait_task` | Block until a task completes | `task.py wait` |
| `get_task_result` | Get task output (supports tail) | `task.py result` |
| `get_task_status` | Get task status and metadata | `task.py status` |
| `list_tasks` | List/filter tasks | `task.py list` |
| `cancel_task` | Cancel a pending/running task | `task.py cancel` |
| `update_thread` | Update thread state | `task.py thread-update` |
| `get_thread_history` | Read thread message history | `task.py thread-history` |
| `get_worker_stats` | Get worker health and queue depths | (new) |

### 7. Authentication

**Phase 1:** No auth — assume the webui runs within a trusted network (same as the rest of the Docker setup).

**Phase 2 (future):** Optional `WEBUI_API_KEY` env var — if set, all `/api/` mutation endpoints require `Authorization: Bearer <key>`. Read endpoints and the UI are public.

### 8. Docker Integration

New service in `docker-compose.yml`:

```yaml
webui:
  image: ${WEBUI_IMAGE:-webui:latest}
  depends_on:
    redis:
      condition: service_healthy
  environment:
    - REDIS_HOST=redis
    - REDIS_PORT=6379
    - DEEPSEEK_API_KEY=${DEEPSEEK_API_KEY}
    - WEBUI_PORT=8000
  ports:
    - "${WEBUI_PORT:-8000}:8000"
  volumes:
    - workspace:/workspace
  restart: unless-stopped
```

New Dockerfile at `docker/webui/Dockerfile`:
- Multi-stage: `golang:latest` builds a static binary (`CGO_ENABLED=0`), then copied into `ai-base` (has git, gh, jq for workspace ops)
- Copies `internal/templates/` and `static/` for template rendering
- `ENTRYPOINT ["/usr/local/bin/webui"]`

### 9. File Structure

```
webui/
  go.mod
  go.sum
  main.go                       # Entry point: flags, Redis connect, router, server start
  internal/
    redis/
      client.go                 # Redis connection, key name helpers, TTL constants
      tasks.go                  # Task CRUD
      threads.go                # Thread CRUD
      workers.go                # Worker stats
      tasks_test.go             # Unit tests with miniredis
      threads_test.go
    master/
      agent.go                  # LLM agent loop (send → tool calls → iterate → final)
      tools.go                  # Tool definitions (JSON schema) + Go implementations
      system_prompt.go          # Master orchestrator system prompt
    api/
      router.go                 # Chi router, middleware (HTMX detection, error recovery)
      tasks.go                  # /api/tasks handlers
      threads.go                # /api/threads handlers
      workers.go                # /api/workers handlers
      system.go                 # /api/health, /api/stats handlers
      master.go                 # /api/master/request handlers
    templates/
      base.html                 # Layout shell (nav, HTMX CDN, polling)
      dashboard.html            # Dashboard page
      threads/
        list.html               # Thread list
        detail.html             # Thread detail
        _state.html             # HTMX partial: state panel
        _history.html           # HTMX partial: message timeline
      tasks/
        list.html               # Task list with filters
        detail.html             # Task detail
        _table.html             # HTMX partial: task table rows
      workers/
        _cards.html             # HTMX partial: worker status cards
      master/
        _request_form.html      # HTMX partial: new request form + response
  static/
    style.css                   # Minimal CSS

docker/
  webui/
    Dockerfile

docker-compose.yml              # MODIFIED: add webui service, master becomes optional
```

### 10. Redis Data Model (unchanged)

The Go server reads and writes the exact same Redis keys used by `task.py` and `worker.py` today. No migration needed. The existing Python CLI and workers continue to work alongside the web UI.

## Implementation Plan

### Step 1: Go project scaffold + Redis data layer

Create `webui/` with `go.mod`, dependencies (`chi`, `go-redis`, `anthropic-sdk-go`, `miniredis`). Implement `internal/redis/` package with all CRUD operations matching current `task.py` behavior. Write unit tests with `miniredis`.

### Step 2: REST API handlers

Build `internal/api/` package with chi router, HTMX detection middleware, and all `/api/` endpoints. Each handler checks `HX-Request` header to return either an HTML partial or JSON.

### Step 3: Master agent

Implement `internal/master/` — system prompt, 9 tool definitions + Go implementations, and the agent loop using `anthropic-sdk-go`. The loop sends a request to the LLM, executes tool calls against Redis, and iterates until the LLM produces a final response. All progress is written to Redis (thread messages, task state) so the UI reflects it in real time.

### Step 4: HTML templates and CSS

Build all Go `html/template` files. Minimal CSS — clean, readable styles, no framework. HTMX attributes for polling (`hx-trigger="every 5s"`) and inline edits (`hx-put`).

### Step 5: Dockerize

Multi-stage Dockerfile. Add `webui` service to `docker-compose.yml`. Update CI (`.github/workflows/build-images.yml`) to build and push `webui` image.

### Step 6: End-to-end test

Spin up the full stack, submit a request via the UI, verify the master agent plans and delegates to workers, results appear in the thread detail page. Test REST API with `curl`. Verify HTMX polling updates.

## Verification

1. `docker compose up -d` starts all services including webui
2. `curl http://localhost:8000/api/health` returns `{"redis":"ok","workers":{...}}`
3. `curl -X POST http://localhost:8000/api/master/request -H 'Content-Type: application/json' -d '{"thread_id":"test","request":"Say hello"}'` starts the master agent planning loop; returns `{"thread_id":"test","status":"planning"}`
4. Browser at `http://localhost:8000` shows dashboard with worker cards, active threads
5. Thread detail page at `/threads/test` shows message history updating in real time via HTMX polling, with master planning messages and worker responses
6. `curl http://localhost:8000/api/tasks` returns JSON matching the UI display
