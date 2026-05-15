#!/bin/bash
set -e

[[ -z "$DEEPSEEK_API_KEY" ]] && { echo "DEEPSEEK_API_KEY is required"; exit 1; }
[[ "$DEEPSEEK_API_KEY" == *"|"* ]] && { echo "DEEPSEEK_API_KEY contains invalid character '|'"; exit 1; }

sed "s|\${DEEPSEEK_API_KEY}|${DEEPSEEK_API_KEY}|g" \
    /etc/moonbridge/config.yml.tmpl > /tmp/config.yml

exec moonbridge -config /tmp/config.yml
