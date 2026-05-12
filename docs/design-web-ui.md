# Web UI for Master Agent — Design & Implementation Plan

## Context

The ai-agents system currently has no UI — everything is CLI-only via `task.py`. A master agent plans tasks, delegates to workers via Redis, and aggregates results. To make the system usable day-to-day (and demoable), we need a web console that exposes the same operations via REST API and provides an HTMX-powered browser UI. The REST API also makes the system scriptable from external tools.

## Architecture

Add a new **`webui`** service to `docker-compose.yml`:

```
webui (FROM ai-base, + FastAPI + Jinja2 + HTMX)
  └─ connects to Redis (read/write)
  └─ serves REST API on :8000
  └─ serves HTMX frontend on :8000
```

The FastAPI app reuses `task.py`'s Redis logic (refactored into a shared `tasklib.py` so both the CLI and the web server can import it). The UI is server-rendered HTML with HTMX for partial updates — no JavaScript build step.

All `/api/` endpoints are proper REST (JSON in/out) and can be called by scripts, CI, or external tools. The same endpoints power the HTMX UI when the `HX-Request` header is present.

## Requirements

### 1. Dashboard (Home Page)

**Route:** `GET /`

A single-page overview showing:

| Section | Content |
|---------|---------|
| **Worker status** | 3 cards (Claude, Copilot, OpenCode) showing: online/offline, queue depth, active task count, last heartbeat |
| **Active threads** | Table: thread ID, status badge, repo, last updated, task count. Click to drill in. |
| **Recent tasks** | Table: task ID, worker type, thread, status, elapsed time, result snippet. Auto-refreshes every 5s via HTMX polling. |
| **Quick enqueue** | Inline form: instruction textarea, worker dropdown, thread ID. Enqueues via POST to `/api/tasks`. |

### 2. Thread Detail View

**Route:** `GET /threads/{thread_id}`

| Section | Content |
|---------|---------|
| **State panel** | Current status, repo, PR number, last design, last updated — all inline-editable via HTMX PUT |
| **Message history** | Chat-like scrollable timeline of all messages (master → worker → result). Color-coded by role. Auto-scrolls to bottom. |
| **Task list** | All tasks for this thread with status icons, click to expand result |
| **Actions** | Enqueue new task (pre-filled thread), update status, cleanup workspace |

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

Each poll hits the existing REST endpoint with `HX-Request` header, returning only the HTML partial for that section. No WebSocket needed — polling via HTMX is adequate for this use case and keeps the architecture simple.

### 6. Authentication

**Phase 1:** No auth — assume the webui runs on localhost or within a trusted network (same as the rest of the Docker setup).

**Phase 2 (future):** Optional `WEBUI_API_KEY` env var — if set, all `/api/` mutation endpoints require `Authorization: Bearer <key>`. Read endpoints and the UI are public.

### 7. Docker Integration

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
    - TASK_TIMEOUT=1800
    - WORKSPACE_DIR=/workspace
    - WEBUI_PORT=8000
  ports:
    - "${WEBUI_PORT:-8000}:8000"
  volumes:
    - workspace:/workspace
  restart: unless-stopped
```

New Dockerfile at `docker/webui/Dockerfile`:
- `FROM ai-base` (already has Python 3, redis-py, git, gh, jq, curl)
- Install FastAPI, uvicorn, Jinja2, python-multipart
- Copy shared `tasklib.py` + webui application code
- `ENTRYPOINT ["python3", "-m", "uvicorn", "webui.main:app", "--host", "0.0.0.0", "--port", "8000"]`

### 8. Code Refactoring Required

`scripts/task.py` currently has all Redis logic inline (497 lines). This must be extracted into `scripts/tasklib.py` as a `TaskQueue` class so both the CLI and the web API can use it:

```python
class TaskQueue:
    def __init__(self, redis_client): ...
    def enqueue(self, worker, thread_id, instruction) -> dict: ...
    def status(self, task_id) -> dict: ...
    def result(self, task_id, tail=None) -> str: ...
    def list_tasks(self, worker=None, status=None, thread_id=None, limit=50) -> list: ...
    def wait(self, task_id, timeout=300) -> dict: ...
    def cancel(self, task_id) -> bool: ...
    def requeue_stale(self, worker=None, older_than=600) -> list: ...
    def unlock_thread(self, thread_id) -> bool: ...
    def thread_create(self, thread_id, repo=None) -> dict: ...
    def thread_history(self, thread_id, tail=None) -> list: ...
    def thread_state(self, thread_id) -> dict: ...
    def thread_update(self, thread_id, **kwargs) -> dict: ...
    def thread_list(self) -> list: ...
    def thread_cleanup(self, thread_id) -> bool: ...
    def worker_stats(self) -> dict: ...
```

`task.py` becomes a thin argparse wrapper around `TaskQueue`.

### 9. Frontend Pages & Templates

| Template | Purpose |
|----------|---------|
| `templates/base.html` | Layout shell (header, nav, main content area). Includes HTMX from CDN. |
| `templates/dashboard.html` | Dashboard page with polling sections |
| `templates/threads/list.html` | Thread list |
| `templates/threads/detail.html` | Full thread detail page |
| `templates/threads/_state.html` | HTMX partial: thread state panel |
| `templates/threads/_history.html` | HTMX partial: message timeline |
| `templates/tasks/list.html` | Task list with filters |
| `templates/tasks/detail.html` | Single task detail |
| `templates/tasks/_table.html` | HTMX partial: task table rows |
| `templates/workers/_cards.html` | HTMX partial: worker status cards |
| `templates/_quick_enqueue.html` | HTMX partial: enqueue form + result |

### 10. File Structure

```
scripts/
  tasklib.py              # NEW: shared Redis logic (extracted from task.py)
  task.py                 # MODIFIED: thin CLI wrapper around tasklib
  worker.py               # unchanged
  test_tasklib.py         # NEW: unit tests for tasklib
  test_task.py            # MODIFIED: updated for refactored CLI
  test_worker.py          # unchanged

docker/webui/
  Dockerfile              # NEW
  webui/
    __init__.py
    main.py               # FastAPI app, routes, lifespan
    templates/            # Jinja2 templates (HTMX)
    static/               # minimal CSS

docker-compose.yml        # MODIFIED: add webui service
```

## Implementation Plan

### Step 1: Extract `tasklib.py` from `task.py`

Refactor `scripts/task.py` — move all Redis operations into a `TaskQueue` class in `scripts/tasklib.py`. The CLI becomes a thin argparse wrapper. Update existing tests. This is the bulk of the work and must be done first.

### Step 2: Build FastAPI app

Create `docker/webui/webui/main.py` with:
- Redis connection via lifespan
- All REST endpoints delegating to `TaskQueue`
- Jinja2 template rendering for HTMX routes
- HTMX partial detection via `HX-Request` header

### Step 3: Create templates and static assets

Build all Jinja2 templates. Minimal CSS (no framework — just clean, readable styles). HTMX attributes for polling and inline edits.

### Step 4: Dockerize

Create `docker/webui/Dockerfile`. Add `webui` service to `docker-compose.yml`.

### Step 5: End-to-end test

Spin up the full stack, enqueue tasks via the UI, verify results appear, test REST API with `curl`, verify HTMX polling updates.

## Verification

1. `docker compose up -d` starts all services including webui
2. `curl http://localhost:8000/api/health` returns `{"redis": "ok", "workers": {...}}`
3. `curl -X POST http://localhost:8000/api/tasks -H 'Content-Type: application/json' -d '{"worker":"claude","thread_id":"test","instruction":"say hello"}'` enqueues a task
4. Browser at `http://localhost:8000` shows dashboard with worker cards, active threads, and the enqueued task
5. Thread detail page shows message history updating in real time via HTMX polling
6. `curl http://localhost:8000/api/tasks` returns JSON matching the UI display
