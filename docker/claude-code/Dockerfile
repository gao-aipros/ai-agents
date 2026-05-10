# Build stage: run install script to extract the binary
FROM debian:trixie AS build

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    curl \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

ARG CLAUDE_VERSION=stable
RUN curl -sL https://claude.ai/install.sh | bash -s -- "$CLAUDE_VERSION"

# Final stage
ARG BASE_IMAGE=ai-base:latest
FROM ${BASE_IMAGE}

COPY --from=build /root/.local/bin/claude /usr/local/bin/claude

# Non-sensitive DeepSeek config — baked as defaults
ENV ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic \
    ANTHROPIC_MODEL=deepseek-v4-pro[1m] \
    ANTHROPIC_DEFAULT_OPUS_MODEL=deepseek-v4-pro[1m] \
    ANTHROPIC_DEFAULT_SONNET_MODEL=deepseek-v4-pro[1m] \
    ANTHROPIC_DEFAULT_HAIKU_MODEL=deepseek-v4-flash \
    CLAUDE_CODE_SUBAGENT_MODEL=deepseek-v4-flash \
    CLAUDE_CODE_EFFORT_LEVEL=max

# Auth token — pass at build time (--build-arg) or runtime (-e)
ARG ANTHROPIC_AUTH_TOKEN
ENV ANTHROPIC_AUTH_TOKEN=${ANTHROPIC_AUTH_TOKEN}

USER agent
ENTRYPOINT ["claude"]
