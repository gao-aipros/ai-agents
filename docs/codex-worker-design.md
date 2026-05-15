# Codex Worker Design

## Summary

Add a 4th worker (`codex`) that runs OpenAI's Codex CLI backed by DeepSeek V4 Pro, using moon-bridge as a protocol translation proxy (Responses API → Chat Completions).

## Architecture

```
Codex CLI ──Responses API──> moon-bridge ──Chat Completions──> DeepSeek API
         localhost:38440      (localhost)                     api.deepseek.com
```

Codex CLI only speaks Responses API (`wire_api = "responses"`). DeepSeek only speaks Chat Completions. moon-bridge translates between the two.

## Container process tree

```
bash (PID 1, entrypoint.sh)
  ├─ moonbridge (background)     ← Responses → Chat Completions proxy
  └─ worker-go codex (foreground) ← Redis task loop
       └─ codex exec ... (per-task subprocess)
```

entrypoint.sh starts moon-bridge, waits for `/health`, then starts worker-go. `wait -n` + `trap` ensures clean shutdown of both processes on SIGTERM or if either crashes.

## Dockerfile (multi-stage)

| Stage | Base image | Purpose |
|-------|-----------|---------|
| moon-bridge-build | `golang:1.25-bookworm` | Clone + build moon-bridge from source (pinned commit) |
| codex-download | `debian:trixie` | Download Codex CLI binary from GitHub releases |
| final | `${WORKER_BASE_IMAGE}` | Copy both binaries, configs, entrypoint |

Moon-bridge is built in a builder stage (no pre-built releases available). Codex CLI is downloaded as a pre-built musl binary (`codex-{arch}-unknown-linux-musl.tar.gz`).

## Decisions

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | Use **moon-bridge** as proxy | 162★, Go, actively maintained, built-in Codex config generation, no new runtime deps |
| 2 | moon-bridge **built from source in Dockerfile** | No pre-built releases; golang builder stage keeps it self-contained |
| 3 | Codex CLI **downloaded as binary** from GitHub releases | Pre-built musl binaries for amd64/arm64: `codex-{arch}-unknown-linux-musl.tar.gz` |
| 4 | Use **entrypoint.sh** for process orchestration | bash starts moon-bridge → waits /health → starts worker-go; SIGTERM via trap + `wait -n` |
| 5 | Codex auth: **none** | `api_key = "any-non-empty-value"` in config.toml; moon-bridge doesn't validate client keys |
| 6 | `AGENT_CMD`: `codex exec --model deepseek-v4-pro --yolo --sandbox danger-full-access --skip-git-repo-check` | Headless non-interactive mode; full access is safe inside Docker |
| 7 | Codex config.toml **static at build time** | Just `base_url` + `api_key` — never changes |
| 8 | Moon-bridge config **template with runtime key injection** | `sed` substitutes `${DEEPSEEK_API_KEY}` at startup; keeps secret out of image |
| 9 | Use **`AGENTS.md`** for agent instructions | Same pattern as copilot/opencode workers |
| 10 | Sandbox: **danger-full-access** | Container isolation is sufficient |

## Files created

- `docker/codex/Dockerfile` — multi-stage build (moon-bridge + codex)
- `docker/codex/entrypoint.sh` — process orchestration
- `docker/codex/moonbridge-config.yml` — moon-bridge config (DeepSeek backend)
- `docker/codex/config.toml` — Codex config (points at localhost:38440)
- `docker/codex/AGENTS.md` — agent instructions

## Files modified

- `tasklib/client.go:18` — add `"codex"` to `WorkerTypes`
- `docker-compose.yml` — add `worker-codex` service
- `.github/workflows/build-images.yml` — add codex job (Phase 3)
- `.env.example` — add `WORKER_CODEX_GH_TOKEN`, `WORKER_CODEX_IMAGE`
- `cmd/webui/internal/templates/templates_test.go` — update `WorkerTypes` in test fixtures

## Open questions

- Model name mapping: does DeepSeek's Chat Completions API accept `deepseek-v4-pro` as a model name, or does it need an alias like `deepseek-chat`? Adjust moon-bridge routes if needed during testing.
