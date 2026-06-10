ARG WORKER_BASE_IMAGE=worker-base:latest

# Build stage: download Codex CLI binary
FROM debian:trixie AS codex-download

ARG CODEX_VERSION=0.139.0
ARG TARGETARCH

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl tar && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

RUN case "${TARGETARCH}" in \
      amd64) ARCH=x86_64-unknown-linux-musl ;; \
      arm64) ARCH=aarch64-unknown-linux-musl ;; \
      *) echo "Unsupported arch: ${TARGETARCH}"; exit 1 ;; \
    esac && \
    echo "Downloading codex version ${CODEX_VERSION} for ${ARCH}" && \
    curl -sSL "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-${ARCH}.tar.gz" \
      | tar -xz -C /usr/local/bin && \
    mv "/usr/local/bin/codex-${ARCH}" /usr/local/bin/codex && \
    chmod +x /usr/local/bin/codex

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

# Codex CLI
COPY --from=codex-download /usr/local/bin/codex /usr/local/bin/codex

RUN mkdir -p /home/agent/.codex && chown -R agent:agent /home/agent/.codex

# Codex config (static — always points at moon-bridge service)
COPY --chown=agent:agent docker/codex/config.toml /home/agent/.codex/config.toml
COPY --chown=agent:agent docker/codex/models_catalog.json /home/agent/.codex/models_catalog.json

# Agent instructions (global scope: ~/.codex/AGENTS.md)
COPY --chown=agent:agent docker/codex/AGENTS.md /home/agent/.codex/AGENTS.md
COPY --from=skills-download --chown=agent:agent /tmp/skills-out /home/agent/.codex/skills/
COPY --chown=agent:agent docs/agents/ /home/agent/.codex/agents-config/

ENV AGENT_CMD="codex exec --json --yolo --sandbox danger-full-access --skip-git-repo-check" \
    AGENT_TYPE=codex

USER agent
ENTRYPOINT ["worker-go"]
