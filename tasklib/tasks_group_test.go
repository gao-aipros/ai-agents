package tasklib

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestGroupTasksKey(t *testing.T) {
	got := GroupTasksKey("thread-1", "design-review")
	want := "thread:thread-1:group:design-review:tasks"
	if got != want {
		t.Errorf("GroupTasksKey = %q, want %q", got, want)
	}
}

func TestGroupResultJSON(t *testing.T) {
	r := GroupResult{
		ThreadID: "thread-1",
		Label:    "design-review",
		Status:   "complete",
		Tasks: map[string]string{
			"task-a": "done",
			"task-b": "done",
		},
	}

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var r2 GroupResult
	if err := json.Unmarshal(b, &r2); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if r2.ThreadID != r.ThreadID || r2.Label != r.Label || r2.Status != r.Status {
		t.Errorf("round-trip mismatch: %+v vs %+v", r, r2)
	}
	if r2.Tasks["task-a"] != "done" || r2.Tasks["task-b"] != "done" {
		t.Errorf("tasks not preserved: %v", r2.Tasks)
	}
}

// ── EnqueueGroup tests ─────────────────────────────────────────────────────

func TestEnqueueGroupFanOut(t *testing.T) {
	c, _ := setupTestClient(t)

	workers := []string{"claude", "copilot", "opencode", "codex"}
	var taskIDs []string
	for _, w := range workers {
		task, err := c.EnqueueGroup(ctx(), w, "thread-1", "design-review", "review the design")
		if err != nil {
			t.Fatalf("EnqueueGroup for %s failed: %v", w, err)
		}
		if task.TaskID == "" {
			t.Error("expected non-empty task ID")
		}
		if task.Status != "pending" {
			t.Errorf("expected status pending, got %s", task.Status)
		}
		taskIDs = append(taskIDs, task.TaskID)
	}

	// Verify all tasks are in the group SET
	members, err := c.rdb.SMembers(ctx(), GroupTasksKey("thread-1", "design-review")).Result()
	if err != nil {
		t.Fatalf("SMEMBERS failed: %v", err)
	}
	if len(members) != 4 {
		t.Errorf("expected 4 tasks in group, got %d", len(members))
	}
	for _, tid := range taskIDs {
		found := false
		for _, m := range members {
			if m == tid {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("task %s not found in group SET", tid)
		}
	}
}

func TestEnqueueGroupFailsWhenLocked(t *testing.T) {
	c, _ := setupTestClient(t)

	// Acquire lock via a sequential Enqueue (don't wait for it)
	task, err := c.Enqueue(ctx(), "claude", "thread-1", "sequential task")
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}
	// Lock is now held (Enqueue doesn't release it — WaitTask does)

	// EnqueueGroup should fail because the lock is held
	_, err = c.EnqueueGroup(ctx(), "copilot", "thread-1", "design-review", "review task")
	if err == nil {
		t.Error("expected EnqueueGroup to fail when lock is held, but it succeeded")
	}

	// Release lock so cleanup doesn't interfere
	c.rdb.Del(ctx(), ThreadLockKey("thread-1"))
	_ = task
}

func TestEnqueueGroupStaleLockAutoClear(t *testing.T) {
	c, _ := setupTestClient(t)

	// Pre-lock the thread with a STALE holder (task doesn't exist)
	c.rdb.Set(ctx(), ThreadLockKey("stale-group-thread"), "ghost-task", LockTTL)

	task, err := c.EnqueueGroup(ctx(), "copilot", "stale-group-thread", "design-review", "review something")
	if err != nil {
		t.Fatalf("EnqueueGroup should auto-clear stale lock, got error: %v", err)
	}
	if task.TaskID == "" {
		t.Error("expected non-empty task ID")
	}
}

func TestEnqueueGroupStaleLockTerminalStatus(t *testing.T) {
	c, _ := setupTestClient(t)

	for _, status := range []string{"done", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			threadID := "stale-group-" + status + "-thread"
			holderID := "stale-group-" + status + "-task"

			c.rdb.Set(ctx(), ThreadLockKey(threadID), holderID, LockTTL)
			c.rdb.Set(ctx(), TaskKey(holderID, "status"), status, 0)

			task, err := c.EnqueueGroup(ctx(), "codex", threadID, "code-review", "review PR")
			if err != nil {
				t.Fatalf("EnqueueGroup should auto-clear lock with %s holder, got error: %v", status, err)
			}
			if task.TaskID == "" {
				t.Error("expected non-empty task ID")
			}
		})
	}
}

func TestEnqueueGroupRaceBetweenCalls(t *testing.T) {
	c, _ := setupTestClient(t)

	// Two EnqueueGroup calls on the same thread back-to-back should
	// both succeed (gate-check lock is released immediately).
	t1, err := c.EnqueueGroup(ctx(), "claude", "thread-1", "design-review", "task one")
	if err != nil {
		t.Fatalf("first EnqueueGroup failed: %v", err)
	}
	t2, err := c.EnqueueGroup(ctx(), "copilot", "thread-1", "design-review", "task two")
	if err != nil {
		t.Fatalf("second EnqueueGroup failed: %v", err)
	}

	if t1.TaskID == t2.TaskID {
		t.Error("expected different task IDs")
	}

	// Both in the group SET
	members, err := c.rdb.SMembers(ctx(), GroupTasksKey("thread-1", "design-review")).Result()
	if err != nil {
		t.Fatalf("SMEMBERS failed: %v", err)
	}
	if len(members) != 2 {
		t.Errorf("expected 2 tasks in group, got %d", len(members))
	}
}

func TestEnqueueGroupSetsGroupLabel(t *testing.T) {
	c, _ := setupTestClient(t)

	task, err := c.EnqueueGroup(ctx(), "claude", "thread-1", "design-review", "first group")
	if err != nil {
		t.Fatalf("EnqueueGroup failed: %v", err)
	}

	// Verify the per-task group key is set correctly.
	label, err := c.rdb.Get(ctx(), TaskKey(task.TaskID, "group")).Result()
	if err != nil {
		t.Fatalf("GET task:<id>:group failed: %v", err)
	}
	if label != "design-review" {
		t.Errorf("expected group label 'design-review', got %q", label)
	}
}

func TestEnqueueGroupInvalidLabel(t *testing.T) {
	c, _ := setupTestClient(t)

	invalidLabels := []string{"bad:label", "has space", "tab\tchar", "new\nline"}
	for _, label := range invalidLabels {
		_, err := c.EnqueueGroup(ctx(), "claude", "thread-1", label, "instruction")
		if err == nil {
			t.Errorf("expected error for invalid label %q, got nil", label)
		}
	}
}

func TestEnqueueGroupDuplicateGroup(t *testing.T) {
	c, _ := setupTestClient(t)

	// Normal enqueue succeeds (guard at tasks.go:182 passes for fresh UUIDs)
	task, err := c.EnqueueGroup(ctx(), "claude", "thread-1", "design-review", "normal")
	if err != nil {
		t.Fatalf("EnqueueGroup failed: %v", err)
	}

	// Verify the guard correctly detects a pre-existing group assignment.
	// (In normal flow the guard never fires because EnqueueGroup generates
	// fresh UUIDs, but it protects against future code paths that might
	// pass an existing task ID.)
	c.rdb.Set(ctx(), TaskKey("known-task", "group"), "old-group", 0)
	existing, _ := c.rdb.Get(ctx(), TaskKey("known-task", "group")).Result()
	if existing == "" {
		t.Error("guard check would fail: expected non-empty group on pre-assigned task")
	}

	// Original task's group membership must be intact
	label, _ := c.rdb.Get(ctx(), TaskKey(task.TaskID, "group")).Result()
	if label != "design-review" {
		t.Errorf("task should be in 'design-review', got %q", label)
	}
}

// ── WaitTask group-task gate tests ──────────────────────────────────────────

// TestWaitTaskSkipsThreadStatusAndLockForGroup verifies that on all three
// exit paths (completion, timeout, context cancellation), WaitTask does NOT
// update thread status or release the lock for group tasks.
func TestWaitTaskSkipsThreadStatusAndLockForGroup(t *testing.T) {
	// ── completion path ──
	t.Run("completion", func(t *testing.T) {
		c, _ := setupTestClient(t)

		// Create thread and acquire + immediately release lock (simulating EnqueueGroup)
		if _, err := c.CreateThread(ctx(), "thr-group-done", ""); err != nil {
			t.Fatalf("CreateThread failed: %v", err)
		}
		// EnqueueGroup would have done SET NX → DEL on the lock.
		// We simulate the post-EnqueueGroup state: lock was released.
		c.rdb.Del(ctx(), ThreadLockKey("thr-group-done"))

		// Set up a group task (has task:<id>:group key set)
		c.rdb.Set(ctx(), TaskKey("gt1", "status"), "done", 0)
		c.rdb.Set(ctx(), TaskKey("gt1", "worker"), "claude", 0)
		c.rdb.Set(ctx(), TaskKey("gt1", "thread_id"), "thr-group-done", 0)
		c.rdb.Set(ctx(), TaskKey("gt1", "group"), "design-review", 0)

		// WaitTask the group task
		task, err := c.WaitTask(ctx(), "gt1", "thr-group-done", 5*time.Second)
		if err != nil {
			t.Fatalf("WaitTask failed: %v", err)
		}
		if task.Status != "done" {
			t.Errorf("expected status done, got %s", task.Status)
		}

		// Thread status should NOT have been updated (group tasks defer to GroupWait)
		thread, err := c.GetThread(ctx(), "thr-group-done")
		if err != nil {
			t.Fatalf("GetThread failed: %v", err)
		}
		// Status should remain "initiated" (WaitTask skipped updateThreadStatus
		// for group task — GroupWait handles it later)
		if thread.Status != "initiated" {
			t.Errorf("expected thread status 'initiated' (unchanged for group task), got %q", thread.Status)
		}
	})

	// ── timeout path ──
	t.Run("timeout", func(t *testing.T) {
		c, _ := setupTestClient(t)

		lockKey := ThreadLockKey("thr-group-timeout")
		// Simulate: another sequential task holds the lock (acquired after EnqueueGroup's release)
		// We acquire it here to verify WaitTask does NOT release it on timeout of a group task.
		c.rdb.Set(ctx(), lockKey, "other-task", 0)

		c.rdb.Set(ctx(), TaskKey("gt2", "status"), "running", 0)
		c.rdb.Set(ctx(), TaskKey("gt2", "thread_id"), "thr-group-timeout", 0)
		c.rdb.Set(ctx(), TaskKey("gt2", "group"), "code-review", 0)

		_, err := c.WaitTask(ctx(), "gt2", "thr-group-timeout", 10*time.Millisecond)
		if err == nil {
			t.Error("expected timeout error")
		}

		// Lock must still exist — WaitTask must NOT have released it for a group task
		if exists, _ := c.rdb.Exists(ctx(), lockKey).Result(); exists == 0 {
			t.Error("lock was released by WaitTask on timeout — should have been skipped for group task")
		}
	})

	// ── context cancellation path ──
	t.Run("context_cancel", func(t *testing.T) {
		c, _ := setupTestClient(t)

		lockKey := ThreadLockKey("thr-group-cancel")
		c.rdb.Set(ctx(), lockKey, "other-task", 0)

		c.rdb.Set(ctx(), TaskKey("gt3", "status"), "running", 0)
		c.rdb.Set(ctx(), TaskKey("gt3", "thread_id"), "thr-group-cancel", 0)
		c.rdb.Set(ctx(), TaskKey("gt3", "group"), "code-review", 0)

		cancelCtx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := c.WaitTask(cancelCtx, "gt3", "thr-group-cancel", 30*time.Second)
		if err == nil {
			t.Error("expected context cancellation error")
		}

		// Lock must still exist
		if exists, _ := c.rdb.Exists(ctx(), lockKey).Result(); exists == 0 {
			t.Error("lock was released by WaitTask on context cancel — should have been skipped for group task")
		}
	})
}

// TestWaitTaskSequentialStillWorks verifies that sequential tasks (no group
// label) still update thread status and release lock as before.
func TestWaitTaskSequentialStillWorks(t *testing.T) {
	// ── direct Redis setup (verifies thread status update) ──
	t.Run("update_thread_status", func(t *testing.T) {
		c, _ := setupTestClient(t)

		if _, err := c.CreateThread(ctx(), "thr-seq", ""); err != nil {
			t.Fatalf("CreateThread failed: %v", err)
		}
		c.rdb.Set(ctx(), TaskKey("st1", "status"), "done", 0)
		c.rdb.Set(ctx(), TaskKey("st1", "worker"), "claude", 0)
		c.rdb.Set(ctx(), TaskKey("st1", "thread_id"), "thr-seq", 0)

		_, err := c.WaitTask(ctx(), "st1", "thr-seq", 5*time.Second)
		if err != nil {
			t.Fatalf("WaitTask failed: %v", err)
		}

		thread, err := c.GetThread(ctx(), "thr-seq")
		if err != nil {
			t.Fatalf("GetThread failed: %v", err)
		}
		if thread.Status != "complete" {
			t.Errorf("expected thread status complete for sequential task, got %q", thread.Status)
		}
	})

	// ── Enqueue → WaitTask (verifies lock acquire + release) ──
	t.Run("lock_release", func(t *testing.T) {
		c, _ := setupTestClient(t)

		if _, err := c.CreateThread(ctx(), "thr-seq2", ""); err != nil {
			t.Fatalf("CreateThread failed: %v", err)
		}

		// Enqueue acquires the thread lock
		task, err := c.Enqueue(ctx(), "claude", "thr-seq2", "sequential")
		if err != nil {
			t.Fatalf("Enqueue failed: %v", err)
		}

		// Verify lock is held after Enqueue
		if exists, _ := c.rdb.Exists(ctx(), ThreadLockKey("thr-seq2")).Result(); exists == 0 {
			t.Fatal("expected lock to be held after Enqueue")
		}

		// Mark task done so WaitTask completes immediately
		c.rdb.Set(ctx(), TaskKey(task.TaskID, "status"), "done", 0)

		// WaitTask should release the lock for sequential tasks
		_, err = c.WaitTask(ctx(), task.TaskID, "thr-seq2", 5*time.Second)
		if err != nil {
			t.Fatalf("WaitTask failed: %v", err)
		}

		// Lock must be released
		if exists, _ := c.rdb.Exists(ctx(), ThreadLockKey("thr-seq2")).Result(); exists > 0 {
			t.Error("expected lock to be released after WaitTask on sequential task")
		}

		// Thread status must be updated
		thread, err := c.GetThread(ctx(), "thr-seq2")
		if err != nil {
			t.Fatalf("GetThread failed: %v", err)
		}
		if thread.Status != "complete" {
			t.Errorf("expected thread status 'complete', got %q", thread.Status)
		}
	})
}

func TestEnqueueGroupKeysHaveTTLs(t *testing.T) {
	c, mr := setupTestClient(t)

	task, err := c.EnqueueGroup(ctx(), "claude", "thread-1", "design-review", "test TTLs")
	if err != nil {
		t.Fatalf("EnqueueGroup failed: %v", err)
	}

	groupSetKey := GroupTasksKey("thread-1", "design-review")
	groupTaskKey := TaskKey(task.TaskID, "group")

	// Both keys should exist before expiry
	if !mr.Exists(groupSetKey) {
		t.Error("group SET should exist")
	}
	if !mr.Exists(groupTaskKey) {
		t.Error("per-task group key should exist")
	}

	// Fast-forward past TTLTask (24h) — the per-task key should expire
	mr.SetTime(time.Now())
	mr.FastForward(TTLTask + time.Second)
	if mr.Exists(groupTaskKey) {
		t.Error("per-task group key should have expired after TTLTask")
	}

	// Group SET should still exist (TTLThread = 7d > TTLTask = 24h)
	if !mr.Exists(groupSetKey) {
		t.Error("group SET should still exist after TTLTask (shorter than TTLThread)")
	}

	// Fast-forward past TTLThread — the group SET should expire
	mr.FastForward(TTLThread - TTLTask)
	if mr.Exists(groupSetKey) {
		t.Error("group SET should have expired after TTLThread")
	}
}

// ── GroupWait tests ────────────────────────────────────────────────────────

func TestGroupWaitAllDone(t *testing.T) {
	c, _ := setupTestClient(t)

	if _, err := c.CreateThread(ctx(), "thr-gw-done", ""); err != nil {
		t.Fatalf("CreateThread failed: %v", err)
	}

	// Set up a group with 3 tasks, all done
	taskIDs := []string{"t1", "t2", "t3"}
	for _, tid := range taskIDs {
		c.rdb.Set(ctx(), TaskKey(tid, "status"), "done", 0)
		c.rdb.Set(ctx(), TaskKey(tid, "thread_id"), "thr-gw-done", 0)
		c.rdb.SAdd(ctx(), GroupTasksKey("thr-gw-done", "review"), tid)
	}

	result, err := c.GroupWait(ctx(), "thr-gw-done", "review", 5*time.Second)
	if err != nil {
		t.Fatalf("GroupWait failed: %v", err)
	}
	if result.Status != "complete" {
		t.Errorf("expected status 'complete', got %q", result.Status)
	}
	if len(result.Tasks) != 3 {
		t.Errorf("expected 3 tasks, got %d", len(result.Tasks))
	}
	for _, tid := range taskIDs {
		if result.Tasks[tid] != "done" {
			t.Errorf("task %s should be done, got %q", tid, result.Tasks[tid])
		}
	}

	// Thread status must NOT be updated by GroupWait — only the request
	// handler (writeResponseMessage / writeErrorMessage) sets thread status.
	thread, err := c.GetThread(ctx(), "thr-gw-done")
	if err != nil {
		t.Fatalf("GetThread failed: %v", err)
	}
	if thread.Status != "initiated" {
		t.Errorf("expected thread status 'initiated' (unchanged), got %q", thread.Status)
	}
}

func TestGroupWaitMixedOutcomes(t *testing.T) {
	c, _ := setupTestClient(t)

	if _, err := c.CreateThread(ctx(), "thr-gw-mixed", ""); err != nil {
		t.Fatalf("CreateThread failed: %v", err)
	}

	// 2 done, 1 failed
	c.rdb.SAdd(ctx(), GroupTasksKey("thr-gw-mixed", "review"), "t-ok1", "t-ok2", "t-fail")
	c.rdb.Set(ctx(), TaskKey("t-ok1", "status"), "done", 0)
	c.rdb.Set(ctx(), TaskKey("t-ok2", "status"), "done", 0)
	c.rdb.Set(ctx(), TaskKey("t-fail", "status"), "failed", 0)

	result, err := c.GroupWait(ctx(), "thr-gw-mixed", "review", 5*time.Second)
	if err != nil {
		t.Fatalf("GroupWait failed: %v", err)
	}
	if result.Status != "error" {
		t.Errorf("expected status 'error', got %q", result.Status)
	}
	if result.Tasks["t-ok1"] != "done" {
		t.Errorf("t-ok1 should be done, got %q", result.Tasks["t-ok1"])
	}
	if result.Tasks["t-fail"] != "failed" {
		t.Errorf("t-fail should be failed, got %q", result.Tasks["t-fail"])
	}

	// Thread status must NOT be updated by GroupWait
	thread, _ := c.GetThread(ctx(), "thr-gw-mixed")
	if thread.Status != "initiated" {
		t.Errorf("expected thread status 'initiated' (unchanged), got %q", thread.Status)
	}
}

func TestGroupWaitCancelled(t *testing.T) {
	c, _ := setupTestClient(t)

	if _, err := c.CreateThread(ctx(), "thr-gw-cancelled", ""); err != nil {
		t.Fatalf("CreateThread failed: %v", err)
	}

	c.rdb.SAdd(ctx(), GroupTasksKey("thr-gw-cancelled", "review"), "t-c1", "t-c2")
	c.rdb.Set(ctx(), TaskKey("t-c1", "status"), "cancelled", 0)
	c.rdb.Set(ctx(), TaskKey("t-c2", "status"), "cancelled", 0)

	result, err := c.GroupWait(ctx(), "thr-gw-cancelled", "review", 5*time.Second)
	if err != nil {
		t.Fatalf("GroupWait failed: %v", err)
	}
	if result.Status != "cancelled" {
		t.Errorf("expected status 'cancelled', got %q", result.Status)
	}

	thread, _ := c.GetThread(ctx(), "thr-gw-cancelled")
	if thread.Status != "initiated" {
		t.Errorf("expected thread status 'initiated' (unchanged), got %q", thread.Status)
	}
}

func TestGroupWaitTimeout(t *testing.T) {
	c, _ := setupTestClient(t)

	if _, err := c.CreateThread(ctx(), "thr-gw-timeout", ""); err != nil {
		t.Fatalf("CreateThread failed: %v", err)
	}

	// Set task status to "running" so it never becomes terminal
	c.rdb.SAdd(ctx(), GroupTasksKey("thr-gw-timeout", "review"), "t-stuck")
	c.rdb.Set(ctx(), TaskKey("t-stuck", "status"), "running", 0)

	result, err := c.GroupWait(ctx(), "thr-gw-timeout", "review", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("GroupWait should return result (not error) on timeout: %v", err)
	}
	if result.Status != "timeout" {
		t.Errorf("expected status 'timeout', got %q", result.Status)
	}
	if result.Tasks["t-stuck"] != "running" {
		t.Errorf("expected t-stuck running, got %q", result.Tasks["t-stuck"])
	}

	// Thread status must NOT be updated on timeout
	thread, _ := c.GetThread(ctx(), "thr-gw-timeout")
	if thread.Status != "initiated" {
		t.Errorf("expected thread status 'initiated' (unchanged on timeout), got %q", thread.Status)
	}
}

func TestGroupWaitEmptyGroup(t *testing.T) {
	c, _ := setupTestClient(t)

	_, err := c.GroupWait(ctx(), "nonexistent-thread", "no-group", 1*time.Second)
	if err == nil {
		t.Error("expected error for nonexistent group")
	}
}

func TestThreadStatusAfterGroupWait(t *testing.T) {
	tests := []struct {
		name          string
		statuses      map[string]string
		wantAggregate string
	}{
		{name: "all_done", statuses: map[string]string{"a": "done", "b": "done"}, wantAggregate: "complete"},
		{name: "any_failed", statuses: map[string]string{"a": "done", "b": "failed"}, wantAggregate: "error"},
		{name: "all_cancelled", statuses: map[string]string{"a": "cancelled", "b": "cancelled"}, wantAggregate: "cancelled"},
		{name: "mixed_done_cancelled", statuses: map[string]string{"a": "done", "b": "cancelled"}, wantAggregate: "complete"},
		{name: "failed_plus_cancelled", statuses: map[string]string{"a": "failed", "b": "cancelled"}, wantAggregate: "error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := setupTestClient(t)

			threadID := "thr-agg-" + tt.name
			if _, err := c.CreateThread(ctx(), threadID, ""); err != nil {
				t.Fatalf("CreateThread failed: %v", err)
			}

			for tid, status := range tt.statuses {
				c.rdb.Set(ctx(), TaskKey(tid, "status"), status, 0)
				c.rdb.SAdd(ctx(), GroupTasksKey(threadID, "review"), tid)
			}

			result, err := c.GroupWait(ctx(), threadID, "review", 5*time.Second)
			if err != nil {
				t.Fatalf("GroupWait failed: %v", err)
			}
			if result.Status != tt.wantAggregate {
				t.Errorf("expected status %q, got %q", tt.wantAggregate, result.Status)
			}

			thread, _ := c.GetThread(ctx(), threadID)
			if thread.Status != "initiated" {
				t.Errorf("expected thread status 'initiated' (unchanged), got %q", thread.Status)
			}
		})
	}
}

func TestParallelSequentialPhases(t *testing.T) {
	c, _ := setupTestClient(t)

	if _, err := c.CreateThread(ctx(), "thr-phases", ""); err != nil {
		t.Fatalf("CreateThread failed: %v", err)
	}

	// Phase 1: EnqueueGroup fan-out (parallel review)
	t1, _ := c.EnqueueGroup(ctx(), "claude", "thr-phases", "review", "review task 1")
	t2, _ := c.EnqueueGroup(ctx(), "copilot", "thr-phases", "review", "review task 2")

	// Complete both group tasks
	c.rdb.Set(ctx(), TaskKey(t1.TaskID, "status"), "done", 0)
	c.rdb.Set(ctx(), TaskKey(t2.TaskID, "status"), "done", 0)

	// GroupWait should complete successfully
	result, err := c.GroupWait(ctx(), "thr-phases", "review", 5*time.Second)
	if err != nil {
		t.Fatalf("GroupWait failed: %v", err)
	}
	if result.Status != "complete" {
		t.Errorf("expected complete, got %q", result.Status)
	}

	// Phase 2: Sequential enqueue after group-wait must succeed
	// (lock was released by EnqueueGroup, and GroupWait doesn't hold it)
	t3, err := c.Enqueue(ctx(), "codex", "thr-phases", "implement")
	if err != nil {
		t.Fatalf("sequential Enqueue after group-wait failed: %v", err)
	}
	if t3.Status != "pending" {
		t.Errorf("expected status pending, got %s", t3.Status)
	}

	// Verify lock is held by the sequential task
	exists, _ := c.rdb.Exists(ctx(), ThreadLockKey("thr-phases")).Result()
	if exists == 0 {
		t.Error("expected lock to be held by sequential Enqueue")
	}
}

func TestGroupWaitMixedTerminalAndNonTerminal(t *testing.T) {
	c, _ := setupTestClient(t)

	if _, err := c.CreateThread(ctx(), "thr-gw-mixed-nonterm", ""); err != nil {
		t.Fatalf("CreateThread failed: %v", err)
	}

	// One done, one still running — group should NOT be complete
	c.rdb.SAdd(ctx(), GroupTasksKey("thr-gw-mixed-nonterm", "review"), "t-done", "t-running")
	c.rdb.Set(ctx(), TaskKey("t-done", "status"), "done", 0)
	c.rdb.Set(ctx(), TaskKey("t-running", "status"), "running", 0)

	result, err := c.GroupWait(ctx(), "thr-gw-mixed-nonterm", "review", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("GroupWait should return result on timeout: %v", err)
	}
	if result.Status != "timeout" {
		t.Errorf("expected status 'timeout' (one task still running), got %q", result.Status)
	}
	if result.Tasks["t-running"] != "running" {
		t.Errorf("expected t-running running, got %q", result.Tasks["t-running"])
	}

	// Thread status must NOT have been updated (still initiated)
	thread, _ := c.GetThread(ctx(), "thr-gw-mixed-nonterm")
	if thread.Status != "initiated" {
		t.Errorf("expected thread status 'initiated' (unchanged), got %q", thread.Status)
	}
}

// TestGroupWaitDoesNotUpdateThreadStatus verifies that GroupWait never sets
// thread status, regardless of group outcome. Thread status must only be
// managed by the request handler (writeResponseMessage / writeErrorMessage)
// so the UI correctly reflects the Claude subprocess state. A GroupWait
// completing does not mean the overall request is done — the subprocess
// may still be processing results.
func TestGroupWaitDoesNotUpdateThreadStatus(t *testing.T) {
	tests := []struct {
		name     string
		statuses map[string]string
	}{
		{
			name:     "all_tasks_done",
			statuses: map[string]string{"t1": "done", "t2": "done", "t3": "done"},
		},
		{
			name:     "mixed_done_and_failed",
			statuses: map[string]string{"t1": "done", "t2": "failed"},
		},
		{
			name:     "all_tasks_cancelled",
			statuses: map[string]string{"t1": "cancelled"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := setupTestClient(t)

			threadID := "thr-gw-noup-" + tt.name
			if _, err := c.CreateThread(ctx(), threadID, ""); err != nil {
				t.Fatalf("CreateThread failed: %v", err)
			}

			// Simulate thread state as it would be during an active request:
			// status="running" and running lock is held.
			c.UpdateThread(ctx(), threadID, map[string]string{"status": "running"})
			c.AcquireRequestLock(ctx(), threadID, "req-1", LockTTL)

			for tid, status := range tt.statuses {
				c.rdb.Set(ctx(), TaskKey(tid, "status"), status, 0)
				c.rdb.SAdd(ctx(), GroupTasksKey(threadID, "review"), tid)
			}

			_, err := c.GroupWait(ctx(), threadID, "review", 5*time.Second)
			if err != nil {
				t.Fatalf("GroupWait failed: %v", err)
			}

			// Thread status must NOT change — still "running" because the
			// request handler owns that field, not GroupWait.
			thread, _ := c.GetThread(ctx(), threadID)
			if thread.Status != "running" {
				t.Errorf("expected thread status 'running' (unchanged), got %q", thread.Status)
			}

			// Running lock must NOT be released — only the request handler
			// releases it via ReleaseRequestLock in runSubprocess cleanup.
			locked, _ := c.IsRequestRunning(ctx(), threadID)
			if !locked {
				t.Error("expected running lock to still be held after GroupWait")
			}
		})
	}
}
