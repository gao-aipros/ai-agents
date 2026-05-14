# Copilot Instructions

## Architecture

Multi-agent Docker orchestration where a master agent (Claude Code) delegates tasks to worker agents (Claude Code, Copilot, OpenCode). All agents run in Docker containers, communicating via a Redis task queue (control plane) and a shared `/workspace` volume (data plane).

**Image dependency layers:**

```
ai-base (debian:trixie + gh, git, jq, python3, redis-tools, curl, ssh)
  ├─ worker-base    (ai-base + Go SDK + gcc/make + worker-go)
  │   ├─ copilot     (worker-base + copilot CLI)
  │   ├─ opencode    (worker-base + opencode CLI)
  │   └─ worker-claude (worker-base + claude CLI)
  └─ master-agent   (ai-base + claude CLI + task-go + webui)
```

The master agent runs a web UI (chi router, HTMX + Go templates on port 8000) and delegates tasks via `task enqueue`. Workers dequeue tasks via `BLMOVE`, execute agent subprocesses with thread context, and post results back to Redis. All agents use DeepSeek as the backend (Anthropic-compatible API). Auth tokens are passed via environment variables. Each agent gets its own GitHub token for isolation and rate limiting.

## Build, Test, and Lint

### Build

```bash
# Pre-build Go binaries (required before any Docker build)
go mod download
GOOS=linux go build -o out/amd64/worker-go ./cmd/worker/
GOOS=linux go build -o out/amd64/task-go   ./cmd/task/
GOOS=linux go build -o out/amd64/webui     ./cmd/webui/

# Phase 1 — base image
docker build --load -t ai-base:latest docker/base/

# Phase 2 — worker-base and master-agent (parallel, both FROM ai-base)
docker build --load -t worker-base:latest -f docker/worker-base/Dockerfile . &
docker build --load -t master-agent:latest -f docker/master-agent/Dockerfile . &
wait

# Phase 3 — workers (parallel, all FROM worker-base)
docker build --load -t copilot:latest -f docker/copilot/Dockerfile . &
docker build --load -t opencode:latest -f docker/opencode/Dockerfile . &
docker build --load -t worker-claude:latest -f docker/worker-claude/Dockerfile . &
wait
```

CI (`.github/workflows/build-images.yml`) pre-builds Go binaries for both architectures, then builds 5 multi-arch images (`linux/amd64,linux/arm64`) in 3 parallel phases and pushes to `ghcr.io/gao-aipros/<image>:latest`.

### Multi-arch builds (CI pattern)

```bash
# Go binaries (cross-compiled in CI via actions/setup-go)
GOOS=linux GOARCH=amd64 go build -o out/amd64/worker-go ./cmd/worker/
GOOS=linux GOARCH=amd64 go build -o out/amd64/task-go   ./cmd/task/
GOOS=linux GOARCH=amd64 go build -o out/amd64/webui     ./cmd/webui/
GOOS=linux GOARCH=arm64 go build -o out/arm64/worker-go ./cmd/worker/
GOOS=linux GOARCH=arm64 go build -o out/arm64/task-go   ./cmd/task/
GOOS=linux GOARCH=arm64 go build -o out/arm64/webui     ./cmd/webui/

# Phase 1
docker buildx build --platform linux/amd64,linux/arm64 --push \
  -t ghcr.io/gao-aipros/ai-base:latest docker/base/

# Phase 2 (parallel)
docker buildx build --platform linux/amd64,linux/arm64 --push \
  --build-arg BASE_IMAGE=ghcr.io/gao-aipros/ai-base:latest \
  -t ghcr.io/gao-aipros/worker-base:latest \
  -f docker/worker-base/Dockerfile . &
docker buildx build --platform linux/amd64,linux/arm64 --push \
  --build-arg BASE_IMAGE=ghcr.io/gao-aipros/ai-base:latest \
  -t ghcr.io/gao-aipros/master-agent:latest \
  -f docker/master-agent/Dockerfile . &
wait

# Phase 3 (parallel)
docker buildx build --platform linux/amd64,linux/arm64 --push \
  --build-arg WORKER_BASE_IMAGE=ghcr.io/gao-aipros/worker-base:latest \
  -t ghcr.io/gao-aipros/copilot:latest \
  -f docker/copilot/Dockerfile . &
docker buildx build --platform linux/amd64,linux/arm64 --push \
  --build-arg WORKER_BASE_IMAGE=ghcr.io/gao-aipros/worker-base:latest \
  -t ghcr.io/gao-aipros/opencode:latest \
  -f docker/opencode/Dockerfile . &
docker buildx build --platform linux/amd64,linux/arm64 --push \
  --build-arg WORKER_BASE_IMAGE=ghcr.io/gao-aipros/worker-base:latest \
  -t ghcr.io/gao-aipros/worker-claude:latest \
  -f docker/worker-claude/Dockerfile . &
wait
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
```

Tests use `miniredis` (in-memory Redis) — no real Redis instance is required. The `tasklib` package is the integration layer and has the most comprehensive tests. CLI tests in `cmd/task/` and `cmd/worker/` verify end-to-end behavior.

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

Local builds use `--load` (no `--platform`). `ARG BASE_IMAGE` and `ARG WORKER_BASE_IMAGE` default to local tags. Go binaries are pre-built outside Docker and copied in via `COPY out/${TARGETARCH}/<binary>`.

### Go module

Module path: `github.com/noodle05/ai-agents`. Key dependencies: `go-redis/v9`, `go-chi/chi/v5`, `spf13/cobra`, `miniredis/v2` (test only).

### Redis key schema

All keys follow a strict pattern:
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

Rename `.env.example` to `.env` and fill in per-agent `*_GH_TOKEN` values and `DEEPSEEK_API_KEY`. Run with `docker compose up -d`. The web UI is at `http://localhost:8000`.
