ARG WORKER_BASE_IMAGE=worker-base:latest

# Stage 1: build moon-bridge from source
FROM golang:1.26.3-trixie AS moon-bridge-build

ARG MOON_BRIDGE_REF=0023e286e9bf

RUN git clone https://github.com/ZhiYi-R/moon-bridge.git /src && \
    cd /src && \
    git checkout ${MOON_BRIDGE_REF}
WORKDIR /src
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/moonbridge ./cmd/moonbridge

# Stage 2: download Codex CLI binary
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
    tar -xzf /tmp/codex.tar.gz -C /usr/local/bin/ codex && \
    chmod +x /usr/local/bin/codex

# Stage 3: final image
FROM ${WORKER_BASE_IMAGE}

USER root

# Moon-bridge proxy
COPY --from=moon-bridge-build /out/moonbridge /usr/local/bin/moonbridge

# Codex CLI
COPY --from=codex-download /usr/local/bin/codex /usr/local/bin/codex

RUN mkdir -p /home/agent/.codex && chown -R agent:agent /home/agent/.codex

# Codex config (static — always points at local moon-bridge)
COPY --chown=agent:agent docker/codex/config.toml /home/agent/.codex/config.toml

# Agent instructions
COPY --chown=agent:agent docker/codex/AGENTS.md /home/agent/AGENTS.md

# Moon-bridge config template (API key injected at startup)
COPY --chown=agent:agent docker/codex/moonbridge-config.yml /home/agent/.codex/moonbridge-config.yml

ENV AGENT_CMD="codex exec --model deepseek-v4-pro --yolo --sandbox danger-full-access --skip-git-repo-check"

# Entrypoint
COPY docker/codex/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

USER agent
ENTRYPOINT ["/entrypoint.sh"]
