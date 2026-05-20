# ai-agents

A task execution platform with a web UI, worker fleet, and Redis-backed coordination layer.

## Language

**Access log**:
Per-request HTTP logging emitted by the web UI, enabled at startup via `--log-access` flag or `LOG_ACCESS` env var.
_Avoid_: request log, HTTP log, traffic log

**Admin endpoint**:
An API endpoint that controls server configuration at runtime (toggles, feature flags). Gated by `ADMIN_API_KEY` separately from the general web UI API key.
_Avoid_: config endpoint, management endpoint

**App logger**:
The primary structured logger for application events (startup, errors, Redis connectivity). Always writes to stderr.
_Avoid_: main logger, system logger

## Example dialogue

> Dev: I want to see which HTTP requests are hitting the web UI without restarting it.
>
> Domain expert: You can toggle the access log at runtime via `PUT /api/admin/log-access`. It's separate from the app logger — the app logger always runs, the access logger is opt-in and can be flipped on or off without killing in-flight master work.
>
> Dev: Does the toggle survive a restart?
>
> Domain expert: No, it's in-memory only. On restart, the flag or env var controls the initial state again.

