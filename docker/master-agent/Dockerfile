# Build stage: extract docker CLI binary
FROM docker:latest AS docker-build

# Final stage
ARG CLAUDE_IMAGE=claude-code:latest
FROM ${CLAUDE_IMAGE}

COPY --from=docker-build /usr/local/bin/docker /usr/local/bin/docker

RUN mkdir -p /home/agent/.claude
COPY --chown=agent:agent CLAUDE.md /home/agent/.claude/CLAUDE.md

# Worker image references — overridable at runtime
ENV WORKER_CLAUDE_IMAGE=worker-claude:latest \
    WORKER_COPILOT_IMAGE=copilot:latest \
    WORKER_OPENCODE_IMAGE=opencode:latest

USER agent
ENTRYPOINT ["claude"]
