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

# Build stage: extract Go toolchain
FROM golang:latest AS go-build

# Final stage
FROM ${BASE_IMAGE}

USER root

# OpenCode CLI
COPY --from=opencode-build /root/.opencode/bin/opencode /usr/local/bin/opencode

# Go toolchain
COPY --from=go-build /usr/local/go /usr/local/go
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

# API key — pass at build time (--build-arg) or runtime (-e)
ARG DEEPSEEK_API_KEY
ENV DEEPSEEK_API_KEY=${DEEPSEEK_API_KEY}

# Worker instructions
COPY --chown=agent:agent AGENTS.md /home/agent/AGENTS.md
COPY --chown=agent:agent opencode.json /home/agent/.config/opencode/opencode.json

COPY entrypoint.sh /entrypoint.sh

USER agent
RUN opencode models --refresh

ENTRYPOINT ["/entrypoint.sh"]
