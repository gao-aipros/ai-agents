ARG CLAUDE_IMAGE=claude-code:latest

# Build stage: compile Go worker binary
FROM golang:latest AS go-build-worker
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY tasklib/ tasklib/
COPY cmd/worker/ cmd/worker/
RUN CGO_ENABLED=0 go build -o /out/worker-go ./cmd/worker/

# Build stage: extract Go toolchain for runtime use by agents
FROM golang:latest AS go-toolchain

# Final stage
FROM ${CLAUDE_IMAGE}

USER root

# Go toolchain (for agent Go development tasks)
COPY --from=go-toolchain /usr/local/go /usr/local/go
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

# Go worker binary (TASKLIB_BACKEND=go)
COPY --from=go-build-worker /out/worker-go /usr/local/bin/worker-go

# Python worker (TASKLIB_BACKEND=python)
COPY scripts/worker.py /usr/local/bin/worker.py

# Worker entrypoint — toggles backend via TASKLIB_BACKEND env var
COPY scripts/worker-entrypoint.sh /usr/local/bin/worker-entrypoint.sh

ENV AGENT_CMD="claude -p --dangerously-skip-permissions"

RUN mkdir -p /home/agent/go /home/agent/.claude \
    && chown -R agent:agent /home/agent/go /home/agent/.claude
COPY --chown=agent:agent docker/worker-claude/CLAUDE.md /home/agent/.claude/CLAUDE.md
COPY --chown=agent:agent docker/worker-claude/settings.json /home/agent/.claude/settings.json

USER agent
ENTRYPOINT ["/usr/local/bin/worker-go", "claude"]
