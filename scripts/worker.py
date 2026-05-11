#!/usr/bin/env python3
"""Generic worker poll loop — shared by all three agent types (claude, copilot, opencode).

Parameterized by env vars:
  WORKER_TYPE   — "claude" | "copilot" | "opencode"
  AGENT_CMD     — shell command for the agent CLI
  REDIS_HOST    — Redis hostname (default: redis)
  REDIS_PORT    — Redis port (default: 6379)
  TASK_TIMEOUT  — subprocess timeout in seconds (default: 1800)
  HISTORY_WINDOW — number of recent thread messages to include as context (default: 10)
"""

import json
import os
import signal
import subprocess
import sys
import time

import redis

WORKER = os.environ["WORKER_TYPE"]
QUEUE = f"tasks:queue:{WORKER}"
PROCESSING = f"tasks:processing:{WORKER}"
AGENT_CMD = os.environ.get("AGENT_CMD", "claude -p")
HISTORY_WINDOW = int(os.environ.get("HISTORY_WINDOW", 10))
TTL_THREAD = 604800   # 7 days
TTL_TASK = 86400      # 24 hours

REDIS_HOST = os.environ.get("REDIS_HOST", "redis")
REDIS_PORT = int(os.environ.get("REDIS_PORT", 6379))
TASK_TIMEOUT = int(os.environ.get("TASK_TIMEOUT", 1800))
WORKSPACE_DIR = os.environ.get("WORKSPACE_DIR", "/workspace")


def _connect():
    return redis.Redis(host=REDIS_HOST, port=REDIS_PORT, decode_responses=True)


r = _connect()
running = True


def log(level, msg, **fields):
    entry = {"level": level, "msg": msg, "worker": WORKER, **fields}
    print(json.dumps(entry), flush=True)


def shutdown(sig, frame):
    global running
    log("info", "received signal", signal=int(sig))
    running = False


def process_one_task(task_json):
    """Process a single task. Extracted for testability."""
    task = json.loads(task_json)
    task_id = task["task_id"]
    thread_id = task["thread_id"]
    instruction = task["instruction"]

    log("info", "task dequeued", task_id=task_id, thread_id=thread_id)

    r.hset("active_tasks", task_id, json.dumps({
        "status": "running", "worker": WORKER,
        "thread_id": thread_id,
        "started_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }))

    r.set(f"task:{task_id}:status", "running", ex=TTL_TASK)
    r.set(f"task:{task_id}:worker", WORKER, ex=TTL_TASK)
    r.set(f"task:{task_id}:thread_id", thread_id, ex=TTL_TASK)
    r.set(f"task:{task_id}:description", instruction, ex=TTL_TASK)
    r.set(f"task:{task_id}:created_at",
          time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), ex=TTL_TASK)

    # Build prompt with thread context
    window = task.get("history_window", HISTORY_WINDOW)
    history = r.lrange(f"thread:{thread_id}:messages", -window, -1)
    state = r.hgetall(f"thread:{thread_id}:current_state") or {}

    context = ""
    if history:
        context = "## Thread History (recent)\n\n"
        for msg in history:
            msg_data = json.loads(msg)
            context += f"[{msg_data['role']}]: {msg_data['content']}\n\n"
    if state:
        context += f"\n## Current State\nstatus: {state.get('status', 'unknown')}\n"
        if state.get("last_design"):
            context += f"design: {state['last_design']}\n"
        if state.get("gh_repo"):
            context += f"repo: {state['gh_repo']}\n"
        if state.get("gh_pr_number"):
            context += f"PR: #{state['gh_pr_number']}\n"

    full_prompt = f"{context}\n## Task\n{instruction}"

    workspace = os.path.join(WORKSPACE_DIR, thread_id)
    os.makedirs(workspace, exist_ok=True)

    # Check for cancellation before starting subprocess
    if r.get(f"task:{task_id}:cancel"):
        log("info", "task cancelled before start", task_id=task_id, thread_id=thread_id)
        r.set(f"task:{task_id}:status", "cancelled", ex=TTL_TASK)
        r.set(f"task:{task_id}:result", "Cancelled by master", ex=TTL_TASK)
        r.set(f"task:{task_id}:exit_code", "-1", ex=TTL_TASK)
        r.set(f"task:{task_id}:completed_at",
              time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), ex=TTL_TASK)
        r.rpush(f"thread:{thread_id}:messages", json.dumps({
            "role": WORKER,
            "content": f"[cancelled] Task {task_id} was cancelled by master",
            "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "metadata": {"task_id": task_id},
        }))
        r.expire(f"thread:{thread_id}:messages", TTL_THREAD)
        r.lrem(PROCESSING, 0, task_json)
        r.hdel("active_tasks", task_id)
        return "cancelled"

    # Execute agent
    timeout_val = task.get("timeout", TASK_TIMEOUT)
    cmd = AGENT_CMD.split() + [full_prompt]
    log("info", "starting agent", task_id=task_id, thread_id=thread_id,
        cmd=AGENT_CMD, timeout=timeout_val)

    try:
        proc = subprocess.run(
            cmd, cwd=workspace, capture_output=True, text=True, timeout=timeout_val,
        )
        exit_code = proc.returncode
        result = proc.stdout
        if proc.stderr:
            result += "\n[stderr]\n" + proc.stderr
        if proc.returncode != 0:
            result = f"[FAILED exit={proc.returncode}]\n" + result
        status = "done" if proc.returncode == 0 else "failed"
        log("info", "agent finished", task_id=task_id, thread_id=thread_id,
            exit_code=exit_code, status=status)
    except subprocess.TimeoutExpired:
        exit_code = -1
        result = f"Task timed out after {timeout_val}s"
        status = "failed"
        log("warn", "agent timed out", task_id=task_id, thread_id=thread_id,
            timeout=timeout_val)

    # Store result
    r.set(f"task:{task_id}:result", result, ex=TTL_TASK)
    r.set(f"task:{task_id}:exit_code", str(exit_code), ex=TTL_TASK)
    r.set(f"task:{task_id}:completed_at",
          time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), ex=TTL_TASK)
    r.set(f"task:{task_id}:status", status, ex=TTL_TASK)

    # Append result to thread history (cap at 10k chars)
    r.rpush(f"thread:{thread_id}:messages", json.dumps({
        "role": WORKER,
        "content": result[:10000],
        "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "metadata": {"task_id": task_id},
    }))
    r.expire(f"thread:{thread_id}:messages", TTL_THREAD)

    # Update thread state (best-effort: worker only sets metadata fields, never status)
    r.hset(f"thread:{thread_id}:current_state", mapping={
        "last_updated_by": WORKER,
        "last_task_id": task_id,
        "updated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    })
    r.expire(f"thread:{thread_id}:current_state", TTL_THREAD)

    # Cleanup
    r.lrem(PROCESSING, 0, task_json)
    r.hdel("active_tasks", task_id)
    log("info", "task complete", task_id=task_id, thread_id=thread_id, status=status)
    return status


def main():
    global r, running
    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)

    log("info", "worker started", queue=QUEUE, agent_cmd=AGENT_CMD)

    while running:
        try:
            task_json = r.blmove(QUEUE, PROCESSING, 5, src="RIGHT", dest="LEFT")
        except redis.exceptions.ConnectionError:
            log("warn", "redis connection lost, reconnecting")
            time.sleep(1)
            r = _connect()
            continue
        if not task_json:
            continue

        process_one_task(task_json)

    log("info", "worker shutting down")


if __name__ == "__main__":
    main()
