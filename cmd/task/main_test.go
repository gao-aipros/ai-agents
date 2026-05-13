package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/noodle05/ai-agents/tasklib"
)

// ── test helpers ──────────────────────────────────────────────────────────────

func setupTestRedis(t *testing.T) (*miniredis.Miniredis, func()) {
	t.Helper()
	mr := miniredis.RunT(t)

	origGetClient := getClient
	getClient = func() *tasklib.Client {
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		return tasklib.NewClient(rdb)
	}

	origDie := die
	die = func(msg string) {
		panic("die: " + msg)
	}

	return mr, func() {
		getClient = origGetClient
		die = origDie
	}
}

func captureOutput(fn func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	w.Close()
	out := <-done
	os.Stdout = old
	return strings.TrimRight(out, "\n")
}

// readTaskKey reads a per-task field via the test's getClient.
func readTaskKey(taskID, field string) string {
	c := getClient()
	val, _ := c.RDB().Get(context.Background(), tasklib.TaskKey(taskID, field)).Result()
	return val
}

// ═══════════════════════════════════════════════════════════════════════════════
// enqueue
// ═══════════════════════════════════════════════════════════════════════════════

func TestCmdEnqueue(t *testing.T) {
	mr, cleanup := setupTestRedis(t)
	defer cleanup()

	// StringVar resets vars to defaults — set after creating the cobra command.
	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&enqueueWorker, "worker", "", "")
	cmd.Flags().StringVar(&enqueueThread, "thread", "", "")
	cmd.Flags().StringVar(&enqueueInstruction, "instruction", "", "")
	enqueueWorker = "claude"
	enqueueThread = "test-thread-001"
	enqueueInstruction = "fix the bug"

	mr.HSet(tasklib.ThreadStateKey(enqueueThread), "status", "initiated")

	output := captureOutput(func() {
		if err := cmdEnqueue(cmd, nil); err != nil {
			t.Fatalf("cmdEnqueue: %v", err)
		}
	})

	var result map[string]string
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if result["task_id"] == "" {
		t.Fatal("expected task_id in output")
	}

	if s := readTaskKey(result["task_id"], "status"); s != "pending" {
		t.Errorf("expected status=pending, got %s", s)
	}
	if w := readTaskKey(result["task_id"], "worker"); w != "claude" {
		t.Errorf("expected worker=claude, got %s", w)
	}

	c := getClient()
	queueItems, _ := c.RDB().LRange(context.Background(), tasklib.QueueKey("claude"), 0, -1).Result()
	if len(queueItems) != 1 {
		t.Errorf("expected 1 item on queue, got %d", len(queueItems))
	}
}

func TestCmdEnqueueThreadLocked(t *testing.T) {
	_, cleanup := setupTestRedis(t)
	defer cleanup()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&enqueueWorker, "worker", "", "")
	cmd.Flags().StringVar(&enqueueThread, "thread", "", "")
	cmd.Flags().StringVar(&enqueueInstruction, "instruction", "", "")
	enqueueWorker = "claude"
	enqueueThread = "test-thread-001"
	enqueueInstruction = "fix the bug"

	c := getClient()
	c.LockThread(context.Background(), enqueueThread, "holder-task", tasklib.LockTTL)

	var panicMsg string
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicMsg = r.(string)
			}
		}()
		captureOutput(func() {
			cmdEnqueue(cmd, nil)
		})
	}()
	if !strings.Contains(panicMsg, "locked") {
		t.Errorf("expected locked error, got: %s", panicMsg)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// status
// ═══════════════════════════════════════════════════════════════════════════════

func TestCmdStatus(t *testing.T) {
	mr, cleanup := setupTestRedis(t)
	defer cleanup()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&statusID, "id", "", "")
	statusID = "my-task-id"

	mr.Set(tasklib.TaskKey(statusID, "status"), "done")
	mr.Set(tasklib.TaskKey(statusID, "worker"), "claude")
	mr.Set(tasklib.TaskKey(statusID, "thread_id"), "thread-1")

	output := captureOutput(func() {
		if err := cmdStatus(cmd, nil); err != nil {
			t.Fatalf("cmdStatus: %v", err)
		}
	})

	var info map[string]interface{}
	json.Unmarshal([]byte(output), &info)
	if info["task_id"] != "my-task-id" {
		t.Errorf("expected task_id=my-task-id, got %v", info["task_id"])
	}
	if info["status"] != "done" {
		t.Errorf("expected status=done, got %v", info["status"])
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// result
// ═══════════════════════════════════════════════════════════════════════════════

func TestCmdResult(t *testing.T) {
	mr, cleanup := setupTestRedis(t)
	defer cleanup()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&resultID, "id", "", "")
	cmd.Flags().IntVar(&resultTail, "tail", -1, "")
	resultID = "my-task-id"

	mr.Set(tasklib.TaskKey(resultID, "result"), "Build successful\nTests pass")

	output := captureOutput(func() {
		if err := cmdResult(cmd, nil); err != nil {
			t.Fatalf("cmdResult: %v", err)
		}
	})

	if !strings.Contains(output, "Build successful") {
		t.Errorf("expected result content, got: %s", output)
	}
}

func TestCmdResultWithTail(t *testing.T) {
	mr, cleanup := setupTestRedis(t)
	defer cleanup()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&resultID, "id", "", "")
	cmd.Flags().IntVar(&resultTail, "tail", -1, "")
	resultID = "my-task-id"

	mr.Set(tasklib.TaskKey(resultID, "result"), "line1\nline2\nline3\nline4\nline5")

	// Need the Changed() check to return true; use Flags().Set to both set
	// the value and mark it as changed.
	cmd.Flags().Set("tail", "2")

	output := captureOutput(func() {
		if err := cmdResult(cmd, nil); err != nil {
			t.Fatalf("cmdResult: %v", err)
		}
	})

	if !strings.Contains(output, "line4") || !strings.Contains(output, "line5") {
		t.Errorf("expected last 2 lines, got: %s", output)
	}
	if strings.Contains(output, "line1") {
		t.Errorf("tail output should not contain line1, got: %s", output)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// list
// ═══════════════════════════════════════════════════════════════════════════════

func TestCmdList(t *testing.T) {
	mr, cleanup := setupTestRedis(t)
	defer cleanup()

	mr.Set(tasklib.TaskKey("t1", "status"), "done")
	mr.Set(tasklib.TaskKey("t1", "worker"), "claude")
	mr.Set(tasklib.TaskKey("t1", "thread_id"), "thread-1")
	mr.Set(tasklib.TaskKey("t1", "created_at"), "2026-01-01T00:00:00Z")
	mr.Set(tasklib.TaskKey("t2", "status"), "running")
	mr.Set(tasklib.TaskKey("t2", "worker"), "copilot")
	mr.Set(tasklib.TaskKey("t2", "thread_id"), "thread-2")
	mr.Set(tasklib.TaskKey("t2", "created_at"), "2026-01-01T00:00:01Z")

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&listWorker, "worker", "", "")
	cmd.Flags().StringVar(&listStatus, "status", "", "")
	cmd.Flags().StringVar(&listThread, "thread", "", "")
	cmd.Flags().IntVar(&listLimit, "limit", 50, "")

	output := captureOutput(func() {
		if err := cmdList(cmd, nil); err != nil {
			t.Fatalf("cmdList: %v", err)
		}
	})

	if !strings.Contains(output, "t1") || !strings.Contains(output, "t2") {
		t.Errorf("expected task IDs in output, got: %s", output)
	}
	if !strings.Contains(output, "done") || !strings.Contains(output, "running") {
		t.Errorf("expected statuses in output, got: %s", output)
	}
}

func TestCmdListEmpty(t *testing.T) {
	_, cleanup := setupTestRedis(t)
	defer cleanup()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&listWorker, "worker", "", "")
	cmd.Flags().StringVar(&listStatus, "status", "", "")
	cmd.Flags().StringVar(&listThread, "thread", "", "")
	cmd.Flags().IntVar(&listLimit, "limit", 50, "")

	output := captureOutput(func() {
		if err := cmdList(cmd, nil); err != nil {
			t.Fatalf("cmdList: %v", err)
		}
	})

	if output != "(no tasks)" {
		t.Errorf("expected '(no tasks)', got: %s", output)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// wait
// ═══════════════════════════════════════════════════════════════════════════════

func TestCmdWaitTaskCompleted(t *testing.T) {
	mr, cleanup := setupTestRedis(t)
	defer cleanup()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&waitID, "id", "", "")
	cmd.Flags().IntVar(&waitTimeout, "timeout", 300, "")
	waitID = "my-task-id"
	waitTimeout = 5

	mr.Set(tasklib.TaskKey(waitID, "status"), "done")
	mr.Set(tasklib.TaskKey(waitID, "worker"), "claude")
	mr.Set(tasklib.TaskKey(waitID, "thread_id"), "thread-1")
	mr.Set(tasklib.TaskKey(waitID, "exit_code"), "0")
	mr.Set(tasklib.TaskKey(waitID, "created_at"), "2026-01-01T00:00:00Z")
	mr.Set(tasklib.TaskKey(waitID, "completed_at"), "2026-01-01T00:05:00Z")

	output := captureOutput(func() {
		if err := cmdWait(cmd, nil); err != nil {
			t.Fatalf("cmdWait: %v", err)
		}
	})

	var info map[string]interface{}
	json.Unmarshal([]byte(output), &info)
	if info["task_id"] != "my-task-id" {
		t.Errorf("expected task_id=my-task-id, got %v", info["task_id"])
	}
	if info["status"] != "done" {
		t.Errorf("expected status=done, got %v", info["status"])
	}
}

func TestCmdWaitTaskNotFound(t *testing.T) {
	_, cleanup := setupTestRedis(t)
	defer cleanup()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&waitID, "id", "", "")
	cmd.Flags().IntVar(&waitTimeout, "timeout", 300, "")
	waitID = "nonexistent"
	waitTimeout = 1

	var panicMsg string
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicMsg = r.(string)
			}
		}()
		captureOutput(func() {
			cmdWait(cmd, nil)
		})
	}()
	if !strings.Contains(panicMsg, "not found") {
		t.Errorf("expected 'not found' error, got: %s", panicMsg)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// requeue-stale
// ═══════════════════════════════════════════════════════════════════════════════

func TestCmdRequeueStale(t *testing.T) {
	mr, cleanup := setupTestRedis(t)
	defer cleanup()

	taskID := "stale-task"
	oldTime := time.Now().UTC().Add(-1 * time.Hour).Format("2006-01-02T15:04:05Z")
	mr.Set(tasklib.TaskKey(taskID, "status"), "running")
	mr.Set(tasklib.TaskKey(taskID, "created_at"), oldTime)

	payload, _ := json.Marshal(tasklib.TaskPayload{TaskID: taskID, ThreadID: "th", Instruction: "x"})
	mr.Lpush(tasklib.ProcessingKey("claude"), string(payload))

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&requeueWorker, "worker", "", "")
	cmd.Flags().IntVar(&requeueOlderThan, "older-than", 600, "")
	requeueWorker = "claude"
	requeueOlderThan = 600

	output := captureOutput(func() {
		if err := cmdRequeueStale(cmd, nil); err != nil {
			t.Fatalf("cmdRequeueStale: %v", err)
		}
	})

	if !strings.Contains(output, "Requeued") {
		t.Errorf("expected 'Requeued' in output, got: %s", output)
	}
	if !strings.Contains(output, taskID) {
		t.Errorf("expected task ID in output, got: %s", output)
	}

	c := getClient()
	queueItems, _ := c.RDB().LRange(context.Background(), tasklib.QueueKey("claude"), 0, -1).Result()
	if len(queueItems) != 1 {
		t.Errorf("expected 1 item requeued, got %d", len(queueItems))
	}
	if s := readTaskKey(taskID, "status"); s != "pending" {
		t.Errorf("expected status=pending after requeue, got %s", s)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// cancel
// ═══════════════════════════════════════════════════════════════════════════════

func TestCmdCancel(t *testing.T) {
	mr, cleanup := setupTestRedis(t)
	defer cleanup()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&cancelID, "id", "", "")
	cancelID = "my-task-id"

	mr.Set(tasklib.TaskKey(cancelID, "status"), "running")

	output := captureOutput(func() {
		if err := cmdCancel(cmd, nil); err != nil {
			t.Fatalf("cmdCancel: %v", err)
		}
	})

	if !strings.Contains(output, "Cancel flag set") {
		t.Errorf("expected 'Cancel flag set', got: %s", output)
	}
	if f := readTaskKey(cancelID, "cancel"); f != "1" {
		t.Errorf("expected cancel flag to be set, got: %s", f)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// unlock
// ═══════════════════════════════════════════════════════════════════════════════

func TestCmdUnlock(t *testing.T) {
	mr, cleanup := setupTestRedis(t)
	defer cleanup()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&unlockThread, "thread", "", "")
	unlockThread = "test-thread"

	mr.Set(tasklib.ThreadLockKey(unlockThread), "some-task-id")

	output := captureOutput(func() {
		if err := cmdUnlock(cmd, nil); err != nil {
			t.Fatalf("cmdUnlock: %v", err)
		}
	})

	if !strings.Contains(output, "Lock released") {
		t.Errorf("expected 'Lock released', got: %s", output)
	}
	if mr.Exists(tasklib.ThreadLockKey(unlockThread)) {
		t.Error("lock key should have been deleted")
	}
}

func TestCmdUnlockNoLock(t *testing.T) {
	_, cleanup := setupTestRedis(t)
	defer cleanup()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&unlockThread, "thread", "", "")
	unlockThread = "test-thread-no-lock"

	output := captureOutput(func() {
		if err := cmdUnlock(cmd, nil); err != nil {
			t.Fatalf("cmdUnlock: %v", err)
		}
	})

	if !strings.Contains(output, "No lock found") {
		t.Errorf("expected 'No lock found', got: %s", output)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// thread-create
// ═══════════════════════════════════════════════════════════════════════════════

func TestCmdThreadCreate(t *testing.T) {
	_, cleanup := setupTestRedis(t)
	defer cleanup()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&tcID, "id", "", "")
	cmd.Flags().StringVar(&tcRepo, "repo", "", "")
	tcID = "my-thread"
	tcRepo = "owner/repo"

	output := captureOutput(func() {
		if err := cmdThreadCreate(cmd, nil); err != nil {
			t.Fatalf("cmdThreadCreate: %v", err)
		}
	})

	if !strings.Contains(output, "Thread 'my-thread' created") {
		t.Errorf("expected 'Thread created', got: %s", output)
	}

	c := getClient()
	state, _ := c.RDB().HGetAll(context.Background(), tasklib.ThreadStateKey("my-thread")).Result()
	if state["status"] != "initiated" {
		t.Errorf("expected status=initiated, got %s", state["status"])
	}
	if state["gh_repo"] != "owner/repo" {
		t.Errorf("expected gh_repo=owner/repo, got %s", state["gh_repo"])
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// thread-history
// ═══════════════════════════════════════════════════════════════════════════════

func TestCmdThreadHistory(t *testing.T) {
	mr, cleanup := setupTestRedis(t)
	defer cleanup()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&thID, "id", "", "")
	cmd.Flags().IntVar(&thTail, "tail", -1, "")
	thID = "my-thread"

	msg := `{"role":"master","content":"Hello","timestamp":"2026-01-01T00:00:00Z"}`
	mr.Lpush(tasklib.ThreadMessagesKey(thID), msg)

	output := captureOutput(func() {
		if err := cmdThreadHistory(cmd, nil); err != nil {
			t.Fatalf("cmdThreadHistory: %v", err)
		}
	})

	if !strings.Contains(output, "[master]") {
		t.Errorf("expected [master] in output, got: %s", output)
	}
	if !strings.Contains(output, "Hello") {
		t.Errorf("expected 'Hello' in output, got: %s", output)
	}
}

func TestCmdThreadHistoryEmpty(t *testing.T) {
	_, cleanup := setupTestRedis(t)
	defer cleanup()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&thID, "id", "", "")
	cmd.Flags().IntVar(&thTail, "tail", -1, "")
	thID = "empty-thread"

	output := captureOutput(func() {
		if err := cmdThreadHistory(cmd, nil); err != nil {
			t.Fatalf("cmdThreadHistory: %v", err)
		}
	})

	if output != "(no messages)" {
		t.Errorf("expected '(no messages)', got: %s", output)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// thread-state
// ═══════════════════════════════════════════════════════════════════════════════

func TestCmdThreadState(t *testing.T) {
	mr, cleanup := setupTestRedis(t)
	defer cleanup()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&tsID, "id", "", "")
	tsID = "my-thread"

	mr.HSet(tasklib.ThreadStateKey(tsID), "status", "implementing", "gh_repo", "owner/repo")

	output := captureOutput(func() {
		if err := cmdThreadState(cmd, nil); err != nil {
			t.Fatalf("cmdThreadState: %v", err)
		}
	})

	if !strings.Contains(output, `"status"`) || !strings.Contains(output, "implementing") {
		t.Errorf("expected status in output, got: %s", output)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// thread-update
// ═══════════════════════════════════════════════════════════════════════════════

func TestCmdThreadUpdate(t *testing.T) {
	mr, cleanup := setupTestRedis(t)
	defer cleanup()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&tuID, "id", "", "")
	cmd.Flags().StringVar(&tuStatus, "status", "", "")
	cmd.Flags().StringVar(&tuDesign, "design", "", "")
	cmd.Flags().IntVar(&tuPR, "pr", -1, "")
	tuID = "my-thread"
	tuStatus = "complete"
	tuDesign = "OAuth2 design"
	tuPR = 42

	mr.HSet(tasklib.ThreadStateKey(tuID), "status", "initiated")

	output := captureOutput(func() {
		if err := cmdThreadUpdate(cmd, nil); err != nil {
			t.Fatalf("cmdThreadUpdate: %v", err)
		}
	})

	if !strings.Contains(output, "Thread 'my-thread' updated") {
		t.Errorf("expected 'Thread updated', got: %s", output)
	}

	c := getClient()
	state, _ := c.RDB().HGetAll(context.Background(), tasklib.ThreadStateKey("my-thread")).Result()
	if state["status"] != "complete" {
		t.Errorf("expected status=complete, got %s", state["status"])
	}
	if state["last_design"] != "OAuth2 design" {
		t.Errorf("expected last_design, got %s", state["last_design"])
	}
	if state["gh_pr_number"] != "42" {
		t.Errorf("expected gh_pr_number=42, got %s", state["gh_pr_number"])
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// thread-list
// ═══════════════════════════════════════════════════════════════════════════════

func TestCmdThreadList(t *testing.T) {
	mr, cleanup := setupTestRedis(t)
	defer cleanup()

	mr.HSet(tasklib.ThreadStateKey("thread-1"), "status", "initiated", "updated_at", "2026-01-01T00:00:00Z")
	mr.HSet(tasklib.ThreadStateKey("thread-2"), "status", "complete", "updated_at", "2026-01-02T00:00:00Z")

	cmd := &cobra.Command{}

	output := captureOutput(func() {
		if err := cmdThreadList(cmd, nil); err != nil {
			t.Fatalf("cmdThreadList: %v", err)
		}
	})

	if !strings.Contains(output, "thread-1") || !strings.Contains(output, "thread-2") {
		t.Errorf("expected thread IDs in output, got: %s", output)
	}
}

func TestCmdThreadListEmpty(t *testing.T) {
	_, cleanup := setupTestRedis(t)
	defer cleanup()

	cmd := &cobra.Command{}

	output := captureOutput(func() {
		if err := cmdThreadList(cmd, nil); err != nil {
			t.Fatalf("cmdThreadList: %v", err)
		}
	})

	if output != "(no threads)" {
		t.Errorf("expected '(no threads)', got: %s", output)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// thread-cleanup
// ═══════════════════════════════════════════════════════════════════════════════

func TestCmdThreadCleanup(t *testing.T) {
	_, cleanup := setupTestRedis(t)
	defer cleanup()

	workspaceDir = t.TempDir()
	threadDir := workspaceDir + "/my-thread"
	os.MkdirAll(threadDir, 0755)
	os.WriteFile(threadDir+"/file.txt", []byte("test"), 0644)

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&tclID, "id", "", "")
	tclID = "my-thread"

	output := captureOutput(func() {
		if err := cmdThreadCleanup(cmd, nil); err != nil {
			t.Fatalf("cmdThreadCleanup: %v", err)
		}
	})

	if !strings.Contains(output, "Deleted") {
		t.Errorf("expected 'Deleted' in output, got: %s", output)
	}
	if _, err := os.Stat(threadDir); !os.IsNotExist(err) {
		t.Error("workspace should have been deleted")
	}
}

func TestCmdThreadCleanupNotExists(t *testing.T) {
	_, cleanup := setupTestRedis(t)
	defer cleanup()

	workspaceDir = t.TempDir()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&tclID, "id", "", "")
	tclID = "nonexistent-thread"

	output := captureOutput(func() {
		if err := cmdThreadCleanup(cmd, nil); err != nil {
			t.Fatalf("cmdThreadCleanup: %v", err)
		}
	})

	if !strings.Contains(output, "Nothing to clean up") {
		t.Errorf("expected 'Nothing to clean up', got: %s", output)
	}
}

func TestCmdThreadCleanupTraversalRejected(t *testing.T) {
	_, cleanup := setupTestRedis(t)
	defer cleanup()

	workspaceDir = t.TempDir()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&tclID, "id", "", "")
	tclID = "../escape"

	var panicMsg string
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicMsg = r.(string)
			}
		}()
		captureOutput(func() {
			cmdThreadCleanup(cmd, nil)
		})
	}()
	if !strings.Contains(panicMsg, "Invalid thread ID") {
		t.Errorf("expected 'Invalid thread ID', got: %s", panicMsg)
	}
}
