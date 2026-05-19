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

# Build stage: download skills
FROM debian:trixie AS skills-download

RUN apt-get update && apt-get install -y --no-install-recommends \
    git ca-certificates \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

ARG SKILLS_REF=67bce91c80cd1020a4f068ced32d0281656842ad
RUN git clone --filter=blob:none \
    https://github.com/mattpocock/skills.git /tmp/skills \
    && git -C /tmp/skills checkout "$SKILLS_REF" \
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

# OpenCode CLI
COPY --from=opencode-build /root/.opencode/bin/opencode /usr/local/bin/opencode

RUN mkdir -p /home/agent/.config/opencode && chown -R agent:agent /home/agent/.config

# Provider/model defaults
ENV OPENCODE_PROVIDER=deepseek \
    OPENCODE_MODEL=deepseek-v4-pro

ENV AGENT_CMD="opencode -m ${OPENCODE_PROVIDER}/${OPENCODE_MODEL} run --dangerously-skip-permissions"

COPY --chown=agent:agent docker/opencode/AGENTS.md /home/agent/.config/opencode/AGENTS.md
COPY --chown=agent:agent docker/opencode/opencode.json /home/agent/.config/opencode/opencode.json
COPY --from=skills-download --chown=agent:agent /tmp/skills-out /home/agent/.config/opencode/skills/
COPY --chown=agent:agent docker/skills/code-review/ /home/agent/.config/opencode/skills/code-review/
COPY --chown=agent:agent docs/agents/ /home/agent/.config/opencode/agents-config/

USER agent
RUN opencode models --refresh

ENTRYPOINT ["/usr/local/bin/worker-go", "opencode"]
