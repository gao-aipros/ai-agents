package tasklib

import (
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func setupTestClientWithRedis(t *testing.T) (*Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return NewClient(rdb), mr
}

func seedThread(t *testing.T, c *Client, threadID, status, updatedAt, createdAt, ghRepo, ghPRNumber, parentThreadID string) {
	t.Helper()
	key := ThreadStateKey(threadID)
	fields := map[string]interface{}{
		"status":           status,
		"updated_at":       updatedAt,
		"created_at":       createdAt,
		"gh_repo":          ghRepo,
		"gh_pr_number":     ghPRNumber,
		"parent_thread_id": parentThreadID,
	}
	if err := c.rdb.HSet(ctx(), key, fields).Err(); err != nil {
		t.Fatalf("HSet failed: %v", err)
	}
}

// ── ThreadScanner tests ─────────────────────────────────────────────────────

func TestScanEmptyKeyset(t *testing.T) {
	c, _ := setupTestClientWithRedis(t)

	results, err := c.Scan(ctx(), func(ts ThreadState) bool { return true })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil, got %v", results)
	}
}

func TestScanPredicateAllFalse(t *testing.T) {
	c, _ := setupTestClientWithRedis(t)

	seedThread(t, c, "t1", "active", "2025-01-01T00:00:00Z", "2025-01-01T00:00:00Z", "", "", "")
	seedThread(t, c, "t2", "active", "2025-01-01T00:00:00Z", "2025-01-01T00:00:00Z", "", "", "")
	seedThread(t, c, "t3", "complete", "2025-01-01T00:00:00Z", "2025-01-01T00:00:00Z", "", "", "")

	results, err := c.Scan(ctx(), func(ts ThreadState) bool { return false })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil, got %v", results)
	}
}

func TestScanPredicateAllTrue(t *testing.T) {
	c, _ := setupTestClientWithRedis(t)

	seedThread(t, c, "my-thread", "active",
		"2025-01-02T10:30:00Z",
		"2025-01-01T08:00:00Z",
		"owner/repo",
		"42",
		"parent-123",
	)

	results, err := c.Scan(ctx(), func(ts ThreadState) bool { return true })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	ts := results[0]
	if ts.ThreadID != "my-thread" {
		t.Errorf("ThreadID: expected my-thread, got %s", ts.ThreadID)
	}
	if ts.Status != "active" {
		t.Errorf("Status: expected active, got %s", ts.Status)
	}
	if ts.UpdatedAt != "2025-01-02T10:30:00Z" {
		t.Errorf("UpdatedAt: expected 2025-01-02T10:30:00Z, got %s", ts.UpdatedAt)
	}
	if ts.CreatedAt != "2025-01-01T08:00:00Z" {
		t.Errorf("CreatedAt: expected 2025-01-01T08:00:00Z, got %s", ts.CreatedAt)
	}
	if ts.GHRepo != "owner/repo" {
		t.Errorf("GHRepo: expected owner/repo, got %s", ts.GHRepo)
	}
	if ts.GHPRNumber != "42" {
		t.Errorf("GHPRNumber: expected 42, got %s", ts.GHPRNumber)
	}
	if ts.ParentThreadID != "parent-123" {
		t.Errorf("ParentThreadID: expected parent-123, got %s", ts.ParentThreadID)
	}
}

func TestScanPredicateFilters(t *testing.T) {
	c, _ := setupTestClientWithRedis(t)

	seedThread(t, c, "t1", "active", "2025-01-01T00:00:00Z", "2025-01-01T00:00:00Z", "", "", "")
	seedThread(t, c, "t2", "complete", "2025-01-01T00:00:00Z", "2025-01-01T00:00:00Z", "", "", "")
	seedThread(t, c, "t3", "active", "2025-01-01T00:00:00Z", "2025-01-01T00:00:00Z", "", "", "")
	seedThread(t, c, "t4", "error", "2025-01-01T00:00:00Z", "2025-01-01T00:00:00Z", "", "", "")

	results, err := c.Scan(ctx(), func(ts ThreadState) bool {
		return ts.Status == "active"
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, ts := range results {
		if ts.Status != "active" {
			t.Errorf("expected only active threads, got status=%s", ts.Status)
		}
	}
}

func TestScanMultipleCursors(t *testing.T) {
	c, _ := setupTestClientWithRedis(t)

	for i := 0; i < 150; i++ {
		threadID := fmt.Sprintf("t-%03d", i)
		seedThread(t, c, threadID, "active", "2025-01-01T00:00:00Z", "2025-01-01T00:00:00Z", "", "", "")
	}

	results, err := c.Scan(ctx(), func(ts ThreadState) bool { return true })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 150 {
		t.Errorf("expected 150 results, got %d", len(results))
	}
}

func TestScanMalformedKey(t *testing.T) {
	c, mr := setupTestClientWithRedis(t)

	// Add non-thread keys to verify they are ignored by the SCAN pattern filter.
	mr.Set("other_key", "value")
	mr.Set("malformed_key", "value")

	seedThread(t, c, "good-thread", "active", "2025-01-01T00:00:00Z", "2025-01-01T00:00:00Z", "", "", "")
	seedThread(t, c, "another-good", "complete", "2025-01-01T00:00:00Z", "2025-01-01T00:00:00Z", "", "", "")

	results, err := c.Scan(ctx(), func(ts ThreadState) bool { return true })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 good threads, got %d", len(results))
	}
}

func TestScanError(t *testing.T) {
	c, mr := setupTestClientWithRedis(t)

	seedThread(t, c, "t1", "active", "2025-01-01T00:00:00Z", "2025-01-01T00:00:00Z", "", "", "")

	// Close miniredis to simulate a connection error.
	mr.Close()

	_, err := c.Scan(ctx(), func(ts ThreadState) bool { return true })
	if err == nil {
		t.Error("expected error after closing redis, got nil")
	}
}
