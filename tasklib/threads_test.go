package tasklib

import (
	"testing"
)

func TestDeleteThread(t *testing.T) {
	c, _ := setupTestClient(t)

	// Create a thread with data in all keys
	threadID := "delete-me"
	_, err := c.CreateThread(ctx(), threadID, "owner/repo", "")
	if err != nil {
		t.Fatalf("CreateThread failed: %v", err)
	}
	c.SetThreadComplete(ctx(), threadID)
	c.SetThreadSessionID(ctx(), threadID, "session-123")
	c.AcquireRequestLock(ctx(), threadID, "req-1", LockTTL)
	c.AppendMessage(ctx(), threadID, Message{
		Role: "user", Type: "request", Content: "hello",
	})
	c.LockThread(ctx(), threadID, "task-1", LockTTL)
	c.UpdateThreadLastActivity(ctx(), threadID)

	// Verify keys exist before delete
	exists, err := c.ThreadExists(ctx(), threadID)
	if err != nil {
		t.Fatalf("ThreadExists: %v", err)
	}
	if !exists {
		t.Fatal("thread should exist before delete")
	}

	// Delete the thread
	if err := c.DeleteThread(ctx(), threadID); err != nil {
		t.Fatalf("DeleteThread failed: %v", err)
	}

	// Verify thread no longer exists
	exists, err = c.ThreadExists(ctx(), threadID)
	if err != nil {
		t.Fatalf("ThreadExists after delete: %v", err)
	}
	if exists {
		t.Error("thread should not exist after delete")
	}

	// Verify session ID is cleared
	sid, _ := c.GetThreadSessionID(ctx(), threadID)
	if sid != "" {
		t.Errorf("session_id should be empty after delete, got %q", sid)
	}
}

func TestDeleteThread_Nonexistent(t *testing.T) {
	c, _ := setupTestClient(t)

	// Deleting a nonexistent thread should not error (DEL is idempotent)
	if err := c.DeleteThread(ctx(), "nonexistent"); err != nil {
		t.Errorf("DeleteThread on nonexistent thread should not error: %v", err)
	}
}

func TestGetThreadHistoryTailForWorker(t *testing.T) {
	c, _ := setupTestClient(t)
	threadID := "th-worker-filter"

	c.CreateThread(ctx(), threadID, "", "")

	msgForClaude := Message{
		Role:      "master",
		Content:   "Instruction for claude",
		Metadata:  map[string]string{"task_id": "t1", "worker": "claude"},
	}
	msgForCopilot := Message{
		Role:      "master",
		Content:   "Instruction for copilot",
		Metadata:  map[string]string{"task_id": "t2", "worker": "copilot"},
	}
	msgNoMeta := Message{
		Role:    "master",
		Content: "Legacy message without worker metadata",
	}
	msgForClaude2 := Message{
		Role:      "master",
		Content:   "Second instruction for claude",
		Metadata:  map[string]string{"task_id": "t3", "worker": "claude"},
	}

	c.AppendMessage(ctx(), threadID, msgForClaude)
	c.AppendMessage(ctx(), threadID, msgForCopilot)
	c.AppendMessage(ctx(), threadID, msgNoMeta)
	c.AppendMessage(ctx(), threadID, msgForClaude2)

	t.Run("filters to only own worker messages", func(t *testing.T) {
		msgs, err := c.GetThreadHistoryTailForWorker(ctx(), threadID, 10, "claude")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Should see: claude, no-meta (pass-through), claude2 = 3 messages
		// Should NOT see: copilot
		if len(msgs) != 3 {
			t.Fatalf("expected 3 messages, got %d", len(msgs))
		}
		for _, m := range msgs {
			if m.Content == "Instruction for copilot" {
				t.Error("should not include copilot's message")
			}
		}
	})

	t.Run("all messages for other worker returns empty", func(t *testing.T) {
		msgs, err := c.GetThreadHistoryTailForWorker(ctx(), threadID, 10, "codex")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// codex has no tagged messages, but the no-metadata message passes through
		if len(msgs) != 1 {
			t.Fatalf("expected 1 message (no-metadata passthrough), got %d", len(msgs))
		}
		if msgs[0].Content != "Legacy message without worker metadata" {
			t.Errorf("expected legacy message, got %q", msgs[0].Content)
		}
	})

	t.Run("result truncated to tail", func(t *testing.T) {
		msgs, err := c.GetThreadHistoryTailForWorker(ctx(), threadID, 2, "claude")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(msgs) != 2 {
			t.Fatalf("expected 2 messages (tail cap), got %d", len(msgs))
		}
		// Should have the last 2 matching: no-meta and claude2
		if msgs[0].Content != "Legacy message without worker metadata" {
			t.Errorf("expected legacy msg first, got %q", msgs[0].Content)
		}
		if msgs[1].Content != "Second instruction for claude" {
			t.Errorf("expected claude2 msg last, got %q", msgs[1].Content)
		}
	})

	t.Run("no messages in thread returns nil", func(t *testing.T) {
		emptyThread := "th-empty"
		c.CreateThread(ctx(), emptyThread, "", "")
		msgs, err := c.GetThreadHistoryTailForWorker(ctx(), emptyThread, 10, "claude")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if msgs != nil {
			t.Errorf("expected nil, got %d messages", len(msgs))
		}
	})

	t.Run("tail <= 0 returns nil", func(t *testing.T) {
		msgs, err := c.GetThreadHistoryTailForWorker(ctx(), threadID, 0, "claude")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if msgs != nil {
			t.Errorf("expected nil for tail=0, got %d messages", len(msgs))
		}
	})
}

// ── parent-child thread tests ─────────────────────────────────────────────

func TestCreateThread_WithParent(t *testing.T) {
	c, _ := setupTestClient(t)

	thread, err := c.CreateThread(ctx(), "child-thread", "owner/repo", "parent-thread")
	if err != nil {
		t.Fatalf("CreateThread with parent: %v", err)
	}
	if thread.ParentThreadID != "parent-thread" {
		t.Errorf("ParentThreadID = %q, want %q", thread.ParentThreadID, "parent-thread")
	}
	if thread.ThreadID != "child-thread" {
		t.Errorf("ThreadID = %q, want %q", thread.ThreadID, "child-thread")
	}

	// Verify stored in Redis
	state, _ := c.rdb.HGetAll(ctx(), ThreadStateKey("child-thread")).Result()
	if state["parent_thread_id"] != "parent-thread" {
		t.Errorf("Redis parent_thread_id = %q, want %q", state["parent_thread_id"], "parent-thread")
	}
	if state["status"] != "initiated" {
		t.Errorf("Redis status = %q, want %q", state["status"], "initiated")
	}
}

func TestCreateThread_WithoutParent(t *testing.T) {
	c, _ := setupTestClient(t)

	thread, err := c.CreateThread(ctx(), "root-thread", "owner/repo", "")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if thread.ParentThreadID != "" {
		t.Errorf("ParentThreadID = %q, want empty", thread.ParentThreadID)
	}

	// Verify parent_thread_id is not stored in Redis
	state, _ := c.rdb.HGetAll(ctx(), ThreadStateKey("root-thread")).Result()
	if v, ok := state["parent_thread_id"]; ok {
		t.Errorf("parent_thread_id should not exist in Redis, got %q", v)
	}
}

func TestGetThread_ReturnsParentThreadID(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "child-1", "repo/x", "parent-abc")

	thread, err := c.GetThread(ctx(), "child-1")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.ParentThreadID != "parent-abc" {
		t.Errorf("ParentThreadID = %q, want %q", thread.ParentThreadID, "parent-abc")
	}
}

func TestGetThread_NoParent(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "root-1", "repo/x", "")

	thread, err := c.GetThread(ctx(), "root-1")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.ParentThreadID != "" {
		t.Errorf("ParentThreadID = %q, want empty", thread.ParentThreadID)
	}
}

func TestListThreads_ReturnsParentThreadID(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "root-thread", "repo/x", "")
	c.CreateThread(ctx(), "child-thread", "repo/y", "root-thread")

	threads, err := c.ListThreads(ctx(), "", "")
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}

	foundRoot := false
	foundChild := false
	for _, th := range threads {
		switch th.ThreadID {
		case "root-thread":
			foundRoot = true
			if th.ParentThreadID != "" {
				t.Errorf("root thread ParentThreadID = %q, want empty", th.ParentThreadID)
			}
		case "child-thread":
			foundChild = true
			if th.ParentThreadID != "root-thread" {
				t.Errorf("child thread ParentThreadID = %q, want %q", th.ParentThreadID, "root-thread")
			}
		}
	}
	if !foundRoot {
		t.Error("root-thread not found in list")
	}
	if !foundChild {
		t.Error("child-thread not found in list")
	}
}

func TestUpdateThread_ParentThreadIDPassedThrough(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "update-me", "", "old-parent")

	// Update with parent_thread_id field
	err := c.UpdateThread(ctx(), "update-me", map[string]string{
		"parent_thread_id": "new-parent",
		"status":           "complete",
	})
	if err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}

	state, _ := c.rdb.HGetAll(ctx(), ThreadStateKey("update-me")).Result()
	if state["parent_thread_id"] != "new-parent" {
		t.Errorf("parent_thread_id in Redis = %q, want %q", state["parent_thread_id"], "new-parent")
	}
	if state["status"] != "complete" {
		t.Errorf("status in Redis = %q, want %q", state["status"], "complete")
	}
}

func TestUpdateThread_UnrelatedFieldsDoNotWipeParent(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "keep-parent", "", "my-parent")

	// Update only status — parent_thread_id should be preserved by HSet (only updated_at is always set)
	err := c.UpdateThread(ctx(), "keep-parent", map[string]string{
		"status": "running",
	})
	if err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}

	state, _ := c.rdb.HGetAll(ctx(), ThreadStateKey("keep-parent")).Result()
	if state["parent_thread_id"] != "my-parent" {
		t.Errorf("parent_thread_id in Redis = %q, want %q", state["parent_thread_id"], "my-parent")
	}
	if state["status"] != "running" {
		t.Errorf("status in Redis = %q, want %q", state["status"], "running")
	}
}
