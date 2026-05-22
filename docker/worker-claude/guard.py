#!/usr/bin/env python3
"""PreToolUse guard: prevents worker from performing master-only operations.

Reads CLAUDE_TOOL_NAME and CLAUDE_TOOL_INPUT from the environment.
Exits 0 to allow the tool call, 1 to block it.
"""

import json
import os
import re
import sys

tool_name = os.environ.get("CLAUDE_TOOL_NAME", "")
tool_input_str = os.environ.get("CLAUDE_TOOL_INPUT", "{}")

try:
    tool_input = json.loads(tool_input_str)
except json.JSONDecodeError:
    tool_input = {}

# Workers must never run master-only task management commands
FORBIDDEN_BASH_PATTERNS = [
    r"\btask\s+enqueue\b",
    r"\btask\s+thread-create\b",
    r"\btask\s+thread-update\b",
    r"\btask\s+thread-cleanup\b",
    r"\btask\s+group-wait\b",
    r"\btask\s+unlock\b",
    r"\btask\s+requeue-stale\b",
    r"\btask\s+cancel\b",
    r"\btask\s+events\b",
    r"\btask\s+list\b",
    r"\btask\s+thread-list\b",
]


def block(reason: str) -> None:
    msg = f"[WORKER-GUARD] BLOCKED: {reason}"
    print(msg, file=sys.stderr)
    sys.exit(1)


def allow() -> None:
    sys.exit(0)


def check_bash(command: str) -> None:
    for pattern in FORBIDDEN_BASH_PATTERNS:
        if re.search(pattern, command):
            block(
                f"Master-only command matched '{pattern}': {command[:120]}. "
                f"Workers execute tasks — they do not delegate or manage threads."
            )


def main() -> None:
    if tool_name == "Bash":
        command = tool_input.get("command", "")
        if not command:
            block("Bash called without command")
        check_bash(command)

    # Workers can write any file type, use any tool — unlike master
    allow()


if __name__ == "__main__":
    main()
