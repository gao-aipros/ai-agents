package tasklib

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

func ctxbg() context.Context { return context.Background() }

// ── stats counter tests ────────────────────────────────────────────────────

func TestStatsCounterOnEnqueue(t *testing.T) {
	c, _ := setupTestClient(t)

	// Enqueue a task → stats:task_total should be incremented
	task, err := c.Enqueue(ctxbg(), "claude", "thr-stats", "count me")
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}
	_ = task

	total, _ := c.rdb.Get(ctxbg(), "stats:task_total").Result()
	if n, _ := strconv.Atoi(total); n < 1 {
		t.Errorf("stats:task_total should be >= 1, got %s", total)
	}

	// Verify TTL is set on the counter key
	ttl := c.rdb.TTL(ctxbg(), "stats:task_total").Val()
	if ttl <= 0 {
		t.Errorf("stats:task_total should have TTL, got %v", ttl)
	}
}

func TestStatsCounterOnEnqueueGroup(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctxbg(), "thr-grp", "")
	task, err := c.EnqueueGroup(ctxbg(), "claude", "thr-grp", "review", "group task")
	if err != nil {
		t.Fatalf("EnqueueGroup failed: %v", err)
	}
	_ = task

	total, _ := c.rdb.Get(ctxbg(), "stats:task_total").Result()
	if n, _ := strconv.Atoi(total); n < 1 {
		t.Errorf("stats:task_total should be >= 1 after EnqueueGroup, got %s", total)
	}
}

func TestCancelTaskSetsFlagAndCancelledBy(t *testing.T) {
	c, _ := setupTestClient(t)

	c.rdb.Set(ctxbg(), TaskKey("ct-stats", "status"), "pending", 0)

	err := c.CancelTask(ctxbg(), "ct-stats", "user")
	if err != nil {
		t.Fatalf("CancelTask failed: %v", err)
	}

	// Cancel flag should be set
	flag, _ := c.rdb.Get(ctxbg(), TaskKey("ct-stats", "cancel")).Result()
	if flag != "1" {
		t.Errorf("expected cancel flag '1', got '%s'", flag)
	}

	// Verify cancelled_by was set
	who, _ := c.rdb.Get(ctxbg(), TaskKey("ct-stats", "cancelled_by")).Result()
	if who != "user" {
		t.Errorf("expected cancelled_by 'user', got '%s'", who)
	}

	// Counter is owned by the worker cancel path — CancelTask only sets the flag
}

// ── Task key tests ─────────────────────────────────────────────────────────

func TestGetTaskNewFields(t *testing.T) {
	c, _ := setupTestClient(t)

	taskID := "full-fields"
	c.rdb.Set(ctxbg(), TaskKey(taskID, "status"), "done", 0)
	c.rdb.Set(ctxbg(), TaskKey(taskID, "worker"), "claude", 0)
	c.rdb.Set(ctxbg(), TaskKey(taskID, "thread_id"), "thr1", 0)
	c.rdb.Set(ctxbg(), TaskKey(taskID, "enqueued_at"), "2025-06-01T10:00:00Z", 0)
	c.rdb.Set(ctxbg(), TaskKey(taskID, "started_at"), "2025-06-01T10:00:05Z", 0)
	c.rdb.Set(ctxbg(), TaskKey(taskID, "last_started_at"), "2025-06-01T10:00:05Z", 0)
	c.rdb.Set(ctxbg(), TaskKey(taskID, "completed_at"), "2025-06-01T10:05:00Z", 0)
	c.rdb.Set(ctxbg(), TaskKey(taskID, "worker_hostname"), "claude-abc123", 0)
	c.rdb.Set(ctxbg(), TaskKey(taskID, "retry_count"), "2", 0)
	c.rdb.Set(ctxbg(), TaskKey(taskID, "correlation_id"), "corr-xyz", 0)
	c.rdb.Set(ctxbg(), TaskKey(taskID, "cancelled_by"), "", 0)
	c.rdb.Set(ctxbg(), TaskKey(taskID, "error_message"), "", 0)

	task, err := c.GetTask(ctxbg(), taskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}

	if task.EnqueuedAt != "2025-06-01T10:00:00Z" {
		t.Errorf("EnqueuedAt: got '%s'", task.EnqueuedAt)
	}
	if task.StartedAt != "2025-06-01T10:00:05Z" {
		t.Errorf("StartedAt: got '%s'", task.StartedAt)
	}
	if task.LastStartedAt != "2025-06-01T10:00:05Z" {
		t.Errorf("LastStartedAt: got '%s'", task.LastStartedAt)
	}
	if task.WorkerHostname != "claude-abc123" {
		t.Errorf("WorkerHostname: got '%s'", task.WorkerHostname)
	}
	if task.RetryCount != "2" {
		t.Errorf("RetryCount: got '%s'", task.RetryCount)
	}
	if task.CorrelationID != "corr-xyz" {
		t.Errorf("CorrelationID: got '%s'", task.CorrelationID)
	}
}

func TestEnqueueCreatesEnqueuedAt(t *testing.T) {
	c, _ := setupTestClient(t)

	task, err := c.Enqueue(ctxbg(), "claude", "thr-ea", "timing test")
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}

	// enqueued_at should be set on the Redis key
	val, _ := c.rdb.Get(ctxbg(), TaskKey(task.TaskID, "enqueued_at")).Result()
	if val == "" {
		t.Error("enqueued_at key should be set")
	}

	// created_at should NOT exist (renamed to enqueued_at)
	oldVal, _ := c.rdb.Get(ctxbg(), TaskKey(task.TaskID, "created_at")).Result()
	if oldVal != "" {
		t.Error("created_at key should not exist (renamed to enqueued_at)")
	}

	// Verify EnqueuedAt is populated on the returned struct
	if task.EnqueuedAt == "" {
		t.Error("task.EnqueuedAt should be populated")
	}
}

func TestCancelTaskSetsCancelledBy(t *testing.T) {
	c, _ := setupTestClient(t)

	taskID := "cancel-audit"
	c.rdb.Set(ctxbg(), TaskKey(taskID, "status"), "pending", 0)

	for _, who := range []string{"user", "timeout", "system"} {
		t.Run(who, func(t *testing.T) {
			taskID := "cancel-" + who
			c.rdb.Set(ctxbg(), TaskKey(taskID, "status"), "pending", 0)

			err := c.CancelTask(ctxbg(), taskID, who)
			if err != nil {
				t.Fatalf("CancelTask(%s) failed: %v", who, err)
			}

			// Verify cancelled_by
			got, _ := c.rdb.Get(ctxbg(), TaskKey(taskID, "cancelled_by")).Result()
			if got != who {
				t.Errorf("cancelled_by: expected '%s', got '%s'", who, got)
			}
		})
	}
}

// ── heartbeat tests ────────────────────────────────────────────────────────

func TestHeartbeatJSONRoundTrip(t *testing.T) {
	c, _ := setupTestClient(t)

	hb := HeartbeatData{
		Hostname:       "worker-claude-abc123",
		TasksProcessed: 42,
		QueueDepth:     3,
		UptimeSeconds:  3600,
	}

	err := c.UpdateWorkerHeartbeat(ctxbg(), "claude", "host1", hb)
	if err != nil {
		t.Fatalf("UpdateWorkerHeartbeat failed: %v", err)
	}

	// Read back and parse
	raw, _ := c.rdb.Get(ctxbg(), HeartbeatKey("claude", "host1")).Result()
	var parsed HeartbeatData
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("heartbeat JSON parse failed: %v (raw=%s)", err, raw)
	}

	if parsed.Hostname != "worker-claude-abc123" {
		t.Errorf("Hostname: got '%s'", parsed.Hostname)
	}
	if parsed.TasksProcessed != 42 {
		t.Errorf("TasksProcessed: got %d", parsed.TasksProcessed)
	}
	if parsed.QueueDepth != 3 {
		t.Errorf("QueueDepth: got %d", parsed.QueueDepth)
	}
	if parsed.UptimeSeconds != 3600 {
		t.Errorf("UptimeSeconds: got %d", parsed.UptimeSeconds)
	}

	// Verify TTL
	ttl := c.rdb.TTL(ctxbg(), HeartbeatKey("claude", "host1")).Val()
	if ttl <= 0 || ttl > 30*time.Second {
		t.Errorf("expected TTL between 0 and 30s, got %v", ttl)
	}
}

func TestGetWorkerInstances(t *testing.T) {
	c, _ := setupTestClient(t)

	c.UpdateWorkerHeartbeat(ctxbg(), "claude", "host-a", HeartbeatData{
		Hostname: "host-a", TasksProcessed: 10, QueueDepth: 2, UptimeSeconds: 100,
	})
	c.UpdateWorkerHeartbeat(ctxbg(), "claude", "host-b", HeartbeatData{
		Hostname: "host-b", TasksProcessed: 20, QueueDepth: 0, UptimeSeconds: 200,
	})

	instances, err := c.GetWorkerInstances(ctxbg(), "claude")
	if err != nil {
		t.Fatalf("GetWorkerInstances failed: %v", err)
	}
	if len(instances) < 2 {
		t.Errorf("expected >= 2 instances, got %d", len(instances))
	}

	// Both should be online (fresh heartbeat TTL)
	for _, inst := range instances {
		if !inst.Online {
			t.Errorf("expected %s to be online", inst.Hostname)
		}
	}
}

func TestGetWorkerInstancesBackwardCompat(t *testing.T) {
	c, _ := setupTestClient(t)

	// Old format: literal "1" as heartbeat value
	c.rdb.SetEx(ctxbg(), HeartbeatKey("claude", "old-host"), "1", 30*time.Second)

	instances, err := c.GetWorkerInstances(ctxbg(), "claude")
	if err != nil {
		t.Fatalf("GetWorkerInstances failed: %v", err)
	}

	found := false
	for _, inst := range instances {
		if inst.Hostname == "old-host" {
			found = true
			if inst.TasksProcessed != 0 {
				t.Errorf("old-format heartbeat should default TasksProcessed=0, got %d", inst.TasksProcessed)
			}
		}
	}
	if !found {
		t.Error("old-format heartbeat host not found in instances")
	}
}

// ── RequeueStale tests ─────────────────────────────────────────────────────

func TestRequeueStaleIncrementsRetryCount(t *testing.T) {
	c, _ := setupTestClient(t)

	// Task that is "running" with an old last_started_at
	payload := `{"task_id":"retry-1","thread_id":"thr1","instruction":"do X"}`
	c.rdb.LPush(ctxbg(), ProcessingKey("claude"), payload)
	c.rdb.Set(ctxbg(), TaskKey("retry-1", "status"), "running", 0)
	c.rdb.Set(ctxbg(), TaskKey("retry-1", "last_started_at"), "2020-01-01T00:00:00Z", 0)

	requeued, err := c.RequeueStale(ctxbg(), "claude", 10*time.Minute)
	if err != nil {
		t.Fatalf("RequeueStale failed: %v", err)
	}
	if len(requeued) != 1 {
		t.Fatalf("expected 1 requeued task, got %v", requeued)
	}

	count, _ := c.rdb.Get(ctxbg(), TaskKey("retry-1", "retry_count")).Result()
	if n, _ := strconv.Atoi(count); n < 1 {
		t.Errorf("retry_count should be incremented, got '%s'", count)
	}

	// Verify retry_count has a TTL (not an orphaned key)
	ttl := c.rdb.TTL(ctxbg(), TaskKey("retry-1", "retry_count")).Val()
	if ttl <= 0 {
		t.Errorf("retry_count should have TTL > 0, got %v", ttl)
	}
}

// ── correlation_id tests ───────────────────────────────────────────────────

func TestCreateThreadGeneratesCorrelationID(t *testing.T) {
	c, _ := setupTestClient(t)

	th, err := c.CreateThread(ctxbg(), "thr-corr", "owner/repo")
	if err != nil {
		t.Fatalf("CreateThread failed: %v", err)
	}

	if th.CorrelationID == "" {
		t.Error("expected non-empty correlation_id on thread")
	}

	// Verify stored in Redis hash
	raw, _ := c.rdb.HGet(ctxbg(), ThreadStateKey("thr-corr"), "correlation_id").Result()
	if raw != th.CorrelationID {
		t.Errorf("correlation_id mismatch: struct=%s, redis=%s", th.CorrelationID, raw)
	}
}

func TestGetThreadReadsCorrelationID(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctxbg(), "thr-read-corr", "")
	th, err := c.GetThread(ctxbg(), "thr-read-corr")
	if err != nil {
		t.Fatalf("GetThread failed: %v", err)
	}

	if th.CorrelationID == "" {
		t.Error("expected non-empty correlation_id from GetThread")
	}
}

// ── DeleteThread / SetThreadTTL completeness tests ─────────────────────────

func TestDeleteThreadRemovesAllKeys(t *testing.T) {
	c, _ := setupTestClient(t)

	// Create thread and add extra keys
	c.CreateThread(ctxbg(), "thr-del", "")
	c.rdb.Set(ctxbg(), ThreadEventsKey("thr-del"), "[]", 0)
	c.rdb.Set(ctxbg(), ThreadLockedAtKey("thr-del"), "2025-01-01T00:00:00Z", 0)

	err := c.DeleteThread(ctxbg(), "thr-del")
	if err != nil {
		t.Fatalf("DeleteThread failed: %v", err)
	}

	// Verify all known keys are gone
	keysToCheck := []string{
		ThreadStateKey("thr-del"),
		ThreadMessagesKey("thr-del"),
		ThreadEventsKey("thr-del"),
		ThreadLockedAtKey("thr-del"),
	}
	for _, key := range keysToCheck {
		exists, _ := c.rdb.Exists(ctxbg(), key).Result()
		if exists > 0 {
			t.Errorf("key %s should be deleted", key)
		}
	}
}

func TestSetThreadTTLCoversNewKeys(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctxbg(), "thr-ttl", "")
	c.rdb.Set(ctxbg(), ThreadEventsKey("thr-ttl"), "[]", 0)
	c.rdb.Set(ctxbg(), ThreadLockedAtKey("thr-ttl"), "2025-01-01T00:00:00Z", 0)

	err := c.SetThreadTTL(ctxbg(), "thr-ttl", 3600*time.Second)
	if err != nil {
		t.Fatalf("SetThreadTTL failed: %v", err)
	}

	// Events key should now have TTL
	ttl := c.rdb.TTL(ctxbg(), ThreadEventsKey("thr-ttl")).Val()
	if ttl <= 0 {
		t.Errorf("ThreadEventsKey should have TTL after SetThreadTTL, got %v", ttl)
	}

	// ThreadLockedAtKey should also have TTL
	ttl2 := c.rdb.TTL(ctxbg(), ThreadLockedAtKey("thr-ttl")).Val()
	if ttl2 <= 0 {
		t.Errorf("ThreadLockedAtKey should have TTL after SetThreadTTL, got %v", ttl2)
	}
}

// ── started_at SETNX preservation test ─────────────────────────────────────

func TestStartedAtPreservedAcrossRetries(t *testing.T) {
	c, _ := setupTestClient(t)

	taskID := "started-at-nx"
	firstStart := "2025-06-01T10:00:00Z"
	secondStart := "2025-06-01T11:00:00Z"

	// Simulate first dequeue: SETNX should succeed
	c.rdb.SetNX(ctxbg(), TaskKey(taskID, "started_at"), firstStart, 0)
	c.rdb.Expire(ctxbg(), TaskKey(taskID, "started_at"), TTLTask)

	// Simulate retry (second dequeue): SETNX should fail, preserving firstStart
	ok, _ := c.rdb.SetNX(ctxbg(), TaskKey(taskID, "started_at"), secondStart, 0).Result()
	if ok {
		t.Error("SETNX on retry should fail (key already exists from first dequeue)")
	}

	// started_at should still be the first start time
	val, _ := c.rdb.Get(ctxbg(), TaskKey(taskID, "started_at")).Result()
	if val != firstStart {
		t.Errorf("started_at should be preserved on retry: expected %s, got %s", firstStart, val)
	}

	// last_started_at should be updated on every dequeue (simulated by SET)
	c.rdb.Set(ctxbg(), TaskKey(taskID, "last_started_at"), secondStart, 0)
	lastVal, _ := c.rdb.Get(ctxbg(), TaskKey(taskID, "last_started_at")).Result()
	if lastVal != secondStart {
		t.Errorf("last_started_at should be updated on retry: expected %s, got %s", secondStart, lastVal)
	}
}

// ── error_message on failure test ──────────────────────────────────────────

func TestErrorMessageSetOnFailure(t *testing.T) {
	c, _ := setupTestClient(t)

	taskID := "err-msg-test"

	// Simulate what the worker does on failure: set error_message key
	c.rdb.Set(ctxbg(), TaskKey(taskID, "error_message"), "fatal: out of memory", TTLTask)

	// Read via GetTask
	task, err := c.GetTask(ctxbg(), taskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}

	if task.ErrorMessage != "fatal: out of memory" {
		t.Errorf("expected error_message 'fatal: out of memory', got '%s'", task.ErrorMessage)
	}

	// Verify TTL is set
	ttl := c.rdb.TTL(ctxbg(), TaskKey(taskID, "error_message")).Val()
	if ttl <= 0 {
		t.Errorf("error_message should have TTL > 0, got %v", ttl)
	}
}

// ── cancelled_by completeness test ─────────────────────────────────────────

func TestCancelTaskAllCancelledByValues(t *testing.T) {
	c, _ := setupTestClient(t)

	for _, who := range []string{"user", "timeout", "system"} {
		t.Run(who, func(t *testing.T) {
			taskID := "cancel-who-" + who
			c.rdb.Set(ctxbg(), TaskKey(taskID, "status"), "pending", 0)

			err := c.CancelTask(ctxbg(), taskID, who)
			if err != nil {
				t.Fatalf("CancelTask(%s) failed: %v", who, err)
			}

			got, _ := c.rdb.Get(ctxbg(), TaskKey(taskID, "cancelled_by")).Result()
			if got != who {
				t.Errorf("cancelled_by: expected '%s', got '%s'", who, got)
			}

			// Verify cancelled_by has TTL
			ttl := c.rdb.TTL(ctxbg(), TaskKey(taskID, "cancelled_by")).Val()
			if ttl <= 0 {
				t.Errorf("cancelled_by should have TTL > 0 for %s, got %v", who, ttl)
			}
		})
	}
}
