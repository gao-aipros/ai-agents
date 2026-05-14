ARG BASE_IMAGE=ai-base:latest

FROM ${BASE_IMAGE}

USER root

# Go SDK via tarball (avoids pulling ~500 MB golang:latest)
ARG GO_VERSION=1.26.3
ARG TARGETARCH
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
    | tar -xzC /usr/local

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

# Pre-built worker binary
ARG TARGETARCH
COPY out/${TARGETARCH}/worker-go /usr/local/bin/worker-go
