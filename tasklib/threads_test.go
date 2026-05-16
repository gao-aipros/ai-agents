package tasklib

import (
	"testing"
)

func TestDeleteThread(t *testing.T) {
	c, _ := setupTestClient(t)

	// Create a thread with data in all keys
	threadID := "delete-me"
	_, err := c.CreateThread(ctx(), threadID, "owner/repo")
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
