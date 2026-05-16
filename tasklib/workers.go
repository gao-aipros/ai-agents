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

// UpdateWorkerHeartbeat sets a heartbeat key with 30s TTL.
func (c *Client) UpdateWorkerHeartbeat(ctx context.Context, workerType, hostname string) error {
	return c.rdb.SetEx(ctx, HeartbeatKey(workerType, hostname), "1", 30*time.Second).Err()
}

