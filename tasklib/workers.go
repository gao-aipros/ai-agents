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
	Instances   int `json:"instances"`
	Online      int `json:"online"`
	TotalActive int `json:"total_active"`
}

// WorkerStats is the full per-worker-type stats map.
type WorkerStats map[string]*WorkerInfo

// WorkerInstance represents a single worker instance (hostname-level).
type WorkerInstance struct {
	Hostname        string `json:"hostname"`
	TasksProcessed  int    `json:"tasks_processed"`
	QueueDepth      int    `json:"queue_depth"`
	UptimeSeconds   int64  `json:"uptime_seconds"`
	LastHeartbeatPayload string `json:"last_heartbeat"`
	Online          bool   `json:"online"`
}

// HeartbeatData is the JSON payload written into heartbeat keys.
type HeartbeatData struct {
	Hostname       string `json:"hostname"`
	TasksProcessed int    `json:"tasks_processed"`
	QueueDepth     int    `json:"queue_depth"`
	UptimeSeconds  int64  `json:"uptime_seconds"`
}

// GetWorkerStats retrieves stats for all worker types via SCAN on heartbeat keys.
// Reports online count based on heartbeat key existence (TTL-based liveness).
func (c *Client) GetWorkerStats(ctx context.Context) (WorkerStats, error) {
	stats := make(WorkerStats)
	for _, wt := range WorkerTypes {
		stats[wt] = &WorkerInfo{}
	}

	// Scan for heartbeat keys: worker:<type>:<hostname>:heartbeat
	var cursor uint64
	for {
		keys, nextCursor, err := c.rdb.Scan(ctx, cursor, "worker:*:*:heartbeat", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("scan workers: %w", err)
		}
		for _, key := range keys {
			// key: worker:<type>:<hostname>:heartbeat
			parts := strings.SplitN(key, ":", 4)
			if len(parts) < 4 {
				continue
			}
			workerType := parts[1]
			// hostname := parts[2]

			// Check TTL — keys without TTL are stale
			ttl, err := c.rdb.TTL(ctx, key).Result()
			if err != nil || ttl <= 0 {
				continue
			}

			if s, ok := stats[workerType]; ok {
				s.Instances++
				s.Online++ // heartbeat key with positive TTL means online
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	// Augment with active_tasks counts per worker type
	active, err := c.rdb.HGetAll(ctx, "active_tasks").Result()
	if err == nil {
		for _, raw := range active {
			var info TaskInfo
			if err := json.Unmarshal([]byte(raw), &info); err != nil {
				continue
			}
			if s, ok := stats[info.Worker]; ok {
				s.TotalActive++
			}
		}
	}

	return stats, nil
}

// GetWorkerInfo returns detailed info for a single worker type.
func (c *Client) GetWorkerInfo(ctx context.Context, workerType string) (*WorkerInfo, error) {
	stats, err := c.GetWorkerStats(ctx)
	if err != nil {
		return nil, err
	}
	info, ok := stats[workerType]
	if !ok {
		return nil, fmt.Errorf("unknown worker type: %s", workerType)
	}
	return info, nil
}

// UpdateWorkerHeartbeat sets a heartbeat key with 30s TTL, writing a JSON
// payload with instance-level data (hostname, tasks_processed, etc.).
func (c *Client) UpdateWorkerHeartbeat(ctx context.Context, workerType, hostname string, data HeartbeatData) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal heartbeat: %w", err)
	}
	return c.rdb.SetEx(ctx, HeartbeatKey(workerType, hostname), string(payload), 30*time.Second).Err()
}

// GetWorkerInstances returns per-hostname detail for a worker type by parsing
// heartbeat JSON values from the existing SCAN.
func (c *Client) GetWorkerInstances(ctx context.Context, workerType string) ([]WorkerInstance, error) {
	pattern := fmt.Sprintf("worker:%s:*:heartbeat", workerType)
	var instances []WorkerInstance

	var cursor uint64
	for {
		keys, nextCursor, err := c.rdb.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return nil, fmt.Errorf("scan worker instances: %w", err)
		}
		for _, key := range keys {
			parts := strings.SplitN(key, ":", 4)
			if len(parts) < 4 {
				continue
			}
			hostname := parts[2]

			raw, err := c.rdb.Get(ctx, key).Result()
			if err != nil {
				continue
			}

			var hb HeartbeatData
			if err := json.Unmarshal([]byte(raw), &hb); err != nil {
				// Backward-compat: old format was literal "1"
				hb = HeartbeatData{Hostname: hostname}
			}

			ttl, _ := c.rdb.TTL(ctx, key).Result()
			online := ttl > 0

			instances = append(instances, WorkerInstance{
				Hostname:        hb.Hostname,
				TasksProcessed:  hb.TasksProcessed,
				QueueDepth:      hb.QueueDepth,
				UptimeSeconds:   hb.UptimeSeconds,
				LastHeartbeatPayload: raw,
				Online:          online,
			})
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return instances, nil
}
