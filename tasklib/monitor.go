package tasklib

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// Monitor polls Redis for alert conditions: stuck tasks and lost worker
// heartbeats. All state is in-memory (dedup maps, cooldown timers).
// Construct with NewMonitor, then call Run in a goroutine.
type Monitor struct {
	sysOps             SystemOps
	cfg                AlertConfig
	lastStuckAlert     map[string]time.Time
	lastOfflineAlert   map[string]time.Time
	cooldown           time.Duration
	lastHeartbeatCheck time.Time
}

// NewMonitor creates a Monitor with the given dependencies.
// Returns a *Monitor ready to call Run().
func NewMonitor(sysOps SystemOps, cfg AlertConfig) *Monitor {
	return &Monitor{
		sysOps:           sysOps,
		cfg:              cfg,
		lastStuckAlert:   make(map[string]time.Time),
		lastOfflineAlert: make(map[string]time.Time),
		cooldown:         5 * time.Minute,
	}
}

// Run starts the background polling loop for alert conditions.
// It exits when ctx is cancelled. Panics are recovered and logged.
func (m *Monitor) Run(ctx context.Context) {
	if !m.cfg.IsEnabled() {
		return
	}
	if !m.cfg.OnStuckThread && !m.cfg.OnWorkerLost {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			slog.Error("alert monitor panic recovered", "panic", r)
		}
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if m.cfg.OnStuckThread {
			m.checkStuckTasks(ctx)
		}
		if m.cfg.OnWorkerLost {
			m.checkLostHeartbeats(ctx)
		}
	}
}

func (m *Monitor) checkStuckTasks(ctx context.Context) {
	cutoff := time.Now().Add(-m.cfg.StuckThreshold)
	now := time.Now()

	raw, err := m.sysOps.GetAllActiveTasks(ctx)
	if err != nil {
		slog.Warn("alert monitor: active_tasks scan failed", "error", err)
		return
	}
	for taskID, data := range raw {
		var info TaskInfo
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

		if prev, ok := m.lastStuckAlert[taskID]; ok && now.Sub(prev) < m.cooldown {
			continue
		}
		m.lastStuckAlert[taskID] = now

		slog.Warn("stuck task detected", "task_id", taskID, "worker", info.Worker, "started_at", info.StartedAt)
		m.cfg.SendAlert(ctx, AlertThreadStuck, map[string]any{
			"task_id":         taskID,
			"thread_id":       info.ThreadID,
			"worker":          info.Worker,
			"worker_hostname": info.WorkerHost,
			"started_at":      info.StartedAt,
			"stale_minutes":   int64(time.Since(started).Minutes()),
		})
	}
}

func (m *Monitor) checkLostHeartbeats(ctx context.Context) {
	if time.Since(m.lastHeartbeatCheck) < 60*time.Second {
		return
	}
	m.lastHeartbeatCheck = time.Now()
	now := m.lastHeartbeatCheck

	keys, err := m.sysOps.ScanKeys(ctx, "worker:*:heartbeat", 100)
	if err != nil {
		return
	}
	for _, key := range keys {
		workerName := ParseHeartbeatWorkerName(key)
		if workerName == "" {
			continue
		}
		val, err := m.sysOps.GetKey(ctx, key)
		if err != nil {
			continue
		}
		var hb HeartbeatData
		if err := json.Unmarshal([]byte(val), &hb); err != nil {
			continue
		}
		if hb.LastHeartbeatAt == "" {
			continue
		}
		lastHB, err := time.Parse("2006-01-02T15:04:05Z", hb.LastHeartbeatAt)
		if err != nil {
			continue
		}
		if now.Sub(lastHB) < m.cfg.WorkerLostThreshold {
			continue
		}

		keyStr := workerName + ":" + hb.Hostname
		if prev, ok := m.lastOfflineAlert[keyStr]; ok && now.Sub(prev) < m.cooldown {
			continue
		}
		m.lastOfflineAlert[keyStr] = now

		slog.Warn("worker heartbeat lost", "worker_name", workerName, "hostname", hb.Hostname, "last_heartbeat", hb.LastHeartbeatAt)
		m.cfg.SendAlert(ctx, AlertWorkerLost, map[string]any{
			"worker_name":       workerName,
			"hostname":          hb.Hostname,
			"last_heartbeat_at": hb.LastHeartbeatAt,
			"since_seconds":     int64(now.Sub(lastHB).Seconds()),
		})
	}
}
