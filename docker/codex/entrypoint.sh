#!/bin/bash
set -e

cleanup() {
    echo "Shutting down..."
    kill "$MOON_PID" 2>/dev/null
    kill "$WORKER_PID" 2>/dev/null
    wait
    exit 0
}
trap cleanup SIGTERM SIGINT

# Inject API key into moon-bridge config
sed "s|\${DEEPSEEK_API_KEY}|${DEEPSEEK_API_KEY}|g" \
    /home/agent/.codex/moonbridge-config.yml > /tmp/moonbridge-config.yml

moonbridge -config /tmp/moonbridge-config.yml &
MOON_PID=$!

until curl -fsSL http://localhost:38440/health; do sleep 0.5; done

worker-go codex &
WORKER_PID=$!

wait -n
cleanup
