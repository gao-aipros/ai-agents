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

THREAD_MODIFY_PATTERNS = [
    # export THREAD=value
    r'\bexport\s+THREAD\s*=',
    # declare THREAD=value (with or without -x flag)
    r'\bdeclare\s+.*THREAD\s*=',
    # typeset THREAD=value (with or without -x flag)
    r'\btypeset\s+.*THREAD\s*=',
    # Inline env var: THREAD=value cmd (preceded by word boundary, line start,
    # semicolon, pipe, or subshell/group-command grouping operators)
    r'(?:^|[\s;&|(){}])\s*THREAD\s*=\s*\S',
    # readonly THREAD=value (sets THREAD and makes it read-only)
    r'\breadonly\s+THREAD\s*=',
    # env THREAD=value cmd
    r'\benv\s+.*\bTHREAD\s*=',
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


def check_thread_create(command: str) -> None:
    """Validate task thread-create commands: --parent must be $THREAD, --id must be valid."""
    m = re.search(r'\btask\s+thread-create\b', command)
    if not m:
        return  # not a thread-create command

    # --- Validate --id ---

    id_match = re.search(r'--id[=\s]+(\S+)', command)

    if not id_match or id_match.group(1).startswith('--'):
        block(
            "task thread-create requires --id with a deterministic value. "
            "Use --id $THREAD-<issue_number> (e.g., --id $THREAD-192). "
            "Never use bare words like 'root'."
        )

    id_value = id_match.group(1)

    # Reject bare "root" as an ID (the specific bug we're fixing)
    if id_value == "root":
        block(
            "task thread-create --id cannot be 'root'. "
            "Use --id $THREAD-<issue_number> (e.g., --id $THREAD-192)."
        )

    # Require $THREAD- prefix for namespacing under the root thread.
    # Accept both $THREAD-... and "$THREAD-... (pre shell expansion).
    if not (id_value.startswith("$THREAD-") or id_value.startswith('"$THREAD-')):
        block(
            f"task thread-create --id must be namespaced under $THREAD. "
            f"Got: {id_value}. Use --id $THREAD-<issue_number> (e.g., --id $THREAD-192)."
        )

    # --- Validate --parent ---

    # Match --parent VALUE or --parent=VALUE (cobra supports both forms).
    parent_match = re.search(r'--parent[=\s]+(\S+)', command)

    # If no match, or the captured "value" is actually another flag
    # (e.g., --parent --repo bar), treat as bare --parent (missing value).
    if not parent_match or parent_match.group(1).startswith('--'):
        if re.search(r'--parent\s*$', command) or re.search(r'--parent\s+--', command):
            block(
                "task thread-create --parent requires a value. "
                "Use --parent $THREAD so the shell expands it to the current thread ID."
            )
        block(
            "task thread-create requires --parent $THREAD. "
            "The shell will expand $THREAD to the current thread ID."
        )

    value = parent_match.group(1)

    # The value as seen by the guard (before shell expansion) must be the
    # literal string "$THREAD". We also accept "$THREAD" (double-quoted)
    # since bash still expands variables inside double quotes.
    if value not in ("$THREAD", '"$THREAD"'):
        block(
            f"task thread-create --parent value must be $THREAD, got: {value}. "
            "Use --parent $THREAD (without quotes) so the shell expands it."
        )


def check_thread_modify(command: str) -> None:
    """Block any command that attempts to modify the THREAD environment variable."""
    for pattern in THREAD_MODIFY_PATTERNS:
        if re.search(pattern, command):
            block(
                f"Attempt to modify THREAD environment variable matched '{pattern}': "
                f"{command[:120]}. THREAD is set by the web UI harness and must not "
                f"be changed. Use task thread-create to create child threads instead."
            )


def check_bash(command: str) -> None:
    # Defense-in-depth: detect if THREAD was already modified before this command
    original_thread = os.environ.get("ORIGINAL_THREAD", "")
    current_thread = os.environ.get("THREAD", "")
    if original_thread and current_thread != original_thread:
        block(
            f"THREAD environment variable was modified (current='{current_thread}', "
            f"original='{original_thread}'). THREAD is set by the web UI harness "
            f"and must not be changed. Use task thread-create to create child threads instead."
        )

    # Block commands that try to modify THREAD
    check_thread_modify(command)

    # Validate thread-create commands
    check_thread_create(command)

    # Then, generic forbidden pattern checks
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
