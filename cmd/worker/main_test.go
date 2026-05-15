package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/noodle05/ai-agents/tasklib"
)

const (
	testWorker      = "claude"
	testThread      = "test-thread-001"
	testTaskID      = "task-00000000-0000-0000-0000-000000000001"
	testInstruction = "Implement OAuth2 support for the authentication module"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func newTestClient(t *testing.T) (*tasklib.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return tasklib.NewClient(rdb), mr
}

func makeTaskPayload(taskID, threadID, instruction string, extra map[string]interface{}) string {
	payload := map[string]interface{}{
		"task_id":     taskID,
		"thread_id":   threadID,
		"instruction": instruction,
	}
	for k, v := range extra {
		payload[k] = v
	}
	data, _ := json.Marshal(payload)
	return string(data)
}

func makeMsg(role, content, ts string, taskID string) string {
	msg := map[string]interface{}{
		"role":      role,
		"content":   content,
		"timestamp": ts,
		"metadata":  map[string]string{"task_id": taskID},
	}
	data, _ := json.Marshal(msg)
	return string(data)
}

func newLogger() *logger {
	return &logger{worker: testWorker}
}

// ── mock execCommand ──────────────────────────────────────────────────────────

func mockExecCmd(stdout, stderr string, exitCode int, err error) func() {
	orig := execCommand
	execCommand = func(ctx context.Context, args []string, dir string) (string, string, int, error) {
		return stdout, stderr, exitCode, err
	}
	return func() { execCommand = orig }
}

// ═══════════════════════════════════════════════════════════════════════════════
// Registration Tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestRegistersInActiveTasks(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	// Capture active_tasks state inside the mock (as Python tests do with
	// side_effect) — cleanup removes it before processOneTask returns.
	var capturedEntry tasklib.TaskInfo
	orig := execCommand
	execCommand = func(ctx context.Context, args []string, dir string) (string, string, int, error) {
		entryRaw, _ := rdb.HGet(context.Background(), "active_tasks", testTaskID).Result()
		json.Unmarshal([]byte(entryRaw), &capturedEntry)
		return "Success", "", 0, nil
	}
	defer func() { execCommand = orig }()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)

	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	if capturedEntry.Status != "running" {
		t.Errorf("expected status=running, got %s", capturedEntry.Status)
	}
	if capturedEntry.Worker != testWorker {
		t.Errorf("expected worker=%s, got %s", testWorker, capturedEntry.Worker)
	}
	if capturedEntry.ThreadID != testThread {
		t.Errorf("expected thread_id=%s, got %s", testThread, capturedEntry.ThreadID)
	}
	if capturedEntry.StartedAt == "" {
		t.Error("expected started_at to be set")
	}
	if capturedEntry.WorkerHost != "testhost" {
		t.Errorf("expected WorkerHost=testhost, got %s", capturedEntry.WorkerHost)
	}
}

func TestSetsPerTaskStatusKeys(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("Build output", "", 0, nil)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	if status, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "status")).Result(); status != "done" {
		t.Errorf("expected status=done, got %s", status)
	}
	if worker, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "worker")).Result(); worker != testWorker {
		t.Errorf("expected worker=%s, got %s", testWorker, worker)
	}
	if tid, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "thread_id")).Result(); tid != testThread {
		t.Errorf("expected thread_id=%s, got %s", testThread, tid)
	}
	if desc, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "description")).Result(); desc != testInstruction {
		t.Errorf("expected description, got %s", desc)
	}
	if ca, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "created_at")).Result(); ca == "" {
		t.Error("expected created_at to be set")
	}
}

func TestPerTaskKeysHaveTTL(t *testing.T) {
	client, mr := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("ok", "", 0, nil)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	for _, suffix := range []string{"status", "worker", "thread_id", "description", "created_at", "result", "exit_code", "completed_at"} {
		ttl := mr.TTL(tasklib.TaskKey(testTaskID, suffix))
		if ttl <= 0 {
			t.Errorf("task:%s:%s has no TTL (ttl=%d)", testTaskID, suffix, ttl)
		}
		if ttl > tasklib.TTLTask {
			t.Errorf("task:%s:%s TTL %v > %v", testTaskID, suffix, ttl, tasklib.TTLTask)
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Context / Prompt Building Tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestIncludesThreadHistoryInPrompt(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	// Pre-populate thread history
	rdb.RPush(context.Background(), tasklib.ThreadMessagesKey(testThread),
		makeMsg("master", "Initial instruction", "2026-05-10T00:00:00Z", "prev-001"))
	rdb.RPush(context.Background(), tasklib.ThreadMessagesKey(testThread),
		makeMsg("claude", "Design v1: OAuth2 with PKCE", "2026-05-10T00:00:01Z", "prev-002"))

	var capturedPrompt string
	orig := execCommand
	execCommand = func(ctx context.Context, args []string, dir string) (string, string, int, error) {
		capturedPrompt = args[len(args)-1]
		return "ok", "", 0, nil
	}
	defer func() { execCommand = orig }()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	if !strings.Contains(capturedPrompt, "## Thread History (recent)") {
		t.Error("prompt missing thread history header")
	}
	if !strings.Contains(capturedPrompt, "[master]") {
		t.Error("prompt missing master role")
	}
	if !strings.Contains(capturedPrompt, "[claude]") {
		t.Error("prompt missing claude role")
	}
	if !strings.Contains(capturedPrompt, "Initial instruction") {
		t.Error("prompt missing initial instruction")
	}
	if !strings.Contains(capturedPrompt, "OAuth2 with PKCE") {
		t.Error("prompt missing design content")
	}
}

func TestRespectsHistoryWindowFromPayload(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	// Add 20 messages, default window is 10
	for i := 0; i < 20; i++ {
		rdb.RPush(context.Background(), tasklib.ThreadMessagesKey(testThread),
			makeMsg("master", fmt.Sprintf("Message %d", i),
				"2026-05-10T00:00:00Z", "prev"))
	}

	var capturedPrompt string
	orig := execCommand
	execCommand = func(ctx context.Context, args []string, dir string) (string, string, int, error) {
		capturedPrompt = args[len(args)-1]
		return "ok", "", 0, nil
	}
	defer func() { execCommand = orig }()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, map[string]interface{}{
		"history_window": 3,
	})
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	if !strings.Contains(capturedPrompt, "Message 17") {
		t.Error("prompt missing message 17")
	}
	if !strings.Contains(capturedPrompt, "Message 18") {
		t.Error("prompt missing message 18")
	}
	if !strings.Contains(capturedPrompt, "Message 19") {
		t.Error("prompt missing message 19")
	}
	if strings.Contains(capturedPrompt, "Message 0") {
		t.Error("prompt contains message outside window")
	}
}

func TestIncludesCurrentStateInPrompt(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	rdb.HSet(context.Background(), tasklib.ThreadStateKey(testThread), map[string]interface{}{
		"status":      "awaiting_review",
		"last_design": "OAuth2 with PKCE design v1",
		"gh_repo":     "owner/repo",
		"gh_pr_number": "42",
	})

	var capturedPrompt string
	orig := execCommand
	execCommand = func(ctx context.Context, args []string, dir string) (string, string, int, error) {
		capturedPrompt = args[len(args)-1]
		return "ok", "", 0, nil
	}
	defer func() { execCommand = orig }()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	if !strings.Contains(capturedPrompt, "## Current State") {
		t.Error("prompt missing current state header")
	}
	if !strings.Contains(capturedPrompt, "status: awaiting_review") {
		t.Error("prompt missing status")
	}
	if !strings.Contains(capturedPrompt, "OAuth2 with PKCE design v1") {
		t.Error("prompt missing last_design")
	}
	if !strings.Contains(capturedPrompt, "repo: owner/repo") {
		t.Error("prompt missing repo")
	}
	if !strings.Contains(capturedPrompt, "PR: #42") {
		t.Error("prompt missing PR number")
	}
}

func TestNoThreadHistoryNoCrash(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	var capturedPrompt string
	orig := execCommand
	execCommand = func(ctx context.Context, args []string, dir string) (string, string, int, error) {
		capturedPrompt = args[len(args)-1]
		return "ok", "", 0, nil
	}
	defer func() { execCommand = orig }()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	if strings.Contains(capturedPrompt, "## Thread History") {
		t.Error("prompt should not have thread history")
	}
	if !strings.Contains(capturedPrompt, "## Task") {
		t.Error("prompt missing task header")
	}
	if !strings.Contains(capturedPrompt, testInstruction) {
		t.Error("prompt missing instruction")
	}
}

func TestNoCurrentStateNoCrash(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	var capturedPrompt string
	orig := execCommand
	execCommand = func(ctx context.Context, args []string, dir string) (string, string, int, error) {
		capturedPrompt = args[len(args)-1]
		return "ok", "", 0, nil
	}
	defer func() { execCommand = orig }()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	if strings.Contains(capturedPrompt, "## Current State") {
		t.Error("prompt should not have current state")
	}
	if !strings.Contains(capturedPrompt, testInstruction) {
		t.Error("prompt missing instruction")
	}
}

func TestCurrentStateMissingFieldsDefaults(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	// Only set status, no optional fields
	rdb.HSet(context.Background(), tasklib.ThreadStateKey(testThread), map[string]interface{}{
		"status": "implementing",
	})

	var capturedPrompt string
	orig := execCommand
	execCommand = func(ctx context.Context, args []string, dir string) (string, string, int, error) {
		capturedPrompt = args[len(args)-1]
		return "ok", "", 0, nil
	}
	defer func() { execCommand = orig }()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	if !strings.Contains(capturedPrompt, "status: implementing") {
		t.Error("prompt missing status")
	}
	if strings.Contains(capturedPrompt, "design:") {
		t.Error("prompt should not have design field")
	}
	if strings.Contains(capturedPrompt, "repo:") {
		t.Error("prompt should not have repo field")
	}
	if strings.Contains(capturedPrompt, "PR:") {
		t.Error("prompt should not have PR field")
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Workspace Tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestCreatesWorkspaceForThread(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("ok", "", 0, nil)
	defer restore()

	workspaceBase := t.TempDir()
	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspaceBase, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	expected := filepath.Join(workspaceBase, testThread)
	if info, err := os.Stat(expected); err != nil || !info.IsDir() {
		t.Errorf("workspace not created at %s: %v", expected, err)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Agent Execution Tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestSuccessfulExecutionStatusDone(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("Task completed", "", 0, nil)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	status, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "status")).Result()
	if status != "done" {
		t.Errorf("expected status=done, got %s", status)
	}
}

func TestFailedExecutionStatusFailed(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("Partial output", "error text", 1, nil)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	status, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "status")).Result()
	if status != "failed" {
		t.Errorf("expected status=failed, got %s", status)
	}
}

func TestFailedResultPrefixedWithFailedTag(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("Output", "", 1, nil)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	result, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "result")).Result()
	if !strings.HasPrefix(result, "[FAILED exit=1]") {
		t.Errorf("expected [FAILED exit=1] prefix, got: %s", result)
	}
}

func TestTimeoutStatusFailed(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("", "", -1, errAgentTimeout)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	status, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "status")).Result()
	if status != "failed" {
		t.Errorf("expected status=failed on timeout, got %s", status)
	}
	exitCode, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "exit_code")).Result()
	if exitCode != "-1" {
		t.Errorf("expected exit_code=-1 on timeout, got %s", exitCode)
	}
}

func TestTimeoutResultMessage(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("", "", -1, errAgentTimeout)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, map[string]interface{}{
		"timeout": 600,
	})
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	result, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "result")).Result()
	if !strings.Contains(strings.ToLower(result), "timed out") {
		t.Errorf("expected 'timed out' in result, got: %s", result)
	}
	if !strings.Contains(result, "600s") {
		t.Errorf("expected '600s' in result, got: %s", result)
	}
}

func TestStderrAppendedToResult(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("stdout here", "stderr here", 0, nil)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	result, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "result")).Result()
	if !strings.Contains(result, "[stderr]") {
		t.Errorf("expected [stderr] delimiter, got: %s", result)
	}
	if !strings.Contains(result, "stderr here") {
		t.Errorf("expected stderr content, got: %s", result)
	}
}

func TestTimeoutValueFromPayload(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	var capturedTimeout time.Duration
	orig := execCommand
	execCommand = func(ctx context.Context, args []string, dir string) (string, string, int, error) {
		deadline, _ := ctx.Deadline()
		capturedTimeout = time.Until(deadline)
		return "ok", "", 0, nil
	}
	defer func() { execCommand = orig }()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, map[string]interface{}{
		"timeout": 3600,
	})
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	// Timeout should be approximately 3600s (allow 2s margin)
	if capturedTimeout < 3598*time.Second || capturedTimeout > 3600*time.Second {
		t.Errorf("expected timeout ~3600s, got %v", capturedTimeout)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Result Storage Tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestResultStored(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("Build output here", "", 0, nil)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	result, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "result")).Result()
	if !strings.Contains(result, "Build output here") {
		t.Errorf("expected 'Build output here' in result, got: %s", result)
	}
}

func TestExitCodeStoredAsString(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("ok", "", 0, nil)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	exitCode, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "exit_code")).Result()
	if exitCode != "0" {
		t.Errorf("expected exit_code='0', got '%s'", exitCode)
	}
}

func TestCompletedAtSet(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("ok", "", 0, nil)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	completed, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "completed_at")).Result()
	if completed == "" {
		t.Error("completed_at not set")
	}
	if !strings.Contains(completed, "T") {
		t.Error("completed_at not ISO8601 format")
	}
	if !strings.HasSuffix(completed, "Z") {
		t.Error("completed_at missing Z suffix")
	}
}

func TestResultAppendedToThreadHistory(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("Result text", "", 0, nil)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	msgs, _ := rdb.LRange(context.Background(), tasklib.ThreadMessagesKey(testThread), 0, -1).Result()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	var msg map[string]interface{}
	json.Unmarshal([]byte(msgs[0]), &msg)
	if msg["role"] != testWorker {
		t.Errorf("expected role=%s, got %v", testWorker, msg["role"])
	}
	if !strings.Contains(fmt.Sprint(msg["content"]), "Result text") {
		t.Errorf("expected 'Result text' in content, got: %v", msg["content"])
	}
}

func TestResultCappedAt10kChars(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	hugeOutput := strings.Repeat("x", 15000)
	restore := mockExecCmd(hugeOutput, "", 0, nil)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	// Full result in task key
	full, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "result")).Result()
	if len(full) != 15000 {
		t.Errorf("expected full result len=15000, got %d", len(full))
	}

	// Capped in thread history
	msgs, _ := rdb.LRange(context.Background(), tasklib.ThreadMessagesKey(testThread), 0, -1).Result()
	var msg map[string]interface{}
	json.Unmarshal([]byte(msgs[0]), &msg)
	content := fmt.Sprint(msg["content"])
	if len(content) != 10000 {
		t.Errorf("expected capped len=10000, got %d", len(content))
	}
}

func TestThreadHistoryTTLRefreshed(t *testing.T) {
	client, mr := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("New result", "", 0, nil)
	defer restore()

	// Pre-populate thread history with TTL
	rdb.RPush(context.Background(), tasklib.ThreadMessagesKey(testThread), makeMsg("master", "prior msg", "2026-05-10T00:00:00Z", "prev"))
	rdb.Expire(context.Background(), tasklib.ThreadMessagesKey(testThread), tasklib.TTLThread)
	initialTTL := mr.TTL(tasklib.ThreadMessagesKey(testThread))

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	finalTTL := mr.TTL(tasklib.ThreadMessagesKey(testThread))
	if finalTTL <= 0 {
		t.Error("thread messages TTL not refreshed")
	}
	// TTL should be within tasklib.TTLThread ± 5s of initial
	if finalTTL < initialTTL-5*time.Second {
		t.Errorf("TTL dropped too much: was %v, now %v", initialTTL, finalTTL)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Cancellation Tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestCancelFlagDetectedBeforeSubprocess(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	rdb.Set(context.Background(), tasklib.TaskKey(testTaskID, "cancel"), "1", 0)

	cmdCalled := false
	orig := execCommand
	execCommand = func(ctx context.Context, args []string, dir string) (string, string, int, error) {
		cmdCalled = true
		return "ok", "", 0, nil
	}
	defer func() { execCommand = orig }()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	if cmdCalled {
		t.Error("execCommand should NOT have been called for cancelled task")
	}
}

func TestCancelledStatusStored(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	rdb.Set(context.Background(), tasklib.TaskKey(testTaskID, "cancel"), "1", 0)

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	status, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "status")).Result()
	if status != "cancelled" {
		t.Errorf("expected status=cancelled, got %s", status)
	}
}

func TestCancelledResultMessage(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	rdb.Set(context.Background(), tasklib.TaskKey(testTaskID, "cancel"), "1", 0)

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	result, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "result")).Result()
	if result != "Cancelled by master" {
		t.Errorf("expected 'Cancelled by master', got: %s", result)
	}
}

func TestCancelledExitCodeMinusOne(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	rdb.Set(context.Background(), tasklib.TaskKey(testTaskID, "cancel"), "1", 0)

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	exitCode, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "exit_code")).Result()
	if exitCode != "-1" {
		t.Errorf("expected exit_code='-1', got '%s'", exitCode)
	}
}

func TestCancelledCompletedAtSet(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	rdb.Set(context.Background(), tasklib.TaskKey(testTaskID, "cancel"), "1", 0)

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	completed, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "completed_at")).Result()
	if completed == "" || !strings.HasSuffix(completed, "Z") {
		t.Errorf("expected completed_at ISO8601 with Z, got: %s", completed)
	}
}

func TestCancellationMessageInThreadHistory(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	rdb.Set(context.Background(), tasklib.TaskKey(testTaskID, "cancel"), "1", 0)

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	msgs, _ := rdb.LRange(context.Background(), tasklib.ThreadMessagesKey(testThread), 0, -1).Result()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	var msg map[string]interface{}
	json.Unmarshal([]byte(msgs[0]), &msg)
	if msg["role"] != testWorker {
		t.Errorf("expected role=%s, got %v", testWorker, msg["role"])
	}
	if !strings.Contains(fmt.Sprint(msg["content"]), "[cancelled]") {
		t.Errorf("expected [cancelled] prefix, got: %v", msg["content"])
	}
}

func TestCancelledRemovedFromProcessingList(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	rdb.Set(context.Background(), tasklib.TaskKey(testTaskID, "cancel"), "1", 0)

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	processingKey := tasklib.ProcessingKey(testWorker)
	rdb.LPush(context.Background(), processingKey, payload)
	if l, _ := rdb.LLen(context.Background(), processingKey).Result(); l != 1 {
		t.Fatalf("expected 1 item in processing, got %d", l)
	}
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", processingKey, "testhost")

	if l, _ := rdb.LLen(context.Background(), processingKey).Result(); l != 0 {
		t.Errorf("expected empty processing list, got %d items", l)
	}
}

func TestCancelledRemovedFromActiveTasks(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	rdb.Set(context.Background(), tasklib.TaskKey(testTaskID, "cancel"), "1", 0)

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	if rdb.HExists(context.Background(), "active_tasks", testTaskID).Val() {
		t.Error("task should have been removed from active_tasks")
	}
}

func TestNoCancelFlagProceedsNormally(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	cmdCalled := false
	orig := execCommand
	execCommand = func(ctx context.Context, args []string, dir string) (string, string, int, error) {
		cmdCalled = true
		return "ok", "", 0, nil
	}
	defer func() { execCommand = orig }()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	if !cmdCalled {
		t.Error("execCommand should have been called")
	}
	status, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "status")).Result()
	if status != "done" {
		t.Errorf("expected status=done, got %s", status)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Thread State Update Tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestSetsMetadataFields(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("ok", "", 0, nil)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	state, _ := rdb.HGetAll(context.Background(), tasklib.ThreadStateKey(testThread)).Result()
	if state["last_updated_by"] != testWorker {
		t.Errorf("expected last_updated_by=%s, got %s", testWorker, state["last_updated_by"])
	}
	if state["last_task_id"] != testTaskID {
		t.Errorf("expected last_task_id=%s, got %s", testTaskID, state["last_task_id"])
	}
	if state["updated_at"] == "" {
		t.Error("expected updated_at to be set")
	}
}

func TestNeverSetsStatusField(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("ok", "", 0, nil)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	state, _ := rdb.HGetAll(context.Background(), tasklib.ThreadStateKey(testThread)).Result()
	// Worker sets only 3 fields: last_updated_by, last_task_id, updated_at
	if len(state) != 3 {
		t.Errorf("expected exactly 3 fields, got %d: %v", len(state), state)
	}
	if _, hasStatus := state["status"]; hasStatus {
		t.Error("worker should never set status field on thread")
	}
}

func TestPreservesExistingStateFields(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("ok", "", 0, nil)
	defer restore()

	// Pre-existing thread state
	rdb.HSet(context.Background(), tasklib.ThreadStateKey(testThread), map[string]interface{}{
		"status":     "existing_status",
		"gh_repo":    "owner/repo",
		"last_design": "some design",
	})

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	state, _ := rdb.HGetAll(context.Background(), tasklib.ThreadStateKey(testThread)).Result()
	// Pre-existing fields should be preserved
	if state["status"] != "existing_status" {
		t.Errorf("status should be preserved, got %s", state["status"])
	}
	if state["gh_repo"] != "owner/repo" {
		t.Errorf("gh_repo should be preserved, got %s", state["gh_repo"])
	}
	if state["last_design"] != "some design" {
		t.Errorf("last_design should be preserved, got %s", state["last_design"])
	}
	// New fields should be set
	if state["last_updated_by"] != testWorker {
		t.Errorf("last_updated_by should be set, got %s", state["last_updated_by"])
	}
}

func TestThreadStateTTLSet(t *testing.T) {
	client, mr := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("ok", "", 0, nil)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	ttl := mr.TTL(tasklib.ThreadStateKey(testThread))
	if ttl <= 0 {
		t.Error("thread state TTL not set")
	}
	if ttl > tasklib.TTLThread {
		t.Errorf("thread state TTL %v > %v", ttl, tasklib.TTLThread)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Cleanup Tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestRemovedFromProcessingList(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("ok", "", 0, nil)
	defer restore()

	processingKey := tasklib.ProcessingKey(testWorker)
	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), processingKey, payload)
	if l, _ := rdb.LLen(context.Background(), processingKey).Result(); l != 1 {
		t.Fatalf("expected 1 item in processing, got %d", l)
	}
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", processingKey, "testhost")

	if l, _ := rdb.LLen(context.Background(), processingKey).Result(); l != 0 {
		t.Errorf("expected empty processing list, got %d items", l)
	}
}

func TestRemovedFromActiveTasks(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("ok", "", 0, nil)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	if rdb.HExists(context.Background(), "active_tasks", testTaskID).Val() {
		t.Error("task should have been removed from active_tasks")
	}
}

func TestCleanupAfterFailedTask(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("Partial", "Error", 1, nil)
	defer restore()

	processingKey := tasklib.ProcessingKey(testWorker)
	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), processingKey, payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", processingKey, "testhost")

	if l, _ := rdb.LLen(context.Background(), processingKey).Result(); l != 0 {
		t.Errorf("expected empty processing list after failure, got %d items", l)
	}
	if rdb.HExists(context.Background(), "active_tasks", testTaskID).Val() {
		t.Error("task should have been removed from active_tasks after failure")
	}
}

func TestCleanupAfterTimeout(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()
	restore := mockExecCmd("", "", -1, errAgentTimeout)
	defer restore()

	processingKey := tasklib.ProcessingKey(testWorker)
	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), processingKey, payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", processingKey, "testhost")

	if l, _ := rdb.LLen(context.Background(), processingKey).Result(); l != 0 {
		t.Errorf("expected empty processing list after timeout, got %d items", l)
	}
	if rdb.HExists(context.Background(), "active_tasks", testTaskID).Val() {
		t.Error("task should have been removed from active_tasks after timeout")
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Malformed Input Tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestMalformedTaskPayloadRemovedFromProcessing(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	processingKey := tasklib.ProcessingKey(testWorker)
	badPayload := "not valid json {{{"
	rdb.LPush(context.Background(), processingKey, badPayload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, badPayload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", processingKey, "testhost")

	if l, _ := rdb.LLen(context.Background(), processingKey).Result(); l != 0 {
		t.Errorf("malformed payload should have been removed, got %d items", l)
	}
}

func TestMalformedThreadMessageSkipped(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	// Add a corrupt message + valid message
	rdb.RPush(context.Background(), tasklib.ThreadMessagesKey(testThread), "not valid json")
	rdb.RPush(context.Background(), tasklib.ThreadMessagesKey(testThread),
		makeMsg("master", "valid message", "2026-05-10T00:00:00Z", "prev"))

	restore := mockExecCmd("ok", "", 0, nil)
	defer restore()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	// Should complete without crashing
	status, _ := rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "status")).Result()
	if status != "done" {
		t.Errorf("expected status=done, got %s", status)
	}
}

func TestSetsRunningStatusImmediately(t *testing.T) {
	client, _ := newTestClient(t)
	rdb := client.RDB()
	log := newLogger()

	var statusBefore string
	orig := execCommand
	execCommand = func(ctx context.Context, args []string, dir string) (string, string, int, error) {
		// Status should already be "running" before subprocess executes
		statusBefore, _ = rdb.Get(context.Background(), tasklib.TaskKey(testTaskID, "status")).Result()
		return "ok", "", 0, nil
	}
	defer func() { execCommand = orig }()

	payload := makeTaskPayload(testTaskID, testThread, testInstruction, nil)
	rdb.LPush(context.Background(), tasklib.ProcessingKey(testWorker), payload)
	workspace := t.TempDir()
	processOneTask(log, client, rdb, payload, testWorker, "claude -p",
		1800, 10, workspace, "/nonexistent", tasklib.ProcessingKey(testWorker), "testhost")

	if statusBefore != "running" {
		t.Errorf("expected status=running before subprocess, got %s", statusBefore)
	}
}

func TestValidWorker(t *testing.T) {
	if !validWorker("claude") {
		t.Error("claude should be valid")
	}
	if !validWorker("copilot") {
		t.Error("copilot should be valid")
	}
	if !validWorker("opencode") {
		t.Error("opencode should be valid")
	}
	if !validWorker("codex") {
		t.Error("codex should be valid")
	}
	if validWorker("invalid") {
		t.Error("invalid should not be valid")
	}
}
