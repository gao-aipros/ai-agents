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
	c, _ := setupTestClient(t)

	if _, err := c.CreateThread(ctx(), "thr-seq", ""); err != nil {
		t.Fatalf("CreateThread failed: %v", err)
	}
	c.rdb.Set(ctx(), TaskKey("st1", "status"), "done", 0)
	c.rdb.Set(ctx(), TaskKey("st1", "worker"), "claude", 0)
	c.rdb.Set(ctx(), TaskKey("st1", "thread_id"), "thr-seq", 0)
	// No task:<id>:group key — this is a sequential task

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

	// Lock should be released
	exists, _ := c.rdb.Exists(ctx(), ThreadLockKey("thr-seq")).Result()
	if exists > 0 {
		t.Error("expected lock to be released for sequential task")
	}
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
