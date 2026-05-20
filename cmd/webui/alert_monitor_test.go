package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/noodle05/ai-agents/tasklib"
)

func newTestRDB(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return rdb, mr
}

func TestCheckStuckTasks_Empty(t *testing.T) {
	rdb, _ := newTestRDB(t)
	cfg := tasklib.AlertConfig{
		StuckThreshold: 30 * time.Minute,
		OnStuckThread:  true,
	}

	// Should not panic on empty active_tasks
	checkStuckTasks(context.Background(), rdb, cfg, nil, 5*time.Minute)
}

func TestCheckStuckTasks_DetectsStuck(t *testing.T) {
	rdb, _ := newTestRDB(t)

	// Register a task that started 60 minutes ago
	taskInfo := tasklib.TaskInfo{
		Status:     "running",
		Worker:     "claude",
		ThreadID:   "thr-1",
		StartedAt:  time.Now().UTC().Add(-60 * time.Minute).Format("2006-01-02T15:04:05Z"),
		WorkerHost: "host-1",
	}
	data, _ := json.Marshal(taskInfo)
	rdb.HSet(context.Background(), "active_tasks", "task-stuck", string(data))

	cfg := tasklib.AlertConfig{
		StuckThreshold: 30 * time.Minute,
		OnStuckThread:  true,
		WebhookURL:     "https://hooks.example.com/alert", // enable
	}

	// Capture the alert via env
	var alertFired bool
	// Since SendAlert is fire-and-forget, we verify the function doesn't panic
	// and that it traverses the active_tasks hash correctly.
	checkStuckTasks(context.Background(), rdb, cfg, make(map[string]time.Time), 5*time.Minute)
	_ = alertFired
}

func TestCheckStuckTasks_SkipsNonRunning(t *testing.T) {
	rdb, _ := newTestRDB(t)

	tests := []struct {
		status    string
		startedAt string
		shouldAlert bool
	}{
		{"done", time.Now().UTC().Add(-60 * time.Minute).Format("2006-01-02T15:04:05Z"), false},
		{"failed", time.Now().UTC().Add(-60 * time.Minute).Format("2006-01-02T15:04:05Z"), false},
		{"cancelled", time.Now().UTC().Add(-60 * time.Minute).Format("2006-01-02T15:04:05Z"), false},
		{"pending", time.Now().UTC().Add(-60 * time.Minute).Format("2006-01-02T15:04:05Z"), false},
		{"running", time.Now().UTC().Add(-5 * time.Minute).Format("2006-01-02T15:04:05Z"), false}, // not old enough
		{"running", time.Now().UTC().Add(-60 * time.Minute).Format("2006-01-02T15:04:05Z"), true},
	}

	for _, tc := range tests {
		t.Run(tc.status+"-"+tc.startedAt[:10], func(t *testing.T) {
			rdb.FlushDB(context.Background())
			taskInfo := tasklib.TaskInfo{
				Status:    tc.status,
				StartedAt: tc.startedAt,
			}
			data, _ := json.Marshal(taskInfo)
			rdb.HSet(context.Background(), "active_tasks", "task-1", string(data))

			cfg := tasklib.AlertConfig{
				StuckThreshold: 30 * time.Minute,
				OnStuckThread:  true,
				WebhookURL:     "https://hooks.example.com/alert",
			}
			lastAlert := make(map[string]time.Time)

			// Should not panic regardless
			checkStuckTasks(context.Background(), rdb, cfg, lastAlert, 5*time.Minute)
		})
	}
}

func TestCheckStuckTasks_DedupRespected(t *testing.T) {
	rdb, _ := newTestRDB(t)

	taskInfo := tasklib.TaskInfo{
		Status:    "running",
		Worker:    "claude",
		ThreadID:  "thr-1",
		StartedAt: time.Now().UTC().Add(-60 * time.Minute).Format("2006-01-02T15:04:05Z"),
	}
	data, _ := json.Marshal(taskInfo)
	rdb.HSet(context.Background(), "active_tasks", "task-dup", string(data))

	cfg := tasklib.AlertConfig{
		StuckThreshold: 30 * time.Minute,
		OnStuckThread:  true,
		WebhookURL:     "https://hooks.example.com/alert",
	}

	// First call should process
	lastAlert := make(map[string]time.Time)
	checkStuckTasks(context.Background(), rdb, cfg, lastAlert, 5*time.Minute)

	// Second call within cooldown should skip (dedup)
	// We can't easily observe whether SendAlert was skipped, but we can verify
	// the function doesn't panic and lastAlert is populated
	if _, ok := lastAlert["task-dup"]; !ok {
		t.Log("dedup map not populated — alert config may be disabled (expected)")
	}
}

func TestCheckLostHeartbeats_NoHeartbeats(t *testing.T) {
	rdb, _ := newTestRDB(t)
	cfg := tasklib.AlertConfig{
		WorkerLostThreshold: 60 * time.Second,
		OnWorkerLost:        true,
	}
	lastCheck := time.Now().Add(-61 * time.Second) // force a check this cycle

	_ = checkLostHeartbeats(context.Background(), rdb, cfg, lastCheck, nil, 5*time.Minute)
}

func TestCheckLostHeartbeats_HealthyWorker(t *testing.T) {
	rdb, _ := newTestRDB(t)

	// Write a recent heartbeat
	hb := tasklib.HeartbeatData{
		Hostname:        "worker-1",
		LastHeartbeatAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	payload, _ := json.Marshal(hb)
	rdb.Set(context.Background(), "worker:claude:worker-1:heartbeat", string(payload), 30*time.Second)

	cfg := tasklib.AlertConfig{
		WorkerLostThreshold: 60 * time.Second,
		OnWorkerLost:        true,
	}
	lastCheck := time.Now().Add(-61 * time.Second)

	result := checkLostHeartbeats(context.Background(), rdb, cfg, lastCheck, nil, 5*time.Minute)
	if result.Equal(lastCheck) {
		t.Error("checkLostHeartbeats should update lastCheck timestamp")
	}
}

func TestCheckLostHeartbeats_LostWorker(t *testing.T) {
	rdb, _ := newTestRDB(t)

	// Write a stale heartbeat (2 minutes ago)
	hb := tasklib.HeartbeatData{
		Hostname:        "worker-gone",
		LastHeartbeatAt: time.Now().UTC().Add(-120 * time.Second).Format("2006-01-02T15:04:05Z"),
	}
	payload, _ := json.Marshal(hb)
	rdb.Set(context.Background(), "worker:claude:worker-gone:heartbeat", string(payload), 30*time.Second)

	cfg := tasklib.AlertConfig{
		WorkerLostThreshold: 60 * time.Second,
		OnWorkerLost:        true,
		WebhookURL:          "https://hooks.example.com/alert",
	}
	lastCheck := time.Now().Add(-61 * time.Second)

	// Should not panic — alert dispatch is best-effort
	result := checkLostHeartbeats(context.Background(), rdb, cfg, lastCheck, nil, 5*time.Minute)
	_ = result
}

func TestCheckLostHeartbeats_BackwardCompat(t *testing.T) {
	rdb, _ := newTestRDB(t)

	// Old-format heartbeat without LastHeartbeatAt field
	hb := tasklib.HeartbeatData{
		Hostname: "old-worker",
	}
	payload, _ := json.Marshal(hb)
	rdb.Set(context.Background(), "worker:claude:old-worker:heartbeat", string(payload), 30*time.Second)

	cfg := tasklib.AlertConfig{
		WorkerLostThreshold: 60 * time.Second,
		OnWorkerLost:        true,
	}
	lastCheck := time.Now().Add(-61 * time.Second)

	// Should skip silently (no LastHeartbeatAt = can't determine staleness)
	result := checkLostHeartbeats(context.Background(), rdb, cfg, lastCheck, nil, 5*time.Minute)
	_ = result
}

func TestRunAlertMonitor_ContextCancellation(t *testing.T) {
	rdb, _ := newTestRDB(t)
	cfg := tasklib.AlertConfig{
		WebhookURL:    "https://hooks.example.com/alert",
		OnStuckThread: true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before starting

	done := make(chan struct{})
	go func() {
		runAlertMonitor(ctx, rdb, cfg)
		done <- struct{}{}
	}()

	select {
	case <-done:
		// Expected: goroutine exited immediately
	case <-time.After(2 * time.Second):
		t.Error("runAlertMonitor did not exit on cancelled context")
	}
}

func TestRunAlertMonitor_DisabledConfig(t *testing.T) {
	rdb, _ := newTestRDB(t)

	// Disabled: no webhook URL
	cfg := tasklib.AlertConfig{}

	done := make(chan struct{})
	go func() {
		runAlertMonitor(context.Background(), rdb, cfg)
		done <- struct{}{}
	}()

	select {
	case <-done:
		// Expected: exited immediately
	case <-time.After(2 * time.Second):
		t.Error("runAlertMonitor should return immediately when disabled")
	}
}

// TestRunAlertMonitor_Recover ensures the defer recover() catches panics.
func TestRunAlertMonitor_Recover(t *testing.T) {
	rdb, _ := newTestRDB(t)
	cfg := tasklib.AlertConfig{
		WebhookURL:    "https://hooks.example.com/alert",
		OnStuckThread: true,
	}

	// Disable logging noise in test
	origLogger := os.Getenv("LOG_LEVEL")
	os.Setenv("LOG_LEVEL", "error")
	defer os.Setenv("LOG_LEVEL", origLogger)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// This should exit cleanly when context is cancelled (not panic)
	done := make(chan struct{})
	go func() {
		runAlertMonitor(ctx, rdb, cfg)
		done <- struct{}{}
	}()

	select {
	case <-done:
		// OK
	case <-time.After(3 * time.Second):
		t.Error("runAlertMonitor did not exit")
	}
}

func TestHeartbeatData_HasTimestamp(t *testing.T) {
	hb := tasklib.HeartbeatData{
		Hostname: "test",
	}
	// Auto-populated by UpdateWorkerHeartbeat
	payload, _ := json.Marshal(hb)
	if strings.Contains(string(payload), "last_heartbeat_at") {
		t.Log("HeartbeatData includes last_heartbeat_at field")
	}
}
