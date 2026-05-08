# Build stage: extract docker CLI binary
FROM docker:latest AS docker-build

# Final stage
FROM claude-code:latest

COPY --from=docker-build /usr/local/bin/docker /usr/local/bin/docker

RUN mkdir -p /home/agent/.claude
COPY --chown=agent:agent CLAUDE.md /home/agent/.claude/CLAUDE.md

USER agent
ENTRYPOINT ["claude"]
