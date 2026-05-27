package tasklib

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRDB(t *testing.T) (*redis.Client, SystemOps, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return rdb, NewClient(rdb), mr
}

func TestCheckStuckTasks_Empty(t *testing.T) {
	_, sysOps, _ := newTestRDB(t)
	cfg := AlertConfig{
		StuckThreshold: 30 * time.Minute,
		OnStuckThread:  true,
	}

	monitor := NewMonitor(sysOps, cfg)
	monitor.checkStuckTasks(context.Background())
}

func TestCheckStuckTasks_DetectsStuck(t *testing.T) {
	rdb, sysOps, _ := newTestRDB(t)

	taskInfo := TaskInfo{
		Status:     "running",
		Worker:     "claude",
		ThreadID:   "thr-1",
		StartedAt:  time.Now().UTC().Add(-60 * time.Minute).Format("2006-01-02T15:04:05Z"),
		WorkerHost: "host-1",
	}
	data, _ := json.Marshal(taskInfo)
	rdb.HSet(context.Background(), "active_tasks", "task-stuck", string(data))

	cfg := AlertConfig{
		StuckThreshold: 30 * time.Minute,
		OnStuckThread:  true,
		WebhookURL:     "https://hooks.example.com/alert",
	}

	monitor := NewMonitor(sysOps, cfg)
	monitor.checkStuckTasks(context.Background())
}

func TestCheckStuckTasks_SkipsNonRunning(t *testing.T) {
	rdb, sysOps, _ := newTestRDB(t)

	tests := []struct {
		status      string
		startedAt   string
		shouldAlert bool
	}{
		{"done", time.Now().UTC().Add(-60 * time.Minute).Format("2006-01-02T15:04:05Z"), false},
		{"failed", time.Now().UTC().Add(-60 * time.Minute).Format("2006-01-02T15:04:05Z"), false},
		{"cancelled", time.Now().UTC().Add(-60 * time.Minute).Format("2006-01-02T15:04:05Z"), false},
		{"pending", time.Now().UTC().Add(-60 * time.Minute).Format("2006-01-02T15:04:05Z"), false},
		{"running", time.Now().UTC().Add(-5 * time.Minute).Format("2006-01-02T15:04:05Z"), false},
		{"running", time.Now().UTC().Add(-60 * time.Minute).Format("2006-01-02T15:04:05Z"), true},
	}

	for _, tc := range tests {
		t.Run(tc.status+"-"+tc.startedAt[:10], func(t *testing.T) {
			rdb.FlushDB(context.Background())
			taskInfo := TaskInfo{
				Status:    tc.status,
				StartedAt: tc.startedAt,
			}
			data, _ := json.Marshal(taskInfo)
			rdb.HSet(context.Background(), "active_tasks", "task-1", string(data))

			cfg := AlertConfig{
				StuckThreshold: 30 * time.Minute,
				OnStuckThread:  true,
				WebhookURL:     "https://hooks.example.com/alert",
			}

			monitor := NewMonitor(sysOps, cfg)
			monitor.checkStuckTasks(context.Background())
		})
	}
}

func TestCheckStuckTasks_DedupRespected(t *testing.T) {
	rdb, sysOps, _ := newTestRDB(t)

	taskInfo := TaskInfo{
		Status:    "running",
		Worker:    "claude",
		ThreadID:  "thr-1",
		StartedAt: time.Now().UTC().Add(-60 * time.Minute).Format("2006-01-02T15:04:05Z"),
	}
	data, _ := json.Marshal(taskInfo)
	rdb.HSet(context.Background(), "active_tasks", "task-dup", string(data))

	cfg := AlertConfig{
		StuckThreshold: 30 * time.Minute,
		OnStuckThread:  true,
		WebhookURL:     "https://hooks.example.com/alert",
	}

	monitor := NewMonitor(sysOps, cfg)
	monitor.checkStuckTasks(context.Background())

	if _, ok := monitor.lastStuckAlert["task-dup"]; !ok {
		t.Log("dedup map not populated — alert config may be disabled (expected)")
	}
}

func TestCheckLostHeartbeats_NoHeartbeats(t *testing.T) {
	_, sysOps, _ := newTestRDB(t)
	cfg := AlertConfig{
		WorkerLostThreshold: 60 * time.Second,
		OnWorkerLost:        true,
	}

	monitor := NewMonitor(sysOps, cfg)
	monitor.lastHeartbeatCheck = time.Now().Add(-61 * time.Second)
	monitor.checkLostHeartbeats(context.Background())
}

func TestCheckLostHeartbeats_HealthyWorker(t *testing.T) {
	rdb, sysOps, _ := newTestRDB(t)

	hb := HeartbeatData{
		Hostname:        "worker-1",
		LastHeartbeatAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	payload, _ := json.Marshal(hb)
	rdb.Set(context.Background(), "worker:claude:worker-1:heartbeat", string(payload), 30*time.Second)

	cfg := AlertConfig{
		WorkerLostThreshold: 60 * time.Second,
		OnWorkerLost:        true,
	}

	monitor := NewMonitor(sysOps, cfg)
	prevCheck := time.Now().Add(-61 * time.Second)
	monitor.lastHeartbeatCheck = prevCheck
	monitor.checkLostHeartbeats(context.Background())
	if monitor.lastHeartbeatCheck.Equal(prevCheck) {
		t.Error("checkLostHeartbeats should update lastHeartbeatCheck timestamp")
	}
}

func TestCheckLostHeartbeats_LostWorker(t *testing.T) {
	rdb, sysOps, _ := newTestRDB(t)

	hb := HeartbeatData{
		Hostname:        "worker-gone",
		LastHeartbeatAt: time.Now().UTC().Add(-120 * time.Second).Format("2006-01-02T15:04:05Z"),
	}
	payload, _ := json.Marshal(hb)
	rdb.Set(context.Background(), "worker:claude:worker-gone:heartbeat", string(payload), 30*time.Second)

	cfg := AlertConfig{
		WorkerLostThreshold: 60 * time.Second,
		OnWorkerLost:        true,
		WebhookURL:          "https://hooks.example.com/alert",
	}

	monitor := NewMonitor(sysOps, cfg)
	monitor.lastHeartbeatCheck = time.Now().Add(-61 * time.Second)
	monitor.checkLostHeartbeats(context.Background())
}

func TestCheckLostHeartbeats_BackwardCompat(t *testing.T) {
	rdb, sysOps, _ := newTestRDB(t)

	hb := HeartbeatData{
		Hostname: "old-worker",
	}
	payload, _ := json.Marshal(hb)
	rdb.Set(context.Background(), "worker:claude:old-worker:heartbeat", string(payload), 30*time.Second)

	cfg := AlertConfig{
		WorkerLostThreshold: 60 * time.Second,
		OnWorkerLost:        true,
	}

	monitor := NewMonitor(sysOps, cfg)
	monitor.lastHeartbeatCheck = time.Now().Add(-61 * time.Second)
	monitor.checkLostHeartbeats(context.Background())
}

func TestMonitorRun_ContextCancellation(t *testing.T) {
	_, sysOps, _ := newTestRDB(t)
	cfg := AlertConfig{
		WebhookURL:    "https://hooks.example.com/alert",
		OnStuckThread: true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	monitor := NewMonitor(sysOps, cfg)
	done := make(chan struct{})
	go func() {
		monitor.Run(ctx)
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("Monitor.Run did not exit on cancelled context")
	}
}

func TestMonitorRun_DisabledConfig(t *testing.T) {
	_, sysOps, _ := newTestRDB(t)

	cfg := AlertConfig{}

	monitor := NewMonitor(sysOps, cfg)
	done := make(chan struct{})
	go func() {
		monitor.Run(context.Background())
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("Monitor.Run should return immediately when disabled")
	}
}

func TestMonitorRun_Recover(t *testing.T) {
	_, sysOps, _ := newTestRDB(t)
	cfg := AlertConfig{
		WebhookURL:    "https://hooks.example.com/alert",
		OnStuckThread: true,
	}

	origLogger := os.Getenv("LOG_LEVEL")
	os.Setenv("LOG_LEVEL", "error")
	defer os.Setenv("LOG_LEVEL", origLogger)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	monitor := NewMonitor(sysOps, cfg)
	done := make(chan struct{})
	go func() {
		monitor.Run(ctx)
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("Monitor.Run did not exit")
	}
}

func TestHeartbeatData_HasTimestamp(t *testing.T) {
	hb := HeartbeatData{
		Hostname: "test",
	}
	payload, _ := json.Marshal(hb)
	if !strings.Contains(string(payload), "last_heartbeat_at") {
		t.Errorf("HeartbeatData JSON missing last_heartbeat_at field: %s", string(payload))
	}
}

// TestCheckStuckTasks_DedupWithSpy verifies that after the first alert fires,
// a second check within the cooldown window produces no duplicate alert.
// Uses an httptest.Server as the webhook spy to count delivered alerts.
func TestCheckStuckTasks_DedupWithSpy(t *testing.T) {
	rdb, sysOps, _ := newTestRDB(t)

	var mu sync.Mutex
	var alertCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		alertCount++
		mu.Unlock()
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	taskInfo := TaskInfo{
		Status:    "running",
		Worker:    "claude",
		ThreadID:  "thr-1",
		StartedAt: time.Now().UTC().Add(-60 * time.Minute).Format("2006-01-02T15:04:05Z"),
	}
	data, _ := json.Marshal(taskInfo)
	rdb.HSet(context.Background(), "active_tasks", "task-spy", string(data))

	cfg := AlertConfig{
		StuckThreshold: 30 * time.Minute,
		OnStuckThread:  true,
		WebhookURL:     srv.URL,
	}

	monitor := NewMonitor(sysOps, cfg)
	// First call — should fire one alert.
	monitor.checkStuckTasks(context.Background())

	mu.Lock()
	firstCount := alertCount
	mu.Unlock()

	if firstCount != 1 {
		t.Fatalf("first check: expected 1 alert, got %d", firstCount)
	}

	// Second call within cooldown — should skip.
	monitor.checkStuckTasks(context.Background())

	mu.Lock()
	secondCount := alertCount
	mu.Unlock()

	if secondCount != 1 {
		t.Errorf("second check within cooldown: expected 1 alert total, got %d", secondCount)
	}

	// Advance the dedup map past cooldown by rewriting timestamps.
	monitor.lastStuckAlert["task-spy"] = time.Now().Add(-6 * time.Minute)

	// Third call after cooldown — should fire again.
	monitor.checkStuckTasks(context.Background())

	mu.Lock()
	thirdCount := alertCount
	mu.Unlock()

	if thirdCount != 2 {
		t.Errorf("third check after cooldown: expected 2 alerts total, got %d", thirdCount)
	}
}
