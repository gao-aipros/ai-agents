#!/bin/bash
exec opencode -m "${OPENCODE_PROVIDER}/${OPENCODE_MODEL}" run --dangerously-skip-permissions "$@"
