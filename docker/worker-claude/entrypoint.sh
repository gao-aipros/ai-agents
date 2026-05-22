#!/bin/bash
set -e

CONFIG_FILE="/home/agent/.claude/claude.json"
SYMLINK="/home/agent/.claude.json"
BACKUP_DIR="/home/agent/.claude/backups"

# Ensure symlink exists (idempotent)
if [ ! -L "$SYMLINK" ]; then
    ln -sf "$CONFIG_FILE" "$SYMLINK"
fi

# Restore config from backup if missing or empty
if [ ! -f "$CONFIG_FILE" ] || [ ! -s "$CONFIG_FILE" ]; then
    latest_backup=$(ls -t "$BACKUP_DIR"/.claude.json.backup.* 2>/dev/null | head -1)
    if [ -n "$latest_backup" ]; then
        echo "Restoring .claude.json from backup: $latest_backup"
        cp "$latest_backup" "$CONFIG_FILE"
    fi
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

if ! jq -e '.hooks.PreToolUse[]?.hooks[]?.command == "python3 /home/agent/guard.py"' "$CONFIG_FILE" >/dev/null 2>&1; then
    echo "Installing worker-enforcement hook in claude.json"
    printf '%s\n' "$HOOK_GUARD" | jq -s '.[0] * .[1]' "$CONFIG_FILE" - > "${CONFIG_FILE}.tmp" \
        && mv "${CONFIG_FILE}.tmp" "$CONFIG_FILE"
fi

exec /usr/local/bin/worker-go "$@"
