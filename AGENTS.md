# AGENTS.md


## Domain terminology

This project maintains a domain glossary at [CONTEXT.md](CONTEXT.md). Read it before making naming decisions — it defines canonical terms (e.g., _access log_, _admin endpoint_) and lists synonyms to avoid. See also `docs/agents/domain.md` for how agents should consume project documentation.
This file provides guidance to Codex agents when working with code in this repository.

## Architecture

Multi-agent Docker orchestration where a master agent delegates tasks to worker agents (Claude Code, Copilot, OpenCode, Codex). All agents run in Docker containers, communicating via a Redis task queue (control plane) and a shared `/workspace` volume (data plane).

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

The master agent delegates tasks via `task enqueue` to a Redis task queue. Long-running worker containers (one per agent type) dequeue tasks via `BLMOVE`, execute them with full thread context, and post results back to Redis. All containers share a `/workspace` volume for file exchange. Auth tokens (`ANTHROPIC_AUTH_TOKEN`, per-agent `GH_TOKEN`) are passed via environment variables.

All agents use DeepSeek as the backend. Claude Code and Copilot use the Anthropic-compatible API (`https://api.deepseek.com/anthropic`); OpenCode uses DeepSeek's native API; Codex uses the Chat Completions API via a separate moon-bridge container that translates OpenAI Responses API calls.

Each agent gets its own GitHub token (`MASTER_GH_TOKEN`, `WORKER_CLAUDE_GH_TOKEN`, etc.) for isolation and rate limiting.


## Agent Roles and Workflow

### Role assignments

| Agent | Role |
|-------|------|
| **master-agent** | Design and planning only. Never writes implementation code or submits reviews. Creates design docs, coordinates the workflow, delegates tasks, and makes final design decisions. |
| **worker-claude** | Implementer + reviewer. Writes code and unit tests when assigned. Reviews PRs and design docs from other workers. |
| **codex** | Implementer + reviewer. Writes code and unit tests when assigned. Reviews PRs and design docs from other workers. |
| **copilot** | Reviewer only. Reviews design docs and PRs. Does not write implementation code. |
| **opencode** | Reviewer only. Reviews design docs and PRs. Does not write implementation code. |

### Workflow

1. **Design** — Master analyzes the request and produces three design documents: `docs/high-level-design.md` (architecture), `docs/detailed-design.md` (APIs and modules), and `docs/implementation-phases.md` (phased rollout plan).
2. **Design review** — Master sets thread status to `"reviewing"`, then fans out all three design documents to all 4 workers in parallel using `--group "design-review"`. Workers produce `docs/design-review-<worker>.md` covering all three documents. Reviewers may propose alternative approaches with rationale. Master uses `group-wait` to wait for all reviews, reads them, decides which feedback to incorporate, and updates the design documents.
3. **Implementation** — Master assigns the implementation to either `worker-claude` or `codex`. The implementing worker writes code, **writes unit tests for their own code**, pushes a branch, and creates a PR. The worker reports the PR number back.
4. **Code review** — Master sets thread status to `"reviewing"`, then sends the PR to all workers **except the implementer** in parallel using `--group "code-review"`. Each reviewer inspects the PR and submits their review as a comment via `gh pr review`. Reviewers also write summary files to `docs/code-review-<worker>.md`. Master uses `group-wait` to wait for all reviews.
5. **Revise** — Master asks the implementing worker to read all review feedback and address the issues. The worker pushes updated commits to the same PR.
6. **Re-review** — Master sends the updated PR back to the reviewers. Steps 4-5 loop until every reviewer approves.
7. **Merge** — Master instructs the implementing worker to merge the PR. The implementing worker runs `gh pr merge ... --squash --delete-branch`. Only the implementing worker merges.

**No self-review**: No worker may review their own PR. The master routes reviews only to workers who did not write the code.

### Parallel task execution (task groups)

Review phases (design review, code review) fan out independent tasks to multiple workers in parallel. The thread lock serializes sequential phases (implement → revise → merge). For parallel review phases, workers operate on read-only copies and write to separate review files — no conflicts.

**Thread status gate**: The master must set thread status to `"reviewing"` via `task thread-update --id $THREAD --status "reviewing"` **before** fanning out group tasks. This signals that the thread is in a parallel-safe phase. Sequential phases (implement, revise, merge) continue using default locked enqueue (no `--group`). The master is the sole enqueuer and runs commands sequentially, so no race exists between `EnqueueGroup` calls.

#### Production workflow (task groups)

```bash
# Master transitions thread status to "reviewing" BEFORE fanning out
task thread-update --id $THREAD --status "reviewing"

# Fan out parallel review tasks under a named group
task enqueue --worker copilot  --thread $THREAD --group "design-review" --instruction "..."
task enqueue --worker opencode --thread $THREAD --group "design-review" --instruction "..."
task enqueue --worker codex    --thread $THREAD --group "design-review" --instruction "..."
task enqueue --worker claude   --thread $THREAD --group "design-review" --instruction "..."

# Wait for group to finish; capture aggregate result
# --timeout defaults to 1200s (20 min); individual tasks can have per-task timeouts via task enqueue --timeout N
RESULT=$(task group-wait --thread $THREAD --group "design-review" --timeout 1200)
STATUS=$(echo "$RESULT" | jq -r .status)

# Handle failures if needed
if [ "$STATUS" = "error" ]; then
  FAILED=$(echo "$RESULT" | jq -r '.tasks | to_entries | map(select(.value != "done")) | .[].key')
  for TID in $FAILED; do
    WORKER=$(task status --id "$TID" | jq -r .worker)
    task enqueue --worker "$WORKER" --thread $THREAD --group "design-review-retry" --instruction "..."
  done
  task group-wait --thread $THREAD --group "design-review-retry" --timeout 1200
fi
```

**Aggregate status** (computed by `group-wait`):
- All `done` → `"complete"`
- Any `failed` → `"error"` (highest priority — overrides `cancelled`; `failed` + `cancelled` mix still yields `"error"`)
- All `cancelled` → `"cancelled"`
- Mixed `done` + `cancelled` → `"complete"` (inspect `.tasks` to distinguish)

**Exit codes**: `"complete"` → 0; `"error"`, `"cancelled"`, `"timeout"` → 1.

**Recovery**: If a worker crashes during a parallel task, the task remains in the group SET as `pending`. `GroupWait` will timeout, reporting the stuck task. Use `task requeue-stale` or `task cancel` + re-enqueue. `task requeue-stale` works identically for group tasks. After a retry group completes, `group-wait` updates thread status based on the retry outcome.

**Lock gate-check**: `EnqueueGroup` uses `SET NX` → immediate `DEL` (with 10s TTL fallback) to gate on the sequential phase being complete. The gate-check lock is released immediately so subsequent group enqueues succeed. If the process crashes between `SET NX` and `DEL`, the 10s TTL prevents blocking sequential enqueues for the full `LockTTL` (7500s). Recovery: `task unlock --thread <id>`.

## Commands

### Build images locally (single-arch)

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

Note: local builds use `--load` (no `--platform` flag). `ARG BASE_IMAGE` and `ARG WORKER_BASE_IMAGE` default to local tags. For arm64 hosts, change `out/amd64/` to `out/arm64/` in the Go build step.

### Build multi-arch (CI)

CI pre-builds Go binaries for both architectures, then uses `docker buildx build --platform linux/amd64,linux/arm64 --push` in 3 parallel phases:

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

## Dockerfile pattern

Multi-stage builds that parameterize `FROM` must declare the `ARG` **before** the first `FROM`:

```dockerfile
ARG BASE_IMAGE=ai-base:latest          # line 1 — before any FROM

FROM debian:trixie AS build             # build stage (uses public images)
...
FROM ${BASE_IMAGE}                      # final stage (parameterized)
...
```

Build stages use `debian:trixie` (Docker Hub), which is already multi-arch. Go binaries are pre-built outside Docker and copied in via `COPY out/${TARGETARCH}/<binary>` — there are no `FROM golang:latest` stages.

The `moon-bridge` image is an exception: it builds from source (`FROM golang:1.26.3-trixie`) since it compiles an external Go project (moon-bridge proxy) that has no pre-built binaries or published images.

## CI

GitHub Actions workflow at `.github/workflows/build-images.yml`. Triggers on push to `main` or manual `workflow_dispatch`. Pushes to `ghcr.io/gao-aipros/<image>:latest`. Single job builds Go binaries first, then 8 images in 3 phases (phase 1: `ai-base` and `moon-bridge` in parallel, phase 2: `worker-base` and `master-agent` in parallel, phase 3: `copilot`, `opencode`, `worker-claude`, `codex` in parallel).

## Environment

See `.env.example` for all variables. Key ones:
- `MASTER_GH_TOKEN`, `WORKER_CLAUDE_GH_TOKEN`, `WORKER_COPILOT_GH_TOKEN`, `WORKER_OPENCODE_GH_TOKEN`, `WORKER_CODEX_GH_TOKEN` — per-agent GitHub auth for `gh` CLI
- `DEEPSEEK_API_KEY` — used by opencode and codex (via moon-bridge)
- `ANTHROPIC_AUTH_TOKEN` — used by master-agent and worker-claude (Anthropic-compatible API via DeepSeek)
- `REDIS_HOST` / `REDIS_PORT` — Redis connection (task queue)
- `CLOUDFLARED_TUNNEL_TOKEN` — Cloudflare Tunnel token for web UI access
- Docker image overrides (compose-level): `WORKER_CLAUDE_IMAGE`, `WORKER_COPILOT_IMAGE`, `WORKER_OPENCODE_IMAGE`, `WORKER_CODEX_IMAGE`, `MASTER_AGENT_IMAGE`, `MOON_BRIDGE_IMAGE`

## Debugging

### Quick reference

| Need | Command |
|------|---------|
| Thread diagnosis | `task why --thread <id>` |
| Task lifecycle | `task status --id <task-id>` |
| Thread events | `task thread-state --id <thread-id>` |
| System events | `task events --limit 50` |
| Worker instances | `curl -H "Authorization: Bearer $WEBUI_API_KEY" http://localhost:8000/api/workers/<type>/instances` |
| Health check | `curl -H "Authorization: Bearer $WEBUI_API_KEY" http://localhost:8000/api/diagnostics` |
| Access log state | `curl -H "Authorization: Bearer $ADMIN_API_KEY" http://localhost:8000/api/admin/log-access` |
| Toggle access log on | `curl -X PUT -H "Authorization: Bearer $ADMIN_API_KEY" -H "Content-Type: application/json" -d '{"enabled":true}' http://localhost:8000/api/admin/log-access` |
| Toggle access log off | `curl -X PUT -H "Authorization: Bearer $ADMIN_API_KEY" -H "Content-Type: application/json" -d '{"enabled":false}' http://localhost:8000/api/admin/log-access` |

### Common workflows

**"Why is this thread stuck?"**
```bash
task why --thread <id>
# Look at: lock_state, stuck_tasks, recent_events
```

**"Did the task actually start?"**
```bash
task status --id <task-id>
# Check: enqueued_at, started_at, last_started_at, retry_count
```

**"Which worker ran this and where?"**
```bash
task status --id <task-id>
# Check: worker_hostname
```

**"Why was this task cancelled?"**
```bash
task status --id <task-id>
# Check: cancelled_by, cancelled_at, cancelled_previous_status
```

**"What happened in this thread?"**
```bash
task thread-state --id <thread-id>
# Check: task summary by status, recent events tail (last 20)
```

**"What happened across the system?"**
```bash
task events --limit 100
# Or: curl -H "Authorization: Bearer $WEBUI_API_KEY" "http://localhost:8000/api/events?limit=100"
```

**"Is anything broken right now?"**
```bash
curl -H "Authorization: Bearer $WEBUI_API_KEY" http://localhost:8000/api/diagnostics
# Check: stale_tasks, locks, queue_depths, redis_memory
```

**"Which instances of a worker type are running?"**
```bash
curl -H "Authorization: Bearer $WEBUI_API_KEY" http://localhost:8000/api/workers/claude/instances
# Returns per-hostname data: uptime, tasks_processed, current_task_id, queue_depth
```

**"How do I toggle access logging at runtime?"**
```bash
# Check current state
curl -H "Authorization: Bearer $ADMIN_API_KEY" http://localhost:8000/api/admin/log-access
# → {"enabled":false}

# Enable (all subsequent HTTP requests logged to stderr as JSON)
curl -X PUT \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"enabled":true}' \
  http://localhost:8000/api/admin/log-access

# Disable
curl -X PUT \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"enabled":false}' \
  http://localhost:8000/api/admin/log-access
```
Admin endpoints use Bearer auth with `ADMIN_API_KEY` (falls back to `WEBUI_API_KEY` if not set). The toggle is in-memory only — restart reverts to the startup flag (`--log-access` / `LOG_ACCESS`). Toggling does not interrupt in-flight master agent work.
