ARG BASE_IMAGE=ai-base:latest

# Build stage: download Claude CLI
FROM debian:trixie AS claude-build

ARG CLAUDE_VERSION=2.1.150
ARG TARGETARCH

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl tar && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

RUN case "${TARGETARCH}" in \
      amd64) ARCH=x64 ;; \
      arm64) ARCH=arm64 ;; \
      *) echo "Unsupported arch: ${TARGETARCH}"; exit 1 ;; \
    esac && \
    echo "Downloading claude version ${CLAUDE_VERSION} for ${ARCH}" && \
    curl -sSL "https://github.com/anthropics/claude-code/releases/download/v${CLAUDE_VERSION}/claude-linux-${ARCH}.tar.gz" \
      | tar -xz -C /usr/local/bin claude && \
    chmod +x /usr/local/bin/claude

# Build stage: download skills
FROM debian:trixie AS skills-download

RUN apt-get update && apt-get install -y --no-install-recommends \
    git ca-certificates \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN git clone --filter=blob:none \
    https://github.com/mattpocock/skills.git /tmp/skills \
    && mkdir -p /tmp/skills-out \
    && for skill in \
      engineering/grill-with-docs \
      engineering/improve-codebase-architecture \
      engineering/diagnose \
      engineering/prototype \
      engineering/to-issues \
      engineering/to-prd \
      engineering/triage \
      engineering/zoom-out \
      productivity/handoff \
      productivity/caveman \
      productivity/grill-me; do \
      cp -r "/tmp/skills/skills/$skill" "/tmp/skills-out/$(basename $skill)"; \
    done \
    && git clone --filter=blob:none \
      https://github.com/Noodle05/skills.git /tmp/noodle-skills \
    && cp -r /tmp/noodle-skills/engineering/code-review /tmp/skills-out/code-review \
    && cp -r /tmp/noodle-skills/engineering/code-author /tmp/skills-out/code-author

# Final stage
FROM ${BASE_IMAGE}

USER root

# Claude CLI
COPY --from=claude-build /usr/local/bin/claude /usr/local/bin/claude

# DeepSeek config (non-sensitive defaults)
ENV ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic \
    ANTHROPIC_MODEL=deepseek-v4-pro[1m] \
    ANTHROPIC_DEFAULT_OPUS_MODEL=deepseek-v4-pro[1m] \
    ANTHROPIC_DEFAULT_SONNET_MODEL=deepseek-v4-pro[1m] \
    ANTHROPIC_DEFAULT_HAIKU_MODEL=deepseek-v4-flash \
    CLAUDE_CODE_SUBAGENT_MODEL=deepseek-v4-flash \
    CLAUDE_CODE_EFFORT_LEVEL=max

# Pre-built Go binaries
ARG TARGETARCH
COPY out/${TARGETARCH}/task-go /usr/local/bin/task
COPY out/${TARGETARCH}/webui /usr/local/bin/webui

RUN mkdir -p /home/agent/.claude && chown agent:agent /home/agent/.claude
VOLUME /home/agent/.claude
COPY --chown=agent:agent docker/master-agent/CLAUDE.md /home/agent/.claude/CLAUDE.md
COPY --chown=agent:agent --chmod=755 docker/master-agent/guard.py /home/agent/guard.py
COPY --from=skills-download --chown=agent:agent /tmp/skills-out /home/agent/.claude/skills/
COPY --chown=agent:agent docs/agents/ /home/agent/.claude/agents-config/

# Reference copies outside the volume so config files update on redeploy
RUN mkdir -p /opt/claude-reference && chown agent:agent /opt/claude-reference
COPY --chown=agent:agent docker/master-agent/CLAUDE.md /opt/claude-reference/CLAUDE.md
COPY --from=skills-download --chown=agent:agent /tmp/skills-out /opt/claude-reference/skills/
COPY --chown=agent:agent docs/agents/ /opt/claude-reference/agents-config/

# Symlink .claude.json into the persisted volume so config survives restarts
RUN ln -sf /home/agent/.claude/claude.json /home/agent/.claude.json \
    && chown -h agent:agent /home/agent/.claude.json

COPY --chown=agent:agent --chmod=755 docker/master-agent/entrypoint.sh /usr/local/bin/entrypoint.sh

EXPOSE 8000

USER agent
ENTRYPOINT ["entrypoint.sh"]
