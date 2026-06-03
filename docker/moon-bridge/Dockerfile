# MOON_BRIDGE_REF=f68cceb2c8
# ^ single source of truth for the moon-bridge ref; CI reads this line to
#   decide which commit to clone and build.

FROM debian:trixie

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

ARG TARGETARCH
COPY out/${TARGETARCH}/moonbridge /usr/local/bin/moonbridge

COPY docker/moon-bridge/moonbridge-config.yml /etc/moonbridge/config.yml.tmpl
COPY docker/moon-bridge/entrypoint.sh /entrypoint.sh

EXPOSE 38440
ENTRYPOINT ["/entrypoint.sh"]
