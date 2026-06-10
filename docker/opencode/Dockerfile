ARG WORKER_BASE_IMAGE=worker-base:latest

# Build stage: download OpenCode CLI
FROM debian:trixie AS opencode-download

ARG OPENCODE_VERSION=1.17.0
ARG TARGETARCH

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl tar && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

RUN case "${TARGETARCH}" in \
      amd64) ARCH=x64 ;; \
      arm64) ARCH=arm64 ;; \
      *) echo "Unsupported arch: ${TARGETARCH}"; exit 1 ;; \
    esac && \
    echo "Downloading opencode version ${OPENCODE_VERSION} for ${ARCH}" && \
    curl -sSL "https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}/opencode-linux-${ARCH}.tar.gz" \
      | tar -xz -C /usr/local/bin opencode && \
    chmod +x /usr/local/bin/opencode

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

# OpenCode CLI
COPY --from=opencode-download /usr/local/bin/opencode /usr/local/bin/opencode

RUN mkdir -p /home/agent/.config/opencode && chown -R agent:agent /home/agent/.config

# Provider/model defaults
ENV OPENCODE_PROVIDER=deepseek \
    OPENCODE_MODEL=deepseek-v4-pro

ENV AGENT_CMD="opencode -m ${OPENCODE_PROVIDER}/${OPENCODE_MODEL} run --dangerously-skip-permissions --format json" \
    AGENT_TYPE=opencode

COPY --chown=agent:agent docker/opencode/AGENTS.md /home/agent/.config/opencode/AGENTS.md
COPY --chown=agent:agent docker/opencode/opencode.json /home/agent/.config/opencode/opencode.json
COPY --from=skills-download --chown=agent:agent /tmp/skills-out /home/agent/.config/opencode/skills/
COPY --chown=agent:agent docs/agents/ /home/agent/.config/opencode/agents-config/

USER agent
ENTRYPOINT ["/usr/local/bin/worker-go"]
