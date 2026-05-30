#!/bin/bash
set -e

# Sync reference config files into the volume on every start.
# This ensures CLAUDE.md, skills, and agents-config from the new image
# replace any stale copies in the persisted volume after a redeploy.
# Runtime data (sessions, claude.json, backups) is not touched.
REF_DIR="/opt/claude-reference"
CLAUDE_DIR="/home/agent/.claude"

if [ -d "$REF_DIR" ] && [ -f "$REF_DIR/CLAUDE.md" ]; then
    cp "$REF_DIR/CLAUDE.md" "$CLAUDE_DIR/CLAUDE.md"
fi
if [ -d "$REF_DIR" ] && [ -f "$REF_DIR/settings.json" ]; then
    cp "$REF_DIR/settings.json" "$CLAUDE_DIR/settings.json"
fi
if [ -d "$REF_DIR/skills" ] && ls -A "$REF_DIR/skills" >/dev/null 2>&1; then
    rm -rf "$CLAUDE_DIR/skills"
    cp -r "$REF_DIR/skills" "$CLAUDE_DIR/skills"
fi
if [ -d "$REF_DIR/agents-config" ] && ls -A "$REF_DIR/agents-config" >/dev/null 2>&1; then
    rm -rf "$CLAUDE_DIR/agents-config"
    cp -r "$REF_DIR/agents-config" "$CLAUDE_DIR/agents-config"
fi

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

# Ensure master-enforcement hook is installed in claude.json
# This blocks Edit/Write to non-.md files and forbidden gh/git commands
HOOK_GUARD='
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Write|Edit|Bash|NotebookEdit|Create",
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
    echo "Installing master-enforcement hook in claude.json"
    printf '%s\n' "$HOOK_GUARD" | jq -s '.[0] * .[1]' "$CONFIG_FILE" - > "${CONFIG_FILE}.tmp" \
        && mv "${CONFIG_FILE}.tmp" "$CONFIG_FILE"
fi

# Save original THREAD for guard defense-in-depth.
# The guard uses this to detect and block any attempt to overwrite THREAD.
export ORIGINAL_THREAD="${THREAD:-}"

exec webui "$@"
