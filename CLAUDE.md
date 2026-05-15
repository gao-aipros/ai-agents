# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

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

GitHub Actions workflow at `.github/workflows/build-images.yml`. Triggers on push to `main` or manual `workflow_dispatch`. Pushes to `ghcr.io/gao-aipros/<image>:latest`. Single job builds Go binaries first, then 7 images in 3 phases (phase 1: `ai-base` and `moon-bridge` in parallel, phase 2: `worker-base` and `master-agent` in parallel, phase 3: `copilot`, `opencode`, `worker-claude`, `codex` in parallel).

## Environment

See `.env.example` for all variables. Key ones:
- `MASTER_GH_TOKEN` / `WORKER_CLAUDE_GH_TOKEN` / etc. — per-agent GitHub auth for `gh` CLI
- `DEEPSEEK_API_KEY` — shared by all agents
- `REDIS_HOST` / `REDIS_PORT` — Redis connection (task queue)
- Docker image overrides (compose-level): `WORKER_CLAUDE_IMAGE`, `WORKER_COPILOT_IMAGE`, `WORKER_OPENCODE_IMAGE`, `WORKER_CODEX_IMAGE`, `MASTER_AGENT_IMAGE`, `MOON_BRIDGE_IMAGE`
