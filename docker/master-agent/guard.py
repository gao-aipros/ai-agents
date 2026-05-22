#!/usr/bin/env python3
"""PreToolUse guard: blocks master from implementing or reviewing.

Reads CLAUDE_TOOL_NAME and CLAUDE_TOOL_INPUT from the environment.
Exits 0 to allow the tool call, 1 to block it.

Note: regex-based enforcement does not block quoted subcommands
(e.g., git 'checkout'). The HARD CONSTRAINT section in CLAUDE.md
is the primary enforcement mechanism; this guard is a safety net.
"""

import json
import os
import re
import sys

# Only allow writing .md files within these directories
ALLOWED_MD_DIRS = [
    "docs/",
    ".claude/",
]

FORBIDDEN_BASH_PATTERNS = [
    # gh write commands the master must never run
    r"\bgh\s+pr\s+create\b",
    r"\bgh\s+pr\s+review\b",
    r"\bgh\s+pr\s+merge\b",
    r"\bgh\s+pr\s+close\b",
    r"\bgh\s+pr\s+reopen\b",
    r"\bgh\s+pr\s+comment\b",
    r"\bgh\s+pr\s+edit\b",
    r"\bgh\s+issue\s+create\b",
    r"\bgh\s+issue\s+edit\b",
    r"\bgh\s+issue\s+comment\b",
    r"\bgh\s+api\b",                  # could POST/PUT/DELETE
    r"\bgh\s+repo\s+create\b",
    r"\bgh\s+repo\s+delete\b",
    r"\bgh\s+repo\s+edit\b",
    r"\bgh\s+release\s+create\b",
    r"\bgh\s+release\s+delete\b",
    r"\bgh\s+secret\s+set\b",
    r"\bgh\s+variable\s+set\b",
    # Destructive git commands
    r"\bgit\s+commit\b",
    r"\bgit\s+push\b",
    r"\bgit\s+branch\b",
    r"\bgit\s+tag\b",
    r"\bgit\s+checkout\b",
    r"\bgit\s+merge\b",
    r"\bgit\s+rebase\b",
    r"\bgit\s+cherry-pick\b",
    r"\bgit\s+reset\b",
    r"\bgit\s+stash\b",
    r"\bgit\s+revert\b",
    r"\bgit\s+rm\b",
    r"\bgit\s+fetch\b",
    r"\bgit\s+pull\b",
    # Build/test commands
    r"\bgo\s+build\b",
    r"\bgo\s+test\b",
    r"\bgo\s+run\b",
    r"\bgo\s+install\b",
    r"(?:^|[;&|])\s*npm\s+",
    r"\bpip\s+install\b",
    r"\bmake\b",
    r"\bdocker\s+build\b",
    r"\bdocker\s+run\b",
    r"\bdocker\s+push\b",
    # Filesystem write commands that could create non-.md files
    r"\btouch\s+",
    r"\brm\s+",
    r"\bchmod\s+",
    r"\bcp\s+",
    r"\bmv\s+",
    # Shell redirects — only flag when target looks like a filesystem path.
    # /dev/null is excluded (safe discard). Boundary check prevents
    # false positives on > inside jq filters, string literals, comparisons.
    r"(?:^|[\s;&|])\s*\d?>>?\s*(?!/dev/null)(?:/dev/|/tmp/|/workspace/|/home/\S|[./]\S|\S+\.\w{1,6})",
    r"\btee\b",
    r"\bdd\b",
]


def block(reason: str) -> None:
    msg = f"[MASTER-GUARD] BLOCKED: {reason}"
    print(msg, file=sys.stderr)
    sys.exit(1)


def allow() -> None:
    sys.exit(0)


def is_md_file(file_path: str) -> bool:
    return file_path.endswith(".md") or file_path.endswith(".mdx")


def is_allowed_dir(file_path: str) -> bool:
    for d in ALLOWED_MD_DIRS:
        if file_path.startswith(d):
            return True
    return False


def check_write(file_path: str) -> None:
    if not is_md_file(file_path):
        block(
            f"Write/Edit to non-Markdown file: {file_path}. "
            f"Master may only write .md files. Delegate implementation to a worker."
        )
    if not is_allowed_dir(file_path):
        block(
            f"Write/Edit to .md file outside allowed directories: {file_path}. "
            f"Master may only write within: {', '.join(ALLOWED_MD_DIRS)}."
        )


def check_bash(command: str) -> None:
    for pattern in FORBIDDEN_BASH_PATTERNS:
        if re.search(pattern, command):
            block(
                f"Forbidden command matched '{pattern}': {command[:120]}. "
                f"Master must delegate this action to a worker."
            )


def main() -> None:
    tool_name = os.environ.get("CLAUDE_TOOL_NAME", "")
    tool_input_str = os.environ.get("CLAUDE_TOOL_INPUT", "{}")

    try:
        tool_input = json.loads(tool_input_str)
    except json.JSONDecodeError:
        tool_input = {}

    if tool_name in ("Write", "Edit", "NotebookEdit", "Create"):
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

    # All other tools (Read, Task*, glob, grep, Agent, etc.) are allowed
    allow()


if __name__ == "__main__":
    main()
