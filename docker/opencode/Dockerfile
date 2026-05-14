ARG BASE_IMAGE=ai-base:latest

# Build stage: install OpenCode binary
FROM debian:trixie AS opencode-build

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    curl \
    git \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://opencode.ai/install | bash -s -- --no-modify-path

# Build stage: compile Go worker binary
FROM golang:latest AS go-build-worker
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY tasklib/ tasklib/
COPY cmd/worker/ cmd/worker/
RUN CGO_ENABLED=0 go build -o /out/worker-go ./cmd/worker/

# Build stage: extract Go toolchain for runtime use by agents
FROM golang:latest AS go-toolchain

# Final stage
FROM ${BASE_IMAGE}

USER root

# OpenCode CLI
COPY --from=opencode-build /root/.opencode/bin/opencode /usr/local/bin/opencode

# Go toolchain (for agent Go development tasks)
COPY --from=go-toolchain /usr/local/go /usr/local/go
ENV GOROOT=/usr/local/go \
    GOPATH=/home/agent/go \
    PATH=/usr/local/go/bin:${PATH}

# CGO support
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc \
    libc6-dev \
    make \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /home/agent/go && chown -R agent:agent /home/agent/go && \
    mkdir -p /home/agent/.config/opencode && chown -R agent:agent /home/agent/.config

# Provider/model defaults — overridable at runtime
ENV OPENCODE_PROVIDER=deepseek \
    OPENCODE_MODEL=deepseek-v4-pro

ENV AGENT_CMD="opencode -m ${OPENCODE_PROVIDER}/${OPENCODE_MODEL} run --dangerously-skip-permissions"

# API key — pass at build time (--build-arg) or runtime (-e)
ARG DEEPSEEK_API_KEY
ENV DEEPSEEK_API_KEY=${DEEPSEEK_API_KEY}

# Worker instructions
COPY --chown=agent:agent docker/opencode/AGENTS.md /home/agent/AGENTS.md
COPY --chown=agent:agent docker/opencode/opencode.json /home/agent/.config/opencode/opencode.json

# Go worker binary (TASKLIB_BACKEND=go)
COPY --from=go-build-worker /out/worker-go /usr/local/bin/worker-go

# Python worker (TASKLIB_BACKEND=python)
COPY scripts/worker.py /usr/local/bin/worker.py

# Worker entrypoint — toggles backend via TASKLIB_BACKEND env var
COPY scripts/worker-entrypoint.sh /usr/local/bin/worker-entrypoint.sh

USER agent
RUN opencode models --refresh

ENTRYPOINT ["/usr/local/bin/worker-go", "opencode"]
