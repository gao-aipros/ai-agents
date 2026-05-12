# Web UI for Master Agent — Design & Implementation Plan

## Context

The ai-agents system uses a master agent (Claude Code with orchestrator instructions) to plan complex tasks, delegate sub-tasks to worker agents via a Redis task queue, and aggregate results. Currently everything is CLI-only: the user types requests directly into the master container's interactive Claude Code session. The master agent uses `task.py` as a skill (a tool it can invoke) to interact with Redis — enqueuing tasks, checking statuses, reading results, and managing threads.

We need a web console that:
1. Forwards user requests to the **existing master agent** (the running Claude Code session) — the master remains the sole planner/orchestrator
2. Provides **monitoring** of threads, workers, and tasks by reading Redis state
3. Is implemented in **Go** with an HTMX frontend

The web UI is an **addon**, not a replacement. The master agent still does all planning, tool calling, and delegation. The web UI just gives it a browser-based front door and a real-time dashboard.

## Architecture

```
master (Claude Code session)          webui (Go binary, :8000)
  ├─ plans & delegates                  ├─ writes requests to Redis inbox
  ├─ invokes task.py skill against      ├─ reads Redis for monitoring
  │   Redis (enqueue, wait, result)     ├─ serves REST API (JSON)
  └─ receives requests via:             └─ serves HTMX frontend
      1. interactive CLI (stdin)
      2. Redis inbox (from web UI)
       └─ inbox-reader sidecar
```

The Go web server has **no LLM client, no tool definitions, no planning logic**. It only:
- Writes incoming user requests to a Redis inbox list
- Reads thread/task/worker state from Redis for display
- Serves REST API + HTMX UI

### Request forwarding

When a user submits a request via the web UI:

1. Web UI writes the request to `requests:inbox` (a Redis list)
2. An `inbox-reader` sidecar in the master container does `BLPOP` on that list, then writes the request text to the master's stdin (the Claude Code session)
3. The master agent processes the request exactly as if the user typed it — plans, delegates via `task.py` to workers, aggregates results
4. All state (thread messages, task statuses, results) is written to Redis by the master and workers — same Redis keys as today
5. The web UI polls Redis (via its own REST API) to show live progress to the browser user

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
| Templates | `html/template` (stdlib) |
| Frontend | HTMX via CDN, no JS build step |
| CSS | Minimal hand-written, no framework |

No `anthropic-sdk-go` — the Go server doesn't talk to any LLM.

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
| `POST` | `/api/requests` | Submit a request to the master agent. Body: `{thread_id?, repo?, request}`. Writes to `requests:inbox` list in Redis, appends to thread history. Returns `{thread_id, status: "submitted"}`. |

#### Tasks (read-only)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/tasks` | List tasks. Query params: `worker`, `status`, `thread_id`, `limit`, `offset`. |
| `GET` | `/api/tasks/{task_id}` | Get task detail including full result. |
| `GET` | `/api/tasks/{task_id}/result` | Get just the result text (supports `?tail=N`). |

#### Threads (read-only for state, but web UI can create threads)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/threads` | Create a new thread. Body: `{thread_id, repo?}`. |
| `GET` | `/api/threads` | List all threads with status summaries. |
| `GET` | `/api/threads/{thread_id}` | Get thread state + recent messages. |
| `GET` | `/api/threads/{thread_id}/history` | Get full message history (supports `?tail=N`). |

#### Workers (read-only)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/workers` | List all workers with queue depth, active tasks, health. |
| `GET` | `/api/workers/{worker_type}` | Detail for one worker type. |

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
  ├─ inbox-reader (shell script or tiny Go binary)  ← NEW
  │   └─ BLPOP requests:inbox → write to claude stdin
  └─ claude (interactive session, ENTRYPOINT)
```

The sidecar runs a loop:

```bash
while true; do
  redis-cli BLPOP requests:inbox 0 | jq -r '.request' > /proc/1/fd/0
done
```

Or in Go: a small goroutine in the webui binary (if run as a sidecar mode) that does `BLPOP` on the inbox and pipes results to stdout, which is connected to Claude's stdin.

The exact mechanism depends on how stdin is plumbed in the container. The Dockerfile will coordinate: run the sidecar with its stdout connected to claude's stdin (e.g., via a pipe or named pipe).

### 7. Authentication

**Phase 1:** No auth — assume the webui runs within a trusted network (same as the rest of the Docker setup).

**Phase 2 (future):** Optional `WEBUI_API_KEY` env var — if set, all `/api/` mutation endpoints (`POST /api/requests`, `POST /api/threads`) require `Authorization: Bearer <key>`. Read endpoints and the UI are public.

### 8. Docker Integration

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

Modified `master` service — adds inbox-reader sidecar:

```yaml
master:
  image: ${MASTER_AGENT_IMAGE:-master-agent:latest}
  stdin_open: true
  tty: true
  restart: unless-stopped
  volumes:
    - workspace:/workspace
  environment:
    REDIS_HOST: redis
    REDIS_PORT: "6379"
    TASK_TIMEOUT: "1800"
    ANTHROPIC_AUTH_TOKEN: ${ANTHROPIC_AUTH_TOKEN}
    GH_TOKEN: ${GH_TOKEN}
    GITHUB_TOKEN: ${GITHUB_TOKEN}
  depends_on:
    redis:
      condition: service_healthy
```

New Dockerfile at `docker/webui/Dockerfile`:
- Multi-stage: `golang:latest` builds a static binary (`CGO_ENABLED=0`), then copied into `ai-base`
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
      client.go                 # Redis connection, key name helpers
      tasks.go                  # Task reads (status, result, list)
      threads.go                # Thread reads + create (state, history, list)
      workers.go                # Worker stats reads
      inbox.go                  # Write to requests:inbox
      tasks_test.go             # Unit tests with miniredis
      threads_test.go
    api/
      router.go                 # Chi router, middleware (HTMX detection, error recovery)
      requests.go               # POST /api/requests (write to inbox)
      tasks.go                  # GET /api/tasks handlers (read-only)
      threads.go                # GET/POST /api/threads handlers
      workers.go                # GET /api/workers handlers
      system.go                 # GET /api/health, /api/stats handlers
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
      requests/
        _form.html              # HTMX partial: request form + submission response
  static/
    style.css                   # Minimal CSS

docker/
  webui/
    Dockerfile

docker-compose.yml              # MODIFIED: add webui service
```

### 10. Redis Data Model

The Go server reads the exact same Redis keys used by `task.py` and `worker.py` today. No migration needed.

One new key is added:

| Key | Type | Purpose |
|-----|------|---------|
| `requests:inbox` | List | Pending web UI requests. The inbox-reader sidecar does `BLPOP` on this list. Each value is JSON: `{thread_id, repo, request, submitted_at}`. |

All other keys are unchanged and read-only from the web UI's perspective.

## Implementation Plan

### Step 1: Go project scaffold + Redis read layer

Create `webui/` with `go.mod`, dependencies (`chi`, `go-redis`, `miniredis`). Implement `internal/redis/` package with read operations for tasks, threads, and workers, plus write operations for the inbox and thread creation. Write unit tests with `miniredis`.

### Step 2: REST API handlers

Build `internal/api/` package with chi router, HTMX detection middleware, and all `/api/` endpoints. Each handler checks `HX-Request` header to return either an HTML partial or JSON. The `POST /api/requests` handler writes to the Redis inbox.

### Step 3: HTML templates and CSS

Build all Go `html/template` files. Minimal CSS — clean, readable styles, no framework. HTMX attributes for polling (`hx-trigger="every 5s"`) on dashboard and thread detail sections.

### Step 4: Inbox-reader sidecar + master CLAUDE.md update

Add inbox-reader to the master container — either a small shell script or a `--sidecar` mode in the webui binary. Update `docker/master-agent/CLAUDE.md` to tell the master agent that requests may arrive from the web UI (via the inbox) in addition to interactive CLI. The processing flow is identical regardless of how the request arrives.

### Step 5: Dockerize

Multi-stage Dockerfile for webui. Add `webui` service to `docker-compose.yml`. Update master container entrypoint to run both the inbox-reader and claude (via a wrapper script or supervisor). Update CI (`.github/workflows/build-images.yml`) to build and push `webui` image.

### Step 6: End-to-end test

Spin up the full stack, submit a request via the web UI, verify the inbox-reader delivers it to the master agent, the master plans and delegates to workers, results appear in the thread detail page. Test REST API with `curl`. Verify HTMX polling updates.

## Verification

1. `docker compose up -d` starts all services including webui
2. `curl http://localhost:8000/api/health` returns `{"redis":"ok","workers":{...}}`
3. `curl -X POST http://localhost:8000/api/requests -H 'Content-Type: application/json' -d '{"thread_id":"test","request":"Say hello"}'` writes to the Redis inbox; returns `{"thread_id":"test","status":"submitted"}`
4. The master agent picks up the request via the inbox-reader, processes it, and results flow back through Redis
5. Browser at `http://localhost:8000` shows dashboard with worker cards, active threads
6. Thread detail page at `/threads/test` shows message history updating in real time via HTMX polling, with master planning messages and worker responses
7. `curl http://localhost:8000/api/tasks` returns JSON matching the UI display
