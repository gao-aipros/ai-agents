package tasklib

import (
	"testing"
)

// ── ScanKeys ─────────────────────────────────────────────────────────────────

func TestScanKeys_HappyPath(t *testing.T) {
	c, mr := setupTestClient(t)
	mr.Set("thread:a:lock", "holder-1")
	mr.Set("thread:b:lock", "holder-2")
	mr.Set("thread:c:data", "some-data")

	keys, err := c.ScanKeys(ctx(), "thread:*:lock", 100)
	if err != nil {
		t.Fatalf("ScanKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("expected 2 keys, got %d: %v", len(keys), keys)
	}
}

func TestScanKeys_Pagination(t *testing.T) {
	c, mr := setupTestClient(t)
	// Create enough keys to trigger multiple SCAN iterations (miniredis count=10).
	for i := range 25 {
		mr.Set("scan:page:key:"+string(rune('a'+i%26))+string(rune('0'+i/10)), "val")
	}

	keys, err := c.ScanKeys(ctx(), "scan:page:*", 10)
	if err != nil {
		t.Fatalf("ScanKeys: %v", err)
	}
	if len(keys) != 25 {
		t.Errorf("expected 25 keys, got %d", len(keys))
	}
}

func TestScanKeys_EmptyResult(t *testing.T) {
	c, _ := setupTestClient(t)

	keys, err := c.ScanKeys(ctx(), "nonexistent:*", 100)
	if err != nil {
		t.Fatalf("ScanKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("expected 0 keys, got %d", len(keys))
	}
}

// ── GetKey ───────────────────────────────────────────────────────────────────

func TestGetKey_HappyPath(t *testing.T) {
	c, mr := setupTestClient(t)
	mr.Set("some-key", "hello")

	val, err := c.GetKey(ctx(), "some-key")
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if val != "hello" {
		t.Errorf("expected 'hello', got %q", val)
	}
}

func TestGetKey_NilReturnsEmpty(t *testing.T) {
	c, _ := setupTestClient(t)

	val, err := c.GetKey(ctx(), "nonexistent")
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if val != "" {
		t.Errorf("expected empty string, got %q", val)
	}
}

// ── ActiveTaskCount ──────────────────────────────────────────────────────────

func TestActiveTaskCount_HappyPath(t *testing.T) {
	c, mr := setupTestClient(t)
	mr.HSet("active_tasks", "task-1", `{"status":"running"}`)
	mr.HSet("active_tasks", "task-2", `{"status":"running"}`)

	count, err := c.ActiveTaskCount(ctx())
	if err != nil {
		t.Fatalf("ActiveTaskCount: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2, got %d", count)
	}
}

func TestActiveTaskCount_Empty(t *testing.T) {
	c, _ := setupTestClient(t)

	count, err := c.ActiveTaskCount(ctx())
	if err != nil {
		t.Fatalf("ActiveTaskCount: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

// ── GetAllActiveTasks ────────────────────────────────────────────────────────

func TestGetAllActiveTasks_HappyPath(t *testing.T) {
	c, mr := setupTestClient(t)
	mr.HSet("active_tasks", "task-1", `{"status":"running"}`)

	all, err := c.GetAllActiveTasks(ctx())
	if err != nil {
		t.Fatalf("GetAllActiveTasks: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("expected 1 task, got %d", len(all))
	}
	if all["task-1"] != `{"status":"running"}` {
		t.Errorf("unexpected value: %s", all["task-1"])
	}
}

func TestGetAllActiveTasks_EmptyHash(t *testing.T) {
	c, _ := setupTestClient(t)

	all, err := c.GetAllActiveTasks(ctx())
	if err != nil {
		t.Fatalf("GetAllActiveTasks: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("expected empty map, got %d entries", len(all))
	}
}

// ── QueueDepth ───────────────────────────────────────────────────────────────

func TestQueueDepth_HappyPath(t *testing.T) {
	c, mr := setupTestClient(t)
	mr.Lpush("tasks:queue:claude", "task-1")
	mr.Lpush("tasks:queue:claude", "task-2")

	dep, err := c.QueueDepth(ctx(), QueueKey("claude"))
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if dep != 2 {
		t.Errorf("expected 2, got %d", dep)
	}
}

func TestQueueDepth_Empty(t *testing.T) {
	c, _ := setupTestClient(t)

	dep, err := c.QueueDepth(ctx(), QueueKey("nonexistent"))
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if dep != 0 {
		t.Errorf("expected 0, got %d", dep)
	}
}

// ── GetCounters ──────────────────────────────────────────────────────────────

func TestGetCounters_HappyPath(t *testing.T) {
	c, mr := setupTestClient(t)
	mr.Set("stats:task_total", "100")
	mr.Set("stats:task_done", "80")

	vals, err := c.GetCounters(ctx(), "stats:task_total", "stats:task_done", "stats:task_failed")
	if err != nil {
		t.Fatalf("GetCounters: %v", err)
	}
	if len(vals) != 3 {
		t.Fatalf("expected 3 values, got %d", len(vals))
	}
	if vals[0] != "100" {
		t.Errorf("expected '100', got %v", vals[0])
	}
	if vals[1] != "80" {
		t.Errorf("expected '80', got %v", vals[1])
	}
	if vals[2] != nil {
		t.Errorf("expected nil for missing key, got %v", vals[2])
	}
}

// ── Info ─────────────────────────────────────────────────────────────────────

func TestInfo_ReturnsNonEmpty(t *testing.T) {
	c, _ := setupTestClient(t)

	info, err := c.Info(ctx(), "clients")
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info == "" {
		t.Error("expected non-empty info string")
	}
}

// ── PersistMasterTokenStats ──────────────────────────────────────────────────

func TestClient_PersistMasterTokenStats(t *testing.T) {
	c, mr := setupTestClient(t)
	mr.HSet(ThreadStateKey("thread-1"), "status", "running")

	stats := TokenStats{
		InputTokens:      500,
		OutputTokens:     200,
		CacheReadTokens:  50,
		CacheWriteTokens: 10,
		ReasoningTokens:  30,
	}
	if err := c.PersistMasterTokenStats(ctx(), "thread-1", stats); err != nil {
		t.Fatalf("PersistMasterTokenStats: %v", err)
	}

	// Verify thread-level fields were incremented
	fields := mr.HGet(ThreadStateKey("thread-1"), "master_input_tokens")
	if fields != "500" {
		t.Errorf("expected master_input_tokens=500, got %s", fields)
	}

	// Verify global counters were incremented
	totalInput := mr.HGet(StatsTotalKey(), "input_tokens")
	if totalInput != "500" {
		t.Errorf("expected global input_tokens=500, got %s", totalInput)
	}
}

// ── Ping ─────────────────────────────────────────────────────────────────────

func TestPing(t *testing.T) {
	c, _ := setupTestClient(t)

	if err := c.Ping(ctx()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// ── Compile-time interface satisfaction ──────────────────────────────────────

func TestSystemOpsInterfaceSatisfaction(t *testing.T) {
	var _ SystemOps = (*Client)(nil)
}
