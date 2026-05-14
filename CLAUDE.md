# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Architecture

Multi-agent Docker orchestration where a master agent delegates tasks to worker agents (Claude Code, Copilot, OpenCode). All agents run in Docker containers, communicating via a Redis task queue (control plane) and a shared `/workspace` volume (data plane).

**Image dependency layers:**

```
ai-base (debian:trixie + gh, git, jq, python3, redis-py, curl, ssh)
  ├─ claude-code (FROM ai-base, + claude CLI)
  ├─ copilot     (FROM ai-base, + copilot CLI, + Go toolchain)
  └─ opencode    (FROM ai-base, + opencode CLI, + Go toolchain)
        │
        ├─ master-agent   (FROM claude-code, + task CLI)
        └─ worker-claude  (FROM claude-code, + Go toolchain)
```

The master agent delegates tasks via `task enqueue` to a Redis task queue. Long-running worker containers (one per agent type) dequeue tasks via `BLMOVE`, execute them with full thread context, and post results back to Redis. All containers share a `/workspace` volume for file exchange. Auth tokens (`ANTHROPIC_AUTH_TOKEN`, per-agent `GH_TOKEN` / `GITHUB_TOKEN`) are passed via environment variables.

All agents use DeepSeek as the backend. Claude Code and Copilot use the Anthropic-compatible API (`https://api.deepseek.com/anthropic`); OpenCode uses DeepSeek's native API.

Each agent gets its own GitHub token (`MASTER_GH_TOKEN`, `WORKER_CLAUDE_GH_TOKEN`, etc.) for isolation and rate limiting.

## Commands

### Build images locally (single-arch, x86_64 only)

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

Note: local builds use `--load` (no `--platform` flag). `ARG BASE_IMAGE` and `ARG CLAUDE_IMAGE` default to local tags.

### Build multi-arch (CI)

CI uses `docker buildx build --platform linux/amd64,linux/arm64 --push` in dependency order, passing registry image references via `--build-arg`:

```bash
docker buildx build --platform linux/amd64,linux/arm64 --push \
  -t ghcr.io/noodle05/ai-base:latest docker/base/

docker buildx build --platform linux/amd64,linux/arm64 --push \
  --build-arg BASE_IMAGE=ghcr.io/noodle05/ai-base:latest \
  -t ghcr.io/noodle05/claude-code:latest docker/claude-code/
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

Build stages (`FROM debian:trixie`, `FROM golang:latest`) use Docker Hub images, which are already multi-arch.

## CI

GitHub Actions workflow at `.github/workflows/build-images.yml`. Triggers on push to `main` or manual `workflow_dispatch`. Pushes to `ghcr.io/noodle05/<image>:latest`. Single non-matrix job builds all 6 images in dependency order.

## Environment

See `.env.example` for all variables. Key ones:
- `MASTER_GH_TOKEN` / `WORKER_CLAUDE_GH_TOKEN` / etc. — per-agent GitHub auth for `gh` CLI
- `DEEPSEEK_API_KEY` — shared by all agents
- `REDIS_HOST` / `REDIS_PORT` — Redis connection (task queue)
- Docker image overrides (compose-level): `WORKER_CLAUDE_IMAGE`, `WORKER_COPILOT_IMAGE`, `WORKER_OPENCODE_IMAGE`, `MASTER_AGENT_IMAGE`
