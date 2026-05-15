FROM golang:1.26.3-trixie AS build

RUN git clone https://github.com/ZhiYi-R/moon-bridge.git /src
WORKDIR /src
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/moonbridge ./cmd/moonbridge

FROM debian:trixie

COPY --from=build /out/moonbridge /usr/local/bin/moonbridge

COPY docker/moon-bridge/moonbridge-config.yml /etc/moonbridge/config.yml.tmpl
COPY docker/moon-bridge/entrypoint.sh /entrypoint.sh

EXPOSE 38440
ENTRYPOINT ["/entrypoint.sh"]
