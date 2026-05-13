# Web UI Design — Revision Notes

## Date: 2026-05-12

## Trigger: Step 0 Pre-implementation Validation

### Finding 1: Prompt marker approach is unnecessary

Tested `ghcr.io/noodle05/claude-code:latest` (v2.1.126):

- `-p --output-format stream-json --verbose` produces structured JSON on stdout
- Each turn ends with an unambiguous `{"type":"result","subtype":"success","stop_reason":"end_turn"}`
- `-p` mode supports complex multi-step tool use within a single invocation (`num_turns: 3+`)
- `--session-id <UUID>` + `--resume <UUID>` persists conversation context across `-p` invocations when `~/.claude` is on a shared volume
- Completely eliminates the need for: FIFO, TTY, prompt marker regex, stdin mutex, persistent child process supervision

### Finding 2: Piped stdin is NOT interactive

Claude Code without TTY buffers all available stdin before processing. Multi-step interaction via FIFO (write request, wait, write follow-up) doesn't work as originally designed. `script` is available for pseudo-TTY, `unbuffer` is not.

### Architecture pivot

| Aspect | Old Design (FIFO) | New Design (`-p` per-request) |
|--------|------------------|-------------------------------|
| Claude invocation | Persistent interactive session | Per-request `claude -p` |
| Completion detection | Prompt marker regex (backup) | `{"type":"result"}` JSON (unambiguous) |
| Stdin delivery | FIFO + TTY mutex | CLI argument (`"prompt"`) |
| Session persistence | In-memory (same process) | `--session-id` + shared volume |
| Process management | PID 1 supervisor + child reaping | None (one-shot invocation) |
| Crash recovery | Stranded inbox_processing sweep | None (no persistent process) |
| Concurrency | stdin mutex with timeout/retry | Redis lock per thread before launch |
| TTY requirement | Yes | No |

### Things eliminated

- `inbox-reader` binary (no FIFO delivery needed)
- `supervisor` child-process management (no persistent process)
- `requests:inbox` / `requests:inbox_processing` lists (no FIFO)
- `requests:inbox:pending:<thread_id>` sentinel
- `supervisor:busy` key
- `MASTER_PROMPT_MARKER` / `MASTER_RESPONSE_TIMEOUT` / `MASTER_MUTEX_TIMEOUT` / `MASTER_RESPONSE_MIN_ELAPSED` / `MASTER_RESPONSE_QUIET_PERIOD` env vars
- `MASTER_RESPONSE_MAX_SIZE` (stream-json is already line-delimited)
- Named pipe (`/tmp/master-inbox.fifo`) in Dockerfile
- TTY/stdin_open in docker-compose

### How multi-step orchestrator works

Single `claude -p` invocation handles the full workflow:

1. Receive request → plan → create thread
2. Enqueue design task → `task wait` (blocks for minutes)
3. Enqueue implementation task → `task wait` (blocks for minutes)
4. Aggregate results → respond
5. Exit (session persists via `--session-id` for follow-ups)

`task wait` is blocking — the `-p` process stays alive through the tool call loop.

### Session splitting (optional)

For human-in-the-loop phases (review plan before implementation):

```
# Phase 1: Plan
claude --dangerously-skip-permissions --bare -p --session-id <A> --output-format stream-json --verbose "Plan the fix"
  → saves plan to thread state via task thread-update

# Phase 2: Implement (new session, fresh context)
claude --dangerously-skip-permissions --bare -p --session-id <B> --output-format stream-json --verbose \
  "Read thread design from thread state. Delegate implementation."
```

Delete session A's files after phase 1 completes.

### New Redis keys needed

| Key | Type | Purpose |
|-----|------|---------|
| `thread:<id>:running` | String (SET NX) | Lock preventing concurrent `claude -p` invocations for same thread |
| `thread:<id>:complete` | String | Set by web UI handler when `type: "result"` received |
| `thread:<id>:session_id` | String | Store the Claude session UUID for `--resume` on follow-ups |

### Step 0 conclusion

**Item 1 (Prompt marker):** NOT MET — but the approach that needed it was wrong. The `stream-json` path is simpler and more reliable.

**Item 2 (Redis commands):** MET — Redis 7 supports all required commands.

## PR Review Resolutions (2026-05-13)

Issues raised by reviewer and how they were addressed:

1. **Session cleanup vs. follow-up contradiction** — Changed from "delete on completion" to TTL-based cleanup (24h after last activity, periodic background goroutine). Follow-ups work within the grace window.
2. **`thread:<id>:complete` has no TTL** — Added 7-day TTL to align with thread lifecycle.
3. **Session UUID generation ambiguity** — Clarified: UUID is generated on first request (step 5), NOT at `POST /api/threads` creation time.
4. **`--dangerously-skip-permissions` justification** — Added lock taxonomy table and justification paragraph: safe because container-bounded filesystem, limited network exposure, no host access.
5. **Stale `thread:<id>:running` after crash** — Added startup sweep: immediately reaps any lock key whose request ID doesn't correspond to a running subprocess PID.
6. **No request queue / 503 burst handling** — Documented `503 + Retry-After` as intentional backpressure pattern. No Redis queue needed for single-user scenario.
7. **Session file cleanup mechanism** — Clarified: periodic background goroutine in web UI server via `os.Remove`.
8. **Thread lock vs task lock confusion** — Added lock taxonomy table distinguishing `thread:<id>:lock` (task serialization) from `thread:<id>:running` (request serialization).
9. **Merged master+webui scaling tradeoff** — Acknowledged: master-agent is intentionally single-instance. For higher throughput, add a lightweight HTTP frontend.
10. **`inbox.go` → `request.go`** — Renamed in both Shared tasklib and File Structure sections. Removed `lua/` directory.
11. **Timeout edge case for long workflows** — Noted that REQUEST_TIMEOUT should be tuned per-deployment. Future: refresh timeout on intermediate output.
12. **"Clear session" button** — Added `POST /api/threads/{id}/reset-session` endpoint and "Reset session" action on Thread Detail View.

### Step consistency audit

| Step | Design claim | Actual code | Resolution |
|------|-------------|-------------|------------|
| 1 | Done, needs post-revision | inbox.go + lua/ still present | Acknowledged — post-revision work needed |
| 2 | Done | test_json_compat.py exists | No change |
| 3 | "DONE" for both task + worker | cmd/worker/ is empty | Fixed: marked worker as NOT STARTED |
| 4-8 | Not started | No code | No change |

Orphaned directories to remove: `cmd/supervisor/`, `cmd/inbox-reader/` (empty, old design).

## Round 2 Review Resolutions (2026-05-13)

1. **Shell injection** — Added explicit note: Go's `exec.Command` uses `execve` directly, no shell. Do NOT use `sh -c`.
2. **Live progress default** — Changed `type: "assistant"` messages from "optional" to written to thread history by default for real-time display.
3. **Stdout memory** — Added 64KB cap on non-result stdout buffering.
4. **Missing session fallback** — Handler detects "No conversation found" on stderr, falls back to fresh `--session-id` + warning log.
5. **Cancellation latency** — `POST /api/threads/{id}/cancel` now immediately calls `cancel()` on the subprocess context, not relying on REQUEST_TIMEOUT.
6. **Session cleanup** — Changed from filesystem scan to Redis-backed: `thread:<id>:last_activity` key enables O(1) lookup of inactive threads.
7. **`thread:<id>:complete` purpose** — Clarified: enables fast dashboard "Ready for review" indicator without parsing message history.
8. **TTL consistency** — Made 7-day TTL explicit in both the flow description (step 7) and Section 6 flow chart (step 8).
9. **Panic recovery** — Added `defer recover()` in background goroutine: logs error, releases lock, kills subprocess, writes error message.
10. **Step numbering** — Clarified step 3: bootstrap-only thread creation, user request written as `role: "user"` message.

New Redis key: `thread:<id>:last_activity` (Unix timestamp, updated on every request).


## Round 3 Review Resolutions (2026-05-13)

1. **CancelRequest comment stale** — Updated to reflect immediate cancel() call, not REQUEST_TIMEOUT.
2. **Missing TTLs** — Added 7-day TTL to `thread:<id>:session_id` and `thread:<id>:last_activity` in Redis Data Model table.
3. **Session-splitting example flags** — Added `--dangerously-skip-permissions --bare --output-format stream-json --verbose` to both examples.
4. **hx-retarget does NOT handle 503** — Replaced with correct `htmx:responseError` event listener approach (exponential backoff).
5. **Cancel race** — Background goroutine now checks `thread:<id>:current_state` status at startup and aborts if "cancelled".
6. **Lock TTL vs context timeout mismatch** — Redis lock TTL is now `REQUEST_TIMEOUT + 5 min` (35 min default) to prevent lock expiry before subprocess kill.
7. **Stream-json → thread message mapping** — Added mapping table: text-only assistant → "plan", tool_use → "tool_call", result → "response"/"error".
8. **Interactive CLI elimination** — Documented as intentional breaking change in Docker Integration table.
9. **Step consistency** — Already resolved (previous round).
10. **Task enqueue lock rationale** — Added explanation: sequential execution preserves causal ordering of worker results within a thread.
