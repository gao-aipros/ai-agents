# Copilot Instructions

## Architecture

Multi-agent Docker orchestration where a master agent (Claude Code) delegates tasks to worker agents (Claude Code, Copilot, OpenCode). All agents run in Docker containers, communicating via a Redis task queue (control plane) and a shared `/workspace` volume (data plane).

**Image dependency layers:**

```
ai-base (debian:trixie + gh, git, jq, python3, redis-py, curl, ssh)
  ├─ claude-code (FROM ai-base, + claude CLI)
  ├─ copilot     (FROM ai-base, + copilot CLI, + Go toolchain)
  └─ opencode    (FROM ai-base, + opencode CLI, + Go toolchain)
        │
        ├─ master-agent   (FROM claude-code, + task CLI + webui)
        └─ worker-claude  (FROM claude-code, + Go toolchain)
```

The master agent runs a web UI (chi router, HTMX + Go templates on port 8000) and delegates tasks via `task enqueue`. Workers dequeue tasks via `BLMOVE`, execute agent subprocesses with thread context, and post results back to Redis. All agents use DeepSeek as the backend (Anthropic-compatible API). Auth tokens are passed via environment variables.

**Dual backend:** The `task` CLI and worker binary have both Go and Python implementations. `TASKLIB_BACKEND` (default `go`) toggles between them via wrapper scripts (`task-wrapper.sh`, `worker-entrypoint.sh`). Both backends must be byte-for-byte compatible at the Redis level.

## Build, Test, and Lint

### Build

```bash
# Build ai-base first (others depend on it)
docker build --load -t ai-base:latest docker/base/

# Layer 1 — can run in parallel
docker build --load -t claude-code:latest docker/claude-code/ &
docker build --load -t copilot:latest -f docker/copilot/Dockerfile . &
docker build --load -t opencode:latest -f docker/opencode/Dockerfile . &
wait

# Layer 2 — depends on layer 1
docker build --load -t master-agent:latest -f docker/master-agent/Dockerfile .
docker build --load -t worker-claude:latest -f docker/worker-claude/Dockerfile .
```

CI (`.github/workflows/build-images.yml`) builds multi-arch (`linux/amd64,linux/arm64`) with `docker buildx build --push` and publishes to `ghcr.io/noodle05/<image>:latest`.

### Multi-arch builds (CI pattern)

```bash
docker buildx build --platform linux/amd64,linux/arm64 --push \
  -t ghcr.io/noodle05/ai-base:latest docker/base/

docker buildx build --platform linux/amd64,linux/arm64 --push \
  --build-arg BASE_IMAGE=ghcr.io/noodle05/ai-base:latest \
  -t ghcr.io/noodle05/claude-code:latest docker/claude-code/
```

### Test

```bash
# Run all Go tests (uses miniredis, no real Redis needed)
go test ./...

# Run a single package
go test ./tasklib/

# Run a single test
go test ./tasklib/ -run TestEnqueue

# Run Web UI API handler tests
go test ./cmd/webui/internal/api/

# Run compatibility tests (Go backend)
go test ./cmd/task/ -run TestCompat

# Run compatibility tests (Python backend, requires COMPAT_TEST_DB env var)
TASKLIB_BACKEND=python COMPAT_TEST_DB=1 python3 scripts/test_task.py
```

Tests use `miniredis` (in-memory Redis) — no real Redis instance is required. The `tasklib` package is the integration layer and has the most comprehensive tests. CLI tests in `cmd/task/` and `cmd/worker/` verify byte-for-byte compatibility with the Python backend.

### Lint

```bash
go vet ./...
```

## Key Conventions

### Dockerfile pattern

Multi-stage builds that parameterize `FROM` must declare the `ARG` **before** the first `FROM`:

```dockerfile
ARG BASE_IMAGE=ai-base:latest          # line 1 — before any FROM

FROM debian:trixie AS build             # build stage (uses public images)
...
FROM ${BASE_IMAGE}                      # final stage (parameterized)
...
```

Local builds use `--load` (no `--platform`). `ARG BASE_IMAGE` and `ARG CLAUDE_IMAGE` default to local tags.

### Go module

Module path: `github.com/noodle05/ai-agents`. Key dependencies: `go-redis/v9`, `go-chi/chi/v5`, `spf13/cobra`, `miniredis/v2` (test only).

### Redis key schema

All keys follow a strict pattern shared between Go and Python backends:
- `task:<taskID>:<field>` — per-task keys (status, worker, thread_id, result, etc.)
- `tasks:queue:<worker>` — task queue (list, LPUSH for enqueue)
- `tasks:processing:<worker>` — in-flight tasks (list, BLMOVE from queue)
- `thread:<threadID>:current_state` — thread state hash
- `thread:<threadID>:messages` — thread history list
- `thread:<threadID>:lock` — per-thread task serialization lock
- `thread:<threadID>:running` — per-thread request lock (web UI)
- `thread:<threadID>:session_id` — Claude session UUID
- `worker:<type>:<hostname>:heartbeat` — worker heartbeat keys (30s TTL)
- `active_tasks` — hash of currently running tasks

TTL constants: tasks 24h, threads 7 days, locks 35 minutes (timeout + margin).

### Web UI architecture

- `cmd/webui/` — chi router with page routes (`/`, `/threads`, `/tasks`) and REST API (`/api/`)
- `cmd/webui/internal/request/` — manages `claude -p` subprocess lifecycles with concurrency limits
- `cmd/webui/internal/templates/` — Go templates + HTMX for live updates
- `cmd/webui/internal/api/` — middleware (auth, CSRF, rate limiting) and resource handlers
- Static assets (CSS, HTMX) are embedded via `embed.go`

### Service environment

Rename `.env.example` to `.env` and fill in `GH_TOKEN`, `GITHUB_TOKEN`, and `DEEPSEEK_API_KEY`. Run with `docker compose up -d`. The web UI is at `http://localhost:8000`.
