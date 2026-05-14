ARG CLAUDE_IMAGE=claude-code:latest

# Build stage: compile Go task CLI binary
FROM golang:latest AS go-build-task
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY tasklib/ tasklib/
COPY cmd/task/ cmd/task/
RUN CGO_ENABLED=0 go build -o /out/task-go ./cmd/task/

# Build stage: compile Go webui binary
FROM golang:latest AS go-build-webui
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY tasklib/ tasklib/
COPY cmd/webui/ cmd/webui/
RUN CGO_ENABLED=0 go build -o /out/webui ./cmd/webui/

# Final stage
FROM ${CLAUDE_IMAGE}

USER root

# Go task CLI binary
COPY --from=go-build-task /out/task-go /usr/local/bin/task

# Go webui binary
COPY --from=go-build-webui /out/webui /usr/local/bin/webui

RUN mkdir -p /home/agent/.claude && chown agent:agent /home/agent/.claude
COPY --chown=agent:agent docker/master-agent/CLAUDE.md /home/agent/.claude/CLAUDE.md

USER agent
ENTRYPOINT ["webui"]
