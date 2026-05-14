ARG BASE_IMAGE=ai-base:latest

FROM ${BASE_IMAGE}

USER root

# Go SDK via tarball (avoids pulling ~500 MB golang:latest)
ARG GO_VERSION=1.26.3
ARG TARGETARCH
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" -o /tmp/go.tar.gz \
    && tar -xzf /tmp/go.tar.gz -C /usr/local \
    && rm /tmp/go.tar.gz

ENV GOROOT=/usr/local/go \
    GOPATH=/home/agent/go \
    PATH=/usr/local/go/bin:${PATH}

# CGO support
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc \
    libc6-dev \
    make \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /home/agent/go && chown -R agent:agent /home/agent/go

# Pre-built worker binary (artifact upload/download strips execute bit)
COPY out/${TARGETARCH}/worker-go /usr/local/bin/worker-go
RUN chmod +x /usr/local/bin/worker-go
