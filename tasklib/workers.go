package tasklib

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// WorkerInfo represents aggregated information about a worker type.
type WorkerInfo struct {
	Instances    int `json:"instances"`
	Online       int `json:"online"`
	TotalActive  int `json:"total_active"`
	TotalThreads int `json:"total_threads"`
}

// WorkerStats is the full per-worker-type stats map.
type WorkerStats map[string]*WorkerInfo

// WorkerInstance represents a single worker instance.
type WorkerInstance struct {
	WorkerName           string `json:"worker_name"`
	AgentType            string `json:"agent_type"`
	Role                 string `json:"role"`
	Hostname             string `json:"hostname"`
	TasksProcessed       int    `json:"tasks_processed"`
	QueueDepth           int    `json:"queue_depth"`
	UptimeSeconds        int64  `json:"uptime_seconds"`
	LastHeartbeatPayload string `json:"last_heartbeat"`
	Online               bool   `json:"online"`
}

// HeartbeatData is the JSON payload written into heartbeat keys.
type HeartbeatData struct {
	WorkerName      string `json:"worker_name"`
	AgentType       string `json:"agent_type"`
	Role            string `json:"role"`
	Hostname        string `json:"hostname"`
	TasksProcessed  int    `json:"tasks_processed"`
	QueueDepth      int    `json:"queue_depth"`
	UptimeSeconds   int64  `json:"uptime_seconds"`
	LastHeartbeatAt string `json:"last_heartbeat_at"`
}

// GetWorkerStats retrieves stats for all online workers via SCAN on heartbeat keys.
// Reports online count based on heartbeat key existence (TTL-based liveness).
func (c *Client) GetWorkerStats(ctx context.Context) (WorkerStats, error) {
	stats := make(WorkerStats)

	// Scan for heartbeat keys: worker:<name>:heartbeat
	var cursor uint64
	for {
		keys, nextCursor, err := c.rdb.Scan(ctx, cursor, "worker:*:heartbeat", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("scan workers: %w", err)
		}
		for _, key := range keys {
			workerName := ParseHeartbeatWorkerName(key)
			if workerName == "" {
				continue
			}

			// Check TTL — keys without TTL are stale
			ttl, err := c.rdb.TTL(ctx, key).Result()
			if err != nil || ttl <= 0 {
				c.PushSystemEvent(ctx, &Event{
					Type:           EventWorkerOffline,
					WorkerType:     workerName,
					WorkerHostname: "",
				})
				continue
			}

			s, ok := stats[workerName]
			if !ok {
				s = &WorkerInfo{}
				stats[workerName] = s
			}
			s.Instances++
			s.Online++
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	// Augment with active_tasks counts per worker
	active, _ := c.rdb.HGetAll(ctx, "active_tasks").Result()
	threadWorkers := make(map[string]map[string]struct{})

	for _, raw := range active {
		var info TaskInfo
		if err := json.Unmarshal([]byte(raw), &info); err != nil {
			continue
		}
		if s, ok := stats[info.Worker]; ok {
			s.TotalActive++
		}
		if info.ThreadID != "" && info.Worker != "" {
			if _, ok := threadWorkers[info.ThreadID]; !ok {
				threadWorkers[info.ThreadID] = make(map[string]struct{})
			}
			threadWorkers[info.ThreadID][info.Worker] = struct{}{}
		}
	}

	// Scan remaining tasks to capture threads not in active_tasks
	var tCursor uint64
	for {
		keys, nc, err := c.rdb.Scan(ctx, tCursor, "task:*:thread_id", 100).Result()
		if err != nil {
			break
		}
		for _, key := range keys {
			taskID := strings.SplitN(key, ":", 3)[1]
			threadID, _ := c.rdb.Get(ctx, key).Result()
			if threadID == "" {
				continue
			}
			worker, _ := c.rdb.Get(ctx, TaskKey(taskID, "worker")).Result()
			if worker == "" {
				continue
			}
			if _, ok := threadWorkers[threadID]; !ok {
				threadWorkers[threadID] = make(map[string]struct{})
			}
			threadWorkers[threadID][worker] = struct{}{}
		}
		tCursor = nc
		if nc == 0 {
			break
		}
	}

	// Count threads per worker
	for _, workers := range threadWorkers {
		for wt := range workers {
			if s, ok := stats[wt]; ok {
				s.TotalThreads++
			}
		}
	}

	return stats, nil
}

// ParseHeartbeatWorkerName extracts the worker name from a heartbeat key.
// Handles both old format (worker:<type>:<hostname>:heartbeat, 4 parts)
// and new format (worker:<name>:heartbeat, 3 parts).
func ParseHeartbeatWorkerName(key string) string {
	parts := strings.SplitN(key, ":", 4)
	if len(parts) == 4 && parts[3] == "heartbeat" {
		// Old format: worker:<type>:<hostname>:heartbeat
		return parts[1]
	}
	// New format: worker:<name>:heartbeat
	parts = strings.SplitN(key, ":", 3)
	if len(parts) == 3 && parts[2] == "heartbeat" {
		return parts[1]
	}
	return ""
}

// GetWorkerInfo returns detailed info for a single worker.
func (c *Client) GetWorkerInfo(ctx context.Context, workerName string) (*WorkerInfo, error) {
	stats, err := c.GetWorkerStats(ctx)
	if err != nil {
		return nil, err
	}
	info, ok := stats[workerName]
	if !ok {
		return &WorkerInfo{}, nil
	}
	return info, nil
}

// UpdateWorkerHeartbeat sets a heartbeat key with 30s TTL, writing a JSON
// payload with instance-level data (hostname, tasks_processed, etc.).
// Auto-populates LastHeartbeatAt if the caller leaves it empty.
func (c *Client) UpdateWorkerHeartbeat(ctx context.Context, workerName string, data HeartbeatData) error {
	if data.LastHeartbeatAt == "" {
		data.LastHeartbeatAt = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal heartbeat: %w", err)
	}
	return c.rdb.SetEx(ctx, HeartbeatKey(workerName), string(payload), 30*time.Second).Err()
}

// GetWorkerInstances returns per-worker detail by reading the single heartbeat
// key for this worker name. Falls back to SCAN for old-format keys
// (worker:<name>:*:heartbeat) when the new-format key is not found, for
// backward compatibility during the 30s deployment transition window.
func (c *Client) GetWorkerInstances(ctx context.Context, workerName string) ([]WorkerInstance, error) {
	key := HeartbeatKey(workerName)
	raw, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		if err.Error() == "redis: nil" {
			// Fall back to SCAN for old-format keys
			return c.getWorkerInstancesFallback(ctx, workerName)
		}
		return nil, fmt.Errorf("get worker instance: %w", err)
	}

	var hb HeartbeatData
	if err := json.Unmarshal([]byte(raw), &hb); err != nil {
		return nil, fmt.Errorf("unmarshal heartbeat: %w", err)
	}

	ttl, _ := c.rdb.TTL(ctx, key).Result()
	online := ttl > 0

	return []WorkerInstance{{
		WorkerName:           hb.WorkerName,
		AgentType:            hb.AgentType,
		Role:                 hb.Role,
		Hostname:             hb.Hostname,
		TasksProcessed:       hb.TasksProcessed,
		QueueDepth:           hb.QueueDepth,
		UptimeSeconds:        hb.UptimeSeconds,
		LastHeartbeatPayload: raw,
		Online:               online,
	}}, nil
}

// getWorkerInstancesFallback scans for old-format heartbeat keys
// (worker:<name>:<hostname>:heartbeat) when the new-format key is not found.
// Used during the 30s deployment transition window.
func (c *Client) getWorkerInstancesFallback(ctx context.Context, workerName string) ([]WorkerInstance, error) {
	pattern := "worker:" + workerName + ":*:heartbeat"
	keys, err := c.rdb.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, nil
	}
	var instances []WorkerInstance
	for _, key := range keys {
		raw, err := c.rdb.Get(ctx, key).Result()
		if err != nil {
			continue
		}
		var hb HeartbeatData
		if err := json.Unmarshal([]byte(raw), &hb); err != nil {
			// Old-format keys may have arbitrary payloads (e.g., just "{}")
			// Treat as a valid instance with just the worker name filled in.
			ttl, _ := c.rdb.TTL(ctx, key).Result()
			instances = append(instances, WorkerInstance{
				WorkerName: workerName,
				Online:     ttl > 0,
			})
			continue
		}
		ttl, _ := c.rdb.TTL(ctx, key).Result()
		instances = append(instances, WorkerInstance{
			WorkerName:           hb.WorkerName,
			AgentType:            hb.AgentType,
			Role:                 hb.Role,
			Hostname:             hb.Hostname,
			TasksProcessed:       hb.TasksProcessed,
			QueueDepth:           hb.QueueDepth,
			UptimeSeconds:        hb.UptimeSeconds,
			LastHeartbeatPayload: raw,
			Online:               ttl > 0,
		})
	}
	return instances, nil
}
