ARG WORKER_BASE_IMAGE=worker-base:latest

# Build stage: install Copilot CLI
FROM debian:trixie AS copilot-build

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    curl \
    git \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://gh.io/copilot-install | bash

# Build stage: download skills
FROM debian:trixie AS skills-download

RUN apt-get update && apt-get install -y --no-install-recommends \
    git ca-certificates \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

ARG SKILLS_REF=67bce91c80cd1020a4f068ced32d0281656842ad
RUN git clone --depth 1 --filter=blob:none \
    --branch "$SKILLS_REF" \
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
    done

# Final stage
FROM ${WORKER_BASE_IMAGE}

USER root

# Copilot CLI
COPY --from=copilot-build /usr/local/bin/copilot /usr/local/bin/copilot

# DeepSeek provider config for Copilot (non-sensitive defaults)
ENV COPILOT_PROVIDER_TYPE=anthropic \
    COPILOT_PROVIDER_BASE_URL=https://api.deepseek.com/anthropic \
    COPILOT_MODEL=deepseek-v4-pro[1m] \
    COPILOT_PROVIDER_MAX_PROMPT_TOKENS=840000 \
    COPILOT_PROVIDER_MAX_OUTPUT_TOKENS=128000 \
    AGENT_CMD="copilot --yolo -p"

RUN mkdir -p /home/agent/.copilot && chown -R agent:agent /home/agent/.copilot
COPY --chown=agent:agent docker/copilot/copilot-instructions.md /home/agent/.copilot/copilot-instructions.md
COPY --from=skills-download --chown=agent:agent /tmp/skills-out /home/agent/.copilot/skills/
COPY --chown=agent:agent docs/agents/ /home/agent/.copilot/agents-config/

USER agent
ENTRYPOINT ["/usr/local/bin/worker-go", "copilot"]
