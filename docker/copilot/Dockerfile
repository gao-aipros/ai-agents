ARG WORKER_BASE_IMAGE=worker-base:latest

# Build stage: download Copilot CLI
FROM debian:trixie AS copilot-download

ARG COPILOT_VERSION=1.0.57
ARG TARGETARCH

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl tar && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

RUN case "${TARGETARCH}" in \
      amd64) ARCH=x64 ;; \
      arm64) ARCH=arm64 ;; \
      *) echo "Unsupported arch: ${TARGETARCH}"; exit 1 ;; \
    esac && \
    echo "Downloading copilot version ${COPILOT_VERSION} for ${ARCH}" && \
    curl -sSL "https://github.com/github/copilot-cli/releases/download/v${COPILOT_VERSION}/copilot-linux-${ARCH}.tar.gz" \
      | tar -xz -C /usr/local/bin copilot && \
    chmod +x /usr/local/bin/copilot

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

# Copilot CLI
COPY --from=copilot-download /usr/local/bin/copilot /usr/local/bin/copilot

# DeepSeek provider config for Copilot (non-sensitive defaults)
ENV COPILOT_PROVIDER_TYPE=anthropic \
    COPILOT_PROVIDER_BASE_URL=https://api.deepseek.com/anthropic \
    COPILOT_MODEL=deepseek-v4-pro[1m] \
    COPILOT_PROVIDER_MAX_PROMPT_TOKENS=840000 \
    COPILOT_PROVIDER_MAX_OUTPUT_TOKENS=128000 \
    AGENT_CMD="copilot --yolo --session-id=__SESSION_ID__ -p" \
    AGENT_TYPE=copilot

RUN mkdir -p /home/agent/.copilot && chown -R agent:agent /home/agent/.copilot
COPY --chown=agent:agent docker/copilot/copilot-instructions.md /home/agent/.copilot/copilot-instructions.md
COPY --from=skills-download --chown=agent:agent /tmp/skills-out /home/agent/.copilot/skills/
COPY --chown=agent:agent docs/agents/ /home/agent/.copilot/agents-config/

USER agent
ENTRYPOINT ["/usr/local/bin/worker-go"]
