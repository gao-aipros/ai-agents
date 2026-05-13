#!/bin/sh
# Task CLI wrapper — toggles between Go and Python task CLI based on TASKLIB_BACKEND.
# Default is "go". Set TASKLIB_BACKEND=python to use the legacy Python task.py.
# Falls back to Python if the Go binary is missing.

BACKEND="${TASKLIB_BACKEND:-go}"

if [ "$BACKEND" = "python" ] || [ ! -x /usr/local/bin/task-go ]; then
    if [ "$BACKEND" != "python" ]; then
        echo "task-go not found, falling back to python" >&2
    fi
    exec python3 /usr/local/bin/task.py "$@"
else
    exec /usr/local/bin/task-go "$@"
fi
