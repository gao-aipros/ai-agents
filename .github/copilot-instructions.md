# Copilot Instructions

## Architecture

Multi-agent Docker orchestration where a master agent (Claude Code) delegates tasks to worker agents (Claude Code, Codex, Copilot, OpenCode). All agents run in Docker containers, communicating via a Redis task queue (control plane) and a shared `/workspace` volume (data plane).

**Image dependency layers:**

```
ai-base (debian:trixie + gh, git, jq, python3, redis-tools, curl, ssh)
  ├─ master-agent   (ai-base + claude CLI + task-go + webui)
  └─ worker-base    (ai-base + Go SDK + gcc/make + worker-go)
       ├─ copilot         (worker-base + copilot CLI)
       ├─ opencode        (worker-base + opencode CLI)
       ├─ worker-claude   (worker-base + claude CLI)
       └─ codex           (worker-base + codex CLI)

moon-bridge (independent, golang:1.26.3-trixie + moon-bridge binary)
```

## Agent Roles and Workflow

### Role assignments

| Agent | Role |
|-------|------|
| **master-agent** | Design and planning only. Never writes implementation code or submits reviews. Creates design docs, coordinates the workflow, delegates tasks, and makes final design decisions. |
| **worker-claude** | Implementer + reviewer. Writes code and unit tests when assigned. Reviews PRs and design docs from other workers. Never reviews own code. |
| **codex** | Implementer + reviewer. Writes code and unit tests when assigned. Reviews PRs and design docs from other workers. Never reviews own code. |
| **copilot** | Reviewer only. Reviews design docs and PRs. Does not write implementation code. |
| **opencode** | Reviewer only. Reviews design docs and PRs. Does not write implementation code. |

### Workflow

1. **Design** — Master analyzes the request and produces three design documents: `docs/high-level-design.md` (architecture), `docs/detailed-design.md` (APIs and modules), and `docs/implementation-phases.md` (phased rollout plan).
2. **Design review** — Master sends all three design documents to all 4 workers for review. Workers produce `docs/design-review-<worker>.md` covering all three documents. Master reads all reviews, decides which feedback to incorporate, and updates the design documents.
3. **Implementation** — Master assigns the implementation to either `worker-claude` or `codex`. The implementing worker writes code, **writes unit tests for their own code**, pushes a branch, and creates a PR. The worker reports the PR number back.
4. **Code review** — Master sends the PR to all workers **except the implementer**. Each reviewer inspects the PR and submits their review as a comment via `gh pr review`. Reviewers also write summary files to `docs/code-review-<worker>.md`.
5. **Revise** — Master asks the implementing worker to read all review feedback and address the issues. The worker pushes updated commits to the same PR.
6. **Re-review** — Master sends the updated PR back to the reviewers. Steps 4-5 loop until every reviewer approves.
7. **Merge** — Master instructs the implementing worker to merge the PR. The implementing worker runs `gh pr merge ... --squash --delete-branch`. Only the implementing worker merges.

**No self-review**: No worker may review their own PR. The master routes reviews only to workers who did not write the code.

## Permissions

- **Allowed directories**: Only the repository root and `/tmp`. All file reads, writes, and modifications must stay within these two locations.
- **No sudo**: Never run `sudo` or attempt to gain elevated privileges. If a command requires root, stop and report it.
- **Auto-execute**: When operating within the allowed directories, proceed with file operations and shell commands without asking for confirmation. Do not prompt the user to approve safe, in-scope actions.

## Build, Test, and Lint

### Build

```bash
# Pre-build Go binaries (required before any Docker build)
go mod download
GOOS=linux go build -o out/amd64/worker-go ./cmd/worker/
GOOS=linux go build -o out/amd64/task-go   ./cmd/task/
GOOS=linux go build -o out/amd64/webui     ./cmd/webui/

# Phase 1 — base images (parallel, no deps)
docker build --load -t ai-base:latest docker/base/ &
docker build --load -t moon-bridge:latest -f docker/moon-bridge/Dockerfile . &
wait

# Phase 2 — worker-base and master-agent (parallel, both FROM ai-base)
docker build --load -t worker-base:latest -f docker/worker-base/Dockerfile . &
docker build --load -t master-agent:latest -f docker/master-agent/Dockerfile . &
wait

# Phase 3 — workers (parallel, all FROM worker-base)
docker build --load -t copilot:latest -f docker/copilot/Dockerfile . &
docker build --load -t opencode:latest -f docker/opencode/Dockerfile . &
docker build --load -t worker-claude:latest -f docker/worker-claude/Dockerfile . &
docker build --load -t codex:latest -f docker/codex/Dockerfile . &
wait
```

CI (`.github/workflows/build-images.yml`) pre-builds Go binaries for both architectures, then builds 8 multi-arch images (`linux/amd64,linux/arm64`) in 3 parallel phases and pushes to `ghcr.io/gao-aipros/<image>:latest`.

### Multi-arch builds (CI pattern)

```bash
# Go binaries (cross-compiled in CI via actions/setup-go)
GOOS=linux GOARCH=amd64 go build -o out/amd64/worker-go ./cmd/worker/
GOOS=linux GOARCH=amd64 go build -o out/amd64/task-go   ./cmd/task/
GOOS=linux GOARCH=amd64 go build -o out/amd64/webui     ./cmd/webui/
GOOS=linux GOARCH=arm64 go build -o out/arm64/worker-go ./cmd/worker/
GOOS=linux GOARCH=arm64 go build -o out/arm64/task-go   ./cmd/task/
GOOS=linux GOARCH=arm64 go build -o out/arm64/webui     ./cmd/webui/

# Phase 1 (parallel)
docker buildx build --platform linux/amd64,linux/arm64 --push \
  -t ghcr.io/gao-aipros/ai-base:latest docker/base/ &
docker buildx build --platform linux/amd64,linux/arm64 --push \
  -t ghcr.io/gao-aipros/moon-bridge:latest \
  -f docker/moon-bridge/Dockerfile . &
wait

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
docker buildx build --platform linux/amd64,linux/arm64 --push \
  --build-arg WORKER_BASE_IMAGE=ghcr.io/gao-aipros/worker-base:latest \
  -t ghcr.io/gao-aipros/codex:latest \
  -f docker/codex/Dockerfile . &
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
