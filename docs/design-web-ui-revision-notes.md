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
claude -p --session-id <A> "Plan the fix"
  → saves plan to thread state via task thread-update

# Phase 2: Implement (new session, fresh context)
claude -p --session-id <B> \
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
