#!/usr/bin/env python3
"""Master CLI for Redis-backed task queue and thread management."""

import argparse
import calendar
import json
import os
import shutil
import sys
import time
import uuid

import redis

REDIS_HOST = os.environ.get("REDIS_HOST", "redis")
REDIS_PORT = int(os.environ.get("REDIS_PORT", 6379))
REDIS_DB = int(os.environ.get("COMPAT_TEST_DB", 0))
TASK_TIMEOUT = int(os.environ.get("TASK_TIMEOUT", 1800))
WORKSPACE_DIR = os.environ.get("WORKSPACE_DIR", "/workspace")
TTL_TASK = 86400       # 24 hours
TTL_THREAD = 604800    # 7 days
LOCK_TTL = TASK_TIMEOUT + 300

WORKERS = ("claude", "copilot", "opencode")


def get_redis():
    return redis.Redis(host=REDIS_HOST, port=REDIS_PORT, db=REDIS_DB, decode_responses=True)


# ── helpers ────────────────────────────────────────────────────────────────

def ts():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def die(msg, code=1):
    print(f"ERROR: {msg}", file=sys.stderr)
    sys.exit(code)


# ── task commands ──────────────────────────────────────────────────────────

def cmd_enqueue(args):
    r = get_redis()
    task_id = str(uuid.uuid4())
    thread_id = args.thread
    worker = args.worker
    instruction = args.instruction

    # Acquire thread lock (serialize tasks on the same thread)
    lock_key = f"thread:{thread_id}:lock"
    if not r.set(lock_key, task_id, nx=True, ex=LOCK_TTL):
        current_holder = r.get(lock_key)
        die(f"Thread '{thread_id}' is locked (holder task: {current_holder}). "
            f"Wait for it to complete or run 'task.py unlock --thread {thread_id}'.")

    # Append instruction to thread history
    msg = json.dumps({
        "role": "master",
        "content": instruction,
        "timestamp": ts(),
        "metadata": {"task_id": task_id}
    })
    r.rpush(f"thread:{thread_id}:messages", msg)
    r.expire(f"thread:{thread_id}:messages", TTL_THREAD)

    # Enqueue task
    payload = json.dumps({
        "task_id": task_id,
        "thread_id": thread_id,
        "instruction": instruction
    })
    r.lpush(f"tasks:queue:{worker}", payload)

    # Initialize task keys
    r.set(f"task:{task_id}:status", "pending", ex=TTL_TASK)
    r.set(f"task:{task_id}:worker", worker, ex=TTL_TASK)
    r.set(f"task:{task_id}:thread_id", thread_id, ex=TTL_TASK)
    r.set(f"task:{task_id}:description", instruction, ex=TTL_TASK)
    r.set(f"task:{task_id}:created_at", ts(), ex=TTL_TASK)

    print(json.dumps({"task_id": task_id}))


def cmd_status(args):
    r = get_redis()
    task_id = args.id

    keys = [
        "status", "worker", "thread_id", "description",
        "result", "exit_code", "created_at", "completed_at"
    ]
    info = {"task_id": task_id}
    for k in keys:
        info[k] = r.get(f"task:{task_id}:{k}")

    print(json.dumps(info, indent=2))


def cmd_result(args):
    r = get_redis()
    result = r.get(f"task:{args.id}:result") or ""
    if args.tail is not None:
        if args.tail == 0:
            result = ""
        else:
            lines = result.splitlines()
            result = "\n".join(lines[-args.tail:])
    print(result)


def cmd_list(args):
    r = get_redis()
    tasks = {}

    # Collect task IDs from active_tasks hash
    active = r.hgetall("active_tasks")
    for task_id, raw in active.items():
        try:
            tasks[task_id] = json.loads(raw)
        except json.JSONDecodeError:
            tasks[task_id] = {"status": "unknown"}

    # Also scan task:*:status keys to catch tasks not in active_tasks
    cursor = 0
    while True:
        cursor, keys = r.scan(cursor, match="task:*:status", count=100)
        for key in keys:
            task_id = key.split(":")[1]
            if task_id not in tasks:
                tasks[task_id] = {}

        if cursor == 0:
            break

    # Enrich from per-task keys and apply filters
    worker_filter = getattr(args, 'worker', None)
    status_filter = getattr(args, 'status', None)
    thread_filter = getattr(args, 'thread', None)
    limit = args.limit if args.limit is not None else 50

    rows = []
    for task_id in sorted(tasks.keys()):
        if len(rows) >= limit:
            break

        entry = tasks[task_id]
        entry["task_id"] = task_id

        # Populate missing fields from Redis keys
        if "status" not in entry:
            entry["status"] = r.get(f"task:{task_id}:status") or "unknown"
        if "worker" not in entry:
            entry["worker"] = r.get(f"task:{task_id}:worker") or "-"
        if "thread_id" not in entry:
            entry["thread_id"] = r.get(f"task:{task_id}:thread_id") or "-"
        if "started_at" not in entry:
            entry["started_at"] = r.get(f"task:{task_id}:created_at") or "-"

        if worker_filter and entry.get("worker") != worker_filter:
            continue
        if status_filter and entry.get("status") != status_filter:
            continue
        if thread_filter and entry.get("thread_id") != thread_filter:
            continue

        rows.append(entry)

    # Print summary table
    if not rows:
        print("(no tasks)")
        return

    header = f"{'TASK ID':<38} {'STATUS':<12} {'WORKER':<10} {'THREAD':<20} {'STARTED':<20}"
    print(header)
    print("-" * len(header))
    for entry in rows:
        tid = entry.get("task_id", "")[:36]
        status = entry.get("status", "-")
        worker = entry.get("worker", "-")
        thread = entry.get("thread_id", "-")[:18]
        started = entry.get("started_at", "-")
        print(f"{tid:<38} {status:<12} {worker:<10} {thread:<20} {started:<20}")


def cmd_wait(args):
    r = get_redis()
    task_id = args.id
    timeout = args.timeout

    if not r.exists(f"task:{task_id}:status"):
        die(f"Task {task_id} not found")

    deadline = time.monotonic() + timeout
    try:
        while True:
            status = r.get(f"task:{task_id}:status")
            if status in ("done", "failed", "cancelled"):
                break
            if time.monotonic() >= deadline:
                die(f"Timed out waiting for task {task_id} (status: {status or 'unknown'})")
            time.sleep(2)
    finally:
        # Release thread lock even on timeout so the thread isn't stuck
        thread_id = r.get(f"task:{task_id}:thread_id")
        if thread_id:
            r.delete(f"thread:{thread_id}:lock")

    # Print final status
    info = {"task_id": task_id}
    for k in ["status", "worker", "thread_id", "exit_code", "created_at", "completed_at"]:
        info[k] = r.get(f"task:{task_id}:{k}")
    print(json.dumps(info))


def cmd_requeue_stale(args):
    r = get_redis()
    worker_filter = args.worker
    older_than = args.older_than
    workers_to_check = [worker_filter] if worker_filter else list(WORKERS)

    for worker in workers_to_check:
        processing_key = f"tasks:processing:{worker}"
        queue_key = f"tasks:queue:{worker}"
        items = r.lrange(processing_key, 0, -1)

        for item_json in items:
            try:
                task = json.loads(item_json)
            except json.JSONDecodeError:
                # Corrupt entry — remove it
                r.lrem(processing_key, 0, item_json)
                continue

            task_id = task["task_id"]
            status = r.get(f"task:{task_id}:status")
            created_at = r.get(f"task:{task_id}:created_at")

            requeue = False

            if status is None or status == "pending":
                # Worker crashed before writing status (None) or after
                # BLMOVE but before HSET to "running" (pending)
                requeue = True
            elif status == "running" and created_at:
                # Check if running too long
                try:
                    started = calendar.timegm(time.strptime(created_at, "%Y-%m-%dT%H:%M:%SZ"))
                    age = time.time() - started
                    if age > older_than:
                        requeue = True
                except (ValueError, OverflowError):
                    pass
            elif status in ("done", "failed", "cancelled"):
                # Terminal state — garbage-collect stale processing entry
                r.lrem(processing_key, 0, item_json)
                continue

            if requeue:
                r.lpush(queue_key, item_json)
                r.lrem(processing_key, 0, item_json)
                r.set(f"task:{task_id}:status", "pending", ex=TTL_TASK)
                print(f"Requeued: {task_id} (worker={worker}, was status={status or 'missing'})")


def cmd_cancel(args):
    r = get_redis()
    task_id = args.id
    if not r.exists(f"task:{task_id}:status"):
        die(f"Task {task_id} not found")
    r.set(f"task:{task_id}:cancel", "1", ex=TTL_TASK)
    print(f"Cancel flag set for task {task_id}")


def cmd_unlock(args):
    r = get_redis()
    lock_key = f"thread:{args.thread}:lock"
    if r.delete(lock_key):
        print(f"Lock released for thread '{args.thread}'")
    else:
        print(f"No lock found for thread '{args.thread}'")


# ── thread commands ────────────────────────────────────────────────────────

def cmd_thread_create(args):
    r = get_redis()
    thread_id = args.id
    mapping = {"status": "initiated", "updated_at": ts()}
    if args.repo:
        mapping["gh_repo"] = args.repo
    r.hset(f"thread:{thread_id}:current_state", mapping=mapping)
    r.expire(f"thread:{thread_id}:current_state", TTL_THREAD)
    print(f"Thread '{thread_id}' created")


def cmd_thread_history(args):
    r = get_redis()
    thread_id = args.id
    key = f"thread:{thread_id}:messages"
    if args.tail is not None:
        if args.tail == 0:
            print("(no messages)")
            return
        msgs = r.lrange(key, -args.tail, -1)
    else:
        msgs = r.lrange(key, 0, -1)

    if not msgs:
        print("(no messages)")
        return

    for msg in msgs:
        try:
            data = json.loads(msg)
            print(f"[{data.get('role', '?')}] {data.get('timestamp', '?')}")
            print(data.get('content', ''))
            print("---")
        except json.JSONDecodeError:
            print(msg)
            print("---")


def cmd_thread_state(args):
    r = get_redis()
    state = r.hgetall(f"thread:{args.id}:current_state") or {}
    print(json.dumps(state, indent=2))


def cmd_thread_update(args):
    r = get_redis()
    thread_id = args.id
    key = f"thread:{thread_id}:current_state"

    if not r.exists(key):
        die(f"Thread '{thread_id}' not found. Run 'task.py thread-create --id {thread_id}' first.")

    mapping = {"status": args.status, "updated_at": ts()}
    if args.design is not None:
        mapping["last_design"] = args.design
    if args.pr is not None:
        mapping["gh_pr_number"] = str(args.pr)

    r.hset(key, mapping=mapping)
    r.expire(key, TTL_THREAD)
    print(f"Thread '{thread_id}' updated")


def cmd_thread_list(args):
    r = get_redis()
    cursor = 0
    threads = []

    while True:
        cursor, keys = r.scan(cursor, match="thread:*:current_state", count=100)
        for key in keys:
            thread_id = key.split(":")[1]
            state = r.hgetall(key) or {}
            threads.append({
                "thread_id": thread_id,
                "status": state.get("status", "unknown"),
                "updated_at": state.get("updated_at", "-"),
                "gh_repo": state.get("gh_repo", "-"),
                "gh_pr_number": state.get("gh_pr_number", "-"),
            })
        if cursor == 0:
            break

    if not threads:
        print("(no threads)")
        return

    threads.sort(key=lambda t: t.get("updated_at", ""), reverse=True)

    header = f"{'THREAD ID':<30} {'STATUS':<16} {'UPDATED':<20} {'REPO':<20} {'PR':<6}"
    print(header)
    print("-" * len(header))
    for t in threads:
        print(f"{t['thread_id']:<30} {t['status']:<16} {t['updated_at']:<20} "
              f"{t['gh_repo']:<20} {t['gh_pr_number']:<6}")


def cmd_thread_cleanup(args):
    thread_id = args.id
    workspace_path = os.path.join(WORKSPACE_DIR, thread_id)
    if os.path.isdir(workspace_path):
        try:
            shutil.rmtree(workspace_path)
            print(f"Deleted {workspace_path}")
        except PermissionError as e:
            die(f"Cannot delete {workspace_path}: {e}")
    else:
        print(f"Nothing to clean up: {workspace_path} does not exist")


# ── cli ────────────────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser(prog="task.py")
    sub = parser.add_subparsers(dest="command", required=True)

    # task enqueue
    p = sub.add_parser("enqueue", help="Push a task onto a worker queue")
    p.add_argument("--worker", required=True, choices=WORKERS)
    p.add_argument("--thread", required=True)
    p.add_argument("--instruction", required=True)

    # task status
    p = sub.add_parser("status", help="Show task status")
    p.add_argument("--id", required=True)

    # task result
    p = sub.add_parser("result", help="Print task result")
    p.add_argument("--id", required=True)
    p.add_argument("--tail", type=int, default=None)

    # task list
    p = sub.add_parser("list", help="List tasks")
    p.add_argument("--worker", choices=WORKERS, default=None)
    p.add_argument("--status", default=None)
    p.add_argument("--thread", default=None)
    p.add_argument("--limit", type=int, default=50)

    # task wait
    p = sub.add_parser("wait", help="Block until task completes")
    p.add_argument("--id", required=True)
    p.add_argument("--timeout", type=int, default=300)

    # task requeue-stale
    p = sub.add_parser("requeue-stale", help="Requeue stale in-flight tasks")
    p.add_argument("--worker", choices=WORKERS, default=None)
    p.add_argument("--older-than", type=int, default=600)

    # task cancel
    p = sub.add_parser("cancel", help="Cancel a pending task")
    p.add_argument("--id", required=True)

    # task unlock
    p = sub.add_parser("unlock", help="Release a stale thread lock")
    p.add_argument("--thread", required=True)

    # thread create
    p = sub.add_parser("thread-create", help="Initialize a new thread")
    p.add_argument("--id", required=True)
    p.add_argument("--repo", default=None)

    # thread history
    p = sub.add_parser("thread-history", help="Print thread message history")
    p.add_argument("--id", required=True)
    p.add_argument("--tail", type=int, default=None)

    # thread state
    p = sub.add_parser("thread-state", help="Print thread current state")
    p.add_argument("--id", required=True)

    # thread update
    p = sub.add_parser("thread-update", help="Update thread current state")
    p.add_argument("--id", required=True)
    p.add_argument("--status", required=True)
    p.add_argument("--design", default=None)
    p.add_argument("--pr", type=int, default=None)

    # thread list
    sub.add_parser("thread-list", help="List all threads")

    # thread cleanup
    p = sub.add_parser("thread-cleanup", help="Delete thread workspace directory")
    p.add_argument("--id", required=True)

    args = parser.parse_args()

    command_map = {
        "enqueue": cmd_enqueue,
        "status": cmd_status,
        "result": cmd_result,
        "list": cmd_list,
        "wait": cmd_wait,
        "requeue-stale": cmd_requeue_stale,
        "cancel": cmd_cancel,
        "unlock": cmd_unlock,
        "thread-create": cmd_thread_create,
        "thread-history": cmd_thread_history,
        "thread-state": cmd_thread_state,
        "thread-update": cmd_thread_update,
        "thread-list": cmd_thread_list,
        "thread-cleanup": cmd_thread_cleanup,
    }

    try:
        command_map[args.command](args)
    except redis.exceptions.ConnectionError as e:
        die(f"Redis connection failed ({REDIS_HOST}:{REDIS_PORT}): {e}")


if __name__ == "__main__":
    main()
