ARG BASE_IMAGE=ai-base:latest

# Build stage: install Copilot CLI binary
FROM debian:trixie AS copilot-build

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    curl \
    git \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://gh.io/copilot-install | bash

# Build stage: extract Go toolchain
FROM golang:latest AS go-build

# Final stage
FROM ${BASE_IMAGE}

USER root

# Copilot CLI
COPY --from=copilot-build /usr/local/bin/copilot /usr/local/bin/copilot

# Go toolchain
COPY --from=go-build /usr/local/go /usr/local/go
ENV GOROOT=/usr/local/go \
    GOPATH=/home/agent/go \
    PATH=/usr/local/go/bin:${PATH}

# DeepSeek provider config for Copilot (non-sensitive)
ENV COPILOT_PROVIDER_TYPE=anthropic \
    COPILOT_PROVIDER_BASE_URL=https://api.deepseek.com/anthropic \
    COPILOT_MODEL=deepseek-v4-pro[1m] \
    COPILOT_PROVIDER_MAX_PROMPT_TOKENS=840000 \
    COPILOT_PROVIDER_MAX_OUTPUT_TOKENS=128000

# API key — pass at build time (--build-arg) or runtime (-e)
ARG COPILOT_PROVIDER_API_KEY
ENV COPILOT_PROVIDER_API_KEY=${COPILOT_PROVIDER_API_KEY}

# CGO support
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc \
    libc6-dev \
    make \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /home/agent/go && chown -R agent:agent /home/agent/go

# Copilot custom instructions (stored in home, copied to workspace at runtime)
COPY --chown=agent:agent AGENTS.md /home/agent/AGENTS.md

COPY entrypoint.sh /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
