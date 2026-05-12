ARG CLAUDE_IMAGE=claude-code:latest

# Build stage: extract Go toolchain
FROM golang:latest AS go-build

# Final stage
FROM ${CLAUDE_IMAGE}

USER root

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

# Worker poll loop
COPY scripts/worker.py /usr/local/bin/worker.py

ENV AGENT_CMD="claude -p --dangerously-skip-permissions"

RUN mkdir -p /home/agent/go /home/agent/.claude \
    && chown -R agent:agent /home/agent/go /home/agent/.claude
COPY --chown=agent:agent docker/worker-claude/CLAUDE.md /home/agent/.claude/CLAUDE.md
COPY --chown=agent:agent docker/worker-claude/settings.json /home/agent/.claude/settings.json

USER agent
ENTRYPOINT ["python3", "/usr/local/bin/worker.py"]
