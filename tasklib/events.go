package tasklib

import (
	"context"
	"encoding/json"
	"fmt"
)

// Event types for the system event system.
const (
	EventTaskEnqueued       = "task_enqueued"
	EventTaskStarted        = "task_started"
	EventTaskCompleted      = "task_completed"
	EventTaskFailed         = "task_failed"
	EventTaskCancelled      = "task_cancelled"
	EventTaskRequeued       = "task_requeued"
	EventLockAcquired       = "lock_acquired"
	EventLockReleased       = "lock_released"
	EventThreadStatusChange = "thread_status_change"
	EventGroupComplete      = "group_complete"
	EventWorkerOnline       = "worker_online"
	EventWorkerOffline      = "worker_offline"
)

// SystemEventsKey returns the Redis key for cross-cutting system events.
func SystemEventsKey() string { return "system:events" }

// Event is the standard envelope for all system events.
type Event struct {
	EventID        string      `json:"event_id"`
	Type           string      `json:"type"`
	Timestamp      string      `json:"timestamp"`
	CorrelationID  string      `json:"correlation_id,omitempty"`
	TaskID         string      `json:"task_id,omitempty"`
	WorkerType     string      `json:"worker_type,omitempty"`
	WorkerHostname string      `json:"worker_hostname,omitempty"`
	Detail         interface{} `json:"detail"`
}

// TaskEnqueuedDetail is the detail payload for task_enqueued events.
type TaskEnqueuedDetail struct {
	QueueDepthAfter int `json:"queue_depth_after"`
}

// TaskCompletedDetail is the detail payload for task_completed events.
type TaskCompletedDetail struct {
	ExitCode   int `json:"exit_code"`
	DurationMs int `json:"duration_ms"`
	InputTokens      int64 `json:"input_tokens,omitempty"`
	OutputTokens     int64 `json:"output_tokens,omitempty"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int64 `json:"reasoning_tokens,omitempty"`
}
// TaskFailedDetail is the detail payload for task_failed events.
type TaskFailedDetail struct {
	ExitCode     int    `json:"exit_code"`
	ErrorMessage string `json:"error_message"`
}

// TaskCancelledDetail is the detail payload for task_cancelled events.
type TaskCancelledDetail struct {
	CancelledBy    string `json:"cancelled_by"`
	PreviousStatus string `json:"previous_status"`
}

// LockDetail is the detail payload for lock_acquired and lock_released events.
type LockDetail struct {
	HolderTaskID    string `json:"holder_task_id"`
	HeldDurationMs  int64  `json:"held_duration_ms,omitempty"`
}

// ThreadStatusChangeDetail is the detail payload for thread_status_change events.
type ThreadStatusChangeDetail struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// WorkerOnlineDetail is the detail payload for worker_online events.
type WorkerOnlineDetail struct {
	WorkerType string `json:"worker_type"`
	Hostname   string `json:"hostname"`
}

// WorkerOfflineDetail is the detail payload for worker_offline events.
type WorkerOfflineDetail struct {
	WorkerType string `json:"worker_type"`
	Hostname   string `json:"hostname"`
}

// PushEvent appends an event to a capped Redis list. Best-effort: logs errors,
// never fails the parent operation.
func (c *Client) PushEvent(ctx context.Context, listKey string, ev *Event) {
	ev.EventID = mustUUID()
	ev.Timestamp = ts()
	data, err := json.Marshal(ev)
	if err != nil {
		// Use package-level log since tasklib has no slog dependency
		fmt.Printf("event marshal error: %v\n", err)
		return
	}
	if err := c.rdb.RPush(ctx, listKey, string(data)).Err(); err != nil {
		fmt.Printf("event rpush error: %v\n", err)
		return
	}
}

// PushThreadEvent pushes an event to thread:{id}:events and trims to 1000.
func (c *Client) PushThreadEvent(ctx context.Context, threadID string, ev *Event) {
	key := ThreadEventsKey(threadID)
	c.PushEvent(ctx, key, ev)
	c.rdb.LTrim(ctx, key, -1000, -1)
	c.rdb.Expire(ctx, key, TTLThread)
}

// PushSystemEvent pushes an event to system:events and trims to 10000.
func (c *Client) PushSystemEvent(ctx context.Context, ev *Event) {
	key := SystemEventsKey()
	c.PushEvent(ctx, key, ev)
	c.rdb.LTrim(ctx, key, -10000, -1)
	c.rdb.Expire(ctx, key, TTLThread)
}

// GetThreadEvents reads the most recent events from a thread's event list.
func (c *Client) GetThreadEvents(ctx context.Context, threadID string, limit int) ([]Event, error) {
	return c.getEvents(ctx, ThreadEventsKey(threadID), limit)
}

// GetSystemEvents reads the most recent system-wide events.
func (c *Client) GetSystemEvents(ctx context.Context, limit int) ([]Event, error) {
	return c.getEvents(ctx, SystemEventsKey(), limit)
}

func (c *Client) getEvents(ctx context.Context, key string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 50
	}
	start := int64(-limit)
	results, err := c.rdb.LRange(ctx, key, start, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("lrange events: %w", err)
	}
	events := make([]Event, 0, len(results))
	for _, raw := range results {
		var ev Event
		if json.Unmarshal([]byte(raw), &ev) == nil {
			events = append(events, ev)
		}
	}
	return events, nil
}

// mustUUID generates a UUID for event IDs. Never fails — returns a fallback
// if the UUID generator errors (which it shouldn't, but we're best-effort).
func mustUUID() string {
	id, err := NewUUID()
	if err != nil {
		return "00000000-0000-0000-0000-000000000000"
	}
	return id
}
