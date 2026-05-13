package tasklib

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

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
	pipe.HSet(ctx, key, "status", "cancelled", "updated_at", ts())
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

// IsThreadComplete checks whether the thread has a completed response.
func (c *Client) IsThreadComplete(ctx context.Context, threadID string) (bool, error) {
	exists, err := c.rdb.Exists(ctx, ThreadCompleteKey(threadID)).Result()
	return exists > 0, err
}

// UpdateThreadLastActivity sets the last-activity timestamp for a thread.
func (c *Client) UpdateThreadLastActivity(ctx context.Context, threadID string) error {
	return c.rdb.Set(ctx, ThreadLastActivityKey(threadID), ts(), TTLThread).Err()
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
