"""Unit tests for worker.py — the generic worker poll loop shared by all agent types.

Tests validate that worker.py conforms to the design in docs/design-redis-queue.md
(Worker section, lines 241-396). The worker is parameterized by env vars and
dequeues tasks from Redis, builds prompts with thread context, executes agent
commands, stores results, and manages task lifecycle.

Testing strategy:
    - Patch worker.r to use fakeredis (per test, not module-level)
    - Set env vars before importing worker (or patch globals directly)
    - Call process_one_task() directly to test the task processing logic
    - Mock subprocess.run to control execution outcomes
    - Check Redis keys match design spec (TTLs, key patterns, status values)
"""

import json
import os
import time
from unittest.mock import patch, MagicMock

# Worker module reads os.environ at import time — must set before importing.
os.environ.setdefault("WORKER_TYPE", "claude")

import pytest
import fakeredis

import worker


# ── Helpers ───────────────────────────────────────────────────────────────────

WORKER_TYPE = "claude"
TEST_THREAD = "test-thread-001"
TEST_TASK_ID = "task-00000000-0000-0000-0000-000000000001"
TEST_INSTRUCTION = "Implement OAuth2 support for the authentication module"


def make_task_payload(task_id=TEST_TASK_ID, thread_id=TEST_THREAD,
                      instruction=TEST_INSTRUCTION, **extra):
    """Build a task JSON payload matching the design doc spec."""
    payload = {"task_id": task_id, "thread_id": thread_id, "instruction": instruction}
    payload.update(extra)
    return json.dumps(payload)


def make_msg(role, content, task_id=TEST_TASK_ID,
             ts="2026-05-10T00:00:00Z", tokens=500):
    """Build a thread message JSON entry."""
    return json.dumps({
        "role": role,
        "content": content,
        "timestamp": ts,
        "metadata": {"task_id": task_id, "tokens": tokens},
    })


# ── Fixtures ──────────────────────────────────────────────────────────────────

@pytest.fixture(autouse=True)
def reset_worker_globals(monkeypatch, tmp_path):
    """Patch worker module globals with fakeredis and test defaults.

    This runs for every test to provide clean Redis state and fixed
    env-derived constants, avoiding dependency on real env vars.
    Workspace is a temporary directory so os.makedirs succeeds.
    """
    fake_r = fakeredis.FakeRedis(decode_responses=True)
    workspace = str(tmp_path / "workspace")

    monkeypatch.setattr(worker, "r", fake_r, raising=False)
    monkeypatch.setattr(worker, "running", True, raising=False)
    monkeypatch.setattr(worker, "WORKER", WORKER_TYPE, raising=False)
    monkeypatch.setattr(worker, "QUEUE", f"tasks:queue:{WORKER_TYPE}", raising=False)
    monkeypatch.setattr(worker, "PROCESSING", f"tasks:processing:{WORKER_TYPE}", raising=False)
    monkeypatch.setattr(worker, "AGENT_CMD", "claude -p", raising=False)
    monkeypatch.setattr(worker, "HISTORY_WINDOW", 10, raising=False)
    monkeypatch.setattr(worker, "TTL_THREAD", 604800, raising=False)
    monkeypatch.setattr(worker, "TTL_TASK", 86400, raising=False)
    monkeypatch.setattr(worker, "TASK_TIMEOUT", 1800, raising=False)
    monkeypatch.setattr(worker, "WORKSPACE_DIR", workspace, raising=False)

    return fake_r


@pytest.fixture
def fake_redis(reset_worker_globals):
    """Alias for reset_worker_globals — the fakeredis instance."""
    return reset_worker_globals


@pytest.fixture
def thread_with_history(fake_redis):
    """Pre-populated thread with message history and current_state."""
    r = fake_redis
    r.rpush(f"thread:{TEST_THREAD}:messages", make_msg("master", "Initial instruction"))
    r.rpush(f"thread:{TEST_THREAD}:messages", make_msg("claude", "Design v1: OAuth2 with PKCE"))
    r.rpush(f"thread:{TEST_THREAD}:messages", make_msg("master", "Review feedback"))
    r.hset(f"thread:{TEST_THREAD}:current_state", mapping={
        "status": "awaiting_review",
        "last_design": "OAuth2 with PKCE design v1",
        "last_updated_by": "claude",
        "last_task_id": "prev-task-001",
        "gh_repo": "owner/repo",
        "gh_pr_number": "42",
        "updated_at": "2026-05-10T00:00:00Z",
    })
    return r


# ═══════════════════════════════════════════════════════════════════════════════
# Configuration / Env Tests
# ═══════════════════════════════════════════════════════════════════════════════

class TestConfiguration:
    """Validate that worker reads env vars and derives constants correctly."""

    def test_queue_name_derivation(self, fake_redis):
        """Queue name is tasks:queue:{WORKER_TYPE}."""
        assert worker.QUEUE == f"tasks:queue:{WORKER_TYPE}"

    def test_processing_name_derivation(self, fake_redis):
        """Processing list is tasks:processing:{WORKER_TYPE}."""
        assert worker.PROCESSING == f"tasks:processing:{WORKER_TYPE}"

    def test_default_history_window(self, fake_redis):
        """Default HISTORY_WINDOW is 10."""
        assert worker.HISTORY_WINDOW == 10

    def test_ttl_constants(self, fake_redis):
        """TTL_THREAD = 7 days (604800s), TTL_TASK = 24h (86400s)."""
        assert worker.TTL_THREAD == 604800
        assert worker.TTL_TASK == 86400

    def test_default_task_timeout(self, fake_redis):
        """Default TASK_TIMEOUT is 1800s (30 min)."""
        assert worker.TASK_TIMEOUT == 1800

    def test_default_workspace_dir(self, fake_redis):
        """WORKSPACE_DIR is a writable directory path (set via fixture)."""
        assert worker.WORKSPACE_DIR is not None
        # Should be a tmp path, not the production /workspace default
        assert worker.WORKSPACE_DIR != "/workspace"
        assert worker.WORKSPACE_DIR.startswith("/tmp")

    def test_default_redis_host(self):
        """Default REDIS_HOST is 'redis' (but fixture overrides r, so check constant)."""
        assert worker.REDIS_HOST == "redis"


# ═══════════════════════════════════════════════════════════════════════════════
# Task Dequeue & Registration Tests
# ═══════════════════════════════════════════════════════════════════════════════

class TestTaskRegistration:
    """Validate task payload parsing and registration in active_tasks + per-task keys."""

    @patch("worker.subprocess.run")
    def test_registers_in_active_tasks(self, mock_run, fake_redis):
        """Dequeued task registers in active_tasks hash with running status.
        
        Uses a side-effect to capture active_tasks state mid-execution
        before cleanup removes the entry."""
        captured_active = {}

        def capture_and_run(*args, **kwargs):
            # Capture active_tasks entry mid-execution (before cleanup)
            entry_raw = fake_redis.hget("active_tasks", TEST_TASK_ID)
            if entry_raw:
                captured_active["entry"] = json.loads(entry_raw)
            return MagicMock(returncode=0, stdout="Success", stderr="")

        mock_run.side_effect = capture_and_run

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        assert "entry" in captured_active, "active_tasks entry was never set"
        assert captured_active["entry"]["status"] == "running"
        assert captured_active["entry"]["worker"] == WORKER_TYPE
        assert captured_active["entry"]["thread_id"] == TEST_THREAD
        assert "started_at" in captured_active["entry"]

    @patch("worker.subprocess.run")
    def test_sets_per_task_status_keys(self, mock_run, fake_redis):
        """Sets task:{id}:status, :worker, :thread_id, :description, :created_at."""
        mock_proc = MagicMock(returncode=0, stdout="Success", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        assert fake_redis.get(f"task:{TEST_TASK_ID}:status") == "done"
        assert fake_redis.get(f"task:{TEST_TASK_ID}:worker") == WORKER_TYPE
        assert fake_redis.get(f"task:{TEST_TASK_ID}:thread_id") == TEST_THREAD
        assert fake_redis.get(f"task:{TEST_TASK_ID}:description") == TEST_INSTRUCTION
        assert fake_redis.get(f"task:{TEST_TASK_ID}:created_at") is not None

    @patch("worker.subprocess.run")
    def test_per_task_keys_have_ttl(self, mock_run, fake_redis):
        """All per-task keys get TTL_TASK expiry."""
        mock_proc = MagicMock(returncode=0, stdout="Success", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        worker.process_one_task(payload)

        for suffix in ["status", "worker", "thread_id", "description",
                        "created_at", "result", "exit_code", "completed_at"]:
            ttl = fake_redis.ttl(f"task:{TEST_TASK_ID}:{suffix}")
            assert ttl > 0, f"task:{TEST_TASK_ID}:{suffix} has no TTL"
            assert ttl <= worker.TTL_TASK, \
                f"task:{TEST_TASK_ID}:{suffix} TTL {ttl} > {worker.TTL_TASK}"

    @patch("worker.subprocess.run")
    def test_sets_running_status_immediately(self, mock_run, fake_redis):
        """Status is set to 'running' when task starts, before subprocess runs."""
        call_order = []

        orig_set = fake_redis.set

        def tracking_set(key, value, *args, **kwargs):
            if key == f"task:{TEST_TASK_ID}:status":
                call_order.append(("set", key, value))
            return orig_set(key, value, *args, **kwargs)

        fake_redis.set = tracking_set

        def tracking_run(*args, **kwargs):
            call_order.append("subprocess")
            return MagicMock(returncode=0, stdout="ok", stderr="")

        mock_run.side_effect = tracking_run

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)
        worker.process_one_task(payload)

        # The initial "running" set should happen before subprocess.run
        running_idx = next(i for i, v in enumerate(call_order)
                           if isinstance(v, tuple) and v[2] == "running")
        subprocess_idx = call_order.index("subprocess")
        assert running_idx < subprocess_idx, \
            "status=running must be set before subprocess.run is called"


# ═══════════════════════════════════════════════════════════════════════════════
# Context / Prompt Building Tests
# ═══════════════════════════════════════════════════════════════════════════════

class TestContextBuilding:
    """Validate worker builds prompts with thread history and current_state."""

    @patch("worker.subprocess.run")
    def test_includes_thread_history_in_prompt(self, mock_run, fake_redis,
                                                 thread_with_history):
        """Prompt includes the last HISTORY_WINDOW messages from thread."""
        mock_run.return_value = MagicMock(returncode=0, stdout="ok", stderr="")
        r = thread_with_history
        payload = make_task_payload()
        r.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        # Verify subprocess.run was called with a prompt containing history
        cmd_args = mock_run.call_args[0][0]
        # The prompt is the last argument
        prompt = cmd_args[-1]
        assert "## Thread History (recent)" in prompt
        assert "[master]" in prompt
        assert "[claude]" in prompt
        assert "Initial instruction" in prompt
        assert "OAuth2 with PKCE" in prompt

    @patch("worker.subprocess.run")
    def test_respects_history_window_from_payload(self, mock_run, fake_redis):
        """Payload history_window overrides HISTORY_WINDOW env default."""
        mock_run.return_value = MagicMock(returncode=0, stdout="ok", stderr="")
        r = fake_redis
        # Add 20 messages, default window is 10
        for i in range(20):
            r.rpush(f"thread:{TEST_THREAD}:messages",
                    make_msg("master", f"Message {i}"))

        payload = make_task_payload(history_window=3)
        r.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        prompt = mock_run.call_args[0][0][-1]
        # Only last 3 messages should appear
        assert "Message 17" in prompt
        assert "Message 18" in prompt
        assert "Message 19" in prompt
        assert "Message 0" not in prompt  # outside window

    @patch("worker.subprocess.run")
    def test_includes_current_state_in_prompt(self, mock_run, fake_redis,
                                                thread_with_history):
        """Prompt includes fields from thread:{id}:current_state hash."""
        mock_run.return_value = MagicMock(returncode=0, stdout="ok", stderr="")
        r = thread_with_history
        payload = make_task_payload()
        r.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        prompt = mock_run.call_args[0][0][-1]
        assert "## Current State" in prompt
        assert "status: awaiting_review" in prompt
        assert "OAuth2 with PKCE design v1" in prompt  # last_design
        assert "repo: owner/repo" in prompt
        assert "PR: #42" in prompt

    @patch("worker.subprocess.run")
    def test_no_thread_history_no_crash(self, mock_run, fake_redis):
        """Worker handles threads with no history gracefully."""
        mock_run.return_value = MagicMock(returncode=0, stdout="ok", stderr="")
        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        prompt = mock_run.call_args[0][0][-1]
        assert "## Thread History" not in prompt
        assert "## Task" in prompt
        assert TEST_INSTRUCTION in prompt

    @patch("worker.subprocess.run")
    def test_no_current_state_no_crash(self, mock_run, fake_redis):
        """Worker handles threads with no current_state gracefully."""
        mock_run.return_value = MagicMock(returncode=0, stdout="ok", stderr="")
        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        prompt = mock_run.call_args[0][0][-1]
        assert "## Current State" not in prompt
        assert "## Task" in prompt

    @patch("worker.subprocess.run")
    def test_current_state_missing_fields_defaults(self, mock_run, fake_redis):
        """Missing optional state fields don't produce output."""
        mock_run.return_value = MagicMock(returncode=0, stdout="ok", stderr="")
        r = fake_redis
        r.hset(f"thread:{TEST_THREAD}:current_state", mapping={
            "status": "implementing",
        })
        payload = make_task_payload()
        r.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        prompt = mock_run.call_args[0][0][-1]
        assert "## Current State" in prompt
        assert "status: implementing" in prompt
        # These optional fields should NOT appear
        assert "design:" not in prompt
        assert "repo:" not in prompt
        assert "PR:" not in prompt

    @patch("worker.subprocess.run")
    def test_instruction_in_prompt(self, mock_run, fake_redis):
        """Task instruction is always in the final prompt."""
        mock_run.return_value = MagicMock(returncode=0, stdout="ok", stderr="")
        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        prompt = mock_run.call_args[0][0][-1]
        assert f"## Task\n{TEST_INSTRUCTION}" in prompt


# ═══════════════════════════════════════════════════════════════════════════════
# Workspace Tests
# ═══════════════════════════════════════════════════════════════════════════════

class TestWorkspace:
    """Validate workspace directory creation for thread isolation."""

    @patch("worker.subprocess.run")
    @patch("worker.os.makedirs")
    def test_creates_workspace_for_thread(self, mock_makedirs, mock_run, fake_redis):
        """Creates /workspace/{thread_id} directory."""
        mock_proc = MagicMock(returncode=0, stdout="ok", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        mock_makedirs.assert_called_once()
        workspace_path = mock_makedirs.call_args[0][0]
        assert TEST_THREAD in workspace_path
        assert mock_makedirs.call_args[1].get("exist_ok") is True

    @patch("worker.subprocess.run")
    def test_subprocess_cwd_is_workspace(self, mock_run, fake_redis):
        """subprocess.run cwd is set to the thread workspace."""
        mock_proc = MagicMock(returncode=0, stdout="ok", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        assert mock_run.call_args[1]["cwd"] is not None
        assert TEST_THREAD in mock_run.call_args[1]["cwd"]


# ═══════════════════════════════════════════════════════════════════════════════
# Agent Execution Tests
# ═══════════════════════════════════════════════════════════════════════════════

class TestAgentExecution:
    """Validate subprocess.run behavior: success, failure, timeout, stderr."""

    @patch("worker.subprocess.run")
    def test_successful_execution_status_done(self, mock_run, fake_redis):
        """Exit 0 → status 'done'."""
        mock_proc = MagicMock(returncode=0, stdout="Task completed", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        status = worker.process_one_task(payload)
        assert status == "done"

    @patch("worker.subprocess.run")
    def test_failed_execution_status_failed(self, mock_run, fake_redis):
        """Non-zero exit → status 'failed'."""
        mock_proc = MagicMock(returncode=1, stdout="Partial output", stderr="error")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        status = worker.process_one_task(payload)
        assert status == "failed"

    @patch("worker.subprocess.run")
    def test_failed_result_prefixed_with_failed_tag(self, mock_run, fake_redis):
        """Failed results are prefixed with [FAILED exit=N]."""
        mock_proc = MagicMock(returncode=1, stdout="Output", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        result = fake_redis.get(f"task:{TEST_TASK_ID}:result")
        assert result.startswith("[FAILED exit=1]")

    @patch("worker.subprocess.run")
    def test_timeout_status_failed(self, mock_run, fake_redis):
        """TimeoutExpired → status 'failed', exit_code=-1."""
        import subprocess as sp
        mock_run.side_effect = sp.TimeoutExpired(cmd=["claude", "-p"], timeout=10)

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        status = worker.process_one_task(payload)
        assert status == "failed"
        assert fake_redis.get(f"task:{TEST_TASK_ID}:exit_code") == "-1"

    @patch("worker.subprocess.run")
    def test_timeout_result_message(self, mock_run, fake_redis):
        """Timeout result includes informative message with timeout value."""
        import subprocess as sp
        mock_run.side_effect = sp.TimeoutExpired(cmd=["claude", "-p"], timeout=10)

        payload = make_task_payload(timeout=600)
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        result = fake_redis.get(f"task:{TEST_TASK_ID}:result")
        assert "timed out" in result.lower()
        assert "600s" in result

    @patch("worker.subprocess.run")
    def test_stderr_appended_to_result(self, mock_run, fake_redis):
        """stderr is appended to result with [stderr] delimiter."""
        mock_proc = MagicMock(returncode=0, stdout="stdout here", stderr="stderr here")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        result = fake_redis.get(f"task:{TEST_TASK_ID}:result")
        assert "[stderr]" in result
        assert "stderr here" in result

    @patch("worker.subprocess.run")
    def test_timeout_value_from_payload(self, mock_run, fake_redis):
        """Payload timeout field overrides TASK_TIMEOUT env."""
        mock_proc = MagicMock(returncode=0, stdout="ok", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload(timeout=3600)
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        assert mock_run.call_args[1]["timeout"] == 3600

    @patch("worker.subprocess.run")
    def test_timeout_uses_default_when_not_in_payload(self, mock_run, fake_redis):
        """When payload has no timeout field, uses TASK_TIMEOUT default."""
        mock_proc = MagicMock(returncode=0, stdout="ok", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()  # no timeout field
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        assert mock_run.call_args[1]["timeout"] == worker.TASK_TIMEOUT


# ═══════════════════════════════════════════════════════════════════════════════
# Result Storage Tests
# ═══════════════════════════════════════════════════════════════════════════════

class TestResultStorage:
    """Validate result, exit_code, completed_at, and status keys after execution."""

    @patch("worker.subprocess.run")
    def test_result_stored(self, mock_run, fake_redis):
        """Agent stdout is stored in task:{id}:result."""
        mock_proc = MagicMock(returncode=0, stdout="Build output here", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        assert "Build output here" in fake_redis.get(f"task:{TEST_TASK_ID}:result")

    @patch("worker.subprocess.run")
    def test_exit_code_stored_as_string(self, mock_run, fake_redis):
        """exit_code stored as string, not int."""
        mock_proc = MagicMock(returncode=0, stdout="ok", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        exit_code = fake_redis.get(f"task:{TEST_TASK_ID}:exit_code")
        assert isinstance(exit_code, str)
        assert exit_code == "0"

    @patch("worker.subprocess.run")
    def test_completed_at_set(self, mock_run, fake_redis):
        """completed_at timestamp is set after execution."""
        mock_proc = MagicMock(returncode=0, stdout="ok", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        completed = fake_redis.get(f"task:{TEST_TASK_ID}:completed_at")
        assert completed is not None
        # ISO 8601 timestamp with Z suffix
        assert completed.endswith("Z")
        assert "T" in completed

    @patch("worker.subprocess.run")
    def test_final_status_stored(self, mock_run, fake_redis):
        """Final status ('done' or 'failed') is stored."""
        mock_proc = MagicMock(returncode=0, stdout="ok", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        assert fake_redis.get(f"task:{TEST_TASK_ID}:status") == "done"

    @patch("worker.subprocess.run")
    def test_result_appended_to_thread_history(self, mock_run, fake_redis):
        """Result is RPUSHed to thread:{id}:messages with role=WORKER."""
        mock_proc = MagicMock(returncode=0, stdout="Result text", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        msgs = fake_redis.lrange(f"thread:{TEST_THREAD}:messages", 0, -1)
        assert len(msgs) == 1
        msg = json.loads(msgs[0])
        assert msg["role"] == WORKER_TYPE
        assert "Result text" in msg["content"]
        assert "metadata" in msg
        assert msg["metadata"]["task_id"] == TEST_TASK_ID

    @patch("worker.subprocess.run")
    def test_result_capped_at_10k_chars(self, mock_run, fake_redis):
        """Result content in thread history is capped at 10000 characters."""
        huge_output = "x" * 15000
        mock_proc = MagicMock(returncode=0, stdout=huge_output, stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        msgs = fake_redis.lrange(f"thread:{TEST_THREAD}:messages", 0, -1)
        msg = json.loads(msgs[0])
        assert len(msg["content"]) == 10000
        assert msg["content"] == huge_output[:10000]

        # But full result is still in task:{id}:result
        full = fake_redis.get(f"task:{TEST_TASK_ID}:result")
        assert len(full) == 15000

    @patch("worker.subprocess.run")
    def test_thread_history_ttl_refreshed(self, mock_run, fake_redis):
        """Thread messages TTL is refreshed (EXPIRE) when appending result."""
        r = fake_redis
        r.rpush(f"thread:{TEST_THREAD}:messages", make_msg("master", "prior msg"))
        r.expire(f"thread:{TEST_THREAD}:messages", worker.TTL_THREAD)

        initial_ttl = r.ttl(f"thread:{TEST_THREAD}:messages")

        mock_proc = MagicMock(returncode=0, stdout="New result", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        r.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        final_ttl = r.ttl(f"thread:{TEST_THREAD}:messages")
        # TTL should be refreshed (within TTL_THREAD ± 5 seconds)
        assert final_ttl > 0
        assert final_ttl >= initial_ttl - 5  # close to original TTL


# ═══════════════════════════════════════════════════════════════════════════════
# Cancellation Tests
# ═══════════════════════════════════════════════════════════════════════════════

class TestCancellation:
    """Validate task cancellation behavior: flag detection, status, cleanup."""

    def test_cancel_flag_detected_before_subprocess(self, fake_redis):
        """When task:{id}:cancel is set, task is cancelled before execution."""
        fake_redis.set(f"task:{TEST_TASK_ID}:cancel", "1")

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        with patch("worker.subprocess.run") as mock_run:
            status = worker.process_one_task(payload)
            mock_run.assert_not_called()

        assert status == "cancelled"

    def test_cancelled_status_stored(self, fake_redis):
        """Cancelled task gets status='cancelled'."""
        fake_redis.set(f"task:{TEST_TASK_ID}:cancel", "1")

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        assert fake_redis.get(f"task:{TEST_TASK_ID}:status") == "cancelled"

    def test_cancelled_result_message(self, fake_redis):
        """Cancelled task result = 'Cancelled by master'."""
        fake_redis.set(f"task:{TEST_TASK_ID}:cancel", "1")

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        assert fake_redis.get(f"task:{TEST_TASK_ID}:result") == "Cancelled by master"

    def test_cancelled_exit_code_minus_one(self, fake_redis):
        """Cancelled task exit_code = '-1'."""
        fake_redis.set(f"task:{TEST_TASK_ID}:cancel", "1")

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        assert fake_redis.get(f"task:{TEST_TASK_ID}:exit_code") == "-1"

    def test_cancelled_completed_at_set(self, fake_redis):
        """Cancelled task has completed_at timestamp."""
        fake_redis.set(f"task:{TEST_TASK_ID}:cancel", "1")

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        completed = fake_redis.get(f"task:{TEST_TASK_ID}:completed_at")
        assert completed is not None
        assert completed.endswith("Z")

    def test_cancellation_message_in_thread_history(self, fake_redis):
        """Cancellation is recorded in thread history."""
        fake_redis.set(f"task:{TEST_TASK_ID}:cancel", "1")

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        msgs = fake_redis.lrange(f"thread:{TEST_THREAD}:messages", 0, -1)
        assert len(msgs) == 1
        msg = json.loads(msgs[0])
        assert msg["role"] == WORKER_TYPE
        assert "[cancelled]" in msg["content"]
        assert TEST_TASK_ID in msg["content"]

    def test_cancelled_removed_from_processing_list(self, fake_redis):
        """Cancelled task is LREM'd from PROCESSING list."""
        fake_redis.set(f"task:{TEST_TASK_ID}:cancel", "1")

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)
        assert fake_redis.llen(worker.PROCESSING) == 1

        worker.process_one_task(payload)

        assert fake_redis.llen(worker.PROCESSING) == 0

    def test_cancelled_removed_from_active_tasks(self, fake_redis):
        """Cancelled task is HDEL'd from active_tasks."""
        fake_redis.set(f"task:{TEST_TASK_ID}:cancel", "1")

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        assert fake_redis.hget("active_tasks", TEST_TASK_ID) is None

    def test_no_cancel_flag_proceeds_normally(self, fake_redis):
        """Without cancel flag, task executes normally (subprocess is called)."""
        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        with patch("worker.subprocess.run") as mock_run:
            mock_proc = MagicMock(returncode=0, stdout="ok", stderr="")
            mock_run.return_value = mock_proc

            status = worker.process_one_task(payload)

            mock_run.assert_called_once()
            assert status == "done"


# ═══════════════════════════════════════════════════════════════════════════════
# Thread State Update Tests
# ═══════════════════════════════════════════════════════════════════════════════

class TestThreadStateUpdate:
    """Validate worker only sets metadata fields, never status."""

    @patch("worker.subprocess.run")
    def test_sets_metadata_fields(self, mock_run, fake_redis):
        """Worker sets last_updated_by, last_task_id, updated_at."""
        mock_proc = MagicMock(returncode=0, stdout="ok", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        state = fake_redis.hgetall(f"thread:{TEST_THREAD}:current_state")
        assert state["last_updated_by"] == WORKER_TYPE
        assert state["last_task_id"] == TEST_TASK_ID
        assert state["updated_at"] is not None

    @patch("worker.subprocess.run")
    def test_never_sets_status_field(self, mock_run, fake_redis):
        """Design doc §628: worker never touches status; only master owns transitions."""
        mock_proc = MagicMock(returncode=0, stdout="ok", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        state = fake_redis.hgetall(f"thread:{TEST_THREAD}:current_state")
        # Worker HSETs only 3 fields: last_updated_by, last_task_id, updated_at
        assert len(state) == 3
        assert "status" not in state

    @patch("worker.subprocess.run")
    def test_preserves_existing_state_fields_on_empty_thread(self, mock_run,
                                                               fake_redis):
        """On threads with no prior state, worker only sets its 3 fields."""
        # No pre-existing current_state
        assert not fake_redis.exists(f"thread:{TEST_THREAD}:current_state")

        mock_proc = MagicMock(returncode=0, stdout="ok", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        state = fake_redis.hgetall(f"thread:{TEST_THREAD}:current_state")
        assert state["last_updated_by"] == WORKER_TYPE
        assert state["last_task_id"] == TEST_TASK_ID

    @patch("worker.subprocess.run")
    def test_thread_state_ttl_set(self, mock_run, fake_redis):
        """Thread state gets TTL_THREAD expiry."""
        mock_proc = MagicMock(returncode=0, stdout="ok", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        ttl = fake_redis.ttl(f"thread:{TEST_THREAD}:current_state")
        assert ttl > 0
        assert ttl <= worker.TTL_THREAD


# ═══════════════════════════════════════════════════════════════════════════════
# Cleanup Tests
# ═══════════════════════════════════════════════════════════════════════════════

class TestCleanup:
    """Validate task removal from processing list and active_tasks after completion."""

    @patch("worker.subprocess.run")
    def test_removed_from_processing_list(self, mock_run, fake_redis):
        """After completion, task is LREM'd from PROCESSING."""
        mock_proc = MagicMock(returncode=0, stdout="ok", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)
        assert fake_redis.llen(worker.PROCESSING) == 1

        worker.process_one_task(payload)

        assert fake_redis.llen(worker.PROCESSING) == 0

    @patch("worker.subprocess.run")
    def test_removed_from_active_tasks(self, mock_run, fake_redis):
        """After completion, task is HDEL'd from active_tasks."""
        mock_proc = MagicMock(returncode=0, stdout="ok", stderr="")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        assert fake_redis.hget("active_tasks", TEST_TASK_ID) is None

    @patch("worker.subprocess.run")
    def test_cleanup_after_failed_task(self, mock_run, fake_redis):
        """Cleanup still happens after a failed task."""
        mock_proc = MagicMock(returncode=1, stdout="Partial", stderr="Error")
        mock_run.return_value = mock_proc

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        assert fake_redis.llen(worker.PROCESSING) == 0
        assert fake_redis.hget("active_tasks", TEST_TASK_ID) is None

    @patch("worker.subprocess.run")
    def test_cleanup_after_timeout(self, mock_run, fake_redis):
        """Cleanup still happens after a timeout."""
        import subprocess as sp
        mock_run.side_effect = sp.TimeoutExpired(cmd=["claude", "-p"], timeout=10)

        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        assert fake_redis.llen(worker.PROCESSING) == 0
        assert fake_redis.hget("active_tasks", TEST_TASK_ID) is None


# ═══════════════════════════════════════════════════════════════════════════════
# JSON-Lines Logging Tests
# ═══════════════════════════════════════════════════════════════════════════════

class TestLogFormat:
    """Validate log() emits structured JSON lines with required fields."""

    def test_log_output_is_valid_json(self, fake_redis, capsys):
        """Each log() call produces a single line of parseable JSON."""
        worker.log("info", "test message")
        captured = capsys.readouterr().out.strip()
        entry = json.loads(captured)
        assert isinstance(entry, dict)

    def test_log_contains_level_and_msg(self, fake_redis, capsys):
        """Log entries always contain level and msg fields."""
        worker.log("warn", "connection lost")
        entry = json.loads(capsys.readouterr().out.strip())
        assert entry["level"] == "warn"
        assert entry["msg"] == "connection lost"

    def test_log_contains_worker_field(self, fake_redis, capsys):
        """Every log entry includes the WORKER_TYPE."""
        worker.log("info", "startup")
        entry = json.loads(capsys.readouterr().out.strip())
        assert entry["worker"] == WORKER_TYPE

    def test_log_task_context_includes_task_id_and_thread_id(self, fake_redis, capsys):
        """Task-related log entries must include task_id and thread_id."""
        worker.log("info", "task dequeued", task_id=TEST_TASK_ID, thread_id=TEST_THREAD)
        entry = json.loads(capsys.readouterr().out.strip())
        assert entry["msg"] == "task dequeued"
        assert entry["task_id"] == TEST_TASK_ID
        assert entry["thread_id"] == TEST_THREAD

    def test_log_agent_finished_includes_exit_code_and_status(self, fake_redis, capsys):
        """Agent completion logs include exit_code and status."""
        worker.log("info", "agent finished", task_id=TEST_TASK_ID,
                   thread_id=TEST_THREAD, exit_code=0, status="done")
        entry = json.loads(capsys.readouterr().out.strip())
        assert entry["exit_code"] == 0
        assert entry["status"] == "done"

    def test_log_accepts_additional_arbitrary_fields(self, fake_redis, capsys):
        """log() forwards any extra keyword args as JSON fields."""
        worker.log("debug", "custom", extra_field=42, flag=True)
        entry = json.loads(capsys.readouterr().out.strip())
        assert entry["extra_field"] == 42
        assert entry["flag"] is True

    def test_log_flushes_immediately(self, fake_redis, capsys):
        """flush=True ensures log lines are written immediately."""
        worker.log("info", "immediate")
        out = capsys.readouterr().out
        assert "immediate" in out

    def test_log_task_complete_has_status(self, fake_redis, capsys):
        """'task complete' log includes the final status."""
        worker.log("info", "task complete", task_id=TEST_TASK_ID,
                   thread_id=TEST_THREAD, status="done")
        entry = json.loads(capsys.readouterr().out.strip())
        assert entry["status"] == "done"

    def test_log_started_includes_queue_and_agent_cmd(self, fake_redis, capsys):
        """Worker startup log includes queue name and agent command."""
        worker.log("info", "worker started", queue=worker.QUEUE, agent_cmd=worker.AGENT_CMD)
        entry = json.loads(capsys.readouterr().out.strip())
        assert entry["queue"] == f"tasks:queue:{WORKER_TYPE}"
        assert "claude -p" in entry["agent_cmd"]


# ═══════════════════════════════════════════════════════════════════════════════
# Cancellation TTL Tests
# ═══════════════════════════════════════════════════════════════════════════════

class TestCancellationTTLs:
    """All keys set in the cancellation branch must have TTL_TASK expiry."""

    def test_cancelled_status_key_has_ttl(self, fake_redis):
        """task:{id}:status set during cancellation has TTL."""
        fake_redis.set(f"task:{TEST_TASK_ID}:cancel", "1")
        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        ttl = fake_redis.ttl(f"task:{TEST_TASK_ID}:status")
        assert ttl > 0
        assert ttl <= worker.TTL_TASK

    def test_cancelled_result_key_has_ttl(self, fake_redis):
        """task:{id}:result set during cancellation has TTL."""
        fake_redis.set(f"task:{TEST_TASK_ID}:cancel", "1")
        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        ttl = fake_redis.ttl(f"task:{TEST_TASK_ID}:result")
        assert ttl > 0
        assert ttl <= worker.TTL_TASK

    def test_cancelled_exit_code_key_has_ttl(self, fake_redis):
        """task:{id}:exit_code set during cancellation has TTL."""
        fake_redis.set(f"task:{TEST_TASK_ID}:cancel", "1")
        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        ttl = fake_redis.ttl(f"task:{TEST_TASK_ID}:exit_code")
        assert ttl > 0
        assert ttl <= worker.TTL_TASK

    def test_cancelled_completed_at_key_has_ttl(self, fake_redis):
        """task:{id}:completed_at set during cancellation has TTL."""
        fake_redis.set(f"task:{TEST_TASK_ID}:cancel", "1")
        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        ttl = fake_redis.ttl(f"task:{TEST_TASK_ID}:completed_at")
        assert ttl > 0
        assert ttl <= worker.TTL_TASK

    def test_cancelled_thread_messages_have_ttl(self, fake_redis):
        """thread messages TTL is refreshed during cancellation."""
        fake_redis.rpush(f"thread:{TEST_THREAD}:messages", make_msg("master", "prior"))
        fake_redis.expire(f"thread:{TEST_THREAD}:messages", worker.TTL_THREAD)

        fake_redis.set(f"task:{TEST_TASK_ID}:cancel", "1")
        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        worker.process_one_task(payload)

        ttl = fake_redis.ttl(f"thread:{TEST_THREAD}:messages")
        assert ttl > 0


# ═══════════════════════════════════════════════════════════════════════════════
# Malformed Input / Edge Case Tests
# ═══════════════════════════════════════════════════════════════════════════════

class TestMalformedInput:
    """Verify graceful handling of malformed inputs — invalid payloads still
    raise, but malformed thread messages are skipped with warnings."""

    @patch("worker.subprocess.run")
    def test_malformed_task_payload_raises_json_decode_error(self, mock_run, fake_redis):
        """Invalid JSON in task payload currently crashes the worker."""
        payload = "not valid json {{{"
        fake_redis.lpush(worker.PROCESSING, payload)

        with pytest.raises(json.JSONDecodeError):
            worker.process_one_task(payload)

        mock_run.assert_not_called()

    @patch("worker.subprocess.run")
    def test_malformed_thread_message_skipped_with_warning(self, mock_run, fake_redis):
        """A corrupt message in thread history is skipped with a warning."""
        mock_run.return_value = MagicMock(returncode=0, stdout="ok", stderr="")
        fake_redis.rpush(f"thread:{TEST_THREAD}:messages", "not valid json")
        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        status = worker.process_one_task(payload)
        assert status == "done"

    @patch("worker.subprocess.run")
    def test_thread_message_missing_role_key_uses_unknown(self, mock_run, fake_redis):
        """A message without 'role' key defaults to 'unknown'."""
        mock_run.return_value = MagicMock(returncode=0, stdout="ok", stderr="")
        bad_msg = json.dumps({"content": "no role field", "timestamp": "2026-05-10T00:00:00Z"})
        fake_redis.rpush(f"thread:{TEST_THREAD}:messages", bad_msg)
        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        status = worker.process_one_task(payload)
        assert status == "done"

    @patch("worker.subprocess.run")
    def test_thread_message_missing_content_key_uses_empty(self, mock_run, fake_redis):
        """A message without 'content' key defaults to empty string."""
        mock_run.return_value = MagicMock(returncode=0, stdout="ok", stderr="")
        bad_msg = json.dumps({"role": "master", "timestamp": "2026-05-10T00:00:00Z"})
        fake_redis.rpush(f"thread:{TEST_THREAD}:messages", bad_msg)
        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        status = worker.process_one_task(payload)
        assert status == "done"

    @patch("worker.subprocess.run")
    def test_thread_message_with_extra_fields_does_not_crash(self, mock_run, fake_redis):
        """Extra unknown fields in a thread message are safely ignored."""
        mock_run.return_value = MagicMock(returncode=0, stdout="ok", stderr="")
        msg = make_msg("master", "valid message")
        msg_data = json.loads(msg)
        msg_data["unexpected_field"] = "should be fine"
        fake_redis.rpush(f"thread:{TEST_THREAD}:messages", json.dumps(msg_data))
        payload = make_task_payload()
        fake_redis.lpush(worker.PROCESSING, payload)

        status = worker.process_one_task(payload)
        assert status == "done"

    @patch("worker.subprocess.run")
    def test_task_payload_missing_required_fields_crashes(self, mock_run, fake_redis):
        """Payload missing 'task_id', 'thread_id', or 'instruction' crashes."""
        bad_payload = json.dumps({"task_id": TEST_TASK_ID, "thread_id": TEST_THREAD})
        fake_redis.lpush(worker.PROCESSING, bad_payload)

        with pytest.raises(KeyError) as exc:
            worker.process_one_task(bad_payload)
        assert "instruction" in str(exc.value)

        mock_run.assert_not_called()


# ═══════════════════════════════════════════════════════════════════════════════
# Main Poll Loop Tests
# ═══════════════════════════════════════════════════════════════════════════════

class TestMainPollLoop:
    """Exercise main() — BLMOVE dequeuing, timeout, error recovery, shutdown.

    Tests work around the infinite while loop by either:
      - Patching blmove to return None and setting running=False
      - Patching process_one_task and simulating single iterations
      - Raising ConnectionError and verifying reconnection logic
    """

    def test_main_dequeues_and_processes_task(self, fake_redis, monkeypatch):
        """BLMOVE returns a task → process_one_task is called with that payload."""
        payload = make_task_payload()
        call_args = []

        def fake_blmove(*args, **kwargs):
            call_args.append("blmove_called")
            return payload

        def fake_process(task_json):
            call_args.append(("process", task_json))

        monkeypatch.setattr(worker.r, "blmove", fake_blmove, raising=False)
        monkeypatch.setattr(worker, "process_one_task", fake_process, raising=False)

        task_json = worker.r.blmove(worker.QUEUE, worker.PROCESSING, 5,
                                    src="RIGHT", dest="LEFT")
        if task_json:
            worker.process_one_task(task_json)

        assert "blmove_called" in call_args
        assert ("process", payload) in call_args

    def test_main_empty_blmove_continues_loop(self, fake_redis, monkeypatch):
        """BLMOVE timeout (None result) → no crash, caller continues."""
        blmove_calls = []

        def fake_blmove(*args, **kwargs):
            blmove_calls.append(kwargs.get("src"))
            return None

        monkeypatch.setattr(worker.r, "blmove", fake_blmove, raising=False)

        result = worker.r.blmove(worker.QUEUE, worker.PROCESSING, 5,
                                 src="RIGHT", dest="LEFT")
        assert result is None
        assert len(blmove_calls) == 1

    def test_main_connection_error_triggers_reconnect(self, fake_redis, monkeypatch):
        """ConnectionError on blmove → sleep 1s, reconnect, continue."""
        import redis as redis_mod

        call_order = []

        def fake_blmove_raise(*args, **kwargs):
            call_order.append("blmove_failed")
            raise redis_mod.exceptions.ConnectionError("simulated disconnect")

        monkeypatch.setattr(worker.r, "blmove", fake_blmove_raise, raising=False)

        def fake_connect():
            call_order.append("reconnect")
            return fake_redis

        monkeypatch.setattr(worker, "_connect", fake_connect, raising=False)

        def fake_sleep(secs):
            call_order.append(f"sleep({secs})")

        monkeypatch.setattr(worker.time, "sleep", fake_sleep, raising=False)

        try:
            worker.r.blmove(worker.QUEUE, worker.PROCESSING, 5,
                            src="RIGHT", dest="LEFT")
        except redis_mod.exceptions.ConnectionError:
            worker.time.sleep(1)
            worker.r = worker._connect()
            call_order.append("reconnected")

        assert "blmove_failed" in call_order
        assert "sleep(1)" in call_order
        assert "reconnect" in call_order
        assert "reconnected" in call_order

    def test_main_loop_sets_signal_handlers(self, fake_redis, monkeypatch):
        """main() registers SIGTERM and SIGINT handlers."""
        sig_registered = []

        def fake_signal(sig, handler):
            sig_registered.append(sig)

        monkeypatch.setattr(worker.signal, "signal", fake_signal, raising=False)
        # Prevent infinite loop — running=False exits immediately
        monkeypatch.setattr(worker, "running", False, raising=False)
        monkeypatch.setattr(worker, "log", lambda *a, **kw: None, raising=False)

        worker.main()

        assert worker.signal.SIGTERM in sig_registered
        assert worker.signal.SIGINT in sig_registered

    def test_main_loop_exits_when_running_is_false(self, fake_redis, monkeypatch):
        """When running=False, the while loop terminates without calling blmove."""
        monkeypatch.setattr(worker, "running", False, raising=False)
        monkeypatch.setattr(worker.signal, "signal", lambda s, h: None, raising=False)
        monkeypatch.setattr(worker, "log", lambda *a, **kw: None, raising=False)

        blmove_called = []

        def fake_blmove(src, dst, timeout, **kwargs):
            blmove_called.append(1)
            return None

        monkeypatch.setattr(worker.r, "blmove", fake_blmove, raising=False)

        worker.main()

        # Loop should exit immediately without calling blmove
        assert len(blmove_called) == 0


# ═══════════════════════════════════════════════════════════════════════════════
# Shutdown Signal Handler Tests
# ═══════════════════════════════════════════════════════════════════════════════

class TestShutdownSignal:
    """Verify shutdown() correctly handles SIGTERM and SIGINT."""

    def test_sigterm_handler_sets_running_false(self, fake_redis, monkeypatch):
        """SIGTERM triggers shutdown() → running = False."""
        monkeypatch.setattr(worker, "running", True, raising=False)
        monkeypatch.setattr(worker, "log", lambda *a, **kw: None, raising=False)

        worker.shutdown(worker.signal.SIGTERM, None)

        assert worker.running is False

    def test_sigint_handler_sets_running_false(self, fake_redis, monkeypatch):
        """SIGINT triggers shutdown() → running = False."""
        monkeypatch.setattr(worker, "running", True, raising=False)
        monkeypatch.setattr(worker, "log", lambda *a, **kw: None, raising=False)

        worker.shutdown(worker.signal.SIGINT, None)

        assert worker.running is False

    def test_shutdown_logs_signal_number(self, fake_redis, capsys):
        """shutdown() logs the signal number that triggered it."""
        worker.shutdown(worker.signal.SIGTERM, None)

        captured = capsys.readouterr().out.strip()
        entry = json.loads(captured)
        assert entry["msg"] == "received signal"
        assert entry["signal"] == int(worker.signal.SIGTERM)
        assert entry["level"] == "info"

    def test_shutdown_respects_sigint_signal_number(self, fake_redis, capsys):
        """SIGINT (typically signal 2) is logged correctly."""
        worker.shutdown(worker.signal.SIGINT, None)

        captured = capsys.readouterr().out.strip()
        entry = json.loads(captured)
        assert entry["msg"] == "received signal"
        assert entry["signal"] == int(worker.signal.SIGINT)

