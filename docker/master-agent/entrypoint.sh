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

exec webui "$@"
