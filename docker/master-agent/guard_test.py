#!/usr/bin/env python3
"""Tests for master-agent guard.py enforcement logic."""

import os
import sys
import tempfile
import unittest

# Add guard.py to path for import
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import guard


class TestCheckWrite(unittest.TestCase):
    """Tests for check_write — only .md files in allowed dirs pass."""

    def test_allows_md_in_docs(self):
        guard.check_write("docs/design.md")

    def test_allows_md_in_claude(self):
        guard.check_write(".claude/memory.md")

    def test_blocks_claude_json(self):
        """Master should not write to .claude.json — it's not a .md file."""
        with self.assertRaises(SystemExit) as cm:
            guard.check_write(".claude.json")
        self.assertEqual(cm.exception.code, 1)

    def test_blocks_non_md_go_file(self):
        with self.assertRaises(SystemExit) as cm:
            guard.check_write("repo/main.go")
        self.assertEqual(cm.exception.code, 1)

    def test_blocks_non_md_py_file(self):
        with self.assertRaises(SystemExit) as cm:
            guard.check_write("scripts/deploy.py")
        self.assertEqual(cm.exception.code, 1)

    def test_blocks_non_md_yaml_file(self):
        with self.assertRaises(SystemExit) as cm:
            guard.check_write("docker-compose.yml")
        self.assertEqual(cm.exception.code, 1)

    def test_blocks_non_md_shell_script(self):
        with self.assertRaises(SystemExit) as cm:
            guard.check_write("entrypoint.sh")
        self.assertEqual(cm.exception.code, 1)

    def test_blocks_non_md_no_extension(self):
        with self.assertRaises(SystemExit) as cm:
            guard.check_write("Dockerfile")
        self.assertEqual(cm.exception.code, 1)

    def test_blocks_md_outside_allowed_dirs(self):
        with self.assertRaises(SystemExit) as cm:
            guard.check_write("repo/README.md")
        self.assertEqual(cm.exception.code, 1)

    def test_blocks_md_in_workspace_root(self):
        with self.assertRaises(SystemExit) as cm:
            guard.check_write("CHANGELOG.md")
        self.assertEqual(cm.exception.code, 1)

    def test_allows_mdx_in_docs(self):
        guard.check_write("docs/guide.mdx")


class TestCheckBashGh(unittest.TestCase):
    """Tests that forbidden gh commands are blocked."""

    def test_blocks_gh_pr_create(self):
        with self.assertRaises(SystemExit) as cm:
            guard.check_bash("gh pr create --title 'test'")
        self.assertEqual(cm.exception.code, 1)

    def test_blocks_gh_pr_review(self):
        with self.assertRaises(SystemExit) as cm:
            guard.check_bash("gh pr review 115 --approve")
        self.assertEqual(cm.exception.code, 1)

    def test_blocks_gh_pr_merge(self):
        with self.assertRaises(SystemExit) as cm:
            guard.check_bash("gh pr merge 115 --squash")
        self.assertEqual(cm.exception.code, 1)

    def test_blocks_gh_pr_close(self):
        with self.assertRaises(SystemExit) as cm:
            guard.check_bash("gh pr close 115")
        self.assertEqual(cm.exception.code, 1)

    def test_blocks_gh_pr_comment(self):
        with self.assertRaises(SystemExit) as cm:
            guard.check_bash("gh pr comment 115 -b 'looks good'")
        self.assertEqual(cm.exception.code, 1)

    def test_blocks_gh_api(self):
        with self.assertRaises(SystemExit) as cm:
            guard.check_bash("gh api repos/owner/repo/issues/1")
        self.assertEqual(cm.exception.code, 1)

    def test_blocks_gh_issue_create(self):
        with self.assertRaises(SystemExit) as cm:
            guard.check_bash("gh issue create --title 'bug'")
        self.assertEqual(cm.exception.code, 1)

    def test_allows_gh_pr_view(self):
        guard.check_bash("gh pr view 115")

    def test_allows_gh_pr_list(self):
        guard.check_bash("gh pr list --state open")

    def test_allows_gh_pr_status(self):
        guard.check_bash("gh pr status")

    def test_allows_gh_pr_checks(self):
        guard.check_bash("gh pr checks 115")

    def test_allows_gh_issue_view(self):
        guard.check_bash("gh issue view 42")

    def test_allows_gh_issue_list(self):
        guard.check_bash("gh issue list --state open")


class TestCheckBashGit(unittest.TestCase):
    """Tests that forbidden git commands are blocked, read-only are allowed."""

    def test_blocks_git_commit(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("git commit -m 'fix'")

    def test_blocks_git_push(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("git push origin main")

    def test_blocks_git_branch(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("git branch -d old-branch")

    def test_blocks_git_checkout(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("git checkout main")

    def test_blocks_git_checkout_b(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("git checkout -b new-branch")

    def test_blocks_git_merge(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("git merge feature-branch")

    def test_blocks_git_rebase(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("git rebase main")

    def test_blocks_git_revert(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("git revert HEAD~1")

    def test_blocks_git_rm(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("git rm old_file.go")

    def test_blocks_git_fetch(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("git fetch origin")

    def test_blocks_git_pull(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("git pull origin main")

    def test_blocks_git_tag(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("git tag v1.0")

    def test_blocks_git_reset(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("git reset --hard HEAD~1")

    def test_blocks_git_stash(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("git stash")

    def test_blocks_git_cherry_pick(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("git cherry-pick abc123")

    def test_allows_git_log(self):
        guard.check_bash("git log --oneline -10")

    def test_allows_git_show(self):
        guard.check_bash("git show HEAD")

    def test_allows_git_diff(self):
        guard.check_bash("git diff main...HEAD")

    def test_allows_git_status(self):
        guard.check_bash("git status")

    def test_allows_git_blame(self):
        guard.check_bash("git blame src/main.go")


class TestCheckBashBuild(unittest.TestCase):
    """Tests that build/test commands are blocked."""

    def test_blocks_go_build(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("go build -o out/ ./...")

    def test_blocks_go_test(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("go test ./...")

    def test_blocks_go_run(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("go run main.go")

    def test_blocks_make(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("make build")

    def test_blocks_docker_build(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("docker build -t foo .")

    def test_blocks_docker_run(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("docker run -it ubuntu bash")

    def test_blocks_npm(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("npm install express")


class TestCheckBashRedirects(unittest.TestCase):
    """Tests for shell redirect pattern — must not false-positive on jq/strings."""

    def test_blocks_redirect_to_tmp(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("echo foo > /tmp/out.txt")

    def test_blocks_redirect_to_workspace(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("cat log > /workspace/thread-1/output")

    def test_blocks_redirect_to_file_with_extension(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("echo 'data' > report.json")

    def test_allows_jq_comparison_gt(self):
        """jq 'select(.value > 0)' must not be blocked."""
        guard.check_bash(
            "jq -r '.tasks | to_entries | map(select(.value > 0))'"
        )

    def test_allows_jq_comparison_gte(self):
        """jq 'select(.count >= 5)' must not be blocked."""
        guard.check_bash(
            "jq 'map(select(.count >= 5)) | length'"
        )

    def test_allows_string_with_arrow(self):
        """echo 'a -> b' must not be blocked."""
        guard.check_bash("echo 'a -> b'")

    def test_allows_task_result_with_jq(self):
        """Real-world fan-out jq pipeline must not be blocked."""
        guard.check_bash(
            'RESULT=$(task group-wait --thread $THREAD --group "code-review" --timeout 1200)'
        )


class TestCheckBashFilesystemWrite(unittest.TestCase):
    """Tests that filesystem write commands are blocked."""

    def test_blocks_touch(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("touch newfile.go")

    def test_blocks_rm(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("rm -rf build/")

    def test_blocks_chmod(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("chmod +x script.sh")

    def test_blocks_cp(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("cp a.txt b.txt")

    def test_blocks_mv(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("mv old.txt new.txt")

    def test_blocks_tee(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("echo log | tee /tmp/log.txt")

    def test_blocks_dd_of(self):
        with self.assertRaises(SystemExit):
            guard.check_bash("dd if=/dev/zero of=disk.img bs=1M count=10")


class TestCheckBashTask(unittest.TestCase):
    """Tests that master task commands are allowed."""

    def test_allows_task_enqueue(self):
        guard.check_bash(
            "task enqueue --worker claude --thread t1 --instruction 'implement'"
        )

    def test_allows_task_status(self):
        guard.check_bash("task status --id abc123")

    def test_allows_task_result(self):
        guard.check_bash("task result --id abc123")

    def test_allows_task_group_wait(self):
        guard.check_bash(
            "task group-wait --thread t1 --group 'code-review' --timeout 1200"
        )

    def test_allows_task_unlock(self):
        guard.check_bash("task unlock --thread t1")

    def test_allows_task_events(self):
        guard.check_bash("task events --limit 50")

    def test_allows_task_cancel(self):
        guard.check_bash("task cancel --id abc123")

    def test_allows_task_requeue_stale(self):
        guard.check_bash("task requeue-stale --worker claude --older-than 600")


if __name__ == "__main__":
    unittest.main()
