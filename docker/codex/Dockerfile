ARG WORKER_BASE_IMAGE=worker-base:latest

# Stage 1: download Codex CLI binary
FROM debian:trixie AS codex-download

ARG CODEX_VERSION=0.130.0
ARG TARGETARCH

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl tar && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

RUN case "${TARGETARCH}" in \
      amd64) ARCH=x86_64-unknown-linux-musl ;; \
      arm64) ARCH=aarch64-unknown-linux-musl ;; \
      *) echo "Unsupported arch: ${TARGETARCH}"; exit 1 ;; \
    esac && \
    curl -sSL "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-${ARCH}.tar.gz" \
      -o /tmp/codex.tar.gz && \
    tar -xzf /tmp/codex.tar.gz -C /tmp && \
    mv "/tmp/codex-${ARCH}" /usr/local/bin/codex && \
    chmod +x /usr/local/bin/codex

# Stage 2: final image
FROM ${WORKER_BASE_IMAGE}

USER root

# Codex CLI
COPY --from=codex-download /usr/local/bin/codex /usr/local/bin/codex

RUN mkdir -p /home/agent/.codex && chown -R agent:agent /home/agent/.codex

# Codex config (static — always points at moon-bridge service)
COPY --chown=agent:agent docker/codex/config.toml /home/agent/.codex/config.toml
COPY --chown=agent:agent docker/codex/models_catalog.json /home/agent/.codex/models_catalog.json

# Agent instructions
COPY --chown=agent:agent docker/codex/AGENTS.md /home/agent/AGENTS.md

ENV AGENT_CMD="codex exec --yolo --sandbox danger-full-access --skip-git-repo-check"

USER agent
ENTRYPOINT ["worker-go", "codex"]
