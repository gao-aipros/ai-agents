FROM golang:1.26.3-trixie AS build

ARG MOON_BRIDGE_REF=0023e286e9bf

RUN git clone https://github.com/ZhiYi-R/moon-bridge.git /src && \
    cd /src && \
    git checkout ${MOON_BRIDGE_REF}
WORKDIR /src
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/moonbridge ./cmd/moonbridge

FROM debian:trixie

COPY --from=build /out/moonbridge /usr/local/bin/moonbridge

COPY docker/moon-bridge/moonbridge-config.yml /etc/moonbridge/config.yml.tmpl
COPY docker/moon-bridge/entrypoint.sh /entrypoint.sh

EXPOSE 38440
ENTRYPOINT ["/entrypoint.sh"]
