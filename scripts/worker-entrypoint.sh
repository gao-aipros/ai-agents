#!/bin/sh
# Worker entrypoint — toggles between Go and Python worker based on TASKLIB_BACKEND.
# Default is "go". Set TASKLIB_BACKEND=python to use the legacy Python worker.
# Falls back to Python if the Go binary is missing.

BACKEND="${TASKLIB_BACKEND:-go}"

if [ "$BACKEND" = "python" ] || [ ! -x /usr/local/bin/worker-go ]; then
    if [ "$BACKEND" != "python" ]; then
        echo "worker-go not found, falling back to python" >&2
    fi
    exec python3 /usr/local/bin/worker.py
else
    exec /usr/local/bin/worker-go "$WORKER_TYPE"
fi
