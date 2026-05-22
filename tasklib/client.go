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
	TTLStats  = 604800 * time.Second // 7 days — global counters survive quiet periods
	LockTTL   = 7500 * time.Second   // REQUEST_TIMEOUT(7200) + 300s margin
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

// acquireLockScript atomically acquires a thread lock and sets locked_at.
// KEYS[1] = lock key, KEYS[2] = locked_at key
// ARGV[1] = task ID (lock value), ARGV[2] = TTL seconds, ARGV[3] = timestamp
// Returns 1 if acquired, 0 if lock already held.
var acquireLockScript = redis.NewScript(`
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'EX', ARGV[2]) then
  redis.call('SET', KEYS[2], ARGV[3], 'EX', ARGV[2])
  return 1
end
return 0
`)

// ts returns the current time as an ISO8601 UTC string (same format as task.py).
func ts() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
