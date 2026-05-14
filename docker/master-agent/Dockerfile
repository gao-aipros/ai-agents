ARG BASE_IMAGE=ai-base:latest

# Build stage: install Claude CLI
FROM debian:trixie AS claude-build

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    curl \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

ARG CLAUDE_VERSION=stable
RUN curl -sL https://claude.ai/install.sh | bash -s -- "$CLAUDE_VERSION"

# Final stage
FROM ${BASE_IMAGE}

USER root

# Claude CLI
COPY --from=claude-build /root/.local/bin/claude /usr/local/bin/claude

# DeepSeek config (non-sensitive defaults)
ENV ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic \
    ANTHROPIC_MODEL=deepseek-v4-pro[1m] \
    ANTHROPIC_DEFAULT_OPUS_MODEL=deepseek-v4-pro[1m] \
    ANTHROPIC_DEFAULT_SONNET_MODEL=deepseek-v4-pro[1m] \
    ANTHROPIC_DEFAULT_HAIKU_MODEL=deepseek-v4-flash \
    CLAUDE_CODE_SUBAGENT_MODEL=deepseek-v4-flash \
    CLAUDE_CODE_EFFORT_LEVEL=max

ARG ANTHROPIC_AUTH_TOKEN
ENV ANTHROPIC_AUTH_TOKEN=${ANTHROPIC_AUTH_TOKEN}

# Pre-built Go binaries
ARG TARGETARCH
COPY out/${TARGETARCH}/task-go /usr/local/bin/task
COPY out/${TARGETARCH}/webui /usr/local/bin/webui

RUN mkdir -p /home/agent/.claude && chown agent:agent /home/agent/.claude
COPY --chown=agent:agent docker/master-agent/CLAUDE.md /home/agent/.claude/CLAUDE.md

USER agent
ENTRYPOINT ["webui"]
