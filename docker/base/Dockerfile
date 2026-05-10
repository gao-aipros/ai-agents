FROM debian:trixie

ARG AGENT_UID=1000
ARG AGENT_GID=1000

# Install gnupg first (needed by apt for signed repos), then add GitHub CLI repo
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    gnupg \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
    -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
    > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends \
    bash \
    curl \
    gh \
    git \
    gnupg \
    jq \
    openssh-client \
    python3 \
    unzip \
    wget \
    vim \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g "$AGENT_GID" agent \
    && useradd -m -d /home/agent -s /bin/bash -u "$AGENT_UID" -g "$AGENT_GID" agent

RUN mkdir -p /workspace /home/agent/.config && chown agent:agent /workspace /home/agent/.config

ARG GH_TOKEN
ARG GITHUB_TOKEN
ARG GH_HOST=github.com
ENV GH_TOKEN=${GH_TOKEN} \
    GITHUB_TOKEN=${GITHUB_TOKEN} \
    GH_HOST=${GH_HOST}

WORKDIR /workspace
