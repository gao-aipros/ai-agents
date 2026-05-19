package tasklib

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// TTL constants — byte-for-byte compatible with task.py.
const (
	TTLTask   = 86400 * time.Second  // 24 hours
	TTLThread = 604800 * time.Second // 7 days
	LockTTL   = 2100 * time.Second   // REQUEST_TIMEOUT(1800) + 300s margin
)

// Valid worker types.
var WorkerTypes = []string{"claude", "copilot", "opencode", "codex"}

// KeyName helpers produce the same Redis key names as task.py.
func TaskKey(taskID, field string) string   { return "task:" + taskID + ":" + field }
func QueueKey(worker string) string          { return "tasks:queue:" + worker }
func ProcessingKey(worker string) string     { return "tasks:processing:" + worker }
func ThreadStateKey(threadID string) string  { return "thread:" + threadID + ":current_state" }
func ThreadMessagesKey(threadID string) string { return "thread:" + threadID + ":messages" }
func ThreadLockKey(threadID string) string       { return "thread:" + threadID + ":lock" }
func ThreadRunningKey(threadID string) string      { return "thread:" + threadID + ":running" }
func ThreadCompleteKey(threadID string) string     { return "thread:" + threadID + ":complete" }
func ThreadSessionIDKey(threadID string) string    { return "thread:" + threadID + ":session_id" }
func ThreadLastActivityKey(threadID string) string { return "thread:" + threadID + ":last_activity" }
func GroupTasksKey(threadID, label string) string {
	return "thread:" + threadID + ":group:" + label + ":tasks"
}
func ThreadEventsKey(threadID string) string  { return "thread:" + threadID + ":events" }
func ThreadLockedAtKey(threadID string) string { return "thread:" + threadID + ":locked_at" }
func SystemEventsKey() string                   { return "system:events" }
func HeartbeatKey(workerType, hostname string) string {
	return "worker:" + workerType + ":" + hostname + ":heartbeat"
}

// Client wraps *redis.Client and provides all task/thread/worker operations.
type Client struct {
	rdb *redis.Client
}

// NewClient creates a new Client from an existing redis.Client.
func NewClient(rdb *redis.Client) *Client {
	return &Client{rdb: rdb}
}

// RDB returns the underlying redis client (useful for testing / raw ops).
func (c *Client) RDB() *redis.Client { return c.rdb }

// Ping checks Redis connectivity.
func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// isTaskActive returns true if the holder task exists and is not in a terminal
// state. Used to detect stale locks — if the lock holder is done/failed/cancelled
// or the task keys have expired, the lock is stale and should be released.
func (c *Client) isTaskActive(ctx context.Context, taskID string) bool {
	if taskID == "" {
		return false
	}
	status, err := c.rdb.Get(ctx, TaskKey(taskID, "status")).Result()
	if err != nil {
		return false // key missing or error → treat as stale
	}
	switch status {
	case "done", "failed", "cancelled":
		return false
	}
	return true
}

// ts returns the current time as an ISO8601 UTC string (same format as task.py).
func ts() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
