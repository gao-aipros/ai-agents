#!/usr/bin/env python3
"""Tests for worker-claude guard.py enforcement logic."""

import os
import sys
import unittest

# Add guard.py to path for import
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import guard


class TestCheckBashBlocksMasterCommands(unittest.TestCase):
    """Tests that master-only task commands are blocked for workers."""

    def test_blocks_task_enqueue(self):
        with self.assertRaises(SystemExit) as cm:
            guard.check_bash(
                "task enqueue --worker copilot --thread t1 --instruction 'review'"
            )
        self.assertEqual(cm.exception.code, 1)

    def test_blocks_task_thread_create(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("task thread-create --id new-thread")

    def test_blocks_task_thread_update(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("task thread-update --id t1 --status reviewing")

    def test_blocks_task_thread_cleanup(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("task thread-cleanup --id t1")

    def test_blocks_task_group_wait(self):
        with self.assertRaises(SystemExit):
            guard.check_bash(
                "task group-wait --thread t1 --group 'code-review' --timeout 1200"
            )

    def test_blocks_task_unlock(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("task unlock --thread t1")

    def test_blocks_task_requeue_stale(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("task requeue-stale --worker claude --older-than 600")

    def test_blocks_task_cancel(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("task cancel --id abc123")

    def test_blocks_task_events(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("task events --limit 50")

    def test_blocks_task_list(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("task list --status running")

    def test_blocks_task_thread_list(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("task thread-list")


class TestCheckBashAllowsWorkerCommands(unittest.TestCase):
    """Tests that worker-level commands are allowed."""

    def test_allows_task_status(self):
        guard.check_bash("task status --id abc123")

    def test_allows_task_result(self):
        guard.check_bash("task result --id abc123")

    def test_allows_gh_pr_view(self):
        guard.check_bash("gh pr view 115")

    def test_allows_gh_pr_comment(self):
        guard.check_bash("gh pr comment 115 -b 'fixed'")

    def test_allows_gh_pr_review(self):
        guard.check_bash("gh pr review 115 --approve")

    def test_allows_gh_pr_create(self):
        guard.check_bash("gh pr create --title 'feat: add feature' --body '...'")

    def test_allows_gh_pr_merge(self):
        guard.check_bash("gh pr merge 115 --squash --delete-branch")

    def test_allows_git_commit(self):
        guard.check_bash("git commit -m 'fix: address review'")

    def test_allows_git_push(self):
        guard.check_bash("git push -u origin HEAD")

    def test_allows_git_checkout_b(self):
        guard.check_bash("git checkout -b feat/new-feature")

    def test_allows_git_checkout(self):
        guard.check_bash("git checkout main")

    def test_allows_go_build(self):
        guard.check_bash("go build -o out/ ./...")

    def test_allows_go_test(self):
        guard.check_bash("go test ./...")

    def test_allows_clone_repo(self):
        guard.check_bash("gh repo clone owner/repo repo")


class TestEdgeCases(unittest.TestCase):
    """Tests for edge cases and defensive handling."""

    def test_handles_empty_command(self):
        """check_bash called with empty command should pass (no false positive)."""
        guard.check_bash("")

    def test_handles_invalid_regex_in_command(self):
        """Complex shell commands should be checked without error."""
        guard.check_bash("python3 -c 'print(123)'")

    def test_handles_very_long_command(self):
        """Long pipeline commands should be checked correctly."""
        with self.assertRaises(SystemExit):
            guard.check_bash("echo start && task enqueue --worker copilot --thread t1"
                             " --group 'design-review' --instruction 'review docs'")

    def test_partial_match_not_blocked(self):
        """Words containing forbidden patterns should not be blocked."""
        # 'task_enqueue' contains 'enqueue' as a substring but
        # '\btask\s+enqueue\b' requires whitespace between task and enqueue
        guard.check_bash("echo 'task_enqueue is a function name'")

    def test_command_with_newlines(self):
        """Multi-line commands should be checked."""
        guard.check_bash("gh pr view 115\ngh pr status")


if __name__ == "__main__":
    unittest.main()
