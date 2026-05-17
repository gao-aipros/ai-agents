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
2. **Design review** — Master sends all three design documents to all 4 workers for review. Workers produce `docs/design-review-<worker>.md` covering all three documents. If reviewers have a better alternative approach, they describe it clearly with rationale. Master reads all reviews, decides which feedback to incorporate, and updates the design documents.
3. **Implementation** — Master assigns the implementation to either `worker-claude` or `codex`. The implementing worker writes code, **writes unit tests for their own code**, pushes a branch, and creates a PR. The worker reports the PR number back.
4. **Code review** — Master sends the PR to all workers **except the implementer**. Each reviewer inspects the PR and submits their review as a comment via `gh pr review`. Reviewers also write summary files to `docs/code-review-<worker>.md`.
5. **Revise** — Master asks the implementing worker to read all review feedback and address the issues. The worker pushes updated commits to the same PR.
6. **Re-review** — Master sends the updated PR back to the reviewers. Steps 4-5 loop until every reviewer approves.
7. **Merge** — Master instructs the implementing worker to merge the PR. The implementing worker runs `gh pr merge ... --squash --delete-branch`. Only the implementing worker merges.

**No self-review**: No worker may review their own PR. The master routes reviews only to workers who did not write the code.

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
