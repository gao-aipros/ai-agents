package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/noodle05/ai-agents/tasklib"
	"github.com/redis/go-redis/v9"
)

type diagnosticsResource struct {
	rdb     *redis.Client
	scanner tasklib.ThreadScanner
}

// GET /api/diagnostics
func (dr *diagnosticsResource) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rdb := dr.rdb

	staleThreshold := 30 // minutes
	if s := r.URL.Query().Get("stale_threshold"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			staleThreshold = n
		}
	}

	diag := map[string]interface{}{}

	locks, err := listLocks(ctx, rdb)
	if err != nil {
		slog.Warn(fmt.Sprintf("[webui] diagnostics: lock scan error: %v", err))
		locks = nil
	}
	diag["locks"] = locks

	staleTasks, err := listStaleTasks(ctx, rdb, staleThreshold)
	if err != nil {
		slog.Warn(fmt.Sprintf("[webui] diagnostics: stale task scan error: %v", err))
		staleTasks = nil
	}
	diag["stale_tasks"] = staleTasks

	queueDepths := make(map[string]int64)
	for _, workerType := range tasklib.WorkerTypes {
		dep, err := rdb.LLen(ctx, tasklib.QueueKey(workerType)).Result()
		if err != nil {
			dep = -1
		}
		queueDepths[workerType] = dep
	}
	diag["queue_depths"] = queueDepths

	totalThreads, activeThreads, stuckThreads := countThreads(ctx, dr.scanner, staleThreshold)
	diag["threads_total"] = totalThreads
	diag["threads_active"] = activeThreads
	diag["threads_stuck"] = stuckThreads

	memInfo, err := redisMemoryInfo(ctx, rdb)
	if err != nil {
		slog.Warn(fmt.Sprintf("[webui] diagnostics: redis INFO error: %v", err))
		memInfo = nil
	}
	diag["redis_memory"] = memInfo

	keyCounts, err := keySpaceCounts(ctx, rdb)
	if err != nil {
		slog.Warn("key-space count error", "error", err)
	}
	diag["key_counts"] = keyCounts

	Respond(w, r, http.StatusOK, diag)
}

type lockInfo struct {
	ThreadID    string `json:"thread_id"`
	HolderTask  string `json:"holder_task"`
	LockedAt    string `json:"locked_at,omitempty"`
	HeldSeconds int64  `json:"held_seconds,omitempty"`
}

func listLocks(ctx context.Context, rdb *redis.Client) ([]lockInfo, error) {
	var locks []lockInfo
	var cursor uint64
	for {
		keys, nextCursor, err := rdb.Scan(ctx, cursor, "thread:*:lock", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			parts := strings.SplitN(key, ":", 3)
			if len(parts) < 2 {
				continue
			}
			threadID := parts[1]
			holder, err := rdb.Get(ctx, key).Result()
			if err != nil {
				continue
			}
			li := lockInfo{ThreadID: threadID, HolderTask: holder}
			lockedAtKey := tasklib.ThreadLockedAtKey(threadID)
			if lockedAt, err := rdb.Get(ctx, lockedAtKey).Result(); err == nil && lockedAt != "" {
				li.LockedAt = lockedAt
				if t, err := time.Parse("2006-01-02T15:04:05Z", lockedAt); err == nil {
					li.HeldSeconds = int64(time.Since(t).Seconds())
				}
			}
			locks = append(locks, li)
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	if locks == nil {
		locks = []lockInfo{}
	}
	return locks, nil
}

type staleTaskInfo struct {
	TaskID       string `json:"task_id"`
	Status       string `json:"status"`
	Worker       string `json:"worker"`
	WorkerHost   string `json:"worker_hostname"`
	StartedAt    string `json:"started_at"`
	StaleMinutes int64  `json:"stale_minutes"`
}

func listStaleTasks(ctx context.Context, rdb *redis.Client, thresholdMinutes int) ([]staleTaskInfo, error) {
	raw, err := rdb.HGetAll(ctx, "active_tasks").Result()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-time.Duration(thresholdMinutes) * time.Minute)
	var stale []staleTaskInfo
	for taskID, data := range raw {
		var info tasklib.TaskInfo
		if err := json.Unmarshal([]byte(data), &info); err != nil {
			continue
		}
		if info.Status != "running" {
			continue
		}
		if info.StartedAt == "" {
			continue
		}
		started, err := time.Parse("2006-01-02T15:04:05Z", info.StartedAt)
		if err != nil {
			continue
		}
		if started.Before(cutoff) {
			stale = append(stale, staleTaskInfo{
				TaskID:       taskID,
				Status:       info.Status,
				Worker:       info.Worker,
				WorkerHost:   info.WorkerHost,
				StartedAt:    info.StartedAt,
				StaleMinutes: int64(time.Since(started).Minutes()),
			})
		}
	}
	if stale == nil {
		stale = []staleTaskInfo{}
	}
	return stale, nil
}

func countThreads(ctx context.Context, scanner tasklib.ThreadScanner, staleThresholdMinutes int) (total, active, stuck int) {
	cutoff := time.Now().Add(-time.Duration(staleThresholdMinutes) * time.Minute)
	threads, err := scanner.Scan(ctx, func(ts tasklib.ThreadState) bool { return true })
	if err != nil {
		return
	}
	for _, ts := range threads {
		total++
		if ts.Status != "complete" && ts.Status != "error" && ts.Status != "cancelled" {
			active++
			if ts.UpdatedAt != "" {
				if t, err := time.Parse("2006-01-02T15:04:05Z", ts.UpdatedAt); err == nil && t.Before(cutoff) {
					stuck++
				}
			}
		}
	}
	return
}

func redisMemoryInfo(ctx context.Context, rdb *redis.Client) (map[string]string, error) {
	raw, err := rdb.Info(ctx, "memory").Result()
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "#") || !strings.Contains(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			result[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return result, nil
}

func keySpaceCounts(ctx context.Context, rdb *redis.Client) (map[string]int, error) {
	patterns := []string{"task:*", "thread:*", "worker:*", "stats:*", "system:*", "tasks:queue:*", "tasks:processing:*"}
	counts := make(map[string]int)
	for _, pattern := range patterns {
		var cursor uint64
		count := 0
		for {
			keys, nextCursor, err := rdb.Scan(ctx, cursor, pattern, 1000).Result()
			if err != nil {
				break
			}
			count += len(keys)
			cursor = nextCursor
			if cursor == 0 {
				break
			}
		}
		counts[pattern] = count
	}
	return counts, nil
}
