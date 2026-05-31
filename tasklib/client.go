package tasklib

import (
	"context"
	"fmt"
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
	// Referenced by cmd/webui/internal/request/handler.go as the REQUEST_TIMEOUT fallback.
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

// KeyName helpers produce the same Redis key names as task.py.
func TaskKey(taskID, field string) string          { return "task:" + taskID + ":" + field }
func QueueKey(worker string) string                { return "tasks:queue:" + worker }
func ProcessingKey(worker string) string           { return "tasks:processing:" + worker }
func ThreadStateKey(threadID string) string        { return "thread:" + threadID + ":current_state" }
func ThreadMessagesKey(threadID string) string     { return "thread:" + threadID + ":messages" }
func ThreadLockKey(threadID string) string         { return "thread:" + threadID + ":lock" }
func ThreadRunningKey(threadID string) string      { return "thread:" + threadID + ":running" }
func ThreadCompleteKey(threadID string) string     { return "thread:" + threadID + ":complete" }
func ThreadSessionIDKey(threadID string) string    { return "thread:" + threadID + ":session_id" }
func ThreadLastActivityKey(threadID string) string { return "thread:" + threadID + ":last_activity" }
func GroupTasksKey(threadID, label string) string {
	return "thread:" + threadID + ":group:" + label + ":tasks"
}
func ThreadEventsKey(threadID string) string   { return "thread:" + threadID + ":events" }
func ThreadLockedAtKey(threadID string) string { return "thread:" + threadID + ":locked_at" }
func HeartbeatKey(workerName string) string {
	return "worker:" + workerName + ":heartbeat"
}

// Client wraps *redis.Client and provides all task/thread/worker operations.
type Client struct {
	rdb       *redis.Client
	AgentName string // orchestrator name used in thread history and stats (default "master")
	AgentRole string // orchestrator role (e.g., "designer"), included in message metadata
}

// NewClient creates a new Client from an existing redis.Client.
func NewClient(rdb *redis.Client) *Client {
	return &Client{rdb: rdb, AgentName: "master"}
}

// Services composes all role interfaces for consumers that need the full
// surface area (CLI tools, DI composition roots).
type Services struct {
	Tasks    TaskStore
	Threads  ThreadStore
	Requests RequestStore  // request locks, session IDs, cancel/running
	History  ThreadHistory // message CRUD, activity stamps
	Events   EventBus
	Workers  WorkerRegistry
	Tokens   TokenLedger
	Scanner  ThreadScanner
	SysOps   SystemOps
}

func agentNameFromEnv() (string, string) {
	name := "master"
	role := ""
	if v := os.Getenv("AGENT_NAME"); v != "" {
		name = v
	}
	if v := os.Getenv("AGENT_ROLE"); v != "" {
		role = v
	}
	return name, role
}

// NewServices creates a Services that composes all role interfaces.
// The single *Client under the hood satisfies every interface, so all
// fields share the same underlying Redis connection.
func NewServices(rdb *redis.Client) *Services {
	c := NewClient(rdb)
	c.AgentName, c.AgentRole = agentNameFromEnv()
	return &Services{
		Tasks:    c,
		Threads:  c,
		Requests: c,
		History:  c,
		Events:   c,
		Workers:  c,
		Tokens:   c,
		Scanner:  c,
		SysOps:   c,
	}
}

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

// Ts returns the current time as an ISO8601 UTC string (same format as task.py).
func Ts() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

// AcquireRequestLock sets thread:<id>:running with TTL via SET NX.
// Returns true if the lock was acquired, false if the thread already has a running request.
func (c *Client) AcquireRequestLock(ctx context.Context, threadID, requestID string, ttl time.Duration) (bool, error) {
	return c.rdb.SetNX(ctx, ThreadRunningKey(threadID), requestID, ttl).Result()
}

// ReleaseRequestLock deletes the thread:<id>:running lock key.
// Safe to call without verifying the requestID because the lock TTL (REQUEST_TIMEOUT+5min)
// exceeds the Go context timeout (REQUEST_TIMEOUT), so the lock cannot expire and be
// re-acquired before the owning goroutine releases it.
func (c *Client) ReleaseRequestLock(ctx context.Context, threadID string) error {
	return c.rdb.Del(ctx, ThreadRunningKey(threadID)).Err()
}

// SetThreadSessionID stores the Claude session UUID for a thread.
func (c *Client) SetThreadSessionID(ctx context.Context, threadID, sessionID string) error {
	return c.rdb.Set(ctx, ThreadSessionIDKey(threadID), sessionID, TTLThread).Err()
}

// GetThreadSessionID retrieves the Claude session UUID for a thread.
// Returns empty string if not set.
func (c *Client) GetThreadSessionID(ctx context.Context, threadID string) (string, error) {
	val, err := c.rdb.Get(ctx, ThreadSessionIDKey(threadID)).Result()
	if err == redis.Nil {
		return "", nil
	}
	return val, err
}

// CancelRequest sets the thread status to cancelled.
// The web UI handler is responsible for calling cancel() on the subprocess context;
// the background goroutine releases the request lock after context cancellation.
func (c *Client) CancelRequest(ctx context.Context, threadID string) error {
	key := ThreadStateKey(threadID)
	exists, err := c.rdb.Exists(ctx, key).Result()
	if err != nil {
		return err
	}
	if exists == 0 {
		return nil // thread doesn't exist, no-op
	}

	pipe := c.rdb.Pipeline()
	pipe.HSet(ctx, key, "status", "cancelled", "updated_at", Ts())
	pipe.Expire(ctx, key, TTLThread)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("cancel request: %w", err)
	}
	return nil
}

// SetThreadComplete marks a thread as having a completed response.
func (c *Client) SetThreadComplete(ctx context.Context, threadID string) error {
	return c.rdb.Set(ctx, ThreadCompleteKey(threadID), "1", TTLThread).Err()
}

// ClearThreadComplete removes the completion marker for a thread.
// Must be called when a follow-up request starts so the UI no longer
// shows the thread as "complete".
func (c *Client) ClearThreadComplete(ctx context.Context, threadID string) error {
	return c.rdb.Del(ctx, ThreadCompleteKey(threadID)).Err()
}

// IsThreadComplete checks whether the thread has a completed response.
func (c *Client) IsThreadComplete(ctx context.Context, threadID string) (bool, error) {
	exists, err := c.rdb.Exists(ctx, ThreadCompleteKey(threadID)).Result()
	return exists > 0, err
}

// UpdateThreadLastActivity sets the last-activity timestamp for a thread.
func (c *Client) UpdateThreadLastActivity(ctx context.Context, threadID string) error {
	return c.rdb.Set(ctx, ThreadLastActivityKey(threadID), Ts(), TTLThread).Err()
}

// GetThreadLastActivity retrieves the last-activity timestamp for a thread.
func (c *Client) GetThreadLastActivity(ctx context.Context, threadID string) (string, error) {
	val, err := c.rdb.Get(ctx, ThreadLastActivityKey(threadID)).Result()
	if err == redis.Nil {
		return "", nil
	}
	return val, err
}

// IsRequestRunning checks whether a thread has a running request lock.
func (c *Client) IsRequestRunning(ctx context.Context, threadID string) (bool, error) {
	exists, err := c.rdb.Exists(ctx, ThreadRunningKey(threadID)).Result()
	return exists > 0, err
}
