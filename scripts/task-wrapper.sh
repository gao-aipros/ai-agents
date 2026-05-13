#!/bin/sh
# Task CLI wrapper — toggles between Go and Python task CLI based on TASKLIB_BACKEND.
# Default is "go". Set TASKLIB_BACKEND=python to use the legacy Python task.py.

BACKEND="${TASKLIB_BACKEND:-go}"

if [ "$BACKEND" = "python" ]; then
    exec python3 /usr/local/bin/task.py "$@"
else
    exec /usr/local/bin/task-go "$@"
fi
