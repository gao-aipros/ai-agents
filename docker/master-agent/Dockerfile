ARG CLAUDE_IMAGE=claude-code:latest

# Build stage: compile Go task CLI binary
FROM golang:latest AS go-build-task
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY tasklib/ tasklib/
COPY cmd/task/ cmd/task/
RUN CGO_ENABLED=0 go build -o /out/task-go ./cmd/task/

# Final stage
FROM ${CLAUDE_IMAGE}

USER root

# Go task CLI binary (TASKLIB_BACKEND=go)
COPY --from=go-build-task /out/task-go /usr/local/bin/task-go

# Python task CLI (TASKLIB_BACKEND=python)
COPY scripts/task.py /usr/local/bin/task.py

# Task wrapper — toggles backend via TASKLIB_BACKEND env var
COPY scripts/task-wrapper.sh /usr/local/bin/task

RUN mkdir -p /home/agent/.claude
COPY --chown=agent:agent docker/master-agent/CLAUDE.md /home/agent/.claude/CLAUDE.md

USER agent
ENTRYPOINT ["claude"]
