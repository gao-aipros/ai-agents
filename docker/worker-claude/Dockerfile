ARG WORKER_BASE_IMAGE=worker-base:latest

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

# Build stage: download skills
FROM debian:trixie AS skills-download

RUN apt-get update && apt-get install -y --no-install-recommends \
    git ca-certificates \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN git clone --depth 1 --filter=blob:none \
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
      engineering/setup-matt-pocock-skills \
      productivity/handoff \
      productivity/caveman \
      productivity/grill-me; do \
      cp -r "/tmp/skills/skills/$skill" "/tmp/skills-out/$(basename $skill)"; \
    done

# Final stage
FROM ${WORKER_BASE_IMAGE}

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

ENV AGENT_CMD="claude -p --dangerously-skip-permissions"

RUN mkdir -p /home/agent/.claude \
    && chown -R agent:agent /home/agent/.claude
COPY --chown=agent:agent docker/worker-claude/CLAUDE.md /home/agent/.claude/CLAUDE.md
COPY --chown=agent:agent docker/worker-claude/settings.json /home/agent/.claude/settings.json
COPY --from=skills-download --chown=agent:agent /tmp/skills-out /home/agent/.claude/skills/

USER agent
ENTRYPOINT ["/usr/local/bin/worker-go", "claude"]
