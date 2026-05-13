#!/usr/bin/env python3
"""Side-by-side compatibility test: task.py (Python) vs task (Go).

Validates that the Go task CLI produces identical Redis state and equivalent
stdout to the Python task.py for every operation.

Requires:
  - Real Redis (REDIS_HOST, REDIS_PORT, COMPAT_TEST_DB env vars)
  - Go binary: go build -o /tmp/task ./cmd/task/

Usage:
  go build -o /tmp/task ./cmd/task/
  REDIS_HOST=172.17.0.2 python3 scripts/test_json_compat.py
"""

import json
import os
import re
import subprocess
import sys

# ── config ──────────────────────────────────────────────────────────────────

REDIS_HOST = os.environ.get("REDIS_HOST", "172.17.0.2")
REDIS_PORT = int(os.environ.get("REDIS_PORT", 6379))
COMPAT_DB = int(os.environ.get("COMPAT_TEST_DB", 15))
GO_BINARY = os.environ.get("GO_BINARY", "/tmp/task")
PYTHON_SCRIPT = os.environ.get("PYTHON_SCRIPT",
    os.path.join(os.path.dirname(__file__), "task.py"))

try:
    import redis
except ImportError:
    print("ERROR: redis module not installed. Run: pip install redis")
    sys.exit(1)

r = redis.Redis(host=REDIS_HOST, port=REDIS_PORT, db=COMPAT_DB, decode_responses=False)

UUID_RE = re.compile(r'[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}')
TIMESTAMP_RE = re.compile(r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z')

passed = 0
failed = 0


def flush():
    r.flushdb()


def snapshot():
    """Capture all Redis keys with decoded, normalized values for comparison."""
    snap = {}
    for key in r.keys("*"):
        k = key.decode()
        v = _decode_key(k)
        snap[k] = v
    return snap


def _decode_key(key):
    """Decode a Redis key's value for comparison. Returns type-prefixed tuple."""
    key_type = r.type(key).decode()
    if key_type == "string":
        val = r.get(key)
        return ("string", val.decode() if val else None)
    elif key_type == "hash":
        raw = r.hgetall(key)
        return ("hash", {k.decode(): v.decode() for k, v in raw.items()})
    elif key_type == "list":
        items = r.lrange(key, 0, -1)
        return ("list", [item.decode() for item in items])
    elif key_type == "none":
        return ("none", None)
    return ("unknown", None)


def normalize_val(val):
    """Normalize variable content in decoded Redis values.
    For JSON strings, parses and normalizes recursively to avoid
    formatting differences (Go vs Python JSON spacing)."""
    if isinstance(val, str):
        # Try JSON parse first — if it's valid JSON, normalize the structure
        try:
            obj = json.loads(val)
            return normalize_val(obj)
        except (json.JSONDecodeError, ValueError):
            pass
        return UUID_RE.sub("<UUID>", TIMESTAMP_RE.sub("<TS>", val))
    elif isinstance(val, list):
        return [normalize_val(v) for v in val]
    elif isinstance(val, dict):
        return {normalize_val(k): normalize_val(v) for k, v in val.items()}
    elif isinstance(val, tuple):
        return tuple(normalize_val(v) for v in val)
    return val


def base_env():
    env = os.environ.copy()
    env["REDIS_HOST"] = REDIS_HOST
    env["REDIS_PORT"] = str(REDIS_PORT)
    env["COMPAT_TEST_DB"] = str(COMPAT_DB)
    return env


def run_py(args):
    """Run task.py, return (stdout, stderr, exit_code)."""
    p = subprocess.run(["python3", PYTHON_SCRIPT] + args,
        capture_output=True, text=True, timeout=30, env=base_env())
    return p.stdout.rstrip("\n"), p.stderr.rstrip("\n"), p.returncode


def run_go(args):
    """Run Go task binary, return (stdout, stderr, exit_code)."""
    p = subprocess.run([GO_BINARY] + args,
        capture_output=True, text=True, timeout=30, env=base_env())
    return p.stdout.rstrip("\n"), p.stderr.rstrip("\n"), p.returncode


def normalize(text):
    """Replace UUIDs and timestamps with placeholders."""
    text = UUID_RE.sub("<UUID>", text)
    text = TIMESTAMP_RE.sub("<TS>", text)
    return text


def is_json(s):
    """Check if a string is valid JSON."""
    try:
        json.loads(s)
        return True
    except (json.JSONDecodeError, ValueError):
        return False


def compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, name):
    """Compare stdout/stderr/exit code. Uses semantic comparison for JSON."""
    global passed, failed
    ok = True

    if py_exit != go_exit:
        print(f"  FAIL: Exit code: Python={py_exit} Go={go_exit}")
        ok = False

    # Compare stderr (normalized)
    py_err_n = normalize(py_err)
    go_err_n = normalize(go_err)
    if py_err_n != go_err_n:
        print(f"  FAIL: stderr differs:")
        print(f"    Python: {py_err[:200]}")
        print(f"    Go:     {go_err[:200]}")
        ok = False

    # Compare stdout — semantic for JSON, normalized-string for tables
    if is_json(py_out) and is_json(go_out):
        py_obj = normalize_val(json.loads(py_out))
        go_obj = normalize_val(json.loads(go_out))
        if py_obj != go_obj:
            print(f"  FAIL: JSON stdout differs:")
            print(f"    Python: {py_out[:200]}")
            print(f"    Go:     {go_out[:200]}")
            ok = False
    else:
        py_n = normalize(py_out)
        go_n = normalize(go_out)
        if py_n != go_n:
            print(f"  FAIL: stdout differs:")
            print(f"    Python: {py_out[:200]}")
            print(f"    Go:     {go_out[:200]}")
            ok = False

    status = "PASS" if ok else "FAIL"
    if ok:
        passed += 1
    else:
        failed += 1
    print(f"  {status}: {name} (output)")
    return ok


def compare_redis(py_snap, go_snap, name):
    """Compare Redis snapshots with value normalization for variable content."""
    global passed, failed
    ok = True

    py_norm = {UUID_RE.sub("<UUID>", k): normalize_val(v) for k, v in py_snap.items()}
    go_norm = {UUID_RE.sub("<UUID>", k): normalize_val(v) for k, v in go_snap.items()}

    py_set = set(py_norm.keys())
    go_set = set(go_norm.keys())

    only_py = py_set - go_set
    if only_py:
        print(f"  FAIL: Keys only in Python: {only_py}")
        ok = False
    only_go = go_set - py_set
    if only_go:
        print(f"  FAIL: Keys only in Go: {only_go}")
        ok = False

    for key in sorted(py_set & go_set):
        if py_norm[key] != go_norm[key]:
            py_type = type(py_norm[key]).__name__
            go_type = type(go_norm[key]).__name__
            print(f"  FAIL: Key '{key}' differs (py={py_type}, go={go_type}):")
            print(f"    Python: {str(py_norm[key])[:200]}")
            print(f"    Go:     {str(go_norm[key])[:200]}")
            ok = False

    status = "PASS" if ok else "FAIL"
    if ok:
        passed += 1
    else:
        failed += 1
    print(f"  {status}: {name} (redis)")
    return ok


def test_roundtrip(name, py_args, go_args=None, check_redis=True, check_output=True):
    """Run Python and Go with same args, compare both Redis and output."""
    if go_args is None:
        go_args = py_args

    print(f"\n-- {name} --")
    all_ok = True

    if check_redis:
        flush()
        run_py(py_args)
        py_snap = snapshot()

        flush()
        run_go(go_args)
        go_snap = snapshot()

        if not compare_redis(py_snap, go_snap, name + " (redis)"):
            all_ok = False

    if check_output:
        flush()
        py_out, py_err, py_exit = run_py(py_args)
        flush()
        go_out, go_err, go_exit = run_go(go_args)

        if not compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, name + " (output)"):
            all_ok = False

    return all_ok


# ── setup helpers ──────────────────────────────────────────────────────────

def setup_task(task_id, worker="claude", thread_id="thr1", status="done",
               result="success output", exit_code="0",
               created="2025-01-01T00:00:00Z", completed="2025-01-01T00:05:00Z",
               desc="do something"):
    """Set up task keys directly in Redis."""
    r.set(f"task:{task_id}:status", status)
    r.set(f"task:{task_id}:worker", worker)
    r.set(f"task:{task_id}:thread_id", thread_id)
    if result:
        r.set(f"task:{task_id}:result", result)
    r.set(f"task:{task_id}:exit_code", exit_code)
    r.set(f"task:{task_id}:created_at", created)
    if completed:
        r.set(f"task:{task_id}:completed_at", completed)
    r.set(f"task:{task_id}:description", desc)


def setup_thread(thread_id="thr-test"):
    """Create a populated thread."""
    r.hset(f"thread:{thread_id}:current_state", mapping={
        "status": "initiated",
        "updated_at": "2025-01-01T00:00:00Z",
        "gh_repo": "owner/repo",
    })
    r.rpush(f"thread:{thread_id}:messages",
        '{"role":"user","type":"request","content":"hello","timestamp":"2025-01-01T00:00:00Z"}',
        '{"role":"master","type":"plan","content":"planning","timestamp":"2025-01-01T00:00:05Z"}',
        '{"role":"master","type":"response","content":"done","timestamp":"2025-01-01T00:01:00Z"}',
    )


# ── tests ──────────────────────────────────────────────────────────────────

def test_enqueue():
    return test_roundtrip("enqueue",
        ["enqueue", "--worker", "claude", "--thread", "thr1", "--instruction", "do X"])

def test_enqueue_copilot():
    return test_roundtrip("enqueue-copilot",
        ["enqueue", "--worker", "copilot", "--thread", "thr2", "--instruction", "fix bug"])

def test_status():
    """status: JSON output format comparison."""
    print("\n-- status --")
    ok = True

    # Python: enqueue + status on its own task
    flush()
    py_out, _, _ = run_py(["enqueue", "--worker", "claude", "--thread", "thr-s", "--instruction", "test"])
    py_id = json.loads(py_out)["task_id"]
    py_out2, py_err2, py_exit2 = run_py(["status", "--id", py_id])

    # Go: enqueue + status on its own task
    flush()
    go_out, _, _ = run_go(["enqueue", "--worker", "claude", "--thread", "thr-s", "--instruction", "test"])
    go_id = json.loads(go_out)["task_id"]
    go_out2, go_err2, go_exit2 = run_go(["status", "--id", go_id])

    if not compare_output(py_out2, go_out2, py_err2, go_err2, py_exit2, go_exit2, "status"):
        ok = False

    # Redis comparison
    flush()
    run_py(["enqueue", "--worker", "claude", "--thread", "thr-s2", "--instruction", "test"])
    py_snap = snapshot()
    flush()
    run_go(["enqueue", "--worker", "claude", "--thread", "thr-s2", "--instruction", "test"])
    go_snap = snapshot()
    if not compare_redis(py_snap, go_snap, "status (redis)"):
        ok = False

    return ok


def test_result():
    print("\n-- result --")
    tid = "result-1"

    flush()
    setup_task(tid)
    py_out, py_err, py_exit = run_py(["result", "--id", tid])

    flush()
    setup_task(tid)
    go_out, go_err, go_exit = run_go(["result", "--id", tid])

    return compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "result")


def test_result_tail():
    print("\n-- result --tail --")
    tid = "rtail-1"

    flush()
    setup_task(tid, result="line1\nline2\nline3\nline4")
    py_out, py_err, py_exit = run_py(["result", "--id", tid, "--tail", "2"])

    flush()
    setup_task(tid, result="line1\nline2\nline3\nline4")
    go_out, go_err, go_exit = run_go(["result", "--id", tid, "--tail", "2"])

    return compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "result --tail 2")


def test_result_tail_zero():
    print("\n-- result --tail 0 --")
    tid = "rtail0-1"

    flush()
    setup_task(tid, result="line1\nline2")
    py_out, py_err, py_exit = run_py(["result", "--id", tid, "--tail", "0"])

    flush()
    setup_task(tid, result="line1\nline2")
    go_out, go_err, go_exit = run_go(["result", "--id", tid, "--tail", "0"])

    return compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "result --tail 0")


def test_list():
    """list: table output (same Redis state, deterministic task IDs)."""
    print("\n-- list --")

    # Use deterministic task IDs so table output is comparable
    flush()
    for i in range(3):
        tid = f"a{i:04d}-1111-2222-3333-444444444444"
        setup_task(tid, worker="claude", thread_id=f"thr{i%2}",
                   created=f"2025-01-0{i+1}T00:00:00Z", result="", completed=None, desc="")

    py_out, py_err, py_exit = run_py(["list"])
    go_out, go_err, go_exit = run_go(["list"])

    return compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "list")


def test_list_empty():
    flush()
    py_out, py_err, py_exit = run_py(["list"])
    go_out, go_err, go_exit = run_go(["list"])
    return compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "list (empty)")


def test_wait_done():
    print("\n-- wait (done) --")
    tid = "wait-done-1"

    flush()
    setup_task(tid)
    py_out, py_err, py_exit = run_py(["wait", "--id", tid, "--timeout", "5"])

    flush()
    setup_task(tid)
    go_out, go_err, go_exit = run_go(["wait", "--id", tid, "--timeout", "5"])

    return compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "wait (done)")


def test_wait_timeout():
    print("\n-- wait (timeout) --")
    tid = "wait-timeout-1"

    flush()
    setup_task(tid, status="running", result="", completed=None, desc="")
    py_out, py_err, py_exit = run_py(["wait", "--id", tid, "--timeout", "1"])

    flush()
    setup_task(tid, status="running", result="", completed=None, desc="")
    go_out, go_err, go_exit = run_go(["wait", "--id", tid, "--timeout", "1"])

    return compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "wait (timeout)")


def test_cancel():
    print("\n-- cancel --")
    tid = "cancel-1"

    flush()
    r.set(f"task:{tid}:status", "pending")
    py_out, py_err, py_exit = run_py(["cancel", "--id", tid])
    py_snap = snapshot()

    flush()
    r.set(f"task:{tid}:status", "pending")
    go_out, go_err, go_exit = run_go(["cancel", "--id", tid])
    go_snap = snapshot()

    ok1 = compare_redis(py_snap, go_snap, "cancel")
    ok2 = compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "cancel")
    return ok1 and ok2


def test_requeue_stale():
    print("\n-- requeue-stale --")

    def setup():
        r.lpush("tasks:processing:claude",
            '{"task_id":"stale-1","thread_id":"thr1","instruction":"do X"}')
        r.lpush("tasks:processing:claude",
            '{"task_id":"stale-2","thread_id":"thr1","instruction":"do Y"}')
        r.set("task:stale-2:status", "running")
        r.set("task:stale-2:worker", "claude")
        r.set("task:stale-2:thread_id", "thr1")
        r.set("task:stale-2:created_at", "2020-01-01T00:00:00Z")
        r.set("task:stale-2:description", "do Y")
        r.lpush("tasks:processing:claude",
            '{"task_id":"done-1","thread_id":"thr1","instruction":"do Z"}')
        r.set("task:done-1:status", "done")
        r.set("task:done-1:worker", "claude")
        r.set("task:done-1:thread_id", "thr1")
        r.set("task:done-1:created_at", "2020-01-01T00:00:00Z")
        r.set("task:done-1:description", "do Z")

    flush(); setup()
    py_out, py_err, py_exit = run_py(["requeue-stale", "--worker", "claude", "--older-than", "600"])
    py_snap = snapshot()

    flush(); setup()
    go_out, go_err, go_exit = run_go(["requeue-stale", "--worker", "claude", "--older-than", "600"])
    go_snap = snapshot()

    ok1 = compare_redis(py_snap, go_snap, "requeue-stale (redis)")
    ok2 = compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "requeue-stale (output)")
    return ok1 and ok2


def test_thread_create():
    return test_roundtrip("thread-create",
        ["thread-create", "--id", "thr-new", "--repo", "owner/repo"])


def test_thread_state():
    print("\n-- thread-state --")
    flush()
    setup_thread("thr-test")
    py_out, py_err, py_exit = run_py(["thread-state", "--id", "thr-test"])
    go_out, go_err, go_exit = run_go(["thread-state", "--id", "thr-test"])
    return compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "thread-state")


def test_thread_update():
    print("\n-- thread-update --")
    flush()
    run_py(["thread-create", "--id", "thr-upd2"])
    py_out, py_err, py_exit = run_py(
        ["thread-update", "--id", "thr-upd2", "--status", "complete", "--design", "use redis", "--pr", "42"])
    py_snap = snapshot()

    flush()
    run_go(["thread-create", "--id", "thr-upd2"])
    go_out, go_err, go_exit = run_go(
        ["thread-update", "--id", "thr-upd2", "--status", "complete", "--design", "use redis", "--pr", "42"])
    go_snap = snapshot()

    ok1 = compare_redis(py_snap, go_snap, "thread-update (redis)")
    ok2 = compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "thread-update (output)")
    return ok1 and ok2


def test_thread_list():
    print("\n-- thread-list --")
    flush()
    r.hset("thread:thr-a:current_state", mapping={
        "status": "initiated", "updated_at": "2025-01-01T00:00:00Z",
        "gh_repo": "a/b", "gh_pr_number": "1",
    })
    r.hset("thread:thr-b:current_state", mapping={
        "status": "complete", "updated_at": "2025-01-02T00:00:00Z",
        "gh_repo": "c/d", "gh_pr_number": "2",
    })
    py_out, py_err, py_exit = run_py(["thread-list"])
    go_out, go_err, go_exit = run_go(["thread-list"])
    return compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "thread-list")


def test_thread_list_empty():
    flush()
    py_out, py_err, py_exit = run_py(["thread-list"])
    go_out, go_err, go_exit = run_go(["thread-list"])
    return compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "thread-list (empty)")


def test_thread_history():
    print("\n-- thread-history --")
    flush()
    setup_thread("thr-test")
    py_out, py_err, py_exit = run_py(["thread-history", "--id", "thr-test"])
    go_out, go_err, go_exit = run_go(["thread-history", "--id", "thr-test"])
    return compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "thread-history")


def test_thread_history_tail():
    flush()
    setup_thread("thr-test")
    py_out, py_err, py_exit = run_py(["thread-history", "--id", "thr-test", "--tail", "2"])
    go_out, go_err, go_exit = run_go(["thread-history", "--id", "thr-test", "--tail", "2"])
    return compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "thread-history --tail 2")


def test_thread_history_empty():
    flush()
    r.hset("thread:thr-empty:current_state", mapping={
        "status": "initiated", "updated_at": "2025-01-01T00:00:00Z",
    })
    py_out, py_err, py_exit = run_py(["thread-history", "--id", "thr-empty"])
    go_out, go_err, go_exit = run_go(["thread-history", "--id", "thr-empty"])
    return compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "thread-history (empty)")


def test_unlock():
    return test_roundtrip("unlock", ["unlock", "--thread", "thr-unlock"])


def test_unlock_existing():
    print("\n-- unlock (existing) --")
    flush()
    r.set("thread:thr-locked:lock", "some-task")
    py_out, py_err, py_exit = run_py(["unlock", "--thread", "thr-locked"])
    py_snap = snapshot()

    flush()
    r.set("thread:thr-locked:lock", "some-task")
    go_out, go_err, go_exit = run_go(["unlock", "--thread", "thr-locked"])
    go_snap = snapshot()

    ok1 = compare_redis(py_snap, go_snap, "unlock existing (redis)")
    ok2 = compare_output(py_out, go_out, py_err, go_err, py_exit, go_exit, "unlock existing (output)")
    return ok1 and ok2


# ── main ────────────────────────────────────────────────────────────────────

def main():
    global passed, failed

    print("=" * 60)
    print("JSON Compatibility Test Suite")
    print(f"Redis: {REDIS_HOST}:{REDIS_PORT} (db={COMPAT_DB})")
    print(f"Go: {GO_BINARY}  Python: {PYTHON_SCRIPT}")
    print("=" * 60)

    try:
        r.ping()
    except Exception as e:
        print(f"ERROR: Cannot connect to Redis: {e}")
        sys.exit(1)
    if not os.path.exists(GO_BINARY):
        print(f"ERROR: Go binary not found: {GO_BINARY}")
        print("Build: go build -o /tmp/task ./cmd/task/")
        sys.exit(1)

    tests = [
        test_enqueue,
        test_enqueue_copilot,
        test_status,
        test_result,
        test_result_tail,
        test_result_tail_zero,
        test_list,
        test_list_empty,
        test_wait_done,
        test_wait_timeout,
        test_cancel,
        test_requeue_stale,
        test_thread_create,
        test_thread_state,
        test_thread_update,
        test_thread_list,
        test_thread_list_empty,
        test_thread_history,
        test_thread_history_tail,
        test_thread_history_empty,
        test_unlock,
        test_unlock_existing,
    ]

    for test_func in tests:
        try:
            test_func()
        except Exception as e:
            failed += 1
            print(f"  ERROR: {test_func.__name__}: {e}")
            import traceback
            traceback.print_exc()

    print(f"\n{'=' * 60}")
    print(f"Results: {passed} passed, {failed} failed")
    print(f"{'=' * 60}")

    if failed > 0:
        sys.exit(1)


if __name__ == "__main__":
    main()
