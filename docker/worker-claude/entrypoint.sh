#!/bin/bash
set -e

CONFIG_FILE="/home/agent/.claude/claude.json"
SYMLINK="/home/agent/.claude.json"

# Ensure symlink exists (idempotent)
if [ ! -L "$SYMLINK" ]; then
    ln -sf "$CONFIG_FILE" "$SYMLINK"
fi

# Ensure claude.json exists
if [ ! -f "$CONFIG_FILE" ]; then
    echo '{}' > "$CONFIG_FILE"
fi

# Ensure worker-enforcement hook is installed in claude.json
# This blocks master-only task commands (task enqueue, task group-wait, etc.)
HOOK_GUARD='
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "python3 /home/agent/guard.py"
          }
        ]
      }
    ]
  }
}'

if ! jq -e '.hooks.PreToolUse' "$CONFIG_FILE" >/dev/null 2>&1; then
    echo "Installing worker-enforcement hook in claude.json"
    printf '%s\n' "$HOOK_GUARD" | jq -s '.[0] * .[1]' "$CONFIG_FILE" - > "${CONFIG_FILE}.tmp" \
        && mv "${CONFIG_FILE}.tmp" "$CONFIG_FILE"
fi

exec /usr/local/bin/worker-go "$@"
