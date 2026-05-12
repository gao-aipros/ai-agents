package tasklib

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Task represents a task entity as stored in Redis.
type Task struct {
	TaskID      string `json:"task_id"`
	ThreadID    string `json:"thread_id,omitempty"`
	Instruction string `json:"instruction,omitempty"`
	Worker      string `json:"worker,omitempty"`
	Status      string `json:"status,omitempty"`
	Description string `json:"description,omitempty"`
	Result      string `json:"result,omitempty"`
	ExitCode    string `json:"exit_code,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
}

// TaskPayload is the JSON serialized into queue lists.
type TaskPayload struct {
	TaskID      string `json:"task_id"`
	ThreadID    string `json:"thread_id"`
	Instruction string `json:"instruction"`
}

// TaskInfo is the value stored in the active_tasks hash.
type TaskInfo struct {
	Status      string `json:"status"`
	Worker      string `json:"worker"`
	ThreadID    string `json:"thread_id"`
	StartedAt   string `json:"started_at"`
	WorkerHost  string `json:"worker_hostname,omitempty"`
}

// Enqueue pushes a task onto a worker queue. Byte-for-byte compatible with
// task.py cmd_enqueue: acquires thread lock, appends to thread history,
// LPUSHes to queue, initializes per-task keys. Returns the created task.
func (c *Client) Enqueue(ctx context.Context, worker, threadID, instruction string) (*Task, error) {
	taskID, err := newUUID()
	if err != nil {
		return nil, fmt.Errorf("generate task id: %w", err)
	}
	now := ts()

	// Acquire thread lock (serialize tasks on the same thread)
	lockKey := ThreadLockKey(threadID)
	ok, err := c.rdb.SetNX(ctx, lockKey, taskID, LockTTL).Result()
	if err != nil {
		return nil, fmt.Errorf("lock acquire: %w", err)
	}
	if !ok {
		holder, _ := c.rdb.Get(ctx, lockKey).Result()
		return nil, fmt.Errorf("thread '%s' is locked (holder task: %s). Wait for it to complete or run 'task unlock --thread %s'", threadID, holder, threadID)
	}

	// Append instruction to thread history
	msg, err := json.Marshal(map[string]interface{}{
		"role":    "master",
		"content": instruction,
		"timestamp": now,
		"metadata": map[string]string{"task_id": taskID},
	})
	if err != nil {
		c.rdb.Del(ctx, lockKey)
		return nil, fmt.Errorf("marshal message: %w", err)
	}
	if err := c.rdb.RPush(ctx, ThreadMessagesKey(threadID), string(msg)).Err(); err != nil {
		c.rdb.Del(ctx, lockKey) // best-effort rollback
		return nil, fmt.Errorf("thread history append: %w", err)
	}
	c.rdb.Expire(ctx, ThreadMessagesKey(threadID), TTLThread)

	// Enqueue task
	payload, err := json.Marshal(TaskPayload{
		TaskID:      taskID,
		ThreadID:    threadID,
		Instruction: instruction,
	})
	if err != nil {
		c.rdb.Del(ctx, lockKey)
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	if err := c.rdb.LPush(ctx, QueueKey(worker), string(payload)).Err(); err != nil {
		c.rdb.Del(ctx, lockKey)
		return nil, fmt.Errorf("queue push: %w", err)
	}

	// Initialize task keys
	pipe := c.rdb.Pipeline()
	pipe.Set(ctx, TaskKey(taskID, "status"), "pending", TTLTask)
	pipe.Set(ctx, TaskKey(taskID, "worker"), worker, TTLTask)
	pipe.Set(ctx, TaskKey(taskID, "thread_id"), threadID, TTLTask)
	pipe.Set(ctx, TaskKey(taskID, "description"), instruction, TTLTask)
	pipe.Set(ctx, TaskKey(taskID, "created_at"), now, TTLTask)
	if _, err := pipe.Exec(ctx); err != nil {
		c.rdb.Del(ctx, lockKey)
		return nil, fmt.Errorf("task init: %w", err)
	}

	return &Task{
		TaskID:      taskID,
		ThreadID:    threadID,
		Instruction: instruction,
		Worker:      worker,
		Status:      "pending",
		Description: instruction,
		CreatedAt:   now,
	}, nil
}

// GetTask retrieves a task by ID from Redis.
func (c *Client) GetTask(ctx context.Context, taskID string) (*Task, error) {
	keys := []string{"status", "worker", "thread_id", "description", "result", "exit_code", "created_at", "completed_at"}
	pipe := c.rdb.Pipeline()
	cmds := make([]*redis.StringCmd, len(keys))
	for i, k := range keys {
		cmds[i] = pipe.Get(ctx, TaskKey(taskID, k))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		// Exec only returns redis.Nil when every command in the pipeline
		// returned nil. Individual key misses are surfaced by the
		// per-command Result() calls below and are handled there.
	}

	t := &Task{TaskID: taskID}
	for i, k := range keys {
		val, _ := cmds[i].Result()
		switch k {
		case "status":
			t.Status = val
		case "worker":
			t.Worker = val
		case "thread_id":
			t.ThreadID = val
		case "description":
			t.Description = val
		case "result":
			t.Result = val
		case "exit_code":
			t.ExitCode = val
		case "created_at":
			t.CreatedAt = val
		case "completed_at":
			t.CompletedAt = val
		}
	}
	return t, nil
}

// GetTaskResult retrieves a task's result, optionally tailing to N lines.
func (c *Client) GetTaskResult(ctx context.Context, taskID string, tail int) (string, error) {
	result, err := c.rdb.Get(ctx, TaskKey(taskID, "result")).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if tail == 0 {
		return "", nil
	}
	if tail > 0 {
		lines := strings.Split(result, "\n")
		if len(lines) > tail {
			result = strings.Join(lines[len(lines)-tail:], "\n")
		}
	}
	return result, nil
}

// ListTasks retrieves tasks matching optional filters.
func (c *Client) ListTasks(ctx context.Context, worker, status, threadID string, limit, offset int) ([]*Task, error) {
	if limit <= 0 {
		limit = 50
	}

	taskMap := make(map[string]*Task)

	// Collect from active_tasks hash
	active, err := c.rdb.HGetAll(ctx, "active_tasks").Result()
	if err != nil {
		return nil, fmt.Errorf("active_tasks: %w", err)
	}
	for taskID, raw := range active {
		var info TaskInfo
		if err := json.Unmarshal([]byte(raw), &info); err != nil {
			info.Status = "unknown"
		}
		taskMap[taskID] = &Task{
			TaskID:    taskID,
			Status:    info.Status,
			Worker:    info.Worker,
			ThreadID:  info.ThreadID,
			StartedAt: info.StartedAt,
		}
	}

	// Scan for task:*:status keys
	var cursor uint64
	for {
		keys, nextCursor, err := c.rdb.Scan(ctx, cursor, "task:*:status", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		for _, key := range keys {
			taskID := strings.SplitN(key, ":", 3)[1]
			if _, exists := taskMap[taskID]; !exists {
				taskMap[taskID] = &Task{TaskID: taskID}
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	// Enrich from per-task keys and apply filters
	var rows []*Task
	for _, task := range taskMap {
		// Fill missing fields from Redis
		if task.Status == "" {
			task.Status, _ = c.rdb.Get(ctx, TaskKey(task.TaskID, "status")).Result()
			if task.Status == "" {
				task.Status = "unknown"
			}
		}
		if task.Worker == "" {
			task.Worker, _ = c.rdb.Get(ctx, TaskKey(task.TaskID, "worker")).Result()
			if task.Worker == "" {
				task.Worker = "-"
			}
		}
		if task.ThreadID == "" {
			task.ThreadID, _ = c.rdb.Get(ctx, TaskKey(task.TaskID, "thread_id")).Result()
			if task.ThreadID == "" {
				task.ThreadID = "-"
			}
		}
		if task.StartedAt == "" {
			task.StartedAt, _ = c.rdb.Get(ctx, TaskKey(task.TaskID, "created_at")).Result()
			if task.StartedAt == "" {
				task.StartedAt = "-"
			}
		}

		// Apply filters
		if worker != "" && task.Worker != worker {
			continue
		}
		if status != "" && task.Status != status {
			continue
		}
		if threadID != "" && task.ThreadID != threadID {
			continue
		}
		rows = append(rows, task)
	}

	// Sort by task ID for deterministic pagination (matching Python sorted(tasks.keys()))
	sort.Slice(rows, func(i, j int) bool { return rows[i].TaskID < rows[j].TaskID })

	// Apply offset
	if offset > 0 && offset < len(rows) {
		rows = rows[offset:]
	} else if offset >= len(rows) {
		rows = nil
	}

	// Trim to limit
	if len(rows) > limit {
		rows = rows[:limit]
	}

	return rows, nil
}

// WaitTask polls until the task reaches a terminal status or timeout expires.
// Releases the thread lock on timeout (same behavior as task.py finally block).
func (c *Client) WaitTask(ctx context.Context, taskID, threadID string, timeout time.Duration) (*Task, error) {
	exists, err := c.rdb.Exists(ctx, TaskKey(taskID, "status")).Result()
	if err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, fmt.Errorf("task %s not found", taskID)
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		status, err := c.rdb.Get(ctx, TaskKey(taskID, "status")).Result()
		if err != nil && err != redis.Nil {
			return nil, err
		}
		if status == "done" || status == "failed" || status == "cancelled" {
			// Build final status output
			t := &Task{TaskID: taskID}
			for _, k := range []string{"status", "worker", "thread_id", "exit_code", "created_at", "completed_at"} {
				val, _ := c.rdb.Get(ctx, TaskKey(taskID, k)).Result()
				switch k {
				case "status":
					t.Status = val
				case "worker":
					t.Worker = val
				case "thread_id":
					t.ThreadID = val
				case "exit_code":
					t.ExitCode = val
				case "created_at":
					t.CreatedAt = val
				case "completed_at":
					t.CompletedAt = val
				}
			}
			return t, nil
		}
		if time.Now().After(deadline) {
			// Release thread lock on timeout (same as task.py finally block)
			if threadID != "" {
				c.rdb.Del(ctx, ThreadLockKey(threadID))
			}
			return nil, fmt.Errorf("Timed out waiting for task %s (status: %s)", taskID, status)
		}

		select {
		case <-ctx.Done():
			if threadID != "" {
				c.rdb.Del(ctx, ThreadLockKey(threadID))
			}
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// CancelTask sets the cancel flag so workers check it before starting.
// Does not change task status — the worker transitions the task to
// "cancelled" when it dequeues and checks the flag. Matches task.py behavior.
func (c *Client) CancelTask(ctx context.Context, taskID string) error {
	exists, err := c.rdb.Exists(ctx, TaskKey(taskID, "status")).Result()
	if err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("task %s not found", taskID)
	}
	return c.rdb.Set(ctx, TaskKey(taskID, "cancel"), "1", TTLTask).Err()
}

// RequeueStale requeues stale in-flight tasks for a given worker type.
// Matches task.py cmd_requeue_stale behavior.
func (c *Client) RequeueStale(ctx context.Context, worker string, olderThan time.Duration) ([]string, error) {
	var requeued []string

	processingKey := ProcessingKey(worker)
	queueKey := QueueKey(worker)

	items, err := c.rdb.LRange(ctx, processingKey, 0, -1).Result()
	if err != nil {
		return nil, err
	}

	for _, itemJSON := range items {
		var task TaskPayload
		if err := json.Unmarshal([]byte(itemJSON), &task); err != nil {
			// Corrupt entry — remove it
			c.rdb.LRem(ctx, processingKey, 0, itemJSON)
			continue
		}

		taskStatus, _ := c.rdb.Get(ctx, TaskKey(task.TaskID, "status")).Result()
		createdAt, _ := c.rdb.Get(ctx, TaskKey(task.TaskID, "created_at")).Result()

		requeue := false

		if taskStatus == "" || taskStatus == "pending" {
			// Worker crashed before writing status, or after BLMOVE but before HSET
			requeue = true
		} else if taskStatus == "running" && createdAt != "" {
			started, parseErr := time.Parse("2006-01-02T15:04:05Z", createdAt)
			if parseErr == nil && time.Since(started) > olderThan {
				requeue = true
			}
		} else if taskStatus == "done" || taskStatus == "failed" || taskStatus == "cancelled" {
			// Terminal — garbage-collect stale processing entry
			c.rdb.LRem(ctx, processingKey, 0, itemJSON)
			continue
		}

		if requeue {
			c.rdb.LPush(ctx, queueKey, itemJSON)
			c.rdb.LRem(ctx, processingKey, 0, itemJSON)
			c.rdb.Set(ctx, TaskKey(task.TaskID, "status"), "pending", TTLTask)
			requeued = append(requeued, task.TaskID)
		}
	}

	return requeued, nil
}
