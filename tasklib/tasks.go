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
	TaskID               string `json:"task_id"`
	ThreadID             string `json:"thread_id,omitempty"`
	Instruction          string `json:"instruction,omitempty"`
	Worker               string `json:"worker,omitempty"`
	Status               string `json:"status,omitempty"`
	Description          string `json:"description,omitempty"`
	Result               string `json:"result,omitempty"`
	ExitCode             string `json:"exit_code,omitempty"`
	EnqueuedAt           string `json:"enqueued_at,omitempty"`
	CompletedAt          string `json:"completed_at,omitempty"`
	StartedAt            string `json:"started_at,omitempty"`
	LastStartedAt        string `json:"last_started_at,omitempty"`
	WorkerHostname       string `json:"worker_hostname,omitempty"`
	RetryCount           string `json:"retry_count,omitempty"`
	ErrorMessage         string `json:"error_message,omitempty"`
	CorrelationID        string `json:"correlation_id,omitempty"`
	CancelledBy          string `json:"cancelled_by,omitempty"`
	CancelledAt          string `json:"cancelled_at,omitempty"`
	CancelledPrevStatus  string `json:"cancelled_previous_status,omitempty"`
}

// TaskPayload is the JSON serialized into queue lists.
type TaskPayload struct {
	TaskID      string `json:"task_id"`
	ThreadID    string `json:"thread_id"`
	Instruction string `json:"instruction"`
}

// GroupResult is returned by GroupWait when all tasks in a group reach a terminal state.
type GroupResult struct {
	ThreadID string            `json:"thread_id"`
	Label    string            `json:"label"`
	Status   string            `json:"status"` // complete | error | cancelled | timeout
	Tasks    map[string]string `json:"tasks"`  // taskID → status
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
	taskID, err := NewUUID()
	if err != nil {
		return nil, fmt.Errorf("generate task id: %w", err)
	}
	now := ts()

	// Acquire thread lock (serialize tasks on the same thread).
	// Auto-clear stale locks where the holder task no longer exists or has
	// reached a terminal state — this handles the case where a previous task
	// left a lock behind after cancellation or crash.
	lockKey := ThreadLockKey(threadID)
	ok, err := c.rdb.SetNX(ctx, lockKey, taskID, LockTTL).Result()
	if err != nil {
		return nil, fmt.Errorf("lock acquire: %w", err)
	}
	if !ok {
		holder, _ := c.rdb.Get(ctx, lockKey).Result()
		if !c.isTaskActive(ctx, holder) {
			if err := c.rdb.Del(ctx, lockKey).Err(); err != nil {
				return nil, fmt.Errorf("lock stale-clear delete: %w", err)
			}
			ok, err = c.rdb.SetNX(ctx, lockKey, taskID, LockTTL).Result()
			if err != nil {
				return nil, fmt.Errorf("lock acquire (after stale clear): %w", err)
			}
			if !ok {
				holder, _ = c.rdb.Get(ctx, lockKey).Result()
			}
		}
		if !ok {
			return nil, fmt.Errorf("thread '%s' is locked (holder task: %s). Wait for it to complete or run 'task unlock --thread %s'", threadID, holder, threadID)
		}
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

	// Initialize task keys + atomic counter
	pipe := c.rdb.Pipeline()
	pipe.Set(ctx, TaskKey(taskID, "status"), "pending", TTLTask)
	pipe.Set(ctx, TaskKey(taskID, "worker"), worker, TTLTask)
	pipe.Set(ctx, TaskKey(taskID, "thread_id"), threadID, TTLTask)
	pipe.Set(ctx, TaskKey(taskID, "description"), instruction, TTLTask)
	pipe.Set(ctx, TaskKey(taskID, "enqueued_at"), now, TTLTask)
	pipe.Incr(ctx, "stats:task_total")
	pipe.Expire(ctx, "stats:task_total", TTLStats)
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
		EnqueuedAt:  now,
	}, nil
}

// EnqueueGroup pushes a task to a worker queue as part of a named group.
// Uses a gate-check lock (acquired and immediately released) so multiple
// group tasks can fan out concurrently on the same thread. Group membership
// keys are set before the queue push — if they fail, the task is never
// dequeued, so there is no risk of lost tasks.
//
// WaitTask is safe for group tasks as of Phase 3 — it checks
// task:<id>:group on all exit paths and skips updateThreadStatus
// and lock release for group tasks. Thread status is managed solely
// by the request handler (writeResponseMessage / writeErrorMessage);
// GroupWait does NOT update it. Prefer GroupWait over WaitTask for
// group tasks to get aggregate status.
func (c *Client) EnqueueGroup(ctx context.Context, worker, threadID, groupLabel, instruction string) (*Task, error) {
	// Validate group label before any Redis operations
	if strings.ContainsAny(groupLabel, ":\t\n\r ") {
		return nil, fmt.Errorf("invalid group label %q: must not contain ':' or whitespace", groupLabel)
	}

	taskID, err := NewUUID()
	if err != nil {
		return nil, fmt.Errorf("generate task id: %w", err)
	}
	now := ts()

	// Gate-check: acquire thread lock momentarily to ensure no sequential
	// task holds it, then release immediately. Short TTL (10s) prevents
	// blocking sequential enqueues if DEL fails.
	lockKey := ThreadLockKey(threadID)
	ok, err := c.rdb.SetNX(ctx, lockKey, taskID, 10*time.Second).Result()
	if err != nil {
		return nil, fmt.Errorf("lock gate-check: %w", err)
	}
	if !ok {
		holder, _ := c.rdb.Get(ctx, lockKey).Result()
		if !c.isTaskActive(ctx, holder) {
			if err := c.rdb.Del(ctx, lockKey).Err(); err != nil {
				return nil, fmt.Errorf("lock stale-clear delete: %w", err)
			}
			ok, err = c.rdb.SetNX(ctx, lockKey, taskID, 10*time.Second).Result()
			if err != nil {
				return nil, fmt.Errorf("lock gate-check (after stale clear): %w", err)
			}
			if !ok {
				holder, _ = c.rdb.Get(ctx, lockKey).Result()
			}
		}
		if !ok {
			return nil, fmt.Errorf("thread '%s' is locked (holder task: %s). Wait for it to complete or run 'task unlock --thread %s'", threadID, holder, threadID)
		}
	}
	c.rdb.Del(ctx, lockKey)

	// Append instruction to thread history
	msg, err := json.Marshal(map[string]interface{}{
		"role":      "master",
		"content":   instruction,
		"timestamp": now,
		"metadata":  map[string]string{"task_id": taskID},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}
	if err := c.rdb.RPush(ctx, ThreadMessagesKey(threadID), string(msg)).Err(); err != nil {
		return nil, fmt.Errorf("thread history append: %w", err)
	}
	c.rdb.Expire(ctx, ThreadMessagesKey(threadID), TTLThread)

	// Prevent duplicate group membership
	if existing, _ := c.rdb.Get(ctx, TaskKey(taskID, "group")).Result(); existing != "" {
		return nil, fmt.Errorf("task %s is already in group %q", taskID, existing)
	}

	// Set group membership before pushing to queue — if these fail, the
	// task is never dequeued by workers, so no lost-task risk.
	groupSetKey := GroupTasksKey(threadID, groupLabel)
	if err := c.rdb.SAdd(ctx, groupSetKey, taskID).Err(); err != nil {
		return nil, fmt.Errorf("group SADD: %w", err)
	}
	c.rdb.Expire(ctx, groupSetKey, TTLThread)
	if err := c.rdb.Set(ctx, TaskKey(taskID, "group"), groupLabel, TTLTask).Err(); err != nil {
		c.rdb.SRem(ctx, groupSetKey, taskID) // best-effort rollback
		return nil, fmt.Errorf("group membership set: %w", err)
	}

	// Enqueue task
	payload, err := json.Marshal(TaskPayload{
		TaskID:      taskID,
		ThreadID:    threadID,
		Instruction: instruction,
	})
	if err != nil {
		c.rdb.SRem(ctx, groupSetKey, taskID) // best-effort rollback
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	if err := c.rdb.LPush(ctx, QueueKey(worker), string(payload)).Err(); err != nil {
		c.rdb.SRem(ctx, groupSetKey, taskID) // best-effort rollback
		return nil, fmt.Errorf("queue push: %w", err)
	}

	// Initialize task keys + atomic counter
	pipe := c.rdb.Pipeline()
	pipe.Set(ctx, TaskKey(taskID, "status"), "pending", TTLTask)
	pipe.Set(ctx, TaskKey(taskID, "worker"), worker, TTLTask)
	pipe.Set(ctx, TaskKey(taskID, "thread_id"), threadID, TTLTask)
	pipe.Set(ctx, TaskKey(taskID, "description"), instruction, TTLTask)
	pipe.Set(ctx, TaskKey(taskID, "enqueued_at"), now, TTLTask)
	pipe.Incr(ctx, "stats:task_total")
	pipe.Expire(ctx, "stats:task_total", TTLStats)
	if _, err := pipe.Exec(ctx); err != nil {
		c.rdb.SRem(ctx, groupSetKey, taskID) // best-effort rollback
		return nil, fmt.Errorf("task init: %w", err)
	}

	return &Task{
		TaskID:      taskID,
		ThreadID:    threadID,
		Instruction: instruction,
		Worker:      worker,
		Status:      "pending",
		Description: instruction,
		EnqueuedAt:  now,
	}, nil
}

// GetTask retrieves a task by ID from Redis.
func (c *Client) GetTask(ctx context.Context, taskID string) (*Task, error) {
	keys := []string{
		"status", "worker", "thread_id", "description", "result", "exit_code",
		"enqueued_at", "started_at", "last_started_at", "completed_at", "created_at",
		"worker_hostname", "retry_count", "error_message", "correlation_id",
		"cancelled_by", "cancelled_at", "cancelled_previous_status",
	}
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
		case "enqueued_at":
			t.EnqueuedAt = val
		case "started_at":
			t.StartedAt = val
		case "last_started_at":
			t.LastStartedAt = val
		case "completed_at":
			t.CompletedAt = val
		case "created_at": // TODO: remove after deploy + TTLTask (24h) migration window
			if t.EnqueuedAt == "" {
				t.EnqueuedAt = val
			}
		case "worker_hostname":
			t.WorkerHostname = val
		case "retry_count":
			t.RetryCount = val
		case "error_message":
			t.ErrorMessage = val
		case "correlation_id":
			t.CorrelationID = val
		case "cancelled_by":
			t.CancelledBy = val
		case "cancelled_at":
			t.CancelledAt = val
		case "cancelled_previous_status":
			t.CancelledPrevStatus = val
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
			task.StartedAt, _ = c.rdb.Get(ctx, TaskKey(task.TaskID, "started_at")).Result()
			if task.StartedAt == "" {
				task.StartedAt, _ = c.rdb.Get(ctx, TaskKey(task.TaskID, "enqueued_at")).Result()
				if task.StartedAt == "" {
					task.StartedAt, _ = c.rdb.Get(ctx, TaskKey(task.TaskID, "created_at")).Result()
					if task.StartedAt == "" {
						task.StartedAt = "-"
					}
				}
			}
		}
		if task.EnqueuedAt == "" {
			task.EnqueuedAt, _ = c.rdb.Get(ctx, TaskKey(task.TaskID, "enqueued_at")).Result()
			if task.EnqueuedAt == "" {
				// TODO: remove after deploy + TTLTask (24h) migration window
				task.EnqueuedAt, _ = c.rdb.Get(ctx, TaskKey(task.TaskID, "created_at")).Result()
				if task.EnqueuedAt == "" {
					task.EnqueuedAt = "-"
				}
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
// On terminal status, updates thread status (done→complete, failed→error,
// cancelled→cancelled) and releases the thread lock. Releases the lock on
// timeout/cancellation as well.
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
			for _, k := range []string{"status", "worker", "thread_id", "exit_code", "enqueued_at", "completed_at"} {
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
				case "enqueued_at":
					t.EnqueuedAt = val
				case "completed_at":
					t.CompletedAt = val
				}
			}
			// Update thread status and release lock on completion.
			// Group tasks: skip both — aggregate status is computed by
			// GroupWait once all group tasks complete, and the lock was
			// already released by EnqueueGroup.
			groupLabel, _ := c.rdb.Get(ctx, TaskKey(taskID, "group")).Result()
			if groupLabel == "" {
				if threadID != "" {
					c.updateThreadStatus(ctx, threadID, status)
					c.rdb.Del(ctx, ThreadLockKey(threadID))
				}
			}
			return t, nil
		}
		if time.Now().After(deadline) {
			// Release thread lock on timeout for sequential tasks only.
			// Group tasks: lock was already released by EnqueueGroup.
			groupLabel, _ := c.rdb.Get(ctx, TaskKey(taskID, "group")).Result()
			if groupLabel == "" {
				if threadID != "" {
					c.rdb.Del(ctx, ThreadLockKey(threadID))
				}
			}
			return nil, fmt.Errorf("Timed out waiting for task %s (status: %s)", taskID, status)
		}

		select {
		case <-ctx.Done():
			groupLabel, _ := c.rdb.Get(ctx, TaskKey(taskID, "group")).Result()
			if groupLabel == "" {
				if threadID != "" {
					c.rdb.Del(ctx, ThreadLockKey(threadID))
				}
			}
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// GroupWait polls until all tasks in a group reach a terminal state or the
// timeout expires. It computes aggregate status client-side: any "failed" →
// "error", all "done" → "complete", all "cancelled" → "cancelled", mixed
// "done" + "cancelled" → "complete". Thread status is NOT updated by
// GroupWait — it is managed solely by the request handler. On timeout,
// thread status is also NOT updated (tasks are still
// running) and Status is "timeout" with a per-task snapshot.
func (c *Client) GroupWait(ctx context.Context, threadID, groupLabel string, timeout time.Duration) (*GroupResult, error) {
	setKey := GroupTasksKey(threadID, groupLabel)

	taskIDs, err := c.rdb.SMembers(ctx, setKey).Result()
	if err != nil {
		return nil, fmt.Errorf("group wait SMEMBERS: %w", err)
	}
	if len(taskIDs) == 0 {
		return nil, fmt.Errorf("group %q not found or has no tasks", groupLabel)
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		// Pipeline GET all task statuses
		pipe := c.rdb.Pipeline()
		cmds := make([]*redis.StringCmd, len(taskIDs))
		for i, tid := range taskIDs {
			cmds[i] = pipe.Get(ctx, TaskKey(tid, "status"))
		}
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			// Transient error: retry on next tick
			continue
		}

		// Collect statuses and check terminality
		statuses := make(map[string]string, len(taskIDs))
		allTerminal := true
		for i, tid := range taskIDs {
			s, _ := cmds[i].Result()
			if s == "" {
				s = "unknown"
			}
			statuses[tid] = s
			if s == "pending" || s == "running" || s == "queued" {
				allTerminal = false
			}
		}

		if allTerminal {
			hasFailed := false
			hasDone := false
			hasCancelled := false
			for _, s := range statuses {
				switch s {
				case "failed":
					hasFailed = true
				case "done":
					hasDone = true
				case "cancelled":
					hasCancelled = true
				}
			}

			var aggregate string
			if hasFailed {
				aggregate = "error"
			} else if hasDone && !hasCancelled {
				aggregate = "complete"
			} else if hasCancelled && !hasDone {
				aggregate = "cancelled"
			} else {
				aggregate = "complete" // mixed done + cancelled
			}

			return &GroupResult{
				ThreadID: threadID,
				Label:    groupLabel,
				Status:   aggregate,
				Tasks:    statuses,
			}, nil
		}

		if time.Now().After(deadline) {
			return &GroupResult{
				ThreadID: threadID,
				Label:    groupLabel,
				Status:   "timeout",
				Tasks:    statuses,
			}, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// CancelTask sets the cancel flag so workers check it before starting.
// Does not change task status — the worker transitions the task to
// "cancelled" when it dequeues and checks the flag. Matches task.py behavior.
// cancelledBy indicates who initiated the cancellation: "user", "timeout", or "system".
func (c *Client) CancelTask(ctx context.Context, taskID, cancelledBy string) error {
	exists, err := c.rdb.Exists(ctx, TaskKey(taskID, "status")).Result()
	if err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("task %s not found", taskID)
	}
	pipe := c.rdb.Pipeline()
	pipe.Set(ctx, TaskKey(taskID, "cancel"), "1", TTLTask)
	pipe.Set(ctx, TaskKey(taskID, "cancelled_by"), cancelledBy, TTLTask)
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	return nil
}

// RequeueStale requeues stale in-flight tasks for a given worker type.
// Uses last_started_at for staleness detection (diverges from task.py which uses created_at).
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
		lastStartedAt, _ := c.rdb.Get(ctx, TaskKey(task.TaskID, "last_started_at")).Result()

		requeue := false

		if taskStatus == "" || taskStatus == "pending" {
			// Worker crashed before writing status, or after BLMOVE but before HSET
			requeue = true
		} else if taskStatus == "running" && lastStartedAt != "" {
			started, parseErr := time.Parse("2006-01-02T15:04:05Z", lastStartedAt)
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
			c.rdb.Incr(ctx, TaskKey(task.TaskID, "retry_count"))
			c.rdb.Expire(ctx, TaskKey(task.TaskID, "retry_count"), TTLTask)
			requeued = append(requeued, task.TaskID)
		}
	}

	return requeued, nil
}

// updateThreadStatus sets the thread status based on terminal task status.
// Silently ignores errors — best-effort, same as lock release.
func (c *Client) updateThreadStatus(ctx context.Context, threadID, taskStatus string) {
	threadStatus := threadStatusFromTask(taskStatus)
	_ = c.UpdateThread(ctx, threadID, map[string]string{"status": threadStatus})
}

func threadStatusFromTask(taskStatus string) string {
	switch taskStatus {
	case "done":
		return "complete"
	case "failed":
		return "error"
	case "cancelled":
		return "cancelled"
	default:
		return taskStatus
	}
}
