#!/usr/bin/env python3
"""PreToolUse guard: blocks master from implementing or reviewing.

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

FORBIDDEN_BASH_PATTERNS = [
    # gh commands the master must never run
    r"\bgh\s+pr\s+create\b",
    r"\bgh\s+pr\s+review\b",
    r"\bgh\s+pr\s+merge\b",
    r"\bgh\s+pr\s+close\b",
    r"\bgh\s+pr\s+reopen\b",
    # Destructive git commands
    r"\bgit\s+commit\b",
    r"\bgit\s+push\b",
    r"\bgit\s+branch\b",
    r"\bgit\s+tag\b",
    r"\bgit\s+checkout\s+-b\b",
    r"\bgit\s+merge\b",
    r"\bgit\s+rebase\b",
    r"\bgit\s+cherry-pick\b",
    r"\bgit\s+reset\b",
    r"\bgit\s+stash\b",
    # Build/test commands
    r"\bgo\s+build\b",
    r"\bgo\s+test\b",
    r"\bgo\s+run\b",
    r"\bgo\s+install\b",
    r"\bnpm\s+",
    r"\bpip\s+install\b",
    r"\bmake\b",
    r"\bdocker\s+build\b",
    r"\bdocker\s+run\b",
    r"\bdocker\s+push\b",
    # Write-like operations via shell
    r">\s*\S",
    r"\btee\b",
    r"\bdd\s+of=",
]

ALLOWED_MD_DIRS = [
    "docs/",
    ".claude/",
]


def block(reason: str) -> None:
    msg = f"[MASTER-GUARD] BLOCKED: {reason}"
    print(msg, file=sys.stderr)
    sys.exit(1)


def allow() -> None:
    sys.exit(0)


def is_md_file(file_path: str) -> bool:
    return file_path.endswith(".md") or file_path.endswith(".mdx")


def check_write(file_path: str) -> None:
    if not is_md_file(file_path):
        block(
            f"Write/Edit to non-Markdown file: {file_path}. "
            f"Master may only write .md files. Delegate implementation to a worker."
        )


def check_bash(command: str) -> None:
    for pattern in FORBIDDEN_BASH_PATTERNS:
        if re.search(pattern, command):
            block(
                f"Forbidden command pattern matched '{pattern}': {command[:120]}. "
                f"Master must delegate this action to a worker."
            )


def main() -> None:
    if tool_name in ("Write", "Edit", "NotebookEdit"):
        file_path = tool_input.get("file_path", "")
        if not file_path:
            block(f"{tool_name} called without file_path")
        check_write(file_path)
        allow()

    elif tool_name == "Bash":
        command = tool_input.get("command", "")
        if not command:
            block("Bash called without command")
        check_bash(command)
        allow()

    # All other tools (Read, Task*, glob, grep, etc.) are allowed
    allow()


if __name__ == "__main__":
    main()
