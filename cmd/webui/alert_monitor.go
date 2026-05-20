package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/noodle05/ai-agents/tasklib"
	"github.com/redis/go-redis/v9"
)

// runAlertMonitor is a background goroutine that polls for alert conditions:
// stale tasks and lost worker heartbeats. Best-effort — panics are recovered.
func runAlertMonitor(ctx context.Context, rdb *redis.Client, cfg tasklib.AlertConfig) {
	if !cfg.IsEnabled() {
		return
	}
	if !cfg.OnStuckThread && !cfg.OnWorkerLost {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			slog.Error("alert monitor panic recovered", "panic", r)
		}
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// In-memory dedup maps to avoid alert storms
	var (
		lastStuckAlert   = make(map[string]time.Time)
		lastOfflineAlert = make(map[string]time.Time)
	)
	alertCooldown := 5 * time.Minute

	var lastHeartbeatCheck time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if cfg.OnStuckThread {
			checkStuckTasks(ctx, rdb, cfg, lastStuckAlert, alertCooldown)
		}
		if cfg.OnWorkerLost {
			lastHeartbeatCheck = checkLostHeartbeats(ctx, rdb, cfg, lastHeartbeatCheck, lastOfflineAlert, alertCooldown)
		}
	}
}

func checkStuckTasks(ctx context.Context, rdb *redis.Client, cfg tasklib.AlertConfig, lastAlert map[string]time.Time, cooldown time.Duration) {
	if lastAlert == nil {
		lastAlert = make(map[string]time.Time)
	}
	cutoff := time.Now().Add(-cfg.StuckThreshold)
	now := time.Now()

	raw, err := rdb.HGetAll(ctx, "active_tasks").Result()
	if err != nil {
		slog.Warn("alert monitor: active_tasks scan failed", "error", err)
		return
	}
	for taskID, data := range raw {
		var info tasklib.TaskInfo
		if err := json.Unmarshal([]byte(data), &info); err != nil {
			continue
		}
		if info.Status != "running" || info.StartedAt == "" {
			continue
		}
		started, err := time.Parse("2006-01-02T15:04:05Z", info.StartedAt)
		if err != nil || started.After(cutoff) {
			continue
		}

		// Dedup: skip if we alerted within the cooldown period
		if prev, ok := lastAlert[taskID]; ok && now.Sub(prev) < cooldown {
			continue
		}
		lastAlert[taskID] = now

		slog.Warn("stuck task detected", "task_id", taskID, "worker", info.Worker, "started_at", info.StartedAt)
		cfg.SendAlert(ctx, tasklib.AlertThreadStuck, map[string]any{
			"task_id":         taskID,
			"thread_id":       info.ThreadID,
			"worker":          info.Worker,
			"worker_hostname": info.WorkerHost,
			"started_at":      info.StartedAt,
			"stale_minutes":   int64(time.Since(started).Minutes()),
		})
	}
}

func checkLostHeartbeats(ctx context.Context, rdb *redis.Client, cfg tasklib.AlertConfig, lastCheck time.Time, lastAlert map[string]time.Time, cooldown time.Duration) time.Time {
	// Only check heartbeats every 60s to avoid noisy false positives
	if time.Since(lastCheck) < 60*time.Second {
		return lastCheck
	}
	if lastAlert == nil {
		lastAlert = make(map[string]time.Time)
	}
	now := time.Now()

	for _, workerType := range tasklib.WorkerTypes {
		var cursor uint64
		for {
			keys, nextCursor, err := rdb.Scan(ctx, cursor, "worker:"+workerType+":*:heartbeat", 100).Result()
			if err != nil {
				break
			}
			for _, key := range keys {
				val, err := rdb.Get(ctx, key).Result()
				if err != nil {
					continue
				}
				var hb tasklib.HeartbeatData
				if err := json.Unmarshal([]byte(val), &hb); err != nil {
					continue
				}
				// Compare timestamp from HeartbeatData — reliable regardless
				// of TTL state, ObjectIdleTime, or other readers.
				if hb.LastHeartbeatAt == "" {
					continue
				}
				lastHB, err := time.Parse("2006-01-02T15:04:05Z", hb.LastHeartbeatAt)
				if err != nil {
					continue
				}
				if now.Sub(lastHB) < cfg.WorkerLostThreshold {
					continue
				}

				keyStr := workerType + ":" + hb.Hostname
				if prev, ok := lastAlert[keyStr]; ok && now.Sub(prev) < cooldown {
					continue
				}
				lastAlert[keyStr] = now

				slog.Warn("worker heartbeat lost", "worker_type", workerType, "hostname", hb.Hostname, "last_heartbeat", hb.LastHeartbeatAt)
				cfg.SendAlert(ctx, tasklib.AlertWorkerLost, map[string]any{
					"worker_type":     workerType,
					"hostname":        hb.Hostname,
					"last_heartbeat_at": hb.LastHeartbeatAt,
					"since_seconds":   int64(now.Sub(lastHB).Seconds()),
				})
			}
			cursor = nextCursor
			if cursor == 0 {
				break
			}
		}
	}
	return now
}
