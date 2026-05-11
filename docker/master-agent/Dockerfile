ARG CLAUDE_IMAGE=claude-code:latest

FROM ${CLAUDE_IMAGE}

USER root

# Master CLI tool
COPY scripts/task.py /usr/local/bin/task.py

RUN mkdir -p /home/agent/.claude
COPY --chown=agent:agent docker/master-agent/CLAUDE.md /home/agent/.claude/CLAUDE.md

USER agent
ENTRYPOINT ["claude"]
