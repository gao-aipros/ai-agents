#!/bin/bash
# Copy AGENTS.md into workspace (runs as root, works regardless of volume mount ownership)
cp -f /home/agent/AGENTS.md /workspace/AGENTS.md 2>/dev/null || true
chown agent:agent /workspace/AGENTS.md 2>/dev/null || true
# Switch to agent user and run copilot
exec runuser -u agent -- copilot -p --allow-all "$@"
