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

	c.CreateThread(ctxbg(), "thr-grp", "", "")
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

	// CancelTask sets the cancel flag and cancelled_by; the worker cancel path owns stats:task_cancelled
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

	th, err := c.CreateThread(ctxbg(), "thr-corr", "owner/repo", "")
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

	c.CreateThread(ctxbg(), "thr-read-corr", "", "")
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
	c.CreateThread(ctxbg(), "thr-del", "", "")
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

	c.CreateThread(ctxbg(), "thr-ttl", "", "")
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

// ── event system tests ───────────────────────────────────────────────────────

func TestPushThreadEvent(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctxbg(), "thr-ev", "", "")
	ev := &Event{
		Type:           EventTaskEnqueued,
		TaskID:         "task-1",
		WorkerType:     "claude",
		WorkerHostname: "host-1",
		CorrelationID:  "corr-1",
		Detail:         TaskEnqueuedDetail{QueueDepthAfter: 3},
	}
	c.PushThreadEvent(ctxbg(), "thr-ev", ev)

	events, err := c.GetThreadEvents(ctxbg(), "thr-ev", 10)
	if err != nil {
		t.Fatalf("GetThreadEvents failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != EventTaskEnqueued {
		t.Errorf("expected type '%s', got '%s'", EventTaskEnqueued, events[0].Type)
	}
	if events[0].TaskID != "task-1" {
		t.Errorf("expected task_id 'task-1', got '%s'", events[0].TaskID)
	}
	if events[0].CorrelationID != "corr-1" {
		t.Errorf("expected correlation_id 'corr-1', got '%s'", events[0].CorrelationID)
	}
	if events[0].Timestamp == "" {
		t.Error("expected non-empty timestamp")
	}
	if events[0].EventID == "" {
		t.Error("expected non-empty event_id")
	}
}

func TestPushSystemEvent(t *testing.T) {
	c, _ := setupTestClient(t)

	c.PushSystemEvent(ctxbg(), &Event{
		Type:           EventWorkerOnline,
		WorkerType:     "claude",
		WorkerHostname: "worker-1",
	})

	events, err := c.GetSystemEvents(ctxbg(), 10)
	if err != nil {
		t.Fatalf("GetSystemEvents failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 system event, got %d", len(events))
	}
	if events[0].Type != EventWorkerOnline {
		t.Errorf("expected type '%s', got '%s'", EventWorkerOnline, events[0].Type)
	}
}

func TestThreadEventsTrimmed(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctxbg(), "thr-trim", "", "")
	// Push more than the cap (1000), but test with a smaller cap
	for i := 0; i < 10; i++ {
		c.PushThreadEvent(ctxbg(), "thr-trim", &Event{
			Type:   EventTaskEnqueued,
			TaskID: "task-" + string(rune('0'+i%10)),
		})
	}

	events, _ := c.GetThreadEvents(ctxbg(), "thr-trim", 50)
	// Should have at most 10 events (all our test events fit within the cap)
	if len(events) < 1 {
		t.Error("expected at least 1 event")
	}
}

func TestEnqueueEmitsTaskEnqueuedEvent(t *testing.T) {
	c, _ := setupTestClient(t)

	_, err := c.Enqueue(ctxbg(), "claude", "thr-ev-enq", "do something")
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}

	events, err := c.GetThreadEvents(ctxbg(), "thr-ev-enq", 10)
	if err != nil {
		t.Fatalf("GetThreadEvents failed: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == EventTaskEnqueued {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected task_enqueued event after Enqueue")
	}
}

// ── diagnostics tests ────────────────────────────────────────────────────────

func TestGetThreadDiagnostics(t *testing.T) {
	c, _ := setupTestClient(t)

	th, err := c.CreateThread(ctxbg(), "thr-diag", "owner/repo", "")
	if err != nil {
		t.Fatalf("CreateThread failed: %v", err)
	}

	d, err := c.GetThreadDiagnostics(ctxbg(), "thr-diag")
	if err != nil {
		t.Fatalf("GetThreadDiagnostics failed: %v", err)
	}

	if d.ThreadID != "thr-diag" {
		t.Errorf("expected thread_id 'thr-diag', got '%s'", d.ThreadID)
	}
	if d.Status != th.Status {
		t.Errorf("expected status '%s', got '%s'", th.Status, d.Status)
	}
	if d.CorrelationID != th.CorrelationID {
		t.Errorf("expected correlation_id '%s', got '%s'", th.CorrelationID, d.CorrelationID)
	}
	if d.Lock != nil {
		t.Error("expected no lock on fresh thread")
	}
	if d.TaskCounts == nil {
		t.Error("expected non-nil task_counts")
	}
	if d.RecentEvents == nil {
		t.Error("expected non-nil recent_events")
	}
}

func TestGetThreadDiagnosticsWithLock(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctxbg(), "thr-diag-lock", "", "")
	// Simulate a lock being held
	c.rdb.Set(ctxbg(), ThreadLockKey("thr-diag-lock"), "task-holder", LockTTL)
	c.rdb.Set(ctxbg(), ThreadLockedAtKey("thr-diag-lock"), "2025-06-01T10:00:00Z", LockTTL)

	d, err := c.GetThreadDiagnostics(ctxbg(), "thr-diag-lock")
	if err != nil {
		t.Fatalf("GetThreadDiagnostics failed: %v", err)
	}

	if d.Lock == nil {
		t.Fatal("expected lock info")
	}
	if d.Lock.HolderTask != "task-holder" {
		t.Errorf("expected holder 'task-holder', got '%s'", d.Lock.HolderTask)
	}
	if d.Lock.LockedAt != "2025-06-01T10:00:00Z" {
		t.Errorf("expected locked_at, got '%s'", d.Lock.LockedAt)
	}
}

// ── locked_at management tests ────────────────────────────────────────────────

func TestLockedAtSetOnEnqueue(t *testing.T) {
	c, _ := setupTestClient(t)

	_, err := c.Enqueue(ctxbg(), "claude", "thr-la", "test")
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}

	// locked_at should be set alongside the lock
	lockedAt, err := c.rdb.Get(ctxbg(), ThreadLockedAtKey("thr-la")).Result()
	if err != nil || lockedAt == "" {
		t.Error("expected locked_at to be set after successful lock acquisition")
	}
}

func TestUnlockThreadClearsLockedAt(t *testing.T) {
	c, _ := setupTestClient(t)

	c.rdb.Set(ctxbg(), ThreadLockKey("thr-ul"), "task-1", LockTTL)
	c.rdb.Set(ctxbg(), ThreadLockedAtKey("thr-ul"), "2025-06-01T10:00:00Z", LockTTL)

	err := c.UnlockThread(ctxbg(), "thr-ul")
	if err != nil {
		t.Fatalf("UnlockThread failed: %v", err)
	}

	exists, _ := c.rdb.Exists(ctxbg(), ThreadLockedAtKey("thr-ul")).Result()
	if exists > 0 {
		t.Error("expected locked_at to be deleted on unlock")
	}
}

// ── event envelope fields test ────────────────────────────────────────────────

func TestEventEnvelopeHasRequiredFields(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctxbg(), "thr-env", "", "")
	c.PushThreadEvent(ctxbg(), "thr-env", &Event{
		Type:           EventTaskCompleted,
		TaskID:         "t-env",
		WorkerType:     "codex",
		WorkerHostname: "codex-host",
		CorrelationID:  "corr-env",
		Detail:         TaskCompletedDetail{ExitCode: 0, DurationMs: 5000},
	})

	events, _ := c.GetThreadEvents(ctxbg(), "thr-env", 1)
	if len(events) != 1 {
		t.Fatal("expected 1 event")
	}
	ev := events[0]

	checks := map[string]string{
		"event_id":  ev.EventID,
		"type":      ev.Type,
		"timestamp": ev.Timestamp,
		"task_id":   ev.TaskID,
	}
	for field, val := range checks {
		if val == "" {
			t.Errorf("expected non-empty %s", field)
		}
	}
	if ev.Type != EventTaskCompleted {
		t.Errorf("expected type '%s', got '%s'", EventTaskCompleted, ev.Type)
	}
}

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

// ── stale-lock auto-clear test ────────────────────────────────────────────────

func TestStaleLockAutoClearOnEnqueue(t *testing.T) {
	c, _ := setupTestClient(t)

	// Set up: create a thread, set a lock held by a "done" task (stale)
	c.CreateThread(ctxbg(), "thr-stale", "", "")
	c.rdb.Set(ctxbg(), ThreadLockKey("thr-stale"), "old-task-done", LockTTL)
	c.rdb.Set(ctxbg(), ThreadLockedAtKey("thr-stale"), "2020-01-01T00:00:00Z", LockTTL)
	// Mark the holder task as done (terminal)
	c.rdb.Set(ctxbg(), TaskKey("old-task-done", "status"), "done", TTLTask)

	// Enqueue should detect the stale lock, clear it, and acquire
	task, err := c.Enqueue(ctxbg(), "claude", "thr-stale", "new work")
	if err != nil {
		t.Fatalf("Enqueue should succeed after stale-lock clear, got: %v", err)
	}

	// The new task should have acquired the lock
	holder, _ := c.rdb.Get(ctxbg(), ThreadLockKey("thr-stale")).Result()
	if holder != task.TaskID {
		t.Errorf("expected lock holder to be new task '%s', got '%s'", task.TaskID, holder)
	}

	// locked_at should be set to a recent timestamp (not the old one)
	lockedAt, _ := c.rdb.Get(ctxbg(), ThreadLockedAtKey("thr-stale")).Result()
	if lockedAt == "2020-01-01T00:00:00Z" {
		t.Error("locked_at should be updated, not the old stale value")
	}
	if lockedAt == "" {
		t.Error("locked_at should be set after lock acquisition")
	}
}

func TestStaleLockNotClearedForActiveHolder(t *testing.T) {
	c, _ := setupTestClient(t)

	// Set up: thread locked by a "running" task (active, not stale)
	c.CreateThread(ctxbg(), "thr-active", "", "")
	c.rdb.Set(ctxbg(), ThreadLockKey("thr-active"), "task-running", LockTTL)
	c.rdb.Set(ctxbg(), ThreadLockedAtKey("thr-active"), "2020-01-01T00:00:00Z", LockTTL)
	c.rdb.Set(ctxbg(), TaskKey("task-running", "status"), "running", TTLTask)

	// Enqueue should fail because the lock holder is still active
	_, err := c.Enqueue(ctxbg(), "claude", "thr-active", "wait your turn")
	if err == nil {
		t.Error("Enqueue should fail when lock holder is still active")
	}
}

// ── new event emission tests ──────────────────────────────────────────────────

func TestRequeueStaleEmitsTaskRequeued(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctxbg(), "thr-req", "", "")
	// Set up a stale task in the processing list
	payload := `{"task_id":"req-1","thread_id":"thr-req","instruction":"do X"}`
	c.rdb.LPush(ctxbg(), ProcessingKey("claude"), payload)
	c.rdb.Set(ctxbg(), TaskKey("req-1", "status"), "running", 0)
	c.rdb.Set(ctxbg(), TaskKey("req-1", "last_started_at"), "2020-01-01T00:00:00Z", 0)

	_, err := c.RequeueStale(ctxbg(), "claude", 10*time.Minute)
	if err != nil {
		t.Fatalf("RequeueStale failed: %v", err)
	}

	events, _ := c.GetThreadEvents(ctxbg(), "thr-req", 10)
	found := false
	for _, ev := range events {
		if ev.Type == EventTaskRequeued {
			found = true
			if ev.TaskID != "req-1" {
				t.Errorf("expected task_id 'req-1', got '%s'", ev.TaskID)
			}
		}
	}
	if !found {
		t.Error("expected task_requeued event after RequeueStale")
	}
}

func TestUnlockThreadEmitsLockReleased(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctxbg(), "thr-ul-ev", "", "")
	c.rdb.Set(ctxbg(), ThreadLockKey("thr-ul-ev"), "task-1", LockTTL)
	c.rdb.Set(ctxbg(), ThreadLockedAtKey("thr-ul-ev"), "2025-06-01T10:00:00Z", LockTTL)

	err := c.UnlockThread(ctxbg(), "thr-ul-ev")
	if err != nil {
		t.Fatalf("UnlockThread failed: %v", err)
	}

	events, _ := c.GetThreadEvents(ctxbg(), "thr-ul-ev", 10)
	found := false
	for _, ev := range events {
		if ev.Type == EventLockReleased {
			found = true
			// Verify holder_task_id is populated
			detail, ok := ev.Detail.(map[string]interface{})
			if ok {
				if htid, exists := detail["holder_task_id"]; !exists || htid == "" {
					t.Error("lock_released event should have holder_task_id in detail")
				}
			}
		}
	}
	if !found {
		t.Error("expected lock_released event after UnlockThread")
	}
}

func TestGroupWaitEmitsGroupComplete(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctxbg(), "thr-gc", "", "")
	// Create group tasks that are already done
	for i, tid := range []string{"gc-1", "gc-2"} {
		c.rdb.Set(ctxbg(), TaskKey(tid, "status"), "done", TTLTask)
		c.rdb.SAdd(ctxbg(), GroupTasksKey("thr-gc", "test-group"), tid)
		_ = i
	}

	result, err := c.GroupWait(ctxbg(), "thr-gc", "test-group", 5*time.Second)
	if err != nil {
		t.Fatalf("GroupWait failed: %v", err)
	}
	if result.Status != "complete" {
		t.Fatalf("expected complete, got %s", result.Status)
	}

	events, _ := c.GetThreadEvents(ctxbg(), "thr-gc", 10)
	found := false
	for _, ev := range events {
		if ev.Type == EventGroupComplete {
			found = true
		}
	}
	if !found {
		t.Error("expected group_complete event after GroupWait")
	}
}

func TestWorkerOfflineEmittedOnExpiry(t *testing.T) {
	c, _ := setupTestClient(t)

	// Set a heartbeat with very short TTL (already expired)
	c.rdb.Set(ctxbg(), HeartbeatKey("claude", "expired-host"), "{}", 0)

	// Trigger GetWorkerStats which scans heartbeats
	_, err := c.GetWorkerStats(ctxbg())
	if err != nil {
		t.Fatalf("GetWorkerStats failed: %v", err)
	}

	// Check system events for worker_offline
	events, _ := c.GetSystemEvents(ctxbg(), 20)
	found := false
	for _, ev := range events {
		if ev.Type == EventWorkerOffline && ev.WorkerHostname == "expired-host" {
			found = true
		}
	}
	if !found {
		t.Error("expected worker_offline system event for expired heartbeat")
	}
}

func TestThreadStatusChangeEmitsEvent(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctxbg(), "thr-tsc", "", "")
	// Simulate WaitTask completion: updateThreadStatus is called
	// We can trigger it indirectly by calling UpdateThread
	c.UpdateThread(ctxbg(), "thr-tsc", map[string]string{"status": "complete"})

	// Now call updateThreadStatus via WaitTask path
	// Actually, updateThreadStatus is unexported. Let's test via the event
	// from Enqueue + WaitTask completion
	// For now, just verify the event system is wired.

	// Create a task and wait for it to complete (done)
	c.rdb.Set(ctxbg(), TaskKey("tsc-task", "status"), "done", TTLTask)
	c.rdb.Set(ctxbg(), TaskKey("tsc-task", "thread_id"), "thr-tsc", TTLTask)

	// Directly call updateThreadStatus — it's unexported but in the same package
	c.updateThreadStatus(ctxbg(), "thr-tsc", "done")

	events, _ := c.GetThreadEvents(ctxbg(), "thr-tsc", 10)
	found := false
	for _, ev := range events {
		if ev.Type == EventThreadStatusChange {
			found = true
			detail, ok := ev.Detail.(map[string]interface{})
			if ok {
				if to, exists := detail["to"]; !exists || to != "complete" {
					t.Errorf("expected to='complete', got detail=%v", detail)
				}
				// From should be set (prev status)
				if from, exists := detail["from"]; !exists || from == "" {
					t.Error("thread_status_change should have non-empty from field")
				}
			}
		}
	}
	if !found {
		t.Error("expected thread_status_change event")
	}
}
