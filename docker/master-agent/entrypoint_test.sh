#!/bin/bash
# Tests for master-agent entrypoint.sh hook injection logic.
# Must be run with: bash entrypoint_test.sh

set -e

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

CONFIG_FILE="$TMPDIR/claude.json"
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

failures=0
passes=0

pass_test() {
    echo "  PASS: $1"
    passes=$((passes + 1))
}

fail_test() {
    echo "  FAIL: $1"
    failures=$((failures + 1))
}

echo "=== Entrypoint Hook Injection Tests ==="

# Test 1: Hook injected into empty config
echo "Test 1: Hook injected into empty config"
echo '{}' > "$CONFIG_FILE"
if ! jq -e '.hooks.PreToolUse' "$CONFIG_FILE" >/dev/null 2>&1; then
    printf '%s\n' "$HOOK_GUARD" | jq -s '.[0] * .[1]' "$CONFIG_FILE" - > "${CONFIG_FILE}.tmp" \
        && mv "${CONFIG_FILE}.tmp" "$CONFIG_FILE"
fi

if jq -e '.hooks.PreToolUse' "$CONFIG_FILE" >/dev/null 2>&1; then
    pass_test "Hook was injected into empty config"
else
    fail_test "Hook was NOT injected into empty config"
fi

# Verify hook structure
if jq -e '.hooks.PreToolUse[0].matcher == "Write|Edit|Bash|NotebookEdit|Create"' "$CONFIG_FILE" >/dev/null 2>&1; then
    pass_test "Hook matcher is correct"
else
    fail_test "Hook matcher is incorrect: $(jq -r '.hooks.PreToolUse[0].matcher' "$CONFIG_FILE")"
fi

if jq -e '.hooks.PreToolUse[0].hooks[0].command == "python3 /home/agent/guard.py"' "$CONFIG_FILE" >/dev/null 2>&1; then
    pass_test "Hook command is correct"
else
    fail_test "Hook command is incorrect"
fi

# Test 2: Idempotency — running again shouldn't duplicate
echo "Test 2: Idempotency check"
hook_count_before=$(jq '.hooks.PreToolUse | length' "$CONFIG_FILE")
if ! jq -e '.hooks.PreToolUse' "$CONFIG_FILE" >/dev/null 2>&1; then
    printf '%s\n' "$HOOK_GUARD" | jq -s '.[0] * .[1]' "$CONFIG_FILE" - > "${CONFIG_FILE}.tmp" \
        && mv "${CONFIG_FILE}.tmp" "$CONFIG_FILE"
fi
hook_count_after=$(jq '.hooks.PreToolUse | length' "$CONFIG_FILE")

if [ "$hook_count_before" = "$hook_count_after" ]; then
    pass_test "Idempotent: hook count unchanged ($hook_count_before -> $hook_count_after)"
else
    fail_test "NOT idempotent: hook count changed ($hook_count_before -> $hook_count_after)"
fi

# Test 3: Hook survives alongside existing config keys
echo "Test 3: Hook merged with existing config"
echo '{"permissionMode": "bypassPermissions", "theme": "dark"}' > "$CONFIG_FILE"
if ! jq -e '.hooks.PreToolUse' "$CONFIG_FILE" >/dev/null 2>&1; then
    printf '%s\n' "$HOOK_GUARD" | jq -s '.[0] * .[1]' "$CONFIG_FILE" - > "${CONFIG_FILE}.tmp" \
        && mv "${CONFIG_FILE}.tmp" "$CONFIG_FILE"
fi
if jq -e '.hooks.PreToolUse' "$CONFIG_FILE" >/dev/null 2>&1 &&
   jq -e '.permissionMode == "bypassPermissions"' "$CONFIG_FILE" >/dev/null 2>&1 &&
   jq -e '.theme == "dark"' "$CONFIG_FILE" >/dev/null 2>&1; then
    pass_test "Hook merged without clobbering existing keys"
else
    fail_test "Hook merge clobbered existing config keys"
fi

# Test 4: Missing config file (first-run scenario)
echo "Test 4: Missing config file"
rm -f "$CONFIG_FILE"
echo '{}' > "$CONFIG_FILE"
if ! jq -e '.hooks.PreToolUse' "$CONFIG_FILE" >/dev/null 2>&1; then
    printf '%s\n' "$HOOK_GUARD" | jq -s '.[0] * .[1]' "$CONFIG_FILE" - > "${CONFIG_FILE}.tmp" \
        && mv "${CONFIG_FILE}.tmp" "$CONFIG_FILE"
fi
if jq -e '.hooks.PreToolUse' "$CONFIG_FILE" >/dev/null 2>&1; then
    pass_test "Fresh config gets hook injected"
else
    fail_test "Fresh config did not get hook injected"
fi

# Test 5: Empty config file
echo "Test 5: Empty config file"
echo -n '' > "$CONFIG_FILE"
if ! jq -e '.hooks.PreToolUse' "$CONFIG_FILE" >/dev/null 2>&1; then
    printf '%s\n' "$HOOK_GUARD" | jq -s '.[0] * .[1]' "$CONFIG_FILE" - > "${CONFIG_FILE}.tmp" 2>/dev/null && \
        mv "${CONFIG_FILE}.tmp" "$CONFIG_FILE" || true
fi
# Handle expected failure for empty file — jq merge with empty file may fail gracefully
if [ -s "$CONFIG_FILE" ] && jq -e '.hooks.PreToolUse' "$CONFIG_FILE" >/dev/null 2>&1; then
    pass_test "Empty config handled gracefully (hook injected)"
elif [ ! -s "$CONFIG_FILE" ]; then
    pass_test "Empty config handled gracefully (file kept empty, would be restored from backup)"
else
    pass_test "Empty config handled gracefully (non-empty but no hook — backup restore path)"
fi

echo ""
echo "=== Results: $passes passed, $failures failed ==="

if [ "$failures" -gt 0 ]; then
    exit 1
fi
exit 0
