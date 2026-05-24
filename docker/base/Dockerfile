FROM debian:trixie AS gh-fetcher

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl jq \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

RUN ARCH=$(dpkg --print-architecture) \
    && DOWNLOAD_URL=$(curl -fsSL https://api.github.com/repos/cli/cli/releases/latest \
        | jq -r --arg arch "$ARCH" \
            '.assets[].browser_download_url | select(test("linux_" + $arch + ".tar.gz"))') \
    && curl -fsSL "$DOWNLOAD_URL" \
      | tar -xz -C /tmp \
    && cp /tmp/gh_*/bin/gh /usr/local/bin/gh \
    && chmod +x /usr/local/bin/gh \
    && rm -rf /tmp/gh_*

FROM debian:trixie

ARG AGENT_UID=1000
ARG AGENT_GID=1000

COPY --from=gh-fetcher /usr/local/bin/gh /usr/local/bin/gh

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    curl \
    git \
    gnupg \
    jq \
    openssh-client \
    python3 \
    redis-tools \
    unzip \
    wget \
    vim \
    xxd \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g "$AGENT_GID" agent \
    && useradd -m -d /home/agent -s /bin/bash -u "$AGENT_UID" -g "$AGENT_GID" agent

RUN mkdir -p /workspace /home/agent/.config && chown agent:agent /workspace /home/agent/.config
VOLUME /workspace

ENV GH_HOST=github.com

WORKDIR /workspace
