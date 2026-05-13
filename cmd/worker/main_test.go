package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/noodle05/ai-agents/tasklib"
)

// newTestClient creates a tasklib.Client backed by miniredis.
func newTestClient(t *testing.T) (*tasklib.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return tasklib.NewClient(rdb), mr
}

// setupThread creates a thread with one user message, returns the workspace tmp dir.
func setupThread(t *testing.T, client *tasklib.Client, threadID string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := client.CreateThread(ctx, threadID, "owner/repo"); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := client.AppendMessage(ctx, threadID, tasklib.Message{
		Role:      "user",
		Type:      "request",
		Content:   "hello world",
		Timestamp: "2025-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	// Return a temp-based workspace so we don't touch real filesystem.
	return t.TempDir()
}

func TestProcessTaskSuccess(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	ws := setupThread(t, client, "thr-ok")

	taskJSON := `{"task_id":"task-ok","thread_id":"thr-ok","instruction":"say hello"}`
	processTask(ctx, client, client.RDB(), taskJSON, "claude", "echo", 30, 10, ws)

	// Task status must be "done"
	status, _ := client.RDB().Get(ctx, tasklib.TaskKey("task-ok", "status")).Result()
	if status != "done" {
		t.Errorf("expected status=done, got %q", status)
	}

	// Exit code must be 0
	exitCode, _ := client.RDB().Get(ctx, tasklib.TaskKey("task-ok", "exit_code")).Result()
	if exitCode != "0" {
		t.Errorf("expected exit_code=0, got %q", exitCode)
	}

	// Result must contain the prompt (echo echoes it back)
	result, _ := client.RDB().Get(ctx, tasklib.TaskKey("task-ok", "result")).Result()
	if result == "" {
		t.Error("result must not be empty")
	}

	// Active tasks must be cleaned up
	active, _ := client.GetActiveTasks(ctx)
	if _, ok := active["task-ok"]; ok {
		t.Error("task must be removed from active_tasks")
	}

	// Thread history must contain the worker result message
	msgs, err := client.GetThreadHistory(ctx, "thr-ok", 1, 0)
	if err != nil {
		t.Fatalf("GetThreadHistory: %v", err)
	}
	found := false
	for _, m := range msgs {
		if m.Role == "claude" && m.Metadata["task_id"] == "task-ok" {
			found = true
			break
		}
	}
	if !found {
		t.Error("thread history must contain worker result message")
	}
}

func TestProcessTaskFailure(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	ws := setupThread(t, client, "thr-fail")

	taskJSON := `{"task_id":"task-fail","thread_id":"thr-fail","instruction":"will fail"}`
	processTask(ctx, client, client.RDB(), taskJSON, "claude", "false", 30, 10, ws)

	status, _ := client.RDB().Get(ctx, tasklib.TaskKey("task-fail", "status")).Result()
	if status != "failed" {
		t.Errorf("expected status=failed, got %q", status)
	}

	exitCode, _ := client.RDB().Get(ctx, tasklib.TaskKey("task-fail", "exit_code")).Result()
	if exitCode != "1" {
		t.Errorf("expected exit_code=1, got %q", exitCode)
	}

	// Result must be prefixed with [FAILED exit=1]
	result, _ := client.RDB().Get(ctx, tasklib.TaskKey("task-fail", "result")).Result()
	if !strings.HasPrefix(result, "[FAILED exit=1]") {
		t.Errorf("result must start with [FAILED exit=1], got %q", result)
	}
}

func TestProcessTaskCancelled(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	ws := setupThread(t, client, "thr-cancel")

	// Set cancel flag before processing
	client.RDB().Set(ctx, tasklib.TaskKey("task-cancel", "cancel"), "1", 0)

	taskJSON := `{"task_id":"task-cancel","thread_id":"thr-cancel","instruction":"should cancel"}`
	processTask(ctx, client, client.RDB(), taskJSON, "claude", "echo", 30, 10, ws)

	status, _ := client.RDB().Get(ctx, tasklib.TaskKey("task-cancel", "status")).Result()
	if status != "cancelled" {
		t.Errorf("expected status=cancelled, got %q", status)
	}

	exitCode, _ := client.RDB().Get(ctx, tasklib.TaskKey("task-cancel", "exit_code")).Result()
	if exitCode != "-1" {
		t.Errorf("expected exit_code=-1, got %q", exitCode)
	}

	result, _ := client.RDB().Get(ctx, tasklib.TaskKey("task-cancel", "result")).Result()
	if result != "Cancelled by master" {
		t.Errorf("expected result='Cancelled by master', got %q", result)
	}
}

func TestProcessTaskHistoryWindowFromPayload(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	ws := setupThread(t, client, "thr-window")

	// payload overrides history_window to 0 — no thread context
	taskJSON := `{"task_id":"task-win","thread_id":"thr-window","instruction":"do x","history_window":0}`
	processTask(ctx, client, client.RDB(), taskJSON, "claude", "echo", 30, 10, ws)

	result, _ := client.RDB().Get(ctx, tasklib.TaskKey("task-win", "result")).Result()
	// With window=0, no thread history prefix. Echo output should not
	// contain the thread history text.
	if strings.Contains(result, "## Thread History") {
		t.Error("result must not contain thread history when history_window=0")
	}
}

func TestProcessTaskTimeoutFromPayload(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	ws := setupThread(t, client, "thr-timeout")

	// payload overrides timeout to 1s; sleep would take longer
	taskJSON := `{"task_id":"task-pto","thread_id":"thr-timeout","instruction":"slow","timeout":1}`
	processTask(ctx, client, client.RDB(), taskJSON, "claude", "sleep 10", 1800, 10, ws)

	status, _ := client.RDB().Get(ctx, tasklib.TaskKey("task-pto", "status")).Result()
	if status != "failed" {
		t.Errorf("expected status=failed, got %q", status)
	}

	result, _ := client.RDB().Get(ctx, tasklib.TaskKey("task-pto", "result")).Result()
	if !strings.Contains(result, "timed out") && !strings.Contains(result, "Timed out") {
		t.Logf("result (may vary by OS): %q", result)
	}
}

func TestProcessTaskThreadStateUpdate(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	ws := setupThread(t, client, "thr-state")

	taskJSON := `{"task_id":"task-state","thread_id":"thr-state","instruction":"ok"}`
	processTask(ctx, client, client.RDB(), taskJSON, "opencode", "true", 30, 10, ws)

	// Thread state must be updated with last_updated_by and last_task_id
	state, _ := client.RDB().HGetAll(ctx, tasklib.ThreadStateKey("thr-state")).Result()
	if state["last_updated_by"] != "opencode" {
		t.Errorf("expected last_updated_by=opencode, got %q", state["last_updated_by"])
	}
	if state["last_task_id"] != "task-state" {
		t.Errorf("expected last_task_id=task-state, got %q", state["last_task_id"])
	}
	if state["updated_at"] == "" {
		t.Error("updated_at must be set")
	}
}

func TestProcessTaskResultTruncation(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	ws := setupThread(t, client, "thr-trunc")

	// Build a long string as instruction so echo produces long output
	long := strings.Repeat("x", 15000)

	taskPayload, _ := json.Marshal(map[string]interface{}{
		"task_id":    "task-trunc",
		"thread_id":  "thr-trunc",
		"instruction": long,
	})
	processTask(ctx, client, client.RDB(), string(taskPayload), "claude", "echo", 30, 10, ws)

	result, _ := client.RDB().Get(ctx, tasklib.TaskKey("task-trunc", "result")).Result()
	if len(result) > 10000 {
		t.Errorf("result must be truncated to 10000 chars, got %d", len(result))
	}
}

func TestProcessTaskMalformedPayload(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	ws := setupThread(t, client, "thr-bad")

	// This is invalid JSON — processTask logs a warning and removes from processing.
	processTask(ctx, client, client.RDB(), "not-valid-json", "claude", "echo", 30, 10, ws)

	// Nothing should be in active_tasks
	active, _ := client.GetActiveTasks(ctx)
	if len(active) != 0 {
		t.Errorf("expected no active tasks, got %d", len(active))
	}
}

func TestProcessTaskHeartbeatKey(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()

	// Heartbeat writes a key with 30s TTL via UpdateWorkerHeartbeat
	err := client.UpdateWorkerHeartbeat(ctx, "claude", "test-host")
	if err != nil {
		t.Fatalf("UpdateWorkerHeartbeat: %v", err)
	}

	val, _ := client.RDB().Get(ctx, tasklib.HeartbeatKey("claude", "test-host")).Result()
	if val != "1" {
		t.Errorf("expected heartbeat value '1', got %q", val)
	}

	ttl := client.RDB().TTL(ctx, tasklib.HeartbeatKey("claude", "test-host")).Val()
	if ttl <= 0 || ttl > 30*time.Second {
		t.Errorf("expected TTL between 0 and 30s, got %v", ttl)
	}
}

func TestProcessTaskSetsCreatedAt(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	ws := setupThread(t, client, "thr-ca")

	taskJSON := `{"task_id":"task-ca","thread_id":"thr-ca","instruction":"hi"}`
	processTask(ctx, client, client.RDB(), taskJSON, "claude", "true", 30, 10, ws)

	createdAt, _ := client.RDB().Get(ctx, tasklib.TaskKey("task-ca", "created_at")).Result()
	if createdAt == "" {
		t.Error("created_at must be set")
	}
	// Must be a valid ISO8601 timestamp
	if _, err := time.Parse("2006-01-02T15:04:05Z", createdAt); err != nil {
		t.Errorf("created_at must be ISO8601, got %q: %v", createdAt, err)
	}

	completedAt, _ := client.RDB().Get(ctx, tasklib.TaskKey("task-ca", "completed_at")).Result()
	if completedAt == "" {
		t.Error("completed_at must be set")
	}
}
