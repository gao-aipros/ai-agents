ARG WORKER_BASE_IMAGE=worker-base:latest

# Build stage: download Claude CLI
FROM debian:trixie AS claude-download

ARG CLAUDE_VERSION=2.1.159
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
FROM ${WORKER_BASE_IMAGE}

USER root

# Claude CLI
COPY --from=claude-download /usr/local/bin/claude /usr/local/bin/claude

ENV AGENT_CMD="claude -p --output-format stream-json --verbose --dangerously-skip-permissions" \
    AGENT_TYPE=claude

RUN mkdir -p /home/agent/.claude \
    && chown -R agent:agent /home/agent/.claude
COPY --chown=agent:agent docker/worker-claude/CLAUDE.md /home/agent/.claude/CLAUDE.md
COPY --chown=agent:agent docker/worker-claude/settings.json /home/agent/.claude/settings.json
COPY --chown=agent:agent --chmod=755 docker/worker-claude/guard.py /home/agent/guard.py
COPY --chown=agent:agent --chmod=755 docker/worker-claude/entrypoint.sh /usr/local/bin/entrypoint.sh
COPY --from=skills-download --chown=agent:agent /tmp/skills-out /home/agent/.claude/skills/
COPY --chown=agent:agent docs/agents/ /home/agent/.claude/agents-config/

# Symlink .claude.json into .claude/ so config survives restarts
RUN ln -sf /home/agent/.claude/claude.json /home/agent/.claude.json \
    && chown -h agent:agent /home/agent/.claude.json

USER agent
ENTRYPOINT ["entrypoint.sh"]
