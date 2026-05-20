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

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	var lastHeartbeatCheck time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if cfg.OnStuckThread {
			checkStuckTasks(ctx, rdb, cfg)
		}
		if cfg.OnWorkerLost {
			lastHeartbeatCheck = checkLostHeartbeats(ctx, rdb, cfg, lastHeartbeatCheck)
		}
	}
}

func checkStuckTasks(ctx context.Context, rdb *redis.Client, cfg tasklib.AlertConfig) {
	cutoff := time.Now().Add(-cfg.StuckThreshold)
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
		slog.Warn("stuck task detected", "task_id", taskID, "worker", info.Worker, "started_at", info.StartedAt)
		cfg.SendAlert(tasklib.AlertThreadStuck, map[string]any{
			"task_id":       taskID,
			"thread_id":     info.ThreadID,
			"worker":        info.Worker,
			"worker_hostname": info.WorkerHost,
			"started_at":    info.StartedAt,
			"stale_minutes": int64(time.Since(started).Minutes()),
		})
	}
}

func checkLostHeartbeats(ctx context.Context, rdb *redis.Client, cfg tasklib.AlertConfig, lastCheck time.Time) time.Time {
	// Only check heartbeats every 60s to avoid noisy alerts
	if time.Since(lastCheck) < 60*time.Second {
		return lastCheck
	}
	now := time.Now()
	for _, workerType := range tasklib.WorkerTypes {
		var cursor uint64
		for {
			pattern := "worker:" + workerType + ":*:heartbeat"
			keys, nextCursor, err := rdb.Scan(ctx, cursor, pattern, 100).Result()
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
					// Backward compat: old heartbeat value was literal "1"
					continue
				}
				// Check if heartbeat is recent enough by checking key TTL
				ttl := rdb.TTL(ctx, key).Val()
				if ttl <= 0 && ttl != -2 {
					// Heartbeat expired — key should have 30s TTL
					// If TTL is negative (key without TTL), check idle time
					idle := rdb.ObjectIdleTime(ctx, key).Val()
					if idle > cfg.WorkerLostThreshold {
						slog.Warn("worker heartbeat lost", "worker_type", workerType, "hostname", hb.Hostname)
						cfg.SendAlert(tasklib.AlertWorkerLost, map[string]any{
							"worker_type":    workerType,
							"hostname":       hb.Hostname,
							"last_seen_secs": int64(idle.Seconds()),
						})
					}
				}
			}
			cursor = nextCursor
			if cursor == 0 {
				break
			}
		}
	}
	return now
}
