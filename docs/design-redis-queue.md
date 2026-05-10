# Redis Task Queue Design

## Architecture

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ docker-compose                                                                │
│                                                                               │
│  ┌──────────┐   LPUSH            ┌──────────┐   BLMOVE         ┌───────────┐ │
│  │  master  │───────────────────▶│  Redis   │─────────────────▶│ worker-   │ │
│  │ (claude) │                    │          │                  │ claude    │ │
│  │          │◀──────────────────│          │◀─────────────────│ (1x)      │ │
│  │          │    GET result      │          │    SET result    │           │ │
│  └──────────┘                    │          │                  └───────────┘ │
│       │                          │          │                                │
│       │                          │          │                  ┌───────────┐ │
│       │                          │          │◀─────────────────│ worker-   │ │
│       │                          │          │                  │ copilot   │ │
│       │                          │          │                  │ (1x)      │ │
│       │                          │          │                  └───────────┘ │
│       │                          │          │                                │
│       │                          │          │                  ┌───────────┐ │
│       │                          │          │◀─────────────────│ worker-   │ │
│       │                          │          │                  │ opencode  │ │
│       │                          └──────────┘                  │ (1x)      │ │
│       │                                                        └───────────┘ │
│       │                             │                               │        │
│       └─────────────────────────────┴───────────────────────────────┘        │
│                          /workspace (shared volume)                           │
└──────────────────────────────────────────────────────────────────────────────┘
```

**Data plane:** Shared `/workspace` volume between master and all workers. Workers clone repos, write code, produce artifacts here.

**Control plane:** Redis with per-worker-type task queues, plus thread-based message history for persistent project context across delegations.

## Redis Data Model

Four groups of keys:

### 1. Task Queues (per worker type)

```
tasks:queue:claude      LIST   — pending tasks for Claude worker
tasks:queue:copilot     LIST   — pending tasks for Copilot worker
tasks:queue:opencode    LIST   — pending tasks for OpenCode worker

tasks:processing:claude   LIST   — in-flight tasks (BLMOVE target, crash recovery)
tasks:processing:copilot
tasks:processing:opencode
```

Task payload (JSON in list):
```json
{
  "task_id": "uuid-abc",
  "thread_id": "thread-123",
  "instruction": "Add OAuth2 support to the existing design"
}
```

### 2. Thread Message History

```
thread:{thread_id}:messages   LIST   — ordered history of all interactions
```

Each entry (JSON string):
```json
{
  "role": "master | claude | copilot | opencode",
  "content": "The actual design text, code diff, or instruction",
  "timestamp": "2026-05-09T10:00:00Z",
  "metadata": {
    "task_id": "uuid-abc",
    "tokens": 1200
  }
}
```

Workers pull context with `LRANGE thread:{id}:messages -5 -1` before executing.
TTL: 7 days (`EXPIRE 604800`), refreshed on each new message. Inactive threads auto-cleanup.

### 3. Thread Current State

```
thread:{thread_id}:current_state   HASH
```

Fields:
```
status           — "awaiting_review" | "refining" | "implementing" | "complete"
last_design      — full text of the latest design version
last_updated_by  — worker type that last updated
last_task_id     — task_id of the last update
gh_pr_number     — GitHub PR number (if code was pushed)
gh_repo          — GitHub repo (e.g., "owner/repo")
updated_at       — ISO timestamp
```

This is a snapshot — the master or worker updates it after each task. Avoids parsing the full message history to find the current state.

### 4. Active Task Registry

```
active_tasks   HASH
```

Field: `task_id → {"status": "running", "worker": "claude", "thread_id": "thread-123", "started_at": "..."}`

The master uses this to see what's in flight. Workers register on dequeue, remove on completion.

### 5. Per-Task Result Keys

```
task:{task_id}:status       STRING — "pending" | "running" | "done" | "failed" | "cancelled"
task:{task_id}:worker       STRING — "claude" | "copilot" | "opencode"
task:{task_id}:thread_id    STRING — parent thread
task:{task_id}:description  STRING — original instruction
task:{task_id}:result       STRING — stdout from agent CLI
task:{task_id}:created_at   STRING — ISO timestamp
task:{task_id}:completed_at STRING — ISO timestamp
task:{task_id}:exit_code    STRING — exit code (0 = success)
```

TTL: 24h for task result keys. Thread history is the long-term record.

### Key inventory

| Key pattern | Type | Purpose | TTL |
|---|---|---|---|
| `tasks:queue:{worker}` | LIST | Pending task payloads | none |
| `tasks:processing:{worker}` | LIST | In-flight tasks (crash recovery) | none |
| `thread:{id}:messages` | LIST | Full interaction history | 7d, refreshed |
| `thread:{id}:current_state` | HASH | Latest design/status snapshot | 7d, refreshed |
| `active_tasks` | HASH | What's running right now | none |
| `task:{id}:status` | STRING | Task lifecycle state | 24h |
| `task:{id}:result` | STRING | Agent stdout/stderr | 24h |
| `task:{id}:cancel` | STRING | Cancellation flag (set by master, checked by worker) | TTL_TASK if set by master |
| `task:{id}:*` | STRING | Other per-task metadata | 24h |
| `thread:{id}:lock` | STRING | Serialization guard (SETNX) | TASK_TIMEOUT + 5 min |

## Master: Python CLI tool

The master runs `claude` interactively. It calls `task.py` for all task and thread management.

### Commands

```
# Task management
task.py enqueue --worker claude|copilot|opencode --thread <thread_id> --instruction "<text>"
    LPUSHes a task onto tasks:queue:<worker>. Also appends the instruction to
    thread:{thread_id}:messages (role=master). If thread:{id}:lock exists, refuses
    with an error (another task is in-flight for this thread). Prints JSON:
    {"task_id": "<id>"}.

task.py status --id <task_id>
    Prints JSON: {task_id, worker, thread_id, status, exit_code, timestamps}.

task.py result --id <task_id> [--tail N]
    Prints the result field.

task.py list [--worker X] [--status X] [--thread X] [--limit N]
    Scans active_tasks and task:* keys, prints summary table. Uses SCAN (not KEYS)
    to avoid blocking Redis if the task count grows beyond trivial scale.

task.py wait --id <task_id> [--timeout 300]
    Blocks until done or failed. Polls every 2s. On completion, deletes
    thread:{thread_id}:lock so another task can be enqueued for the same thread.

task.py requeue-stale [--worker X] [--older-than 600]
    Scans tasks:processing:<worker>. For each task, checks task:{id}:status. If missing (worker
    crashed before writing status) or "running" for > older-than, requeues it. Pushes the task
    back onto tasks:queue:<worker> and removes it from tasks:processing:<worker>.
    Edge case: if a worker crashed after writing result but before `lrem(PROCESSING, ...)`, the
    task has correct status (done/failed) and won't be re-queued, but a stale entry remains in
    the processing list. Harmless — the entry is never dequeued again — but it accumulates over
    time. A future `requeue-stale` could also garbage-collect entries whose task:{id}:status has
    reached a terminal state.

# Thread management
task.py thread-create --id <thread_id> [--repo owner/repo]
    Initializes thread:{id}:current_state with status=initiated.

task.py thread-history --id <thread_id> [--tail N]
    Prints the last N messages from thread:{id}:messages.

task.py thread-state --id <thread_id>
    Prints the current_state hash as JSON.

task.py thread-update --id <thread_id> --status <status> [--design "<text>"] [--pr N]
    Updates fields in thread:{id}:current_state. Used by master after reviewing results.

task.py thread-list
    Lists all thread:{*}:current_state keys with status summary.

task.py thread-cleanup --id <thread_id>
    Deletes /workspace/{thread_id}/ (cloned repos, build artifacts). Safe to
    run after thread-update --status complete. Prevents volume bloat.

task.py cancel --id <task_id>
    Sets task:{id}:cancel = "1" (with TTL_TASK expiry so it auto-cleans up). The worker checks this key before starting the
    subprocess; if set, marks the task cancelled and moves on. (Mid-execution
    cancellation via SIGTERM would require a Popen polling loop — deferred.)

task.py unlock --thread <thread_id>
    Deletes thread:{id}:lock. Used to clear a stale lock left by a crashed master.
```

### Example flow

```bash
# Start a new project thread
task.py thread-create --id "add-oauth2" --repo "owner/repo"

# Delegate design to Claude worker
DESIGN_TASK=$(task.py enqueue --worker claude --thread add-oauth2 \
    --instruction "Design OAuth2 support. Read thread history for context." | jq -r '.task_id')

# Wait for design to complete before starting the next step
task.py wait --id "$DESIGN_TASK"

# Delegate review to Copilot worker
REVIEW_TASK=$(task.py enqueue --worker copilot --thread add-oauth2 \
    --instruction "Review the OAuth2 design in thread history. Find security gaps." | jq -r '.task_id')

# Wait for review to complete before starting implementation
task.py wait --id "$REVIEW_TASK"

# Delegate implementation to OpenCode worker
task.py enqueue --worker opencode --thread add-oauth2 \
    --instruction "Implement OAuth2 based on design and review in thread history."

# Check progress
task.py thread-state --id add-oauth2
task.py thread-history --id add-oauth2 --tail 10
```

The script lives at `scripts/task.py` and is copied into the master image at `/usr/local/bin/task.py`.

## Worker: Python poll loop

A single generic `worker.py` shared by all three worker types. Parameterized by env vars:

| Env var | Claude | Copilot | OpenCode |
|---|---|---|---|
| `WORKER_TYPE` | `claude` | `copilot` | `opencode` |
| `AGENT_CMD` | `claude -p` | `copilot -p --allow-all` | `opencode run --dangerously-skip-permissions` |

```python
import os, json, subprocess, signal, time
import redis

WORKER = os.environ["WORKER_TYPE"]
QUEUE = f"tasks:queue:{WORKER}"
PROCESSING = f"tasks:processing:{WORKER}"
AGENT_CMD = os.environ.get("AGENT_CMD", "claude -p")
HISTORY_WINDOW = int(os.environ.get("HISTORY_WINDOW", 10))  # last N messages for context
TTL_THREAD = 604800   # 7 days
TTL_TASK = 86400      # 24 hours

r = redis.Redis(host=os.environ.get("REDIS_HOST", "redis"),
                port=int(os.environ.get("REDIS_PORT", 6379)),
                decode_responses=True)

running = True

def shutdown(sig, frame):
    global running
    running = False

signal.signal(signal.SIGTERM, shutdown)
signal.signal(signal.SIGINT, shutdown)

while running:
    try:
        task_json = r.blmove(QUEUE, PROCESSING, "RIGHT", "LEFT", timeout=5)
    except redis.exceptions.ConnectionError:
        time.sleep(1)
        r = redis.Redis(host=os.environ.get("REDIS_HOST", "redis"),
                        port=int(os.environ.get("REDIS_PORT", 6379)),
                        decode_responses=True)
        continue
    if not task_json:
        continue

    task = json.loads(task_json)
    task_id = task["task_id"]
    thread_id = task["thread_id"]
    instruction = task["instruction"]

    # Register as active
    r.hset("active_tasks", task_id, json.dumps({
        "status": "running", "worker": WORKER,
        "thread_id": thread_id, "started_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    }))

    # Set task status (all task keys get TTL_TASK so they auto-expire)
    r.set(f"task:{task_id}:status", "running", ex=TTL_TASK)
    r.set(f"task:{task_id}:worker", WORKER, ex=TTL_TASK)
    r.set(f"task:{task_id}:thread_id", thread_id, ex=TTL_TASK)
    r.set(f"task:{task_id}:description", instruction, ex=TTL_TASK)
    r.set(f"task:{task_id}:created_at", time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), ex=TTL_TASK)

    # Build prompt with thread context
    history = r.lrange(f"thread:{thread_id}:messages", -HISTORY_WINDOW, -1)
    state = r.hgetall(f"thread:{thread_id}:current_state") or {}

    context = ""
    if history:
        context = "## Thread History (recent)\n\n"
        for msg in history:
            msg_data = json.loads(msg)
            context += f"[{msg_data['role']}]: {msg_data['content']}\n\n"
    if state:
        context += f"\n## Current State\nstatus: {state.get('status', 'unknown')}\n"
        if state.get('last_design'):
            context += f"design: {state['last_design']}\n"
        if state.get('gh_repo'):
            context += f"repo: {state['gh_repo']}\n"
        if state.get('gh_pr_number'):
            context += f"PR: #{state['gh_pr_number']}\n"

    full_prompt = f"{context}\n## Task\n{instruction}"

    # Per-thread workspace isolation: avoid collisions when multiple
    # threads are active across different workers.
    workspace = f"/workspace/{thread_id}"
    os.makedirs(workspace, exist_ok=True)

    # Check for cancellation before starting subprocess
    if r.get(f"task:{task_id}:cancel"):
        r.set(f"task:{task_id}:status", "cancelled", ex=TTL_TASK)
        r.set(f"task:{task_id}:result", "Cancelled by master", ex=TTL_TASK)
        r.set(f"task:{task_id}:exit_code", "-1", ex=TTL_TASK)
        r.set(f"task:{task_id}:completed_at", time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), ex=TTL_TASK)
        # Append cancellation to thread history so it's visible in context
        r.rpush(f"thread:{thread_id}:messages", json.dumps({
            "role": WORKER,
            "content": f"[cancelled] Task {task_id} was cancelled by master",
            "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "metadata": {"task_id": task_id}
        }))
        r.expire(f"thread:{thread_id}:messages", TTL_THREAD)
        r.lrem(PROCESSING, 0, task_json)
        r.hdel("active_tasks", task_id)
        continue

    # Execute agent
    cmd = AGENT_CMD.split() + [full_prompt]
    try:
        proc = subprocess.run(
            cmd, cwd=workspace, capture_output=True, text=True,
            timeout=int(os.environ.get("TASK_TIMEOUT", 1800)),
        )
        exit_code = proc.returncode
        # Combine stdout+stderr regardless of exit code — agents often
        # produce useful output on stdout even when they exit non-zero.
        result = proc.stdout
        if proc.stderr:
            result += "\n[stderr]\n" + proc.stderr
        if proc.returncode != 0:
            result = f"[FAILED exit={proc.returncode}]\n" + result
        status = "done" if proc.returncode == 0 else "failed"
    except subprocess.TimeoutExpired:
        exit_code = -1
        result = f"Task timed out after {os.environ.get('TASK_TIMEOUT', 1800)}s"
        status = "failed"

    # Store result (with TTLs so keys don't accumulate forever)
    r.set(f"task:{task_id}:result", result, ex=TTL_TASK)
    r.set(f"task:{task_id}:exit_code", str(exit_code), ex=TTL_TASK)
    r.set(f"task:{task_id}:completed_at", time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), ex=TTL_TASK)
    r.set(f"task:{task_id}:status", status, ex=TTL_TASK)

    # Append result to thread history
    r.rpush(f"thread:{thread_id}:messages", json.dumps({
        "role": WORKER,
        "content": result[:10000],  # cap per message at 10k chars
        "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "metadata": {"task_id": task_id}
    }))
    r.expire(f"thread:{thread_id}:messages", TTL_THREAD)

    # Update thread state (best-effort: worker sets status to awaiting_review after producing output)
    r.hset(f"thread:{thread_id}:current_state", mapping={
        "last_updated_by": WORKER,
        "last_task_id": task_id,
        "updated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    })
    r.expire(f"thread:{thread_id}:current_state", TTL_THREAD)

    # Cleanup
    r.lrem(PROCESSING, 0, task_json)
    r.hdel("active_tasks", task_id)
```

### Build prompt flow

```
Worker dequeues task {task_id, thread_id, instruction}
    │
    ├─ LRANGE thread:{thread_id}:messages -10 -1   → recent context
    ├─ HGETALL thread:{thread_id}:current_state    → snapshot (status, design, PR#)
    │
    └─ Full prompt = context + instruction → claude -p "..."
                                               │
                                               ▼
                                          result (stdout)
                                               │
                                    RPUSH thread:{id}:messages
                                    HSET thread:{id}:current_state
```

### Behavior

- Each worker runs one task at a time, sequentially.
- Before executing, the worker fetches the last `HISTORY_WINDOW` messages from the thread + the current_state snapshot. This gives the agent full context without the master needing to repeat it.
- After executing, the worker appends its result to the thread history. The next worker in the pipeline sees it.
- On SIGTERM: lets the current subprocess finish, then exits. In practice Docker
  Compose sends SIGKILL after the stop grace period (default 10s), so the
  container is killed and the in-flight task is left in `tasks:processing:*`
  for `requeue-stale` to recover. No data loss, but the task must be re-queued.
- Result content is capped at 10k chars when appended to thread history (avoids bloating the list with huge diffs; full result is still in `task:{id}:result`).
- **Thread serialization:** `enqueue` acquires `SETNX thread:{id}:lock` (with TTL = TASK_TIMEOUT + 300s to avoid expiry races near task completion). If the lock exists, enqueue refuses. `task.py wait` deletes the lock on completion, allowing the next task for that thread. This prevents concurrent tasks on the same thread from racing on state updates. Stale locks (e.g. master crashed) are cleared by `task.py unlock --thread <id>`.
- **Task cancellation:** The master may call `task.py cancel --id <task_id>`, which sets `task:{id}:cancel`. The worker checks this key before starting the subprocess; if set, it marks the task `cancelled` and skips execution. Mid-execution cancellation is not supported in v1 — `subprocess.run()` blocks until the task finishes or times out.
- **Healthcheck interaction:** If a worker fails 3 healthchecks (90s) during a long subprocess run, Docker restarts the container. The task is left in `tasks:processing:*` and recovered by `requeue-stale`. Docker waits up to 100s (30s × 3 retries + 10s start_period) before restarting an unhealthy container.
- **Logging:** The worker emits JSON-lines logs with `task_id` and `thread_id` fields to stdout. Docker's `json-file` logging driver captures these natively, enabling correlation across services.

## docker-compose

```yaml
services:
  redis:
    image: redis:7-alpine
    restart: unless-stopped
    volumes:
      - redis_data:/data
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s

  master:
    image: master-agent:latest
    stdin_open: true
    tty: true
    restart: unless-stopped
    volumes:
      - workspace:/workspace
    environment:
      REDIS_HOST: redis
      REDIS_PORT: "6379"
      TASK_TIMEOUT: "1800"
      ANTHROPIC_AUTH_TOKEN: ${ANTHROPIC_AUTH_TOKEN}
      GH_TOKEN: ${GH_TOKEN}
      GITHUB_TOKEN: ${GITHUB_TOKEN}
    depends_on:
      redis:
        condition: service_healthy

  worker-claude:
    image: worker-claude:latest
    restart: unless-stopped
    volumes:
      - workspace:/workspace
    environment:
      REDIS_HOST: redis
      WORKER_TYPE: claude
      AGENT_CMD: "claude -p"
      TASK_TIMEOUT: "1800"
      HISTORY_WINDOW: "10"
      ANTHROPIC_AUTH_TOKEN: ${ANTHROPIC_AUTH_TOKEN}
      GH_TOKEN: ${GH_TOKEN}
      GITHUB_TOKEN: ${GITHUB_TOKEN}
    depends_on:
      redis:
        condition: service_healthy
    healthcheck:
      test: ["CMD", "python3", "-c", "import redis; assert redis.Redis(host='redis').ping()"]
      interval: 30s
      retries: 3
      start_period: 10s

  worker-copilot:
    image: copilot:latest
    restart: unless-stopped
    volumes:
      - workspace:/workspace
    environment:
      REDIS_HOST: redis
      WORKER_TYPE: copilot
      AGENT_CMD: "copilot -p --allow-all"
      TASK_TIMEOUT: "1800"
      HISTORY_WINDOW: "10"
      COPILOT_PROVIDER_API_KEY: ${DEEPSEEK_API_KEY}
      GH_TOKEN: ${GH_TOKEN}
      GITHUB_TOKEN: ${GITHUB_TOKEN}
    depends_on:
      redis:
        condition: service_healthy
    healthcheck:
      test: ["CMD", "python3", "-c", "import redis; assert redis.Redis(host='redis').ping()"]
      interval: 30s
      retries: 3
      start_period: 10s

  worker-opencode:
    image: opencode:latest
    restart: unless-stopped
    volumes:
      - workspace:/workspace
    environment:
      REDIS_HOST: redis
      WORKER_TYPE: opencode
      AGENT_CMD: "opencode run --dangerously-skip-permissions"
      TASK_TIMEOUT: "1800"
      HISTORY_WINDOW: "10"
      DEEPSEEK_API_KEY: ${DEEPSEEK_API_KEY}
      GH_TOKEN: ${GH_TOKEN}
      GITHUB_TOKEN: ${GITHUB_TOKEN}
    depends_on:
      redis:
        condition: service_healthy
    healthcheck:
      test: ["CMD", "python3", "-c", "import redis; assert redis.Redis(host='redis').ping()"]
      interval: 30s
      retries: 3
      start_period: 10s

volumes:
  workspace:
    driver: local
  redis_data:
    driver: local
```

## What Changes vs. Current

| Thing | Before | After |
|---|---|---|
| Master delegates | `docker run worker "task"` (blocks) | `task.py enqueue --worker X --thread Y --instruction "..."` (non-blocking) |
| Worker context | Single prompt string, no history | Thread history (last N messages) + current state snapshot |
| Context persistence | None — each task is isolated | Thread history accumulates across delegations (7d TTL) |
| Master reads result | stdout from `docker run` | `task.py result --id ...` OR `task.py thread-history --id ...` |
| Worker lifecycle | Ephemeral, one per task | Long-running, BLMOVE loop |
| Worker entrypoint | `claude -p` / `copilot -p --allow-all` / `opencode run` | `python3 worker.py` → builds prompt with context → agent CLI |
| Docker socket | Required for delegation | Not required for delegation |
| Task visibility | None (container runs until exit) | `active_tasks` hash + per-task status keys |
| Project state | Ad-hoc via /workspace files | `thread:{id}:current_state` hash — structured snapshot |

## Design Rationale

### Python vs Go for the orchestration scripts

Python wins for this use case. `task.py` and `worker.py` are thin orchestration
glue — CLI arg parsing, Redis commands, subprocess management.
`redis-py`, `argparse`, and `subprocess` are mature and concise for this. Go
would add build complexity (multi-arch binaries), a Go toolchain dependency in
the build pipeline, and more code for the same logic — without benefit, since
the worker is single-task-at-a-time (`subprocess.run()` matches the model
perfectly). No goroutines needed.

### Shared /workspace volume tradeoffs

The shared volume is the data plane — where workers clone repos, write code,
and produce artifacts. Without it:

- The master can't inspect files a worker produced (only sees text output in Redis)
- Thread workspace isolation (`/workspace/{thread_id}/`) is lost
- A restarted worker loses in-progress clones and builds

What's not lost: all task delegation, results, and thread history flow through
Redis. If workers push code to GitHub (branches/PRs), the repo is the real
artifact store and the workspace is scratch space.

Verdict: keep the shared volume. It's lightweight and gives the master a direct
window into worker output. For a future iteration it could become optional,
with workers using local ephemeral storage (`/tmp/{thread_id}`) and relying on
git as the sole artifact store.

### Agent session save/load

Claude Code supports `--resume <session-id>` and `--continue` (resumes the last
session from `~/.claude/projects/`). Copilot (`copilot -p`) and OpenCode
(`opencode run`) are one-shot — no built-in session persistence.

This design intentionally does not rely on agent-native sessions. Instead it
rebuilds context from Redis thread history before each task. This is
tool-agnostic and works uniformly across all three workers. The tradeoff: the
agent's internal reasoning chain from the previous invocation is lost; each run
is a cold start with conversation history injected.

For v2, `--resume` could be added as an optimization for the Claude worker
without changing the overall architecture. The thread history remains the
portable, cross-agent context store.

## Files to Create or Modify

| File | Action |
|---|---|
| `scripts/task.py` | **Create** — master CLI: enqueue, thread management, status, wait |
| `scripts/worker.py` | **Create** — generic worker poll loop (shared by all 3 types) |
| `docker/worker-claude/Dockerfile` | **Modify** — add redis dep, copy worker.py, change ENTRYPOINT |
| `docker/copilot/Dockerfile` | **Modify** — add redis dep, copy worker.py, change ENTRYPOINT (drop entrypoint.sh) |
| `docker/opencode/Dockerfile` | **Modify** — add redis dep, copy worker.py, change ENTRYPOINT (drop entrypoint.sh) |
| `docker/master-agent/Dockerfile` | **Modify** — add redis dep, copy task.py |
| `docker/master-agent/CLAUDE.md` | **Modify** — replace `docker run` with `task.py` thread-based workflow |
| `docker/worker-claude/CLAUDE.md` | **Modify** — update worker instructions for thread-based context |
| `docker-compose.yml` | **Create** — Redis + master + 3 workers |

## Open Questions

1. **Task timeout?** **Decision:** 30 min default is reasonable for clone + implement + test
   workloads. Make it per-task overridable via a `timeout` field in the task payload so
   lightweight tasks don't needlessly wait and heavy ones get headroom. Default stays 1800s.

2. **Reaper cron loop?** **Decision:** Manual for v1. Docker Compose already restarts unhealthy
   containers (declared in healthcheck config), so worker process death self-heals. The
   `requeue-stale` command exists as an admin tool for scenarios where a worker crashes due
   to a logic error (OOM, segfault) that Docker restart can't fix. Automation (cron or
   background thread in the master container) is deferred to v2.

3. **Result size in thread history?** **Decision:** 10k chars is the right cap for continuity
   context. Full results are always available in `task:{id}:result` (24h TTL), and the real
   artifact (code) lives in git. The message history is for the next agent to understand
   what happened, not to replay the full diff.

4. **Thread state updates by workers?** **Decision:** The worker only sets `last_updated_by`,
   `last_task_id`, `updated_at`. It never touches `status`. The master owns state transitions
   via `task.py thread-update`. No change needed.

5. **Automatic retry on failure?** **Decision:** Deferred to v2. A `--retry N` flag on `enqueue` (stored
   in payload, decremented by worker on failure, moved to dead-letter queue when exhausted)
   would improve throughput for transient failures (network blips, API rate limits). For
   v1, the master re-enqueues manually after reviewing the failure.
