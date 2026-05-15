#!/bin/bash

cleanup() {
    echo "Shutting down..."
    kill "$MOON_PID" 2>/dev/null
    kill "$WORKER_PID" 2>/dev/null
    wait
    exit 0
}
trap cleanup SIGTERM SIGINT

# Validate required env var; also reject keys with '|' (sed delimiter)
[[ -z "$DEEPSEEK_API_KEY" ]] && { echo "DEEPSEEK_API_KEY is required"; exit 1; }
[[ "$DEEPSEEK_API_KEY" == *"|"* ]] && { echo "DEEPSEEK_API_KEY contains invalid character '|'"; exit 1; }

# Inject API key into moon-bridge config
sed "s|\${DEEPSEEK_API_KEY}|${DEEPSEEK_API_KEY}|g" \
    /home/agent/.codex/moonbridge-config.yml > /tmp/moonbridge-config.yml

moonbridge -config /tmp/moonbridge-config.yml &
MOON_PID=$!

# Wait for moon-bridge health with timeout
if ! timeout 30 bash -c 'until curl -fsSL http://localhost:38440/health; do sleep 0.5; done'; then
    echo "moon-bridge failed to start"
    kill "$MOON_PID" 2>/dev/null
    exit 1
fi

worker-go codex &
WORKER_PID=$!

wait -n || true
cleanup
