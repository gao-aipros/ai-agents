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

// ── DiscoverDescendants tests ────────────────────────────────────────────

func TestDiscoverDescendants_NoChildren(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "root-no-kids", "", "")

	desc, err := c.DiscoverDescendants(ctx(), "root-no-kids")
	if err != nil {
		t.Fatalf("DiscoverDescendants failed: %v", err)
	}
	if len(desc) != 0 {
		t.Errorf("expected 0 descendants, got %d", len(desc))
	}
}

func TestDiscoverDescendants_SingleChild(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "parent", "", "")
	c.CreateThread(ctx(), "child", "", "parent")

	desc, err := c.DiscoverDescendants(ctx(), "parent")
	if err != nil {
		t.Fatalf("DiscoverDescendants failed: %v", err)
	}
	if len(desc) != 1 {
		t.Errorf("expected 1 descendant, got %d", len(desc))
	}
	if !desc["child"] {
		t.Error("expected child in descendants")
	}
	if desc["parent"] {
		t.Error("parent should not be in its own descendants")
	}
}

func TestDiscoverDescendants_MultipleChildren(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "parent-2", "", "")
	c.CreateThread(ctx(), "child-a", "", "parent-2")
	c.CreateThread(ctx(), "child-b", "", "parent-2")

	desc, err := c.DiscoverDescendants(ctx(), "parent-2")
	if err != nil {
		t.Fatalf("DiscoverDescendants failed: %v", err)
	}
	if len(desc) != 2 {
		t.Errorf("expected 2 descendants, got %d", len(desc))
	}
	if !desc["child-a"] {
		t.Error("expected child-a in descendants")
	}
	if !desc["child-b"] {
		t.Error("expected child-b in descendants")
	}
}

func TestDiscoverDescendants_DeepChain(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "grandparent", "", "")
	c.CreateThread(ctx(), "parent-d", "", "grandparent")
	c.CreateThread(ctx(), "child-d", "", "parent-d")

	desc, err := c.DiscoverDescendants(ctx(), "grandparent")
	if err != nil {
		t.Fatalf("DiscoverDescendants failed: %v", err)
	}
	if len(desc) != 2 {
		t.Errorf("expected 2 descendants (parent-d + child-d), got %d", len(desc))
	}
	if !desc["parent-d"] {
		t.Error("expected parent-d in descendants")
	}
	if !desc["child-d"] {
		t.Error("expected child-d in descendants")
	}
}

func TestDiscoverDescendants_MultipleBranches(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "root-b", "", "")
	c.CreateThread(ctx(), "b-child-1", "", "root-b")
	c.CreateThread(ctx(), "b-child-2", "", "root-b")
	c.CreateThread(ctx(), "b-grandchild", "", "b-child-1")

	desc, err := c.DiscoverDescendants(ctx(), "root-b")
	if err != nil {
		t.Fatalf("DiscoverDescendants failed: %v", err)
	}
	if len(desc) != 3 {
		t.Errorf("expected 3 descendants, got %d", len(desc))
	}
}

func TestDiscoverDescendants_Cycle(t *testing.T) {
	c, _ := setupTestClient(t)

	// Create threads with a cycle: A→B, B→A
	c.CreateThread(ctx(), "cycle-a", "", "")
	c.CreateThread(ctx(), "cycle-b", "", "cycle-a")

	// Manually set cycle-b's parent back to create the cycle
	// Thread A has child B via parent_thread_id, now make B the parent of A
	c.UpdateThread(ctx(), "cycle-a", map[string]string{"parent_thread_id": "cycle-b"})

	// Should not loop infinitely
	desc, err := c.DiscoverDescendants(ctx(), "cycle-a")
	if err != nil {
		t.Fatalf("DiscoverDescendants with cycle failed: %v", err)
	}
	// B is child of A (via B's parent_thread_id="cycle-a")
	// A is child of B (via A's parent_thread_id="cycle-b")
	// BFS: start with cycle-a → visits cycle-b → visits cycle-a (already visited, skip)
	if !desc["cycle-b"] {
		t.Error("expected cycle-b in descendants")
	}
	// cycle-a should NOT be in its own descendants even though B→A
	if desc["cycle-a"] {
		t.Error("cycle-a should not be in its own descendants")
	}
}

func TestDiscoverDescendants_ThreadWithNoThreadsAtAll(t *testing.T) {
	c, _ := setupTestClient(t)

	// No threads exist at all — should return empty
	desc, err := c.DiscoverDescendants(ctx(), "nonexistent")
	if err != nil {
		t.Fatalf("DiscoverDescendants failed: %v", err)
	}
	if len(desc) != 0 {
		t.Errorf("expected 0 descendants for nonexistent thread, got %d", len(desc))
	}
}

// ── DeleteThread cascade tests ──────────────────────────────────────────

func TestDeleteThread_Cascade(t *testing.T) {
	c, _ := setupTestClient(t)

	// Create thread A with children B, C. B has child D.
	c.CreateThread(ctx(), "cascade-a", "", "")
	c.CreateThread(ctx(), "cascade-b", "", "cascade-a")
	c.CreateThread(ctx(), "cascade-c", "", "cascade-a")
	c.CreateThread(ctx(), "cascade-d", "", "cascade-b")

	// Set session IDs and messages on children to verify their keys are removed
	c.SetThreadSessionID(ctx(), "cascade-b", "session-b")
	c.SetThreadSessionID(ctx(), "cascade-c", "session-c")
	c.SetThreadSessionID(ctx(), "cascade-d", "session-d")
	c.AppendMessage(ctx(), "cascade-b", Message{Role: "user", Content: "hello"})
	c.AppendMessage(ctx(), "cascade-d", Message{Role: "user", Content: "hi"})

	// Verify all 4 threads exist before delete
	for _, id := range []string{"cascade-a", "cascade-b", "cascade-c", "cascade-d"} {
		exists, err := c.ThreadExists(ctx(), id)
		if err != nil {
			t.Fatalf("ThreadExists(%s): %v", id, err)
		}
		if !exists {
			t.Fatalf("thread %s should exist before delete", id)
		}
	}

	// Delete root thread A — cascade should delete B, C, D too
	if err := c.DeleteThread(ctx(), "cascade-a"); err != nil {
		t.Fatalf("DeleteThread cascade failed: %v", err)
	}

	// Verify none of A, B, C, D exist
	for _, id := range []string{"cascade-a", "cascade-b", "cascade-c", "cascade-d"} {
		exists, err := c.ThreadExists(ctx(), id)
		if err != nil {
			t.Fatalf("ThreadExists(%s) after delete: %v", id, err)
		}
		if exists {
			t.Errorf("thread %s should not exist after cascade delete", id)
		}
	}

	// Verify session IDs for children are cleared
	for _, id := range []string{"cascade-b", "cascade-c", "cascade-d"} {
		sid, _ := c.GetThreadSessionID(ctx(), id)
		if sid != "" {
			t.Errorf("session_id for %s should be empty after delete, got %q", id, sid)
		}
	}
}

func TestDeleteThread_NoChildren(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "no-kids", "", "")
	c.SetThreadSessionID(ctx(), "no-kids", "session-x")

	exists, _ := c.ThreadExists(ctx(), "no-kids")
	if !exists {
		t.Fatal("thread should exist before delete")
	}

	if err := c.DeleteThread(ctx(), "no-kids"); err != nil {
		t.Fatalf("DeleteThread no-children failed: %v", err)
	}

	exists, _ = c.ThreadExists(ctx(), "no-kids")
	if exists {
		t.Error("thread should not exist after delete")
	}

	sid, _ := c.GetThreadSessionID(ctx(), "no-kids")
	if sid != "" {
		t.Errorf("session_id should be empty after delete, got %q", sid)
	}
}

func TestDeleteThread_PartialRedisFailure(t *testing.T) {
	c, mr := setupTestClient(t)

	// Create parent + child
	c.CreateThread(ctx(), "partial-root", "", "")
	c.CreateThread(ctx(), "partial-child", "", "partial-root")

	// Verify both exist
	exists, _ := c.ThreadExists(ctx(), "partial-root")
	if !exists {
		t.Fatal("root should exist")
	}
	exists, _ = c.ThreadExists(ctx(), "partial-child")
	if !exists {
		t.Fatal("child should exist")
	}

	// Close miniredis to simulate Redis failure during deletion
	mr.Close()

	err := c.DeleteThread(ctx(), "partial-root")
	if err == nil {
		t.Error("expected error when Redis is down")
	}

	// The error should be about discovering descendants (ListThreads fails first)
	// This tests that the failure path is handled without panics.
}

// ── DeleteThreadKnown tests ─────────────────────────────────────────────

func TestDeleteThreadKnown_Cascade(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "dtn-a", "", "")
	c.CreateThread(ctx(), "dtn-b", "", "dtn-a")
	c.CreateThread(ctx(), "dtn-c", "", "dtn-a")
	c.CreateThread(ctx(), "dtn-d", "", "dtn-b")

	c.SetThreadSessionID(ctx(), "dtn-b", "session-b")
	c.SetThreadSessionID(ctx(), "dtn-c", "session-c")
	c.AppendMessage(ctx(), "dtn-b", Message{Role: "user", Content: "hello"})

	// Pre-discover descendants
	desc, err := c.DiscoverDescendants(ctx(), "dtn-a")
	if err != nil {
		t.Fatalf("DiscoverDescendants: %v", err)
	}

	// Delete via DeleteThreadKnown
	if err := c.DeleteThreadKnown(ctx(), "dtn-a", desc); err != nil {
		t.Fatalf("DeleteThreadKnown: %v", err)
	}

	// All 4 threads gone
	for _, id := range []string{"dtn-a", "dtn-b", "dtn-c", "dtn-d"} {
		exists, _ := c.ThreadExists(ctx(), id)
		if exists {
			t.Errorf("thread %s should not exist after cascade delete", id)
		}
	}
}

func TestDeleteThreadKnown_NoChildren(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "dtn-nokids", "", "")
	c.SetThreadSessionID(ctx(), "dtn-nokids", "session-x")

	desc, _ := c.DiscoverDescendants(ctx(), "dtn-nokids")

	if err := c.DeleteThreadKnown(ctx(), "dtn-nokids", desc); err != nil {
		t.Fatalf("DeleteThreadKnown: %v", err)
	}

	exists, _ := c.ThreadExists(ctx(), "dtn-nokids")
	if exists {
		t.Error("thread should not exist after delete")
	}
}

func TestDeleteThreadKnown_EmptyDescendantsMap(t *testing.T) {
	c, _ := setupTestClient(t)

	c.CreateThread(ctx(), "dtn-empty", "", "")

	// Pass nil descendants — should still delete the root
	if err := c.DeleteThreadKnown(ctx(), "dtn-empty", nil); err != nil {
		t.Fatalf("DeleteThreadKnown with nil descendants: %v", err)
	}

	exists, _ := c.ThreadExists(ctx(), "dtn-empty")
	if exists {
		t.Error("thread should not exist after delete")
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
