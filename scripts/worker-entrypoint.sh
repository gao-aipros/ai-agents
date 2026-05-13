#!/bin/sh
# Worker entrypoint — toggles between Go and Python worker based on TASKLIB_BACKEND.
# Default is "go". Set TASKLIB_BACKEND=python to use the legacy Python worker.

BACKEND="${TASKLIB_BACKEND:-go}"

if [ "$BACKEND" = "python" ]; then
    exec python3 /usr/local/bin/worker.py
else
    exec /usr/local/bin/worker-go "$WORKER_TYPE"
fi
