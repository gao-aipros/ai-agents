package tasklib

import (
	"context"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// TTL constants — byte-for-byte compatible with task.py.
const (
	TTLTask   = 86400 * time.Second  // 24 hours
	TTLThread = 604800 * time.Second // 7 days
	TTLStats  = 604800 * time.Second // 7 days — global counters survive quiet periods

	// DefaultRequestTimeout is the fallback for REQUEST_TIMEOUT env var (2.5 h).
	// Must match the default in cmd/webui/internal/request/handler.go DefaultConfig().
	DefaultRequestTimeout = 9000
)

func init() {
	LockTTL = computeLockTTL(os.LookupEnv)
}

// computeLockTTL resolves LockTTL from env vars. Exported for testing.
// Returns REQUEST_TIMEOUT+300 by default, or LOCK_TTL if set.
func computeLockTTL(lookup func(string) (string, bool)) time.Duration {
	rt := DefaultRequestTimeout
	if v, ok := lookup("REQUEST_TIMEOUT"); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			rt = n
		}
	}
	ttl := rt + 300 // REQUEST_TIMEOUT + 5 min margin
	if v, ok := lookup("LOCK_TTL"); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ttl = n
		}
	}
	return time.Duration(ttl) * time.Second
}

// LockTTL is the thread-lock TTL (default 9300s = 155 min).
// When LOCK_TTL is unset, falls back to REQUEST_TIMEOUT + 300s margin.
// Configurable via LOCK_TTL env var (in seconds), e.g. LOCK_TTL=9300.
var LockTTL time.Duration

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
