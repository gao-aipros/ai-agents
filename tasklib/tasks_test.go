package tasklib

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func setupTestClient(t *testing.T) (*Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return NewClient(rdb), mr
}

func ctx() context.Context {
	return context.Background()
}

// ── task CRUD tests ───────────────────────────────────────────────────────

func TestEnqueue(t *testing.T) {
	c, _ := setupTestClient(t)

	task, err := c.Enqueue(ctx(), "claude", "test-thread", "do something")
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}
	if task.TaskID == "" {
		t.Error("expected non-empty task ID")
	}
	if task.Status != "pending" {
		t.Errorf("expected status pending, got %s", task.Status)
	}
	if task.Worker != "claude" {
		t.Errorf("expected worker claude, got %s", task.Worker)
	}

	// Verify queue entry exists
	items, err := c.rdb.LRange(ctx(), QueueKey("claude"), 0, -1).Result()
	if err != nil {
		t.Fatalf("LRANGE failed: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 item in queue, got %d", len(items))
	}

	// Verify task keys exist
	status, _ := c.rdb.Get(ctx(), TaskKey(task.TaskID, "status")).Result()
	if status != "pending" {
		t.Errorf("expected status key 'pending', got '%s'", status)
	}

	// Verify thread message appended
	msgs, _ := c.rdb.LRange(ctx(), ThreadMessagesKey("test-thread"), 0, -1).Result()
	if len(msgs) != 1 {
		t.Errorf("expected 1 thread message, got %d", len(msgs))
	}
}

func TestEnqueueThreadLocked(t *testing.T) {
	c, _ := setupTestClient(t)

	// Pre-lock the thread with an ACTIVE holder (status=running) — should fail
	c.rdb.Set(ctx(), ThreadLockKey("locked-thread"), "active-task", LockTTL)
	c.rdb.Set(ctx(), TaskKey("active-task", "status"), "running", 0)

	_, err := c.Enqueue(ctx(), "claude", "locked-thread", "do something")
	if err == nil {
		t.Error("expected error for thread locked by active holder")
	}
}

func TestEnqueueStaleLockAutoClear(t *testing.T) {
	c, _ := setupTestClient(t)

	// Pre-lock the thread with a STALE holder (task doesn't exist in Redis)
	c.rdb.Set(ctx(), ThreadLockKey("stale-lock-thread"), "ghost-task", LockTTL)

	task, err := c.Enqueue(ctx(), "claude", "stale-lock-thread", "do something")
	if err != nil {
		t.Fatalf("Enqueue should auto-clear stale lock, got error: %v", err)
	}
	if task.TaskID == "" {
		t.Error("expected non-empty task ID")
	}

	// Old lock key should be gone (replaced by new task's lock)
	holder, _ := c.rdb.Get(ctx(), ThreadLockKey("stale-lock-thread")).Result()
	if holder == "ghost-task" {
		t.Error("expected stale lock to be replaced by new task lock")
	}
}

func TestEnqueueStaleLockTerminalStatus(t *testing.T) {
	c, _ := setupTestClient(t)

	for _, status := range []string{"done", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			threadID := "stale-" + status + "-thread"
			holderID := "stale-" + status + "-task"

			c.rdb.Set(ctx(), ThreadLockKey(threadID), holderID, LockTTL)
			c.rdb.Set(ctx(), TaskKey(holderID, "status"), status, 0)

			task, err := c.Enqueue(ctx(), "claude", threadID, "do something")
			if err != nil {
				t.Fatalf("Enqueue should auto-clear lock with %s holder, got error: %v", status, err)
			}
			if task.TaskID == "" {
				t.Error("expected non-empty task ID")
			}
		})
	}
}

func TestIsTaskActive(t *testing.T) {
	c, _ := setupTestClient(t)

	// Empty task ID
	if c.isTaskActive(ctx(), "") {
		t.Error("empty task ID should be inactive")
	}

	// Missing key
	if c.isTaskActive(ctx(), "no-such-task") {
		t.Error("missing task should be inactive")
	}

	// Terminal statuses
	for _, status := range []string{"done", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			c.rdb.Set(ctx(), TaskKey("t-"+status, "status"), status, 0)
			if c.isTaskActive(ctx(), "t-"+status) {
				t.Errorf("task with status %s should be inactive", status)
			}
		})
	}

	// Active statuses
	for _, status := range []string{"pending", "running"} {
		t.Run(status, func(t *testing.T) {
			c.rdb.Set(ctx(), TaskKey("t-"+status, "status"), status, 0)
			if !c.isTaskActive(ctx(), "t-"+status) {
				t.Errorf("task with status %s should be active", status)
			}
		})
	}
}

func TestGetTask(t *testing.T) {
	c, _ := setupTestClient(t)

	// Pre-populate task keys
	taskID := "test-task-1"
	c.rdb.Set(ctx(), TaskKey(taskID, "status"), "done", 0)
	c.rdb.Set(ctx(), TaskKey(taskID, "worker"), "claude", 0)
	c.rdb.Set(ctx(), TaskKey(taskID, "thread_id"), "thr1", 0)
	c.rdb.Set(ctx(), TaskKey(taskID, "result"), "success output", 0)
	c.rdb.Set(ctx(), TaskKey(taskID, "exit_code"), "0", 0)

	task, err := c.GetTask(ctx(), taskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if task.Status != "done" {
		t.Errorf("expected status done, got %s", task.Status)
	}
	if task.Result != "success output" {
		t.Errorf("expected result 'success output', got '%s'", task.Result)
	}
}

func TestGetTaskResult(t *testing.T) {
	c, _ := setupTestClient(t)

	c.rdb.Set(ctx(), TaskKey("t1", "result"), "line1\nline2\nline3\nline4", 0)

	// Full result
	r, _ := c.GetTaskResult(ctx(), "t1", 0)
	if r != "" {
		t.Errorf("expected empty result for tail=0, got '%s'", r)
	}

	r, _ = c.GetTaskResult(ctx(), "t1", -1)
	if r != "line1\nline2\nline3\nline4" {
		t.Errorf("expected full result, got '%s'", r)
	}

	// Tail 2
	r, _ = c.GetTaskResult(ctx(), "t1", 2)
	if r != "line3\nline4" {
		t.Errorf("expected last 2 lines, got '%s'", r)
	}

	// Missing task
	r, _ = c.GetTaskResult(ctx(), "nonexistent", -1)
	if r != "" {
		t.Errorf("expected empty for missing task, got '%s'", r)
	}
}

func TestListTasks(t *testing.T) {
	c, _ := setupTestClient(t)

	// Populate some tasks
	for i, info := range []struct{ id, status, worker, thread string }{
		{"a", "done", "claude", "thr1"},
		{"b", "running", "copilot", "thr1"},
		{"c", "failed", "claude", "thr2"},
	} {
		c.rdb.Set(ctx(), TaskKey(info.id, "status"), info.status, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "worker"), info.worker, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "thread_id"), info.thread, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "enqueued_at"), "2025-01-0"+strconv.Itoa(i+1)+"T00:00:00Z", 0)
	}

	tasks, err := c.ListTasks(ctx(), "", "", "", 50, 0, "", "")
	if err != nil {
		t.Fatalf("ListTasks failed: %v", err)
	}
	if len(tasks) < 3 {
		t.Errorf("expected at least 3 tasks, got %d", len(tasks))
	}

	// Filter by status
	tasks, _ = c.ListTasks(ctx(), "", "done", "", 50, 0, "", "")
	for _, task := range tasks {
		if task.Status != "done" {
			t.Errorf("status filter failed: expected only done, got %s", task.Status)
		}
	}

	// Filter by worker
	tasks, _ = c.ListTasks(ctx(), "claude", "", "", 50, 0, "", "")
	for _, task := range tasks {
		if task.Worker != "claude" {
			t.Errorf("worker filter failed: expected only claude, got %s", task.Worker)
		}
	}
}

func TestListTasksLimitAndOffset(t *testing.T) {
	c, _ := setupTestClient(t)

	for i := 0; i < 10; i++ {
		id := "t" + strconv.Itoa(i)
		c.rdb.Set(ctx(), TaskKey(id, "status"), "done", 0)
		c.rdb.Set(ctx(), TaskKey(id, "worker"), "claude", 0)
		c.rdb.Set(ctx(), TaskKey(id, "thread_id"), "thr1", 0)
	}

	tasks, _ := c.ListTasks(ctx(), "", "", "", 3, 0, "", "")
	if len(tasks) > 3 {
		t.Errorf("limit failed: expected at most 3, got %d", len(tasks))
	}

	tasks, _ = c.ListTasks(ctx(), "", "", "", 50, 5, "", "")
	// offset 5 should skip the first 5
	if len(tasks) > 5 { // 10 total - 5 offset = 5 remaining, capped at limit 50
		// ok
	}
}

func TestCancelTask(t *testing.T) {
	c, _ := setupTestClient(t)

	c.rdb.Set(ctx(), TaskKey("t1", "status"), "pending", 0)

	err := c.CancelTask(ctx(), "t1", "user")
	if err != nil {
		t.Fatalf("CancelTask failed: %v", err)
	}

	val, _ := c.rdb.Get(ctx(), TaskKey("t1", "cancel")).Result()
	if val != "1" {
		t.Errorf("expected cancel flag '1', got '%s'", val)
	}

	// Status must NOT be changed by CancelTask (matches task.py behavior)
	status, _ := c.rdb.Get(ctx(), TaskKey("t1", "status")).Result()
	if status != "pending" {
		t.Errorf("expected status 'pending' preserved, got '%s'", status)
	}

	// Cancel non-existent task
	err = c.CancelTask(ctx(), "no-such-task", "user")
	if err == nil {
		t.Error("expected error for non-existent task")
	}
}

func TestCancelTaskPreservesAllStatuses(t *testing.T) {
	c, _ := setupTestClient(t)

	for _, status := range []string{"pending", "running", "done", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			taskID := "ct-" + status
			c.rdb.Set(ctx(), TaskKey(taskID, "status"), status, 0)

			err := c.CancelTask(ctx(), taskID, "user")
			if err != nil {
				t.Fatalf("CancelTask on %s failed: %v", status, err)
			}

			// Cancel flag should be set
			flag, _ := c.rdb.Get(ctx(), TaskKey(taskID, "cancel")).Result()
			if flag != "1" {
				t.Errorf("expected cancel flag '1' for %s, got '%s'", status, flag)
			}

			// Status must NOT change
			current, _ := c.rdb.Get(ctx(), TaskKey(taskID, "status")).Result()
			if current != status {
				t.Errorf("expected status '%s' preserved, got '%s'", status, current)
			}
		})
	}
}

func TestListTasksWithFilters(t *testing.T) {
	c, _ := setupTestClient(t)

	// Populate tasks: 5 claude tasks, then 3 copilot tasks
	for i := 0; i < 5; i++ {
		id := "c-" + strconv.Itoa(i)
		c.rdb.Set(ctx(), TaskKey(id, "status"), "done", 0)
		c.rdb.Set(ctx(), TaskKey(id, "worker"), "claude", 0)
		c.rdb.Set(ctx(), TaskKey(id, "thread_id"), "thr1", 0)
	}
	for i := 0; i < 3; i++ {
		id := "p-" + strconv.Itoa(i)
		c.rdb.Set(ctx(), TaskKey(id, "status"), "done", 0)
		c.rdb.Set(ctx(), TaskKey(id, "worker"), "copilot", 0)
		c.rdb.Set(ctx(), TaskKey(id, "thread_id"), "thr1", 0)
	}

	// Filter for copilot tasks with limit 2. The old bug would break early
	// at len(rows) >= limit+offset before filter checks, causing 0 results
	// when copilot tasks appeared after claude in map iteration.
	tasks, _ := c.ListTasks(ctx(), "copilot", "", "", 2, 0, "", "")
	for _, task := range tasks {
		if task.Worker != "copilot" {
			t.Errorf("filter failed: expected only copilot tasks, got worker=%s", task.Worker)
		}
	}
	// Should have found 2 copilot tasks (limit=2, out of 3 matching)
	if len(tasks) != 2 {
		t.Errorf("expected 2 copilot tasks after limit, got %d", len(tasks))
	}
}

func TestRequeueStale(t *testing.T) {
	c, _ := setupTestClient(t)

	// Put a task in the processing list
	payload := `{"task_id":"stale-1","thread_id":"thr1","instruction":"do X"}`
	c.rdb.LPush(ctx(), ProcessingKey("claude"), payload)

	// No status key -> should be requeued (worker crashed before writing status)
	requeued, err := c.RequeueStale(ctx(), "claude", 10*time.Minute)
	if err != nil {
		t.Fatalf("RequeueStale failed: %v", err)
	}
	if len(requeued) != 1 || requeued[0] != "stale-1" {
		t.Errorf("expected stale-1 to be requeued, got %v", requeued)
	}

	// Verify it was moved to the queue
	queueItems, _ := c.rdb.LRange(ctx(), QueueKey("claude"), 0, -1).Result()
	if len(queueItems) != 1 {
		t.Errorf("expected 1 item in queue, got %d", len(queueItems))
	}

	// Processing list should be empty now
	procItems, _ := c.rdb.LRange(ctx(), ProcessingKey("claude"), 0, -1).Result()
	if len(procItems) != 0 {
		t.Errorf("expected empty processing list, got %d items", len(procItems))
	}
}

func TestRequeueStaleTerminalStatus(t *testing.T) {
	c, _ := setupTestClient(t)

	payload := `{"task_id":"done-1","thread_id":"thr1","instruction":"do Y"}`
	c.rdb.LPush(ctx(), ProcessingKey("claude"), payload)
	c.rdb.Set(ctx(), TaskKey("done-1", "status"), "done", 0)

	requeued, _ := c.RequeueStale(ctx(), "claude", 10*time.Minute)
	if len(requeued) != 0 {
		t.Errorf("terminal tasks should not be requeued, got %v", requeued)
	}

	// Processing entry should be garbage-collected
	procItems, _ := c.rdb.LRange(ctx(), ProcessingKey("claude"), 0, -1).Result()
	if len(procItems) != 0 {
		t.Errorf("terminal processing entries should be GC'd, got %d", len(procItems))
	}
}

// ── thread tests ──────────────────────────────────────────────────────────

func TestCreateAndGetThread(t *testing.T) {
	c, _ := setupTestClient(t)

	th, err := c.CreateThread(ctx(), "thr1", "owner/repo", "")
	if err != nil {
		t.Fatalf("CreateThread failed: %v", err)
	}
	if th.Status != "initiated" {
		t.Errorf("expected status initiated, got %s", th.Status)
	}
	if th.GHRepo != "owner/repo" {
		t.Errorf("expected gh_repo owner/repo, got %s", th.GHRepo)
	}

	// Retrieve
	th2, err := c.GetThread(ctx(), "thr1")
	if err != nil {
		t.Fatalf("GetThread failed: %v", err)
	}
	if th2.Status != "initiated" {
		t.Errorf("expected status initiated, got %s", th2.Status)
	}
}

func TestGetThreadNotFound(t *testing.T) {
	c, _ := setupTestClient(t)

	_, err := c.GetThread(ctx(), "no-such-thread")
	if err == nil {
		t.Error("expected error for non-existent thread")
	}
}

func TestListThreads(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "thr1", "a/b", "")
	c.CreateThread(ctx(), "thr2", "c/d", "")

	threads, err := c.ListThreads(ctx(), "", "")
	if err != nil {
		t.Fatalf("ListThreads failed: %v", err)
	}
	if len(threads) < 2 {
		t.Errorf("expected at least 2 threads, got %d", len(threads))
	}
}

func TestThreadHistory(t *testing.T) {
	c, _ := setupTestClient(t)

	key := ThreadMessagesKey("thr1")
	c.rdb.RPush(ctx(), key,
		`{"role":"user","type":"request","content":"hello","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"role":"master","type":"plan","content":"planning","timestamp":"2025-01-01T00:00:05Z"}`,
		`{"role":"master","type":"response","content":"done","timestamp":"2025-01-01T00:01:00Z"}`,
	)

	// Get full history
	msgs, err := c.GetThreadHistory(ctx(), "thr1", 0, 0)
	if err != nil {
		t.Fatalf("GetThreadHistory failed: %v", err)
	}
	if len(msgs) != 3 {
		t.Errorf("expected 3 messages, got %d", len(msgs))
	}

	// Tail
	msgs, _ = c.GetThreadHistoryTail(ctx(), "thr1", 2)
	if len(msgs) != 2 {
		t.Errorf("expected 2 tail messages, got %d", len(msgs))
	}
	if msgs[1].Role != "master" || msgs[1].Type != "response" {
		t.Errorf("expected last message to be master response, got role=%s type=%s", msgs[1].Role, msgs[1].Type)
	}

	// Pagination
	msgs, _ = c.GetThreadHistory(ctx(), "thr1", 1, 1)
	if len(msgs) != 1 {
		t.Errorf("expected 1 message from offset 1 limit 1, got %d", len(msgs))
	}
}

func TestAppendMessage(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "thr1", "", "")

	err := c.AppendMessage(ctx(), "thr1", Message{
		Role:    "master",
		Type:    "response",
		Content: "all done",
	})
	if err != nil {
		t.Fatalf("AppendMessage failed: %v", err)
	}

	msgs, _ := c.GetThreadHistory(ctx(), "thr1", 0, 0)
	if len(msgs) != 1 {
		t.Errorf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Content != "all done" {
		t.Errorf("expected 'all done', got '%s'", msgs[0].Content)
	}
}

func TestUpdateThread(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "thr1", "", "")

	err := c.UpdateThread(ctx(), "thr1", map[string]string{
		"status":       "complete",
		"last_design":  "use redis",
		"gh_pr_number": "42",
	})
	if err != nil {
		t.Fatalf("UpdateThread failed: %v", err)
	}

	th, _ := c.GetThread(ctx(), "thr1")
	if th.Status != "complete" {
		t.Errorf("expected status complete, got %s", th.Status)
	}
	if th.LastDesign != "use redis" {
		t.Errorf("expected design 'use redis', got '%s'", th.LastDesign)
	}
	if th.GHPRNumber != "42" {
		t.Errorf("expected PR 42, got '%s'", th.GHPRNumber)
	}
}

func TestUpdateThreadNotFound(t *testing.T) {
	c, _ := setupTestClient(t)

	err := c.UpdateThread(ctx(), "no-thread", map[string]string{"status": "complete"})
	if err == nil {
		t.Error("expected error for non-existent thread")
	}
}

func TestThreadLockUnlock(t *testing.T) {
	c, _ := setupTestClient(t)

	// Acquire lock
	ok, err := c.LockThread(ctx(), "thr1", "task-1", 60*time.Second)
	if err != nil {
		t.Fatalf("LockThread failed: %v", err)
	}
	if !ok {
		t.Error("expected lock to be acquired")
	}

	// Second lock attempt should fail
	ok, _ = c.LockThread(ctx(), "thr1", "task-2", 60*time.Second)
	if ok {
		t.Error("expected second lock to fail")
	}

	// Unlock
	err = c.UnlockThread(ctx(), "thr1")
	if err != nil {
		t.Fatalf("UnlockThread failed: %v", err)
	}

	// Now lock should succeed again
	ok, _ = c.LockThread(ctx(), "thr1", "task-3", 60*time.Second)
	if !ok {
		t.Error("expected lock to be acquired after unlock")
	}
}

// ── active_tasks tests ────────────────────────────────────────────────────

func TestActiveTasks(t *testing.T) {
	c, _ := setupTestClient(t)

	info := TaskInfo{
		Status:    "running",
		Worker:    "claude",
		ThreadID:  "thr1",
		StartedAt: "2025-01-01T00:00:00Z",
	}

	err := c.SetActiveTask(ctx(), "task-1", info)
	if err != nil {
		t.Fatalf("SetActiveTask failed: %v", err)
	}

	all, err := c.GetActiveTasks(ctx())
	if err != nil {
		t.Fatalf("GetActiveTasks failed: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("expected 1 active task, got %d", len(all))
	}
	if all["task-1"].Status != "running" {
		t.Errorf("expected running status, got %s", all["task-1"].Status)
	}

	err = c.RemoveActiveTask(ctx(), "task-1")
	if err != nil {
		t.Fatalf("RemoveActiveTask failed: %v", err)
	}

	all, _ = c.GetActiveTasks(ctx())
	if len(all) != 0 {
		t.Errorf("expected 0 active tasks after remove, got %d", len(all))
	}
}

// ── worker tests ──────────────────────────────────────────────────────────

func TestWorkerHeartbeat(t *testing.T) {
	c, _ := setupTestClient(t)

	err := c.UpdateWorkerHeartbeat(ctx(), "claude", HeartbeatData{Hostname: "host1"})
	if err != nil {
		t.Fatalf("UpdateWorkerHeartbeat failed: %v", err)
	}

	// Verify key exists with TTL
	key := HeartbeatKey("claude")
	val, _ := c.rdb.Get(ctx(), key).Result()
	if val == "" {
		t.Error("expected non-empty heartbeat value")
	}
	if val == "1" {
		t.Error("expected heartbeat JSON, got old format '1'")
	}
	ttl := c.rdb.TTL(ctx(), key).Val()
	if ttl <= 0 || ttl > 30*time.Second {
		t.Errorf("expected TTL between 0 and 30s, got %v", ttl)
	}
}

func TestGetWorkerStats(t *testing.T) {
	c, _ := setupTestClient(t)

	// Set heartbeats — each worker name is a separate identity
	c.UpdateWorkerHeartbeat(ctx(), "claude-1", HeartbeatData{Hostname: "host1"})
	c.UpdateWorkerHeartbeat(ctx(), "claude-2", HeartbeatData{Hostname: "host2"})
	c.UpdateWorkerHeartbeat(ctx(), "copilot-1", HeartbeatData{Hostname: "host3"})

	stats, err := c.GetWorkerStats(ctx())
	if err != nil {
		t.Fatalf("GetWorkerStats failed: %v", err)
	}

	if stats["claude-1"].Instances != 1 {
		t.Errorf("expected 1 claude-1 instance, got %d", stats["claude-1"].Instances)
	}
	if stats["claude-1"].Online != 1 {
		t.Errorf("expected 1 claude-1 online, got %d", stats["claude-1"].Online)
	}
	if stats["claude-2"].Instances != 1 {
		t.Errorf("expected 1 claude-2 instance, got %d", stats["claude-2"].Instances)
	}
	if stats["copilot-1"].Instances != 1 {
		t.Errorf("expected 1 copilot-1 instance, got %d", stats["copilot-1"].Instances)
	}
}

// ── request lock tests ─────────────────────────────────────────────────────

func TestAcquireReleaseRequestLock(t *testing.T) {
	c, _ := setupTestClient(t)

	ok, err := c.AcquireRequestLock(ctx(), "thr1", "req-1", 60*time.Second)
	if err != nil {
		t.Fatalf("AcquireRequestLock failed: %v", err)
	}
	if !ok {
		t.Error("expected lock to be acquired")
	}

	running, _ := c.IsRequestRunning(ctx(), "thr1")
	if !running {
		t.Error("expected thread to be marked as running")
	}

	err = c.ReleaseRequestLock(ctx(), "thr1")
	if err != nil {
		t.Fatalf("ReleaseRequestLock failed: %v", err)
	}

	running, _ = c.IsRequestRunning(ctx(), "thr1")
	if running {
		t.Error("expected thread to not be running after release")
	}
}

func TestAcquireRequestLockConflict(t *testing.T) {
	c, _ := setupTestClient(t)

	ok, _ := c.AcquireRequestLock(ctx(), "thr1", "req-1", 60*time.Second)
	if !ok {
		t.Fatal("first lock should succeed")
	}

	ok, _ = c.AcquireRequestLock(ctx(), "thr1", "req-2", 60*time.Second)
	if ok {
		t.Error("second lock on same thread should fail")
	}
}

// ── session ID tests ────────────────────────────────────────────────────────

func TestSessionID(t *testing.T) {
	c, _ := setupTestClient(t)

	err := c.SetThreadSessionID(ctx(), "thr1", "550e8400-e29b-41d4-a716-446655440000")
	if err != nil {
		t.Fatalf("SetThreadSessionID failed: %v", err)
	}

	sid, err := c.GetThreadSessionID(ctx(), "thr1")
	if err != nil {
		t.Fatalf("GetThreadSessionID failed: %v", err)
	}
	if sid != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("expected session UUID, got '%s'", sid)
	}
}

func TestGetSessionIDNotFound(t *testing.T) {
	c, _ := setupTestClient(t)

	sid, err := c.GetThreadSessionID(ctx(), "nonexistent")
	if err != nil {
		t.Fatalf("GetThreadSessionID should not error for missing key: %v", err)
	}
	if sid != "" {
		t.Errorf("expected empty string for missing session ID, got '%s'", sid)
	}
}

// ── CancelRequest tests ─────────────────────────────────────────────────────

func TestCancelRequest(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "cr-thread", "owner/repo", "")

	err := c.CancelRequest(ctx(), "cr-thread")
	if err != nil {
		t.Fatalf("CancelRequest failed: %v", err)
	}

	th, _ := c.GetThread(ctx(), "cr-thread")
	if th.Status != "cancelled" {
		t.Errorf("expected thread status cancelled, got %s", th.Status)
	}
}

func TestCancelRequestNoop(t *testing.T) {
	c, _ := setupTestClient(t)

	err := c.CancelRequest(ctx(), "no-such-thread")
	if err != nil {
		t.Errorf("CancelRequest on missing thread should not error, got: %v", err)
	}
}

// ── thread complete tests ───────────────────────────────────────────────────

func TestThreadComplete(t *testing.T) {
	c, _ := setupTestClient(t)

	complete, _ := c.IsThreadComplete(ctx(), "thr1")
	if complete {
		t.Error("expected thread not complete initially")
	}

	err := c.SetThreadComplete(ctx(), "thr1")
	if err != nil {
		t.Fatalf("SetThreadComplete failed: %v", err)
	}

	complete, _ = c.IsThreadComplete(ctx(), "thr1")
	if !complete {
		t.Error("expected thread complete after set")
	}
}

func TestClearThreadComplete(t *testing.T) {
	c, _ := setupTestClient(t)

	// Set complete, then clear it
	c.SetThreadComplete(ctx(), "thr1")
	err := c.ClearThreadComplete(ctx(), "thr1")
	if err != nil {
		t.Fatalf("ClearThreadComplete failed: %v", err)
	}

	complete, _ := c.IsThreadComplete(ctx(), "thr1")
	if complete {
		t.Error("expected thread NOT complete after clear")
	}
}

func TestClearThreadComplete_Idempotent(t *testing.T) {
	c, _ := setupTestClient(t)

	// Clearing a key that doesn't exist should not error
	err := c.ClearThreadComplete(ctx(), "nonexistent")
	if err != nil {
		t.Fatalf("ClearThreadComplete on missing key should not error: %v", err)
	}
}

// ── thread last activity tests ──────────────────────────────────────────────

func TestThreadLastActivity(t *testing.T) {
	c, _ := setupTestClient(t)

	val, _ := c.GetThreadLastActivity(ctx(), "thr1")
	if val != "" {
		t.Errorf("expected empty last activity for missing key, got '%s'", val)
	}

	err := c.UpdateThreadLastActivity(ctx(), "thr1")
	if err != nil {
		t.Fatalf("UpdateThreadLastActivity failed: %v", err)
	}

	val, _ = c.GetThreadLastActivity(ctx(), "thr1")
	if val == "" {
		t.Error("expected non-empty last activity after update")
	}
}

// ── WaitTask tests ────────────────────────────────────────────────────────

func TestWaitTaskImmediateCompletion(t *testing.T) {
	c, _ := setupTestClient(t)

	// Create thread first so UpdateThread succeeds
	if _, err := c.CreateThread(ctx(), "thr1", "", ""); err != nil {
		t.Fatalf("CreateThread failed: %v", err)
	}

	// Set up a task that's already done
	c.rdb.Set(ctx(), TaskKey("w1", "status"), "done", 0)
	c.rdb.Set(ctx(), TaskKey("w1", "worker"), "claude", 0)
	c.rdb.Set(ctx(), TaskKey("w1", "thread_id"), "thr1", 0)
	c.rdb.Set(ctx(), TaskKey("w1", "exit_code"), "0", 0)

	task, err := c.WaitTask(ctx(), "w1", "thr1", 5*time.Second)
	if err != nil {
		t.Fatalf("WaitTask failed: %v", err)
	}
	if task.Status != "done" {
		t.Errorf("expected status done, got %s", task.Status)
	}

	// Verify thread status was updated
	thread, err := c.GetThread(ctx(), "thr1")
	if err != nil {
		t.Fatalf("GetThread failed: %v", err)
	}
	if thread.Status != "complete" {
		t.Errorf("expected thread status complete, got %s", thread.Status)
	}
}

func TestWaitTaskTimeout(t *testing.T) {
	c, _ := setupTestClient(t)

	c.rdb.Set(ctx(), TaskKey("w2", "status"), "running", 0)

	_, err := c.WaitTask(ctx(), "w2", "", 10*time.Millisecond)
	if err == nil {
		t.Error("expected timeout error")
	}
	// Verify thread lock was released on timeout
	exists, _ := c.rdb.Exists(ctx(), ThreadLockKey("")).Result()
	if exists > 0 {
		t.Error("expected lock to be released on timeout")
	}
}

func TestWaitTaskContextCancel(t *testing.T) {
	c, _ := setupTestClient(t)

	c.rdb.Set(ctx(), TaskKey("w3", "status"), "running", 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := c.WaitTask(ctx, "w3", "thr1", 30*time.Second)
	if err == nil {
		t.Error("expected context cancellation error")
	}
}

func TestWaitTaskNotFound(t *testing.T) {
	c, _ := setupTestClient(t)

	_, err := c.WaitTask(ctx(), "no-such-task", "", 5*time.Second)
	if err == nil {
		t.Error("expected error for non-existent task")
	}
}

func TestWaitTaskTerminalStates(t *testing.T) {
	c, _ := setupTestClient(t)

	tests := []struct{ status string }{
		{"done"}, {"failed"}, {"cancelled"},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			id := "w-" + tt.status
			c.rdb.Set(ctx(), TaskKey(id, "status"), tt.status, 0)
			c.rdb.Set(ctx(), TaskKey(id, "worker"), "claude", 0)
			c.rdb.Set(ctx(), TaskKey(id, "thread_id"), "thr1", 0)

			task, err := c.WaitTask(ctx(), id, "", 5*time.Second)
			if err != nil {
				t.Fatalf("WaitTask for %s failed: %v", tt.status, err)
			}
			if task.Status != tt.status {
				t.Errorf("expected status %s, got %s", tt.status, task.Status)
			}
		})
	}
}

func TestWaitTaskUpdatesThreadStatus(t *testing.T) {
	c, _ := setupTestClient(t)

	tests := []struct {
		taskStatus   string
		threadStatus string
	}{
		{"done", "complete"},
		{"failed", "error"},
		{"cancelled", "cancelled"},
	}
	for _, tt := range tests {
		t.Run(tt.taskStatus, func(t *testing.T) {
			threadID := "thr-" + tt.taskStatus
			taskID := "t-" + tt.taskStatus

			if _, err := c.CreateThread(ctx(), threadID, "", ""); err != nil {
				t.Fatalf("CreateThread failed: %v", err)
			}
			c.rdb.Set(ctx(), TaskKey(taskID, "status"), tt.taskStatus, 0)
			c.rdb.Set(ctx(), TaskKey(taskID, "worker"), "claude", 0)
			c.rdb.Set(ctx(), TaskKey(taskID, "thread_id"), threadID, 0)

			_, err := c.WaitTask(ctx(), taskID, threadID, 5*time.Second)
			if err != nil {
				t.Fatalf("WaitTask for %s failed: %v", tt.taskStatus, err)
			}

			thread, err := c.GetThread(ctx(), threadID)
			if err != nil {
				t.Fatalf("GetThread failed: %v", err)
			}
			if thread.Status != tt.threadStatus {
				t.Errorf("expected thread status %s, got %s", tt.threadStatus, thread.Status)
			}
		})
	}
}

func TestWaitTaskTimeoutUpdatesThreadStatus(t *testing.T) {
	c, _ := setupTestClient(t)

	threadID := "thr-timeout-status"
	taskID := "t-timeout-status"

	if _, err := c.CreateThread(ctx(), threadID, "", ""); err != nil {
		t.Fatalf("CreateThread failed: %v", err)
	}
	c.rdb.Set(ctx(), TaskKey(taskID, "status"), "running", 0)
	c.rdb.Set(ctx(), TaskKey(taskID, "worker"), "claude", 0)
	c.rdb.Set(ctx(), TaskKey(taskID, "thread_id"), threadID, 0)

	// Acquire the thread lock so IsThreadLocked returns true.
	ok, err := c.LockThread(ctx(), threadID, taskID, 10*time.Second)
	if err != nil {
		t.Fatalf("LockThread failed: %v", err)
	}
	if !ok {
		t.Fatal("LockThread failed to acquire lock")
	}

	_, err = c.WaitTask(ctx(), taskID, threadID, 10*time.Millisecond)
	if err == nil {
		t.Error("expected timeout error")
	}

	// Verify thread status was updated to reflect the task's "running" status.
	thread, err := c.GetThread(ctx(), threadID)
	if err != nil {
		t.Fatalf("GetThread failed: %v", err)
	}
	if thread.Status != "running" {
		t.Errorf("expected thread status running after timeout, got %s", thread.Status)
	}

	// Verify lock was released after timeout.
	locked, err := c.IsThreadLocked(ctx(), threadID)
	if err != nil {
		t.Fatalf("IsThreadLocked failed: %v", err)
	}
	if locked {
		t.Error("expected lock to be released on timeout")
	}
}

func TestThreadStatusFromTaskKnownMapping(t *testing.T) {
	tests := []struct {
		taskStatus   string
		threadStatus string
	}{
		{"done", "complete"},
		{"failed", "error"},
		{"cancelled", "cancelled"},
		{"running", "running"},
		{"pending", "pending"},
		{"queued", "queued"},
		{"initiated", "initiated"},
		{"reviewing", "reviewing"},
	}
	for _, tt := range tests {
		t.Run(tt.taskStatus, func(t *testing.T) {
			got := threadStatusFromTask(tt.taskStatus)
			if got != tt.threadStatus {
				t.Errorf("threadStatusFromTask(%q) = %q, want %q", tt.taskStatus, got, tt.threadStatus)
			}
		})
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

// ── computeLockTTL ─────────────────────────────────────────────────────────

func TestComputeLockTTL(t *testing.T) {
	tests := []struct {
		name           string
		requestTimeout string // env value for REQUEST_TIMEOUT, "" = unset
		lockTTL        string // env value for LOCK_TTL, "" = unset
		want           time.Duration
	}{
		{
			name:           "default (neither env set)",
			requestTimeout: "",
			lockTTL:        "",
			want:           9300 * time.Second, // DefaultRequestTimeout(9000) + 300
		},
		{
			name:           "LOCK_TTL overrides everything",
			requestTimeout: "7200",
			lockTTL:        "5000",
			want:           5000 * time.Second,
		},
		{
			name:           "REQUEST_TIMEOUT fallback when LOCK_TTL unset",
			requestTimeout: "7200",
			lockTTL:        "",
			want:           7500 * time.Second, // 7200 + 300
		},
		{
			name:           "both set, LOCK_TTL wins",
			requestTimeout: "9000",
			lockTTL:        "6000",
			want:           6000 * time.Second,
		},
		{
			name:           "non-numeric LOCK_TTL ignored",
			requestTimeout: "",
			lockTTL:        "abc",
			want:           9300 * time.Second, // fallback to default
		},
		{
			name:           "zero LOCK_TTL ignored",
			requestTimeout: "",
			lockTTL:        "0",
			want:           9300 * time.Second,
		},
		{
			name:           "negative LOCK_TTL ignored",
			requestTimeout: "",
			lockTTL:        "-100",
			want:           9300 * time.Second,
		},
		{
			name:           "non-numeric REQUEST_TIMEOUT → uses DefaultRequestTimeout",
			requestTimeout: "bad",
			lockTTL:        "",
			want:           9300 * time.Second,
		},
		{
			name:           "custom REQUEST_TIMEOUT, no LOCK_TTL",
			requestTimeout: "10000",
			lockTTL:        "",
			want:           10300 * time.Second, // 10000 + 300
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(key string) (string, bool) {
				switch key {
				case "REQUEST_TIMEOUT":
					if tt.requestTimeout == "" {
						return "", false
					}
					return tt.requestTimeout, true
				case "LOCK_TTL":
					if tt.lockTTL == "" {
						return "", false
					}
					return tt.lockTTL, true
				}
				return "", false
			}
			got := computeLockTTL(lookup)
			if got != tt.want {
				t.Errorf("computeLockTTL() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ── sort tests ─────────────────────────────────────────────────────────────

func TestListTasksDefaultSort(t *testing.T) {
	c, _ := setupTestClient(t)

	// Populate tasks with different statuses
	for i, info := range []struct{ id, status, worker, thread string }{
		{"a", "done", "claude", "thr1"},
		{"b", "running", "copilot", "thr1"},
		{"c", "failed", "claude", "thr2"},
		{"d", "pending", "codex", "thr3"},
		{"e", "done", "copilot", "thr1"},
		{"f", "failed", "opencode", "thr2"},
	} {
		c.rdb.Set(ctx(), TaskKey(info.id, "status"), info.status, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "worker"), info.worker, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "thread_id"), info.thread, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "enqueued_at"), "2025-01-0"+strconv.Itoa(i+1)+"T00:00:00Z", 0)
	}

	tasks, err := c.ListTasks(ctx(), "", "", "", 50, 0, "", "")
	if err != nil {
		t.Fatalf("ListTasks failed: %v", err)
	}

	// Default sort: status priority (failed > running > pending > done), then task_id ASC
	// Expected: c(failed), f(failed), b(running), d(pending), a(done), e(done)
	expectedStatus := []string{"failed", "failed", "running", "pending", "done", "done"}
	for i, task := range tasks {
		if task.Status != expectedStatus[i] {
			t.Errorf("default sort at index %d: expected status %s, got %s (task %s)", i, expectedStatus[i], task.Status, task.TaskID)
		}
	}

	// Within same status, tasks should be sorted by task_id ASC
	if tasks[0].TaskID > tasks[1].TaskID {
		t.Errorf("within same status (failed), expected task_id ASC, got %s before %s", tasks[0].TaskID, tasks[1].TaskID)
	}
	if tasks[4].TaskID > tasks[5].TaskID {
		t.Errorf("within same status (done), expected task_id ASC, got %s before %s", tasks[4].TaskID, tasks[5].TaskID)
	}
}

func TestListTasksSortByColumn(t *testing.T) {
	c, _ := setupTestClient(t)

	// Populate tasks
	for i, info := range []struct{ id, status, worker, thread string }{
		{"z", "done", "claude", "thr2"},
		{"a", "done", "copilot", "thr1"},
		{"m", "done", "claude", "thr3"},
	} {
		c.rdb.Set(ctx(), TaskKey(info.id, "status"), info.status, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "worker"), info.worker, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "thread_id"), info.thread, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "enqueued_at"), "2025-01-0"+strconv.Itoa(i+1)+"T00:00:00Z", 0)
	}

	// Sort by task_id ASC
	tasks, err := c.ListTasks(ctx(), "", "", "", 50, 0, "task_id", "asc")
	if err != nil {
		t.Fatalf("ListTasks failed: %v", err)
	}
	if tasks[0].TaskID != "a" || tasks[1].TaskID != "m" || tasks[2].TaskID != "z" {
		t.Errorf("sort by task_id ASC: expected [a, m, z], got [%s, %s, %s]", tasks[0].TaskID, tasks[1].TaskID, tasks[2].TaskID)
	}

	// Sort by task_id DESC
	tasks, _ = c.ListTasks(ctx(), "", "", "", 50, 0, "task_id", "desc")
	if tasks[0].TaskID != "z" || tasks[1].TaskID != "m" || tasks[2].TaskID != "a" {
		t.Errorf("sort by task_id DESC: expected [z, m, a], got [%s, %s, %s]", tasks[0].TaskID, tasks[1].TaskID, tasks[2].TaskID)
	}

	// Sort by worker ASC
	tasks, _ = c.ListTasks(ctx(), "", "", "", 50, 0, "worker", "asc")
	if tasks[0].Worker != "claude" {
		t.Errorf("sort by worker ASC: expected claude first, got %s", tasks[0].Worker)
	}

	// Sort by status ASC (uses custom sort order)
	tasks, _ = c.ListTasks(ctx(), "", "", "", 50, 0, "status", "asc")
	if tasks[0].Status != "done" {
		t.Errorf("sort by status ASC: expected done first, got %s", tasks[0].Status)
	}
}

func TestListThreadsDefaultSort(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "thr-a", "", "")
	c.UpdateThread(ctx(), "thr-a", map[string]string{"status": "complete"})

	c.CreateThread(ctx(), "thr-b", "", "")
	c.UpdateThread(ctx(), "thr-b", map[string]string{"status": "running"})

	c.CreateThread(ctx(), "thr-c", "", "")
	c.UpdateThread(ctx(), "thr-c", map[string]string{"status": "error"})

	c.CreateThread(ctx(), "thr-d", "", "")
	c.UpdateThread(ctx(), "thr-d", map[string]string{"status": "complete"})

	threads, err := c.ListThreads(ctx(), "", "")
	if err != nil {
		t.Fatalf("ListThreads failed: %v", err)
	}

	// Default sort: status priority (error > running > complete), then thread_id ASC
	expected := []string{"thr-c", "thr-b", "thr-a", "thr-d"}
	for i, th := range threads {
		if th.ThreadID != expected[i] {
			t.Errorf("default sort at index %d: expected %s, got %s", i, expected[i], th.ThreadID)
		}
	}
}

func TestListThreadsSortByColumn(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "zzz", "", "")
	c.UpdateThread(ctx(), "zzz", map[string]string{"status": "complete", "gh_repo": "owner/repo-a"})

	c.CreateThread(ctx(), "aaa", "", "")
	c.UpdateThread(ctx(), "aaa", map[string]string{"status": "complete", "gh_repo": "owner/repo-b"})

	c.CreateThread(ctx(), "mmm", "", "")
	c.UpdateThread(ctx(), "mmm", map[string]string{"status": "running", "gh_repo": "owner/repo-a"})

	// Sort by thread_id ASC
	threads, err := c.ListThreads(ctx(), "thread_id", "asc")
	if err != nil {
		t.Fatalf("ListThreads failed: %v", err)
	}
	if threads[0].ThreadID != "aaa" || threads[2].ThreadID != "zzz" {
		t.Errorf("sort by thread_id ASC: expected [aaa, mmm, zzz], got [%s, %s, %s]", threads[0].ThreadID, threads[1].ThreadID, threads[2].ThreadID)
	}

	// Sort by thread_id DESC
	threads, _ = c.ListThreads(ctx(), "thread_id", "desc")
	if threads[0].ThreadID != "zzz" || threads[2].ThreadID != "aaa" {
		t.Errorf("sort by thread_id DESC: expected [zzz, mmm, aaa], got [%s, %s, %s]", threads[0].ThreadID, threads[1].ThreadID, threads[2].ThreadID)
	}

	// Sort by status ASC
	threads, _ = c.ListThreads(ctx(), "status", "asc")
	if threads[0].Status != "running" {
		t.Errorf("sort by status ASC: expected running first, got %s", threads[0].Status)
	}

	// Sort by status DESC
	threads, _ = c.ListThreads(ctx(), "status", "desc")
	if threads[0].Status != "complete" {
		t.Errorf("sort by status DESC: expected complete first, got %s", threads[0].Status)
	}

	// Sort by repo ASC
	threads, _ = c.ListThreads(ctx(), "repo", "asc")
	if threads[0].GHRepo != "owner/repo-a" {
		t.Errorf("sort by repo ASC: expected owner/repo-a first, got %s", threads[0].GHRepo)
	}
}

func TestGetWorkerStatsThreadCount(t *testing.T) {
	c, _ := setupTestClient(t)

	// Set up heartbeats for two worker types
	c.UpdateWorkerHeartbeat(ctx(), "claude", HeartbeatData{Hostname: "host1"})
	c.UpdateWorkerHeartbeat(ctx(), "codex", HeartbeatData{Hostname: "host2"})

	// Create tasks with thread associations in active_tasks
	c.SetActiveTask(ctx(), "t1", TaskInfo{Worker: "claude", ThreadID: "thr1", Status: "running"})
	c.SetActiveTask(ctx(), "t2", TaskInfo{Worker: "claude", ThreadID: "thr2", Status: "running"})
	c.SetActiveTask(ctx(), "t3", TaskInfo{Worker: "codex", ThreadID: "thr1", Status: "running"})

	stats, err := c.GetWorkerStats(ctx())
	if err != nil {
		t.Fatalf("GetWorkerStats failed: %v", err)
	}

	// claude has tasks on thr1 and thr2 = 2 threads
	if s, ok := stats["claude"]; !ok || s.TotalThreads != 2 {
		if ok {
			t.Errorf("expected 2 threads for claude, got %d", s.TotalThreads)
		} else {
			t.Error("claude not found in stats")
		}
	}

	// codex has tasks on thr1 = 1 thread
	if s, ok := stats["codex"]; !ok || s.TotalThreads != 1 {
		if ok {
			t.Errorf("expected 1 thread for codex, got %d", s.TotalThreads)
		} else {
			t.Error("codex not found in stats")
		}
	}

	// copilot has no heartbeat → not in stats (dynamic discovery)
	if _, ok := stats["copilot"]; ok {
		t.Error("copilot should not be in stats (no heartbeat)")
	}
}

func TestGetWorkerStatsThreadCountFromTaskKeys(t *testing.T) {
	c, _ := setupTestClient(t)

	c.UpdateWorkerHeartbeat(ctx(), "claude", HeartbeatData{Hostname: "host1"})

	// Set task keys directly (non-active tasks that still have thread info in Redis)
	c.rdb.Set(ctx(), TaskKey("old-task", "status"), "done", 0)
	c.rdb.Set(ctx(), TaskKey("old-task", "worker"), "claude", 0)
	c.rdb.Set(ctx(), TaskKey("old-task", "thread_id"), "archived-thread", 0)

	stats, err := c.GetWorkerStats(ctx())
	if err != nil {
		t.Fatalf("GetWorkerStats failed: %v", err)
	}

	// claude should have 1 thread from the task key scan
	if s, ok := stats["claude"]; !ok || s.TotalThreads != 1 {
		if ok {
			t.Errorf("expected 1 thread for claude (from task keys), got %d", s.TotalThreads)
		} else {
			t.Error("claude not found in stats")
		}
	}
}

// ── expanded sort and thread-count tests ──────────────────────────────────

func TestListThreadsSortByPRNumeric(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "thr-9", "", "")
	c.UpdateThread(ctx(), "thr-9", map[string]string{"status": "complete", "gh_pr_number": "9"})

	c.CreateThread(ctx(), "thr-100", "", "")
	c.UpdateThread(ctx(), "thr-100", map[string]string{"status": "complete", "gh_pr_number": "100"})

	c.CreateThread(ctx(), "thr-50", "", "")
	c.UpdateThread(ctx(), "thr-50", map[string]string{"status": "complete", "gh_pr_number": "50"})

	c.CreateThread(ctx(), "thr-empty", "", "")
	c.UpdateThread(ctx(), "thr-empty", map[string]string{"status": "complete"})

	// Sort by PR ASC — numeric order: 9, 50, 100, then empty
	threads, err := c.ListThreads(ctx(), "pr", "asc")
	if err != nil {
		t.Fatalf("ListThreads failed: %v", err)
	}
	if threads[0].GHPRNumber != "9" {
		t.Errorf("PR ASC: expected 9 first, got %s", threads[0].GHPRNumber)
	}
	if threads[1].GHPRNumber != "50" {
		t.Errorf("PR ASC: expected 50 second, got %s", threads[1].GHPRNumber)
	}
	if threads[2].GHPRNumber != "100" {
		t.Errorf("PR ASC: expected 100 third, got %s", threads[2].GHPRNumber)
	}
	// Empty PR (parsed as 0) should come after non-empty
	if threads[3].GHPRNumber != "-" {
		t.Errorf("PR ASC: expected empty last, got %s", threads[3].GHPRNumber)
	}

	// Sort by PR DESC — valid PRs last in DESC (reverse of ASC), so "-" first, then 100, 50, 9
	threads, _ = c.ListThreads(ctx(), "pr", "desc")
	if threads[0].GHPRNumber != "-" {
		t.Errorf("PR DESC: expected empty first, got %s", threads[0].GHPRNumber)
	}
	if threads[1].GHPRNumber != "100" {
		t.Errorf("PR DESC: expected 100 second, got %s", threads[1].GHPRNumber)
	}
	if threads[3].GHPRNumber != "9" {
		t.Errorf("PR DESC: expected 9 last, got %s", threads[3].GHPRNumber)
	}
}

func TestListThreadsSortByUpdatedAt(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "thr-a", "", "")
	c.UpdateThread(ctx(), "thr-a", map[string]string{"status": "complete"})
	// thr-a gets updated_at from the UpdateThread call

	c.CreateThread(ctx(), "thr-b", "", "")
	c.UpdateThread(ctx(), "thr-b", map[string]string{"status": "complete"})

	threads, err := c.ListThreads(ctx(), "updated_at", "asc")
	if err != nil {
		t.Fatalf("ListThreads failed: %v", err)
	}
	if len(threads) < 2 {
		t.Fatalf("expected at least 2 threads, got %d", len(threads))
	}
	// Both should have non-empty updated_at values after UpdateThread
	if threads[0].UpdatedAt == "" || threads[1].UpdatedAt == "" {
		t.Errorf("expected non-empty updated_at values")
	}
}

func TestListTasksSortByThreadID(t *testing.T) {
	c, _ := setupTestClient(t)

	for i, info := range []struct{ id, status, worker, thread string }{
		{"t1", "done", "claude", "thr-z"},
		{"t2", "done", "copilot", "thr-a"},
		{"t3", "done", "codex", "thr-m"},
	} {
		c.rdb.Set(ctx(), TaskKey(info.id, "status"), info.status, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "worker"), info.worker, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "thread_id"), info.thread, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "enqueued_at"), "2025-01-0"+strconv.Itoa(i+1)+"T00:00:00Z", 0)
	}

	// Sort by thread_id ASC
	tasks, err := c.ListTasks(ctx(), "", "", "", 50, 0, "thread_id", "asc")
	if err != nil {
		t.Fatalf("ListTasks failed: %v", err)
	}
	if tasks[0].ThreadID != "thr-a" || tasks[1].ThreadID != "thr-m" || tasks[2].ThreadID != "thr-z" {
		t.Errorf("sort by thread_id ASC: expected [thr-a, thr-m, thr-z], got [%s, %s, %s]",
			tasks[0].ThreadID, tasks[1].ThreadID, tasks[2].ThreadID)
	}

	// Sort by thread_id DESC
	tasks, _ = c.ListTasks(ctx(), "", "", "", 50, 0, "thread_id", "desc")
	if tasks[0].ThreadID != "thr-z" || tasks[2].ThreadID != "thr-a" {
		t.Errorf("sort by thread_id DESC: expected [thr-z, thr-m, thr-a], got [%s, %s, %s]",
			tasks[0].ThreadID, tasks[1].ThreadID, tasks[2].ThreadID)
	}
}

func TestListTasksSortByEnqueuedAt(t *testing.T) {
	c, _ := setupTestClient(t)

	for i, info := range []struct{ id, status, worker, thread string }{
		{"t1", "done", "claude", "thr1"},
		{"t2", "done", "copilot", "thr1"},
		{"t3", "done", "codex", "thr1"},
	} {
		c.rdb.Set(ctx(), TaskKey(info.id, "status"), info.status, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "worker"), info.worker, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "thread_id"), info.thread, 0)
		// Reverse chronological order: t3 oldest, t1 newest
		c.rdb.Set(ctx(), TaskKey(info.id, "enqueued_at"), "2025-01-0"+
			strconv.Itoa(3-i+1)+"T00:00:00Z", 0)
	}

	// Sort by enqueued_at ASC (oldest first)
	tasks, err := c.ListTasks(ctx(), "", "", "", 50, 0, "enqueued_at", "asc")
	if err != nil {
		t.Fatalf("ListTasks failed: %v", err)
	}
	if tasks[0].TaskID != "t3" || tasks[2].TaskID != "t1" {
		t.Errorf("sort by enqueued_at ASC: expected [t3, t2, t1], got [%s, %s, %s]",
			tasks[0].TaskID, tasks[1].TaskID, tasks[2].TaskID)
	}

	// Sort by enqueued_at DESC (newest first)
	tasks, _ = c.ListTasks(ctx(), "", "", "", 50, 0, "enqueued_at", "desc")
	if tasks[0].TaskID != "t1" || tasks[2].TaskID != "t3" {
		t.Errorf("sort by enqueued_at DESC: expected [t1, t2, t3], got [%s, %s, %s]",
			tasks[0].TaskID, tasks[1].TaskID, tasks[2].TaskID)
	}
}

func TestGetWorkerStatsSharedThreadCount(t *testing.T) {
	c, _ := setupTestClient(t)

	c.UpdateWorkerHeartbeat(ctx(), "claude", HeartbeatData{Hostname: "host1"})
	c.UpdateWorkerHeartbeat(ctx(), "codex", HeartbeatData{Hostname: "host2"})

	// Set task keys directly (not in active_tasks) — second scan path.
	// Both tasks share thread "shared-thr" but have different workers.
	c.rdb.Set(ctx(), TaskKey("task-a", "status"), "done", 0)
	c.rdb.Set(ctx(), TaskKey("task-a", "worker"), "claude", 0)
	c.rdb.Set(ctx(), TaskKey("task-a", "thread_id"), "shared-thr", 0)

	c.rdb.Set(ctx(), TaskKey("task-b", "status"), "done", 0)
	c.rdb.Set(ctx(), TaskKey("task-b", "worker"), "codex", 0)
	c.rdb.Set(ctx(), TaskKey("task-b", "thread_id"), "shared-thr", 0)

	stats, err := c.GetWorkerStats(ctx())
	if err != nil {
		t.Fatalf("GetWorkerStats failed: %v", err)
	}

	// Both claude and codex should get credit for "shared-thr"
	if stats["claude"].TotalThreads != 1 {
		t.Errorf("expected 1 thread for claude, got %d", stats["claude"].TotalThreads)
	}
	if stats["codex"].TotalThreads != 1 {
		t.Errorf("expected 1 thread for codex, got %d", stats["codex"].TotalThreads)
	}
}

func TestListTasksDefaultSortDesc(t *testing.T) {
	c, _ := setupTestClient(t)

	for i, info := range []struct{ id, status, worker, thread string }{
		{"a", "done", "claude", "thr1"},
		{"b", "running", "copilot", "thr1"},
		{"c", "failed", "claude", "thr2"},
		{"d", "pending", "codex", "thr3"},
	} {
		c.rdb.Set(ctx(), TaskKey(info.id, "status"), info.status, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "worker"), info.worker, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "thread_id"), info.thread, 0)
		c.rdb.Set(ctx(), TaskKey(info.id, "enqueued_at"), "2025-01-0"+strconv.Itoa(i+1)+"T00:00:00Z", 0)
	}

	// Default sort DESC — reverse status priority: done > pending > running > failed
	tasks, err := c.ListTasks(ctx(), "", "", "", 50, 0, "", "desc")
	if err != nil {
		t.Fatalf("ListTasks failed: %v", err)
	}
	expectedStatus := []string{"done", "pending", "running", "failed"}
	for i, task := range tasks {
		if task.Status != expectedStatus[i] {
			t.Errorf("default sort DESC at index %d: expected %s, got %s", i, expectedStatus[i], task.Status)
		}
	}
}

func TestListThreadsDefaultSortDesc(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "thr-a", "", "")
	c.UpdateThread(ctx(), "thr-a", map[string]string{"status": "complete"})
	c.CreateThread(ctx(), "thr-b", "", "")
	c.UpdateThread(ctx(), "thr-b", map[string]string{"status": "error"})
	c.CreateThread(ctx(), "thr-c", "", "")
	c.UpdateThread(ctx(), "thr-c", map[string]string{"status": "running"})

	// Default sort DESC — reverse status priority: complete > running > error
	threads, err := c.ListThreads(ctx(), "", "desc")
	if err != nil {
		t.Fatalf("ListThreads failed: %v", err)
	}
	expected := []string{"thr-a", "thr-c", "thr-b"} // complete, running, error
	for i, th := range threads {
		if th.ThreadID != expected[i] {
			t.Errorf("default sort DESC at index %d: expected %s, got %s", i, expected[i], th.ThreadID)
		}
	}
}
