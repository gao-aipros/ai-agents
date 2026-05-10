# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Architecture

Multi-agent Docker orchestration where a master agent delegates tasks to worker agents (Claude Code, Copilot, OpenCode). All agents run in Docker containers, communicating via Docker socket (control plane) and a shared `/workspace` volume (data plane).

**Image dependency layers:**

```
ai-base (debian:trixie + gh, git, jq, python3, curl, ssh)
  ├─ claude-code (FROM ai-base, + claude CLI)
  ├─ copilot     (FROM ai-base, + copilot CLI, + Go toolchain)
  └─ opencode    (FROM ai-base, + opencode CLI, + Go toolchain)
        │
        ├─ master-agent   (FROM claude-code, + docker CLI binary)
        └─ worker-claude  (FROM claude-code, + Go toolchain)
```

The master agent spawns workers via `docker run` using the mounted Docker socket (`/var/run/docker.sock`). Workers receive tasks as CLI prompts (`claude -p "..."`) and write results to `/workspace/result.md`. Auth tokens (`ANTHROPIC_AUTH_TOKEN`, `GH_TOKEN`, `GITHUB_TOKEN`) are forwarded via `-e` flags.

All agents use DeepSeek as the backend. Claude Code and Copilot use the Anthropic-compatible API (`https://api.deepseek.com/anthropic`); OpenCode uses DeepSeek's native API.

## Commands

### Build images locally (single-arch, x86_64 only)

```bash
# Build ai-base first (others depend on it)
docker build --load -t ai-base:latest docker/base/

# Layer 1 — can run in parallel
docker build --load -t claude-code:latest docker/claude-code/ &
docker build --load -t copilot:latest docker/copilot/ &
docker build --load -t opencode:latest docker/opencode/ &
wait

# Layer 2 — depends on layer 1
docker build --load -t master-agent:latest docker/master-agent/
docker build --load -t worker-claude:latest docker/worker-claude/
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

Build stages (`FROM debian:trixie`, `FROM golang:latest`, `FROM docker:latest`) use Docker Hub images, which are already multi-arch.

## CI

GitHub Actions workflow at `.github/workflows/build-images.yml`. Triggers on push to `main` or manual `workflow_dispatch`. Pushes to `ghcr.io/noodle05/<image>:latest`. Single non-matrix job builds all 6 images in dependency order.

## Environment

See `.env.example` for all variables. Key ones:
- `GH_TOKEN` / `GITHUB_TOKEN` — GitHub auth for `gh` CLI
- `DEEPSEEK_API_KEY` — shared by all agents
- Docker image overrides for workers: `WORKER_CLAUDE_IMAGE`, `WORKER_COPILOT_IMAGE`, `WORKER_OPENCODE_IMAGE`
