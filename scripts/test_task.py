"""Unit tests for task.py — the master CLI tool for Redis-based task/thread management.

Tests match the actual implementation at scripts/task.py:
    - Each command function takes only (args) — Redis client is obtained via get_redis()
    - Errors call die(msg, code) which does sys.exit(code)
    - Output goes to stdout (print) or stderr (die)
    - Functions do not return values

Testing strategy:
    - Mock task.get_redis() → fakeredis instance
    - Mock task.die() → raise DieError(code, msg) instead of sys.exit()
    - Use capsys to capture stdout/stderr
    - Patch time where needed (wait polling, stale age checks)
"""

import argparse
import json
import os
import time
from unittest.mock import patch

import pytest
import fakeredis

import task


# ── Helpers ───────────────────────────────────────────────────────────────────

TEST_THREAD = "test-thread"
TEST_TASK_ID = "task-00000000-0000-0000-0000-000000000001"
WORKER = "claude"


class DieError(Exception):
    """Raised instead of sys.exit when die() is mocked."""
    def __init__(self, msg, code=1):
        self.msg = msg
        self.code = code
        super().__init__(msg)


def make_task_payload(task_id=TEST_TASK_ID, thread_id=TEST_THREAD, instruction="Test instruction"):
    return json.dumps({"task_id": task_id, "thread_id": thread_id, "instruction": instruction})


def make_msg(role, content, task_id=TEST_TASK_ID, ts=None):
    return json.dumps({
        "role": role,
        "content": content,
        "timestamp": ts or "2026-05-10T00:00:00Z",
        "metadata": {"task_id": task_id, "tokens": 500},
    })


def seconds_ago(s):
    """Return ISO timestamp s seconds in the past (UTC)."""
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - s))


def mock_die(msg, code=1):
    raise DieError(msg, code)


# ── Fixtures ──────────────────────────────────────────────────────────────────

@pytest.fixture(autouse=True)
def mock_redis(monkeypatch):
    """Replace task.get_redis() with a fakeredis instance for every test."""
    r = fakeredis.FakeRedis(decode_responses=True)
    monkeypatch.setattr(task, "get_redis", lambda: r)
    monkeypatch.setattr(task, "die", mock_die)
    return r


@pytest.fixture
def thread_with_history(mock_redis):
    """Pre-populated thread with message history and current_state."""
    r = mock_redis
    r.rpush(f"thread:{TEST_THREAD}:messages", make_msg("master", "Initial design"))
    r.rpush(f"thread:{TEST_THREAD}:messages", make_msg("claude", "Design result"))
    r.hset(f"thread:{TEST_THREAD}:current_state", mapping={
        "status": "awaiting_review",
        "last_design": "v1 design text",
        "last_updated_by": "claude",
        "last_task_id": "prev-task-id",
        "updated_at": "2026-05-10T00:00:00Z",
    })
    return r


@pytest.fixture
def running_task(mock_redis):
    """Pre-populated running task with all keys set."""
    r = mock_redis
    r.set(f"task:{TEST_TASK_ID}:status", "running")
    r.set(f"task:{TEST_TASK_ID}:worker", WORKER)
    r.set(f"task:{TEST_TASK_ID}:thread_id", TEST_THREAD)
    r.set(f"task:{TEST_TASK_ID}:description", "Test instruction")
    r.set(f"task:{TEST_TASK_ID}:created_at", "2026-05-10T00:00:00Z")
    r.set(f"task:{TEST_TASK_ID}:result", "")
    r.set(f"task:{TEST_TASK_ID}:exit_code", "")
    r.set(f"task:{TEST_TASK_ID}:completed_at", "")
    r.hset("active_tasks", TEST_TASK_ID, json.dumps({
        "status": "running", "worker": WORKER,
        "thread_id": TEST_THREAD, "started_at": "2026-05-10T00:00:00Z",
    }))
    return r


def parse_stdout_json(capsys):
    """Parse captured stdout as JSON."""
    return json.loads(capsys.readouterr().out.strip())


# ═══════════════════════════════════════════════════════════════════════════════
# Task Management: enqueue
# ═══════════════════════════════════════════════════════════════════════════════

class TestEnqueue:
    """task.py enqueue --worker <w> --thread <t> --instruction "<text>"

    LPUSHes task JSON onto tasks:queue:<worker>, acquires thread lock via
    SET NX, and RPUSHes the instruction to thread:{id}:messages (role=master).
    Prints JSON: {"task_id": "<id>"}.
    """

    def test_enqueue_success(self, mock_redis, capsys):
        r = mock_redis
        args = argparse.Namespace(worker=WORKER, thread=TEST_THREAD, instruction="Implement OAuth2")
        task.cmd_enqueue(args)

        result = parse_stdout_json(capsys)
        assert "task_id" in result
        task_id = result["task_id"]

        queue = r.lrange(f"tasks:queue:{WORKER}", 0, -1)
        assert len(queue) == 1
        payload = json.loads(queue[0])
        assert payload["thread_id"] == TEST_THREAD
        assert payload["instruction"] == "Implement OAuth2"

        assert r.exists(f"thread:{TEST_THREAD}:lock")
        assert r.get(f"thread:{TEST_THREAD}:lock") == task_id

        msgs = r.lrange(f"thread:{TEST_THREAD}:messages", 0, -1)
        assert len(msgs) == 1
        msg = json.loads(msgs[0])
        assert msg["role"] == "master"
        assert msg["content"] == "Implement OAuth2"

        assert r.get(f"task:{task_id}:status") == "pending"
        assert r.get(f"task:{task_id}:worker") == WORKER

    def test_enqueue_thread_locked(self, mock_redis):
        r = mock_redis
        r.set(f"thread:{TEST_THREAD}:lock", "other-task-id")

        args = argparse.Namespace(worker=WORKER, thread=TEST_THREAD, instruction="Implement OAuth2")
        with pytest.raises(DieError) as exc:
            task.cmd_enqueue(args)

        assert "locked" in exc.value.msg.lower()
        assert r.llen(f"tasks:queue:{WORKER}") == 0

    def test_enqueue_no_existing_thread(self, mock_redis, capsys):
        r = mock_redis
        assert not r.exists(f"thread:{TEST_THREAD}:current_state")

        args = argparse.Namespace(worker=WORKER, thread=TEST_THREAD, instruction="Implement OAuth2")
        task.cmd_enqueue(args)

        assert "task_id" in capsys.readouterr().out
        assert r.exists(f"thread:{TEST_THREAD}:lock")

    def test_enqueue_unique_ids(self, mock_redis, capsys):
        args = argparse.Namespace(worker=WORKER, thread=TEST_THREAD, instruction="Task A")
        task.cmd_enqueue(args)
        id1 = parse_stdout_json(capsys)["task_id"]
        mock_redis.delete(f"thread:{TEST_THREAD}:lock")
        task.cmd_enqueue(args)
        id2 = parse_stdout_json(capsys)["task_id"]
        assert id1 != id2
        assert len(id1) == 36 and id1.count("-") == 4

    def test_enqueue_instruction_in_thread(self, mock_redis):
        args = argparse.Namespace(worker=WORKER, thread=TEST_THREAD, instruction="Design OAuth2 with PKCE")
        task.cmd_enqueue(args)
        msgs = mock_redis.lrange(f"thread:{TEST_THREAD}:messages", 0, -1)
        msg = json.loads(msgs[0])
        assert msg["role"] == "master"
        assert msg["content"] == "Design OAuth2 with PKCE"


# ═══════════════════════════════════════════════════════════════════════════════
# Task Management: status
# ═══════════════════════════════════════════════════════════════════════════════

class TestStatus:
    """task.py status --id <task_id>

    Prints JSON with task_id, status, worker, thread_id, description,
    result, exit_code, created_at, completed_at. Uses indent=2.
    """

    def test_status_running(self, mock_redis, running_task, capsys):
        args = argparse.Namespace(id=TEST_TASK_ID)
        task.cmd_status(args)

        result = parse_stdout_json(capsys)
        assert result["task_id"] == TEST_TASK_ID
        assert result["status"] == "running"
        assert result["worker"] == WORKER
        assert result["thread_id"] == TEST_THREAD

    def test_status_done(self, mock_redis, capsys):
        r = mock_redis
        for f, v in [("status", "done"), ("worker", "opencode"), ("thread_id", TEST_THREAD),
                      ("exit_code", "0"), ("created_at", "2026-05-10T00:00:00Z"),
                      ("completed_at", "2026-05-10T00:05:00Z")]:
            r.set(f"task:{TEST_TASK_ID}:{f}", v)

        args = argparse.Namespace(id=TEST_TASK_ID)
        task.cmd_status(args)

        result = parse_stdout_json(capsys)
        assert result["status"] == "done"
        assert result["exit_code"] == "0"
        assert result["completed_at"] == "2026-05-10T00:05:00Z"

    def test_status_nonexistent(self, mock_redis, capsys):
        args = argparse.Namespace(id="nonexistent-id")
        # status command does NOT call die() for missing tasks —
        # it just returns all None values for the keys
        task.cmd_status(args)
        result = parse_stdout_json(capsys)
        assert result["status"] is None

    def test_status_failed(self, mock_redis, capsys):
        r = mock_redis
        r.set(f"task:{TEST_TASK_ID}:status", "failed")
        r.set(f"task:{TEST_TASK_ID}:worker", WORKER)
        r.set(f"task:{TEST_TASK_ID}:exit_code", "1")

        args = argparse.Namespace(id=TEST_TASK_ID)
        task.cmd_status(args)

        result = parse_stdout_json(capsys)
        assert result["status"] == "failed"
        assert result["exit_code"] == "1"

    def test_status_cancelled(self, mock_redis, capsys):
        r = mock_redis
        r.set(f"task:{TEST_TASK_ID}:status", "cancelled")
        r.set(f"task:{TEST_TASK_ID}:exit_code", "-1")

        args = argparse.Namespace(id=TEST_TASK_ID)
        task.cmd_status(args)

        result = parse_stdout_json(capsys)
        assert result["status"] == "cancelled"


# ═══════════════════════════════════════════════════════════════════════════════
# Task Management: result
# ═══════════════════════════════════════════════════════════════════════════════

class TestResult:
    """task.py result --id <task_id> [--tail N]

    Prints the result field. With --tail, prints last N lines.
    """

    RESULT_TEXT = "line1\nline2\nline3\nline4\nline5"

    @pytest.fixture
    def done_task(self, mock_redis):
        r = mock_redis
        r.set(f"task:{TEST_TASK_ID}:status", "done")
        r.set(f"task:{TEST_TASK_ID}:result", self.RESULT_TEXT)

    def test_result_full(self, mock_redis, done_task, capsys):
        args = argparse.Namespace(id=TEST_TASK_ID, tail=None)
        task.cmd_result(args)

        stdout = capsys.readouterr().out.strip()
        assert stdout == self.RESULT_TEXT

    def test_result_tail_lines(self, mock_redis, done_task, capsys):
        """--tail N returns last N lines (not characters)."""
        args = argparse.Namespace(id=TEST_TASK_ID, tail=2)
        task.cmd_result(args)

        stdout = capsys.readouterr().out.strip()
        assert stdout == "line4\nline5"

    def test_result_nonexistent(self, mock_redis, capsys):
        args = argparse.Namespace(id="nonexistent-id", tail=None)
        task.cmd_result(args)
        # Empty string when no result key exists
        assert capsys.readouterr().out.strip() == ""

    def test_result_empty(self, mock_redis, capsys):
        r = mock_redis
        r.set(f"task:{TEST_TASK_ID}:status", "done")
        r.set(f"task:{TEST_TASK_ID}:result", "")

        args = argparse.Namespace(id=TEST_TASK_ID, tail=None)
        task.cmd_result(args)
        assert capsys.readouterr().out.strip() == ""

    def test_result_tail_exceeds_lines(self, mock_redis, capsys):
        """--tail larger than line count returns all lines."""
        r = mock_redis
        r.set(f"task:{TEST_TASK_ID}:status", "done")
        r.set(f"task:{TEST_TASK_ID}:result", "short")

        args = argparse.Namespace(id=TEST_TASK_ID, tail=100)
        task.cmd_result(args)
        assert capsys.readouterr().out.strip() == "short"


# ═══════════════════════════════════════════════════════════════════════════════
# Task Management: list
# ═══════════════════════════════════════════════════════════════════════════════

class TestList:
    """task.py list [--worker X] [--status X] [--thread X] [--limit N]

    Scans active_tasks and task:*:status keys, prints formatted summary table.
    """

    def _seed_tasks(self, r):
        for tid, w, st, th in [
            ("task-a", "claude", "done", "thread-0"),
            ("task-b", "copilot", "running", "thread-1"),
            ("task-c", "claude", "failed", "thread-2"),
        ]:
            r.set(f"task:{tid}:status", st)
            r.set(f"task:{tid}:worker", w)
            r.set(f"task:{tid}:thread_id", th)
            r.set(f"task:{tid}:created_at", "2026-05-10T00:00:00Z")
            r.hset("active_tasks", tid, json.dumps({"status": st, "worker": w}))

    def test_list_all(self, mock_redis, capsys):
        self._seed_tasks(mock_redis)

        args = argparse.Namespace(worker=None, status=None, thread=None, limit=50)
        task.cmd_list(args)

        stdout = capsys.readouterr().out
        assert "task-a" in stdout
        assert "task-b" in stdout
        assert "task-c" in stdout

    def test_list_filter_by_worker(self, mock_redis, capsys):
        self._seed_tasks(mock_redis)

        args = argparse.Namespace(worker="claude", status=None, thread=None, limit=50)
        task.cmd_list(args)

        stdout = capsys.readouterr().out
        assert "task-a" in stdout and "task-c" in stdout
        assert "task-b" not in stdout

    def test_list_filter_by_status(self, mock_redis, capsys):
        self._seed_tasks(mock_redis)

        args = argparse.Namespace(worker=None, status="running", thread=None, limit=50)
        task.cmd_list(args)

        stdout = capsys.readouterr().out
        assert "task-b" in stdout
        assert "task-a" not in stdout and "task-c" not in stdout

    def test_list_filter_by_thread(self, mock_redis, capsys):
        self._seed_tasks(mock_redis)

        args = argparse.Namespace(worker=None, status=None, thread="thread-0", limit=50)
        task.cmd_list(args)

        stdout = capsys.readouterr().out
        assert "task-a" in stdout
        assert "task-b" not in stdout

    def test_list_empty(self, mock_redis, capsys):
        args = argparse.Namespace(worker=None, status=None, thread=None, limit=50)
        task.cmd_list(args)

        stdout = capsys.readouterr().out.strip()
        assert "(no tasks)" in stdout

    def test_list_limit(self, mock_redis, capsys):
        self._seed_tasks(mock_redis)

        args = argparse.Namespace(worker=None, status=None, thread=None, limit=2)
        task.cmd_list(args)

        stdout = capsys.readouterr().out
        count = sum(1 for t in ["task-a", "task-b", "task-c"] if t in stdout)
        assert count == 2


# ═══════════════════════════════════════════════════════════════════════════════
# Task Management: wait
# ═══════════════════════════════════════════════════════════════════════════════

class TestWait:
    """task.py wait --id <task_id> [--timeout 300]

    Blocks until task is done/failed/cancelled. Polls every 2s.
    On completion: deletes thread lock, prints status JSON.
    On timeout: calls die().
    """

    def test_wait_already_done(self, mock_redis, capsys):
        r = mock_redis
        r.set(f"task:{TEST_TASK_ID}:status", "done")
        r.set(f"task:{TEST_TASK_ID}:thread_id", TEST_THREAD)
        r.set(f"task:{TEST_TASK_ID}:exit_code", "0")
        r.set(f"thread:{TEST_THREAD}:lock", "1")

        args = argparse.Namespace(id=TEST_TASK_ID, timeout=300)
        task.cmd_wait(args)

        result = parse_stdout_json(capsys)
        assert result["status"] == "done"
        assert not r.exists(f"thread:{TEST_THREAD}:lock")

    def test_wait_polls_until_done(self, mock_redis, capsys):
        r = mock_redis
        r.set(f"task:{TEST_TASK_ID}:status", "running")
        r.set(f"task:{TEST_TASK_ID}:thread_id", TEST_THREAD)
        r.set(f"thread:{TEST_THREAD}:lock", "1")

        call_count = [0]

        def fake_sleep(seconds):
            call_count[0] += 1
            if call_count[0] >= 1:
                r.set(f"task:{TEST_TASK_ID}:status", "done")
                r.set(f"task:{TEST_TASK_ID}:exit_code", "0")

        args = argparse.Namespace(id=TEST_TASK_ID, timeout=300)
        with patch("task.time.sleep", fake_sleep):
            task.cmd_wait(args)

        assert "done" in capsys.readouterr().out
        assert not r.exists(f"thread:{TEST_THREAD}:lock")

    def test_wait_timeout(self, mock_redis):
        r = mock_redis
        r.set(f"task:{TEST_TASK_ID}:status", "running")
        r.set(f"task:{TEST_TASK_ID}:thread_id", TEST_THREAD)

        args = argparse.Namespace(id=TEST_TASK_ID, timeout=1)
        with pytest.raises(DieError) as exc:
            task.cmd_wait(args)

        assert "timed" in exc.value.msg.lower()

    def test_wait_task_failed(self, mock_redis, capsys):
        r = mock_redis
        r.set(f"task:{TEST_TASK_ID}:status", "failed")
        r.set(f"task:{TEST_TASK_ID}:thread_id", TEST_THREAD)
        r.set(f"task:{TEST_TASK_ID}:exit_code", "1")
        r.set(f"thread:{TEST_THREAD}:lock", "1")

        args = argparse.Namespace(id=TEST_TASK_ID, timeout=300)
        task.cmd_wait(args)

        result = parse_stdout_json(capsys)
        assert result["status"] == "failed"
        assert not r.exists(f"thread:{TEST_THREAD}:lock")

    def test_wait_task_cancelled(self, mock_redis, capsys):
        r = mock_redis
        r.set(f"task:{TEST_TASK_ID}:status", "cancelled")
        r.set(f"task:{TEST_TASK_ID}:thread_id", TEST_THREAD)
        r.set(f"task:{TEST_TASK_ID}:exit_code", "-1")
        r.set(f"thread:{TEST_THREAD}:lock", "1")

        args = argparse.Namespace(id=TEST_TASK_ID, timeout=300)
        task.cmd_wait(args)

        result = parse_stdout_json(capsys)
        assert result["status"] == "cancelled"
        assert not r.exists(f"thread:{TEST_THREAD}:lock")

    def test_wait_no_lock_to_delete(self, mock_redis, capsys):
        r = mock_redis
        r.set(f"task:{TEST_TASK_ID}:status", "done")
        r.set(f"task:{TEST_TASK_ID}:thread_id", TEST_THREAD)
        r.set(f"task:{TEST_TASK_ID}:exit_code", "0")

        args = argparse.Namespace(id=TEST_TASK_ID, timeout=300)
        task.cmd_wait(args)
        assert "done" in capsys.readouterr().out


# ═══════════════════════════════════════════════════════════════════════════════
# Task Management: requeue-stale
# ═══════════════════════════════════════════════════════════════════════════════

class TestRequeueStale:
    """task.py requeue-stale [--worker X] [--older-than 600]

    Scans tasks:processing:<worker>. Requeues tasks with missing status or
    "running" for too long. Terminal states (done/failed/cancelled) are
    garbage-collected from the processing list.
    """

    def _add_to_processing(self, r, task_id=TEST_TASK_ID, worker=WORKER):
        payload = make_task_payload(task_id)
        r.rpush(f"tasks:processing:{worker}", payload)

    def test_no_stale_tasks(self, mock_redis, capsys):
        args = argparse.Namespace(worker=WORKER, older_than=600)
        task.cmd_requeue_stale(args)
        # Should not crash
        assert True

    def test_requeues_missing_status(self, mock_redis, capsys):
        r = mock_redis
        self._add_to_processing(r)

        args = argparse.Namespace(worker=WORKER, older_than=600)
        task.cmd_requeue_stale(args)

        queue = r.lrange(f"tasks:queue:{WORKER}", 0, -1)
        assert len(queue) == 1
        assert json.loads(queue[0])["task_id"] == TEST_TASK_ID
        assert r.llen(f"tasks:processing:{WORKER}") == 0
        # Status reset to pending
        assert r.get(f"task:{TEST_TASK_ID}:status") == "pending"
        # Confirmation printed
        assert "Requeued" in capsys.readouterr().out

    def test_requeues_running_too_long(self, mock_redis, capsys):
        r = mock_redis
        self._add_to_processing(r)
        r.set(f"task:{TEST_TASK_ID}:status", "running")
        r.set(f"task:{TEST_TASK_ID}:created_at", seconds_ago(900))

        args = argparse.Namespace(worker=WORKER, older_than=600)
        task.cmd_requeue_stale(args)

        assert r.llen(f"tasks:queue:{WORKER}") == 1
        assert r.llen(f"tasks:processing:{WORKER}") == 0

    def test_skips_running_recent(self, mock_redis):
        r = mock_redis
        self._add_to_processing(r)
        r.set(f"task:{TEST_TASK_ID}:status", "running")
        r.set(f"task:{TEST_TASK_ID}:created_at", seconds_ago(60))

        args = argparse.Namespace(worker=WORKER, older_than=600)
        task.cmd_requeue_stale(args)

        assert r.llen(f"tasks:queue:{WORKER}") == 0
        assert r.llen(f"tasks:processing:{WORKER}") == 1

    def test_gc_terminal_from_processing(self, mock_redis, capsys):
        """Terminal status (done/failed/cancelled) → GC from processing list."""
        r = mock_redis
        for st in ["done", "failed", "cancelled"]:
            tid = f"task-{st}"
            payload = make_task_payload(tid)
            r.rpush(f"tasks:processing:{WORKER}", payload)
            r.set(f"task:{tid}:status", st)
            r.set(f"task:{tid}:created_at", seconds_ago(900))

        args = argparse.Namespace(worker=WORKER, older_than=600)
        task.cmd_requeue_stale(args)

        # All terminal entries removed from processing
        assert r.llen(f"tasks:processing:{WORKER}") == 0
        # None requeued
        assert r.llen(f"tasks:queue:{WORKER}") == 0

    def test_requeue_corrupt_json(self, mock_redis):
        """Corrupt JSON in processing list gets removed."""
        r = mock_redis
        r.rpush(f"tasks:processing:{WORKER}", "not-valid-json")
        r.rpush(f"tasks:processing:{WORKER}", make_task_payload("task-valid"))

        args = argparse.Namespace(worker=WORKER, older_than=600)
        task.cmd_requeue_stale(args)

        # Corrupt entry removed, valid one requeued (no status → stale)
        assert r.llen(f"tasks:processing:{WORKER}") == 0
        assert r.llen(f"tasks:queue:{WORKER}") == 1

    def test_all_workers_scanned(self, mock_redis, capsys):
        r = mock_redis
        all_workers = ("claude", "copilot", "opencode")
        for w in all_workers:
            tid = f"task-stale-{w}"
            payload = make_task_payload(tid)
            r.rpush(f"tasks:processing:{w}", payload)
            # No status key → stale

        args = argparse.Namespace(worker=None, older_than=600)
        task.cmd_requeue_stale(args)

        for w in all_workers:
            assert r.llen(f"tasks:queue:{w}") == 1
            assert r.llen(f"tasks:processing:{w}") == 0


# ═══════════════════════════════════════════════════════════════════════════════
# Task Management: cancel
# ═══════════════════════════════════════════════════════════════════════════════

class TestCancel:
    """task.py cancel --id <task_id>

    Sets task:{id}:cancel = "1" with TTL_TASK expiry.
    """

    def test_cancel_sets_flag(self, mock_redis, capsys):
        r = mock_redis
        r.set(f"task:{TEST_TASK_ID}:status", "running")  # must exist

        args = argparse.Namespace(id=TEST_TASK_ID)
        task.cmd_cancel(args)

        assert r.get(f"task:{TEST_TASK_ID}:cancel") == "1"
        assert r.ttl(f"task:{TEST_TASK_ID}:cancel") > 0
        assert "Cancel flag set" in capsys.readouterr().out

    def test_cancel_nonexistent_task(self, mock_redis):
        """die() is called when task doesn't exist."""
        args = argparse.Namespace(id="nonexistent")

        with pytest.raises(DieError) as exc:
            task.cmd_cancel(args)

        assert "not found" in exc.value.msg.lower()

    def test_cancel_idempotent(self, mock_redis, capsys):
        r = mock_redis
        r.set(f"task:{TEST_TASK_ID}:status", "running")
        r.set(f"task:{TEST_TASK_ID}:cancel", "1")  # already cancelled

        args = argparse.Namespace(id=TEST_TASK_ID)
        task.cmd_cancel(args)  # second cancel

        # Still 1, no error
        assert r.get(f"task:{TEST_TASK_ID}:cancel") == "1"


# ═══════════════════════════════════════════════════════════════════════════════
# Task Management: unlock
# ═══════════════════════════════════════════════════════════════════════════════

class TestUnlock:
    """task.py unlock --thread <thread_id>

    Deletes thread:{id}:lock.
    """

    def test_unlock_deletes_lock(self, mock_redis, capsys):
        r = mock_redis
        r.set(f"thread:{TEST_THREAD}:lock", "1")

        args = argparse.Namespace(thread=TEST_THREAD)
        task.cmd_unlock(args)

        assert not r.exists(f"thread:{TEST_THREAD}:lock")
        assert "released" in capsys.readouterr().out.lower()

    def test_unlock_no_lock_exists(self, mock_redis, capsys):
        args = argparse.Namespace(thread=TEST_THREAD)
        task.cmd_unlock(args)

        assert "no lock" in capsys.readouterr().out.lower()


# ═══════════════════════════════════════════════════════════════════════════════
# Thread Management: thread-create
# ═══════════════════════════════════════════════════════════════════════════════

class TestThreadCreate:
    """task.py thread-create --id <thread_id> [--repo owner/repo]

    Initializes thread:{id}:current_state with status=initiated.
    Overwrites if already exists (no duplicate check).
    """

    def test_create_basic(self, mock_redis, capsys):
        args = argparse.Namespace(id=TEST_THREAD, repo=None)
        task.cmd_thread_create(args)

        state = mock_redis.hgetall(f"thread:{TEST_THREAD}:current_state")
        assert state["status"] == "initiated"
        assert "updated_at" in state
        assert "Thread" in capsys.readouterr().out

    def test_create_with_repo(self, mock_redis):
        args = argparse.Namespace(id=TEST_THREAD, repo="owner/repo")
        task.cmd_thread_create(args)

        state = mock_redis.hgetall(f"thread:{TEST_THREAD}:current_state")
        assert state["gh_repo"] == "owner/repo"

    def test_create_overwrites_existing(self, mock_redis, capsys):
        r = mock_redis
        r.hset(f"thread:{TEST_THREAD}:current_state", "status", "awaiting_review")

        args = argparse.Namespace(id=TEST_THREAD, repo=None)
        task.cmd_thread_create(args)

        # Overwritten — status reset to initiated
        state = r.hgetall(f"thread:{TEST_THREAD}:current_state")
        assert state["status"] == "initiated"

    def test_create_sets_ttl(self, mock_redis):
        args = argparse.Namespace(id=TEST_THREAD, repo=None)
        task.cmd_thread_create(args)

        ttl = mock_redis.ttl(f"thread:{TEST_THREAD}:current_state")
        assert ttl > 0


# ═══════════════════════════════════════════════════════════════════════════════
# Thread Management: thread-history
# ═══════════════════════════════════════════════════════════════════════════════

class TestThreadHistory:
    """task.py thread-history --id <thread_id> [--tail N]

    Prints formatted messages with role, timestamp, content, and separator.
    """

    def test_history_full(self, mock_redis, thread_with_history, capsys):
        args = argparse.Namespace(id=TEST_THREAD, tail=None)
        task.cmd_thread_history(args)

        stdout = capsys.readouterr().out
        assert "Initial design" in stdout
        assert "Design result" in stdout
        assert "[master]" in stdout
        assert "[claude]" in stdout
        assert "---" in stdout

    def test_history_tail(self, mock_redis, thread_with_history, capsys):
        args = argparse.Namespace(id=TEST_THREAD, tail=1)
        task.cmd_thread_history(args)

        stdout = capsys.readouterr().out
        assert "Design result" in stdout
        assert "Initial design" not in stdout

    def test_history_empty(self, mock_redis, capsys):
        args = argparse.Namespace(id=TEST_THREAD, tail=None)
        task.cmd_thread_history(args)

        assert "(no messages)" in capsys.readouterr().out

    def test_history_corrupt_message(self, mock_redis, capsys):
        r = mock_redis
        r.rpush(f"thread:{TEST_THREAD}:messages", "not-valid-json")
        r.rpush(f"thread:{TEST_THREAD}:messages", make_msg("claude", "valid msg"))

        args = argparse.Namespace(id=TEST_THREAD, tail=None)
        task.cmd_thread_history(args)

        stdout = capsys.readouterr().out
        # Corrupt message shown raw, valid one parsed
        assert "not-valid-json" in stdout
        assert "valid msg" in stdout


# ═══════════════════════════════════════════════════════════════════════════════
# Thread Management: thread-state
# ═══════════════════════════════════════════════════════════════════════════════

class TestThreadState:
    """task.py thread-state --id <thread_id>

    Prints the current_state hash as JSON (indent=2).
    """

    def test_state_json(self, mock_redis, thread_with_history, capsys):
        args = argparse.Namespace(id=TEST_THREAD)
        task.cmd_thread_state(args)

        result = parse_stdout_json(capsys)
        assert result["status"] == "awaiting_review"
        assert result["last_design"] == "v1 design text"

    def test_state_nonexistent(self, mock_redis, capsys):
        args = argparse.Namespace(id="nonexistent-thread")
        task.cmd_thread_state(args)

        # Prints empty JSON object
        result = parse_stdout_json(capsys)
        assert result == {}


# ═══════════════════════════════════════════════════════════════════════════════
# Thread Management: thread-update
# ═══════════════════════════════════════════════════════════════════════════════

class TestThreadUpdate:
    """task.py thread-update --id <thread_id> --status <status>
                              [--design "<text>"] [--pr N]

    Updates fields in thread:{id}:current_state hash.
    """

    def _init_thread(self, r):
        r.hset(f"thread:{TEST_THREAD}:current_state", mapping={
            "status": "initiated", "updated_at": "2026-05-10T00:00:00Z",
        })

    def test_update_status(self, mock_redis, capsys):
        self._init_thread(mock_redis)

        args = argparse.Namespace(id=TEST_THREAD, status="complete", design=None, pr=None)
        task.cmd_thread_update(args)

        state = mock_redis.hgetall(f"thread:{TEST_THREAD}:current_state")
        assert state["status"] == "complete"
        assert "updated" in capsys.readouterr().out.lower()

    def test_update_design(self, mock_redis):
        self._init_thread(mock_redis)

        args = argparse.Namespace(id=TEST_THREAD, status="awaiting_review",
                                   design="OAuth2 v2 design", pr=None)
        task.cmd_thread_update(args)

        state = mock_redis.hgetall(f"thread:{TEST_THREAD}:current_state")
        assert state["last_design"] == "OAuth2 v2 design"

    def test_update_pr(self, mock_redis):
        self._init_thread(mock_redis)

        args = argparse.Namespace(id=TEST_THREAD, status="implementing", design=None, pr=42)
        task.cmd_thread_update(args)

        state = mock_redis.hgetall(f"thread:{TEST_THREAD}:current_state")
        assert state["gh_pr_number"] == "42"

    def test_update_combined(self, mock_redis):
        self._init_thread(mock_redis)

        args = argparse.Namespace(id=TEST_THREAD, status="complete",
                                   design="Final design", pr=99)
        task.cmd_thread_update(args)

        state = mock_redis.hgetall(f"thread:{TEST_THREAD}:current_state")
        assert state["status"] == "complete"
        assert state["last_design"] == "Final design"
        assert state["gh_pr_number"] == "99"
        assert state["updated_at"] != "2026-05-10T00:00:00Z"

    def test_update_nonexistent_thread(self, mock_redis):
        args = argparse.Namespace(id="nonexistent", status="complete", design=None, pr=None)

        with pytest.raises(DieError) as exc:
            task.cmd_thread_update(args)

        assert "not found" in exc.value.msg.lower()

    def test_update_refreshes_ttl(self, mock_redis):
        self._init_thread(mock_redis)

        args = argparse.Namespace(id=TEST_THREAD, status="complete", design=None, pr=None)
        task.cmd_thread_update(args)

        ttl = mock_redis.ttl(f"thread:{TEST_THREAD}:current_state")
        assert ttl > 0


# ═══════════════════════════════════════════════════════════════════════════════
# Thread Management: thread-list
# ═══════════════════════════════════════════════════════════════════════════════

class TestThreadList:
    """task.py thread-list

    Lists all thread:{*}:current_state keys with formatted table.
    """

    def test_thread_list_summary(self, mock_redis, capsys):
        r = mock_redis
        for i in range(3):
            r.hset(f"thread:thread-{i}:current_state", mapping={
                "status": ["initiated", "awaiting_review", "complete"][i],
                "updated_at": "2026-05-10T00:00:00Z",
            })

        args = argparse.Namespace()
        task.cmd_thread_list(args)

        stdout = capsys.readouterr().out
        assert "thread-0" in stdout and "thread-1" in stdout and "thread-2" in stdout
        assert "initiated" in stdout and "awaiting_review" in stdout and "complete" in stdout

    def test_thread_list_empty(self, mock_redis, capsys):
        args = argparse.Namespace()
        task.cmd_thread_list(args)

        assert "(no threads)" in capsys.readouterr().out

    def test_thread_list_missing_fields(self, mock_redis, capsys):
        """Thread with only partial state — shows defaults."""
        r = mock_redis
        r.hset(f"thread:{TEST_THREAD}:current_state", "status", "initiated")
        # No updated_at, gh_repo, etc.

        args = argparse.Namespace()
        task.cmd_thread_list(args)

        stdout = capsys.readouterr().out
        assert TEST_THREAD in stdout
        assert "initiated" in stdout


# ═══════════════════════════════════════════════════════════════════════════════
# Thread Management: thread-cleanup
# ═══════════════════════════════════════════════════════════════════════════════

class TestThreadCleanup:
    """task.py thread-cleanup --id <thread_id>

    Deletes /workspace/{thread_id}/ directory tree. Does NOT touch Redis.
    """

    def test_cleanup_removes_workspace(self, mock_redis, capsys, monkeypatch):
        monkeypatch.setattr(task, "WORKSPACE_DIR", "/tmp")

        workspace = f"/tmp/{TEST_THREAD}"
        os.makedirs(workspace, exist_ok=True)
        with open(os.path.join(workspace, "test.txt"), "w") as f:
            f.write("hello")

        args = argparse.Namespace(id=TEST_THREAD)
        task.cmd_thread_cleanup(args)

        assert not os.path.exists(workspace)
        assert "Deleted" in capsys.readouterr().out

    def test_cleanup_nonexistent(self, mock_redis, capsys):
        args = argparse.Namespace(id="thread-that-never-existed")
        task.cmd_thread_cleanup(args)

        assert "does not exist" in capsys.readouterr().out

    def test_cleanup_preserves_redis(self, mock_redis, capsys, monkeypatch):
        r = mock_redis
        monkeypatch.setattr(task, "WORKSPACE_DIR", "/tmp")

        r.hset(f"thread:{TEST_THREAD}:current_state", mapping={
            "status": "complete", "gh_repo": "owner/repo",
        })
        workspace = f"/tmp/{TEST_THREAD}"
        os.makedirs(workspace, exist_ok=True)

        args = argparse.Namespace(id=TEST_THREAD)
        task.cmd_thread_cleanup(args)

        # Redis state still intact
        assert r.exists(f"thread:{TEST_THREAD}:current_state")
        state = r.hgetall(f"thread:{TEST_THREAD}:current_state")
        assert state["status"] == "complete"


# ═══════════════════════════════════════════════════════════════════════════════
# Integration / Workflow Tests
# ═══════════════════════════════════════════════════════════════════════════════

class TestWorkflows:
    """End-to-end workflow tests spanning multiple commands."""

    def test_full_lifecycle(self, mock_redis, capsys, monkeypatch):
        r = mock_redis
        monkeypatch.setattr(task, "WORKSPACE_DIR", "/tmp")

        # 1. Create thread
        cargs = argparse.Namespace(id=TEST_THREAD, repo="owner/repo")
        task.cmd_thread_create(cargs)
        capsys.readouterr()  # clear
        assert r.hget(f"thread:{TEST_THREAD}:current_state", "status") == "initiated"

        # 2. Enqueue task
        eargs = argparse.Namespace(worker="claude", thread=TEST_THREAD, instruction="Design OAuth2")
        task.cmd_enqueue(eargs)
        task_id = json.loads(capsys.readouterr().out.strip())["task_id"]

        # 3. Simulate worker completing
        r.set(f"task:{task_id}:status", "done")
        r.set(f"task:{task_id}:worker", "claude")
        r.set(f"task:{task_id}:thread_id", TEST_THREAD)
        r.set(f"task:{task_id}:result", "OAuth2: use PKCE flow")
        r.set(f"task:{task_id}:exit_code", "0")
        r.set(f"task:{task_id}:created_at", seconds_ago(60))
        r.set(f"task:{task_id}:completed_at", seconds_ago(0))
        r.rpush(f"thread:{TEST_THREAD}:messages", make_msg("claude", "OAuth2: use PKCE flow", task_id))

        # 4. Wait
        wargs = argparse.Namespace(id=task_id, timeout=5)
        task.cmd_wait(wargs)
        out = capsys.readouterr().out
        assert "done" in out

        # 5. Get result
        rargs = argparse.Namespace(id=task_id, tail=None)
        task.cmd_result(rargs)
        out = capsys.readouterr().out
        assert "PKCE" in out

        # 6. Update thread state
        uargs = argparse.Namespace(id=TEST_THREAD, status="implementing",
                                    design="OAuth2 with PKCE", pr=None)
        task.cmd_thread_update(uargs)
        capsys.readouterr()  # clear
        state = r.hgetall(f"thread:{TEST_THREAD}:current_state")
        assert state["status"] == "implementing"

        # 7. Thread history
        hargs = argparse.Namespace(id=TEST_THREAD, tail=None)
        task.cmd_thread_history(hargs)
        stdout = capsys.readouterr().out
        assert "Design OAuth2" in stdout and "PKCE" in stdout

    def test_crash_recovery_workflow(self, mock_redis, capsys):
        r = mock_redis

        # Enqueue
        eargs = argparse.Namespace(worker="claude", thread=TEST_THREAD, instruction="Implement feature")
        task.cmd_enqueue(eargs)
        task_id = json.loads(capsys.readouterr().out.strip())["task_id"]

        # Simulate worker dequeues then crashes
        task_json = r.lpop(f"tasks:queue:claude")
        r.rpush(f"tasks:processing:claude", task_json)
        r.hset("active_tasks", task_id, json.dumps({
            "status": "running", "worker": "claude",
            "thread_id": TEST_THREAD, "started_at": seconds_ago(0),
        }))
        # No task:{id}:status — crashed before writing it

        # Requeue stale
        rargs = argparse.Namespace(worker="claude", older_than=0)
        task.cmd_requeue_stale(rargs)

        # Task is back in the queue
        queue = r.lrange("tasks:queue:claude", 0, -1)
        assert len(queue) == 1
        assert json.loads(queue[0])["task_id"] == task_id
        assert r.llen("tasks:processing:claude") == 0

    def test_serialization_blocked(self, mock_redis):
        """Two enqueues for same thread: second blocked by lock."""
        args1 = argparse.Namespace(worker="claude", thread=TEST_THREAD, instruction="Task 1")
        task.cmd_enqueue(args1)

        args2 = argparse.Namespace(worker="copilot", thread=TEST_THREAD, instruction="Task 2")
        with pytest.raises(DieError) as exc:
            task.cmd_enqueue(args2)

        assert "locked" in exc.value.msg.lower()

    def test_multiple_threads_independent(self, mock_redis, capsys):
        r = mock_redis
        args_a = argparse.Namespace(worker="claude", thread="A", instruction="Task A")
        task.cmd_enqueue(args_a)
        id_a = parse_stdout_json(capsys)["task_id"]

        args_b = argparse.Namespace(worker="copilot", thread="B", instruction="Task B")
        task.cmd_enqueue(args_b)
        id_b = parse_stdout_json(capsys)["task_id"]

        assert r.exists("thread:A:lock") and r.exists("thread:B:lock")
        assert id_a != id_b

    def test_wait_then_next_enqueue(self, mock_redis, capsys):
        r = mock_redis
        eargs1 = argparse.Namespace(worker="claude", thread=TEST_THREAD, instruction="Task 1")
        task.cmd_enqueue(eargs1)
        task_id = parse_stdout_json(capsys)["task_id"]

        r.set(f"task:{task_id}:status", "done")
        r.set(f"task:{task_id}:thread_id", TEST_THREAD)
        r.set(f"task:{task_id}:exit_code", "0")

        wargs = argparse.Namespace(id=task_id, timeout=5)
        task.cmd_wait(wargs)

        # Lock released — second enqueue succeeds
        eargs2 = argparse.Namespace(worker="copilot", thread=TEST_THREAD, instruction="Task 2")
        task.cmd_enqueue(eargs2)
        assert "task_id" in capsys.readouterr().out


# ═══════════════════════════════════════════════════════════════════════════════
# Edge Cases
# ═══════════════════════════════════════════════════════════════════════════════

class TestEdgeCases:
    """Edge cases and error scenarios."""

    def test_very_long_instruction(self, mock_redis, capsys):
        long_instruction = "Implement " + "x" * 50000

        args = argparse.Namespace(worker=WORKER, thread=TEST_THREAD, instruction=long_instruction)
        task.cmd_enqueue(args)

        tid = parse_stdout_json(capsys)["task_id"]
        queue = mock_redis.lrange(f"tasks:queue:{WORKER}", 0, -1)
        payload = json.loads(queue[0])
        assert payload["instruction"] == long_instruction

    def test_special_characters(self, mock_redis, capsys):
        instruction = 'Use "double quotes" and \'single\' plus\nnewlines\nand 🚀 emoji'

        args = argparse.Namespace(worker=WORKER, thread=TEST_THREAD, instruction=instruction)
        task.cmd_enqueue(args)

        tid = parse_stdout_json(capsys)["task_id"]
        queue = mock_redis.lrange(f"tasks:queue:{WORKER}", 0, -1)
        payload = json.loads(queue[0])
        assert payload["instruction"] == instruction

    def test_invalid_worker_type(self, mock_redis):
        """Enqueue with invalid worker — argparse validates this with choices."""
        # argparse.Namespace bypasses validation; test the underlying function
        args = argparse.Namespace(worker="invalid_worker", thread=TEST_THREAD, instruction="Test")
        task.cmd_enqueue(args)  # Task goes to tasks:queue:invalid_worker queue

        # The queue name is derived from the worker string — it works but creates
        # a queue no real worker listens on. This is acceptable behavior.
        queue = mock_redis.lrange("tasks:queue:invalid_worker", 0, -1)
        assert len(queue) == 1

    def test_empty_instruction(self, mock_redis, capsys):
        # Empty instructions are allowed (argparse requires the flag though)
        args = argparse.Namespace(worker=WORKER, thread=TEST_THREAD, instruction="")
        task.cmd_enqueue(args)

        tid = parse_stdout_json(capsys)["task_id"]
        desc = mock_redis.get(f"task:{tid}:description")
        assert desc == ""

    def test_result_tail_zero(self, mock_redis, capsys):
        r = mock_redis
        r.set(f"task:{TEST_TASK_ID}:status", "done")
        r.set(f"task:{TEST_TASK_ID}:result", "line1\nline2")

        args = argparse.Namespace(id=TEST_TASK_ID, tail=0)
        task.cmd_result(args)

        # Python truthiness: if args.tail: treats 0 as falsy, so full result
        # is printed. This matches task.py's current behavior.
        assert capsys.readouterr().out.strip() == "line1\nline2"

    def test_list_limit_zero(self, mock_redis, capsys):
        r = mock_redis
        r.set("task:a:status", "done")
        r.set("task:a:worker", WORKER)
        r.hset("active_tasks", "a", json.dumps({"status": "done"}))

        args = argparse.Namespace(worker=None, status=None, thread=None, limit=0)
        task.cmd_list(args)

        # Should not crash
        assert True

    def test_redis_connection_error(self, monkeypatch):
        """When Redis is unreachable, ConnectionError → die()."""
        monkeypatch.setattr(task, "get_redis", lambda: (_ for _ in ()).throw(
            __import__("redis").exceptions.ConnectionError("connection refused")
        ))

        # Reset die mock — we want to verify the try/except in main()
        # For unit tests of cmd_* functions, get_redis is already mocked.
        # This test verifies main() handles connection errors.
        pass  # main() has the ConnectionError handler; covered by integration test


def test_main_connection_error(monkeypatch, capsys):
    """main() catches ConnectionError and calls die()."""
    monkeypatch.setattr(task, "get_redis", lambda: (_ for _ in ()).throw(
        __import__("redis").exceptions.ConnectionError("connection refused")
    ))
    monkeypatch.setattr(task, "die", mock_die)

    # Simulate argv for a command that calls get_redis()
    with patch("sys.argv", ["task.py", "status", "--id", "some-id"]):
        with pytest.raises(DieError) as exc:
            task.main()

    assert "Redis connection failed" in exc.value.msg


def test_main_invalid_command(monkeypatch, capsys):
    """argparse rejects invalid command (built-in behavior)."""
    with patch("sys.argv", ["task.py", "nonexistent-command"]):
        with pytest.raises(SystemExit):
            task.main()
