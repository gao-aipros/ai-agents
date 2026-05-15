ARG WORKER_BASE_IMAGE=worker-base:latest

# Build stage: install OpenCode CLI
FROM debian:trixie AS opencode-build

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    curl \
    git \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://opencode.ai/install | bash -s -- --no-modify-path

# Final stage
FROM ${WORKER_BASE_IMAGE}

USER root

# OpenCode CLI
COPY --from=opencode-build /root/.opencode/bin/opencode /usr/local/bin/opencode

RUN mkdir -p /home/agent/.config/opencode && chown -R agent:agent /home/agent/.config

# Provider/model defaults
ENV OPENCODE_PROVIDER=deepseek \
    OPENCODE_MODEL=deepseek-v4-pro

ENV AGENT_CMD="opencode -m ${OPENCODE_PROVIDER}/${OPENCODE_MODEL} run --dangerously-skip-permissions"

COPY --chown=agent:agent docker/opencode/AGENTS.md /home/agent/AGENTS.md
COPY --chown=agent:agent docker/opencode/opencode.json /home/agent/.config/opencode/opencode.json

USER agent
RUN opencode models --refresh

ENTRYPOINT ["/usr/local/bin/worker-go", "opencode"]
