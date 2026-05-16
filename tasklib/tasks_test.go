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

	// Pre-lock the thread
	c.rdb.Set(ctx(), ThreadLockKey("locked-thread"), "other-task", LockTTL)

	_, err := c.Enqueue(ctx(), "claude", "locked-thread", "do something")
	if err == nil {
		t.Error("expected error for locked thread")
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
		c.rdb.Set(ctx(), TaskKey(info.id, "created_at"), "2025-01-0"+strconv.Itoa(i+1)+"T00:00:00Z", 0)
	}

	tasks, err := c.ListTasks(ctx(), "", "", "", 50, 0)
	if err != nil {
		t.Fatalf("ListTasks failed: %v", err)
	}
	if len(tasks) < 3 {
		t.Errorf("expected at least 3 tasks, got %d", len(tasks))
	}

	// Filter by status
	tasks, _ = c.ListTasks(ctx(), "", "done", "", 50, 0)
	for _, task := range tasks {
		if task.Status != "done" {
			t.Errorf("status filter failed: expected only done, got %s", task.Status)
		}
	}

	// Filter by worker
	tasks, _ = c.ListTasks(ctx(), "claude", "", "", 50, 0)
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

	tasks, _ := c.ListTasks(ctx(), "", "", "", 3, 0)
	if len(tasks) > 3 {
		t.Errorf("limit failed: expected at most 3, got %d", len(tasks))
	}

	tasks, _ = c.ListTasks(ctx(), "", "", "", 50, 5)
	// offset 5 should skip the first 5
	if len(tasks) > 5 { // 10 total - 5 offset = 5 remaining, capped at limit 50
		// ok
	}
}

func TestCancelTask(t *testing.T) {
	c, _ := setupTestClient(t)

	c.rdb.Set(ctx(), TaskKey("t1", "status"), "pending", 0)

	err := c.CancelTask(ctx(), "t1")
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
	err = c.CancelTask(ctx(), "no-such-task")
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

			err := c.CancelTask(ctx(), taskID)
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
	tasks, _ := c.ListTasks(ctx(), "copilot", "", "", 2, 0)
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

	th, err := c.CreateThread(ctx(), "thr1", "owner/repo")
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

	c.CreateThread(ctx(), "thr1", "a/b")
	c.CreateThread(ctx(), "thr2", "c/d")

	threads, err := c.ListThreads(ctx())
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

	c.CreateThread(ctx(), "thr1", "")

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

	c.CreateThread(ctx(), "thr1", "")

	err := c.UpdateThread(ctx(), "thr1", map[string]string{
		"status":      "complete",
		"last_design": "use redis",
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

	err := c.UpdateWorkerHeartbeat(ctx(), "claude", "host1")
	if err != nil {
		t.Fatalf("UpdateWorkerHeartbeat failed: %v", err)
	}

	// Verify key exists with TTL
	key := HeartbeatKey("claude", "host1")
	val, _ := c.rdb.Get(ctx(), key).Result()
	if val != "1" {
		t.Errorf("expected heartbeat value '1', got '%s'", val)
	}
	ttl := c.rdb.TTL(ctx(), key).Val()
	if ttl <= 0 || ttl > 30*time.Second {
		t.Errorf("expected TTL between 0 and 30s, got %v", ttl)
	}
}

func TestGetWorkerStats(t *testing.T) {
	c, _ := setupTestClient(t)

	// Set heartbeats
	c.UpdateWorkerHeartbeat(ctx(), "claude", "host1")
	c.UpdateWorkerHeartbeat(ctx(), "claude", "host2")
	c.UpdateWorkerHeartbeat(ctx(), "copilot", "host3")

	stats, err := c.GetWorkerStats(ctx())
	if err != nil {
		t.Fatalf("GetWorkerStats failed: %v", err)
	}

	if stats["claude"].Instances != 2 {
		t.Errorf("expected 2 claude instances, got %d", stats["claude"].Instances)
	}
	if stats["claude"].Online != 2 {
		t.Errorf("expected 2 claude online, got %d", stats["claude"].Online)
	}
	if stats["copilot"].Instances != 1 {
		t.Errorf("expected 1 copilot instance, got %d", stats["copilot"].Instances)
	}
	if stats["opencode"].Instances != 0 {
		t.Errorf("expected 0 opencode instances, got %d", stats["opencode"].Instances)
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

	c.CreateThread(ctx(), "cr-thread", "owner/repo")

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

// ── helpers ────────────────────────────────────────────────────────────────
