package tasklib

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// ScanKeys returns all keys matching the given glob-style pattern.
// count is the SCAN COUNT hint (0 = use default).
func (c *Client) ScanKeys(ctx context.Context, pattern string, count int64) ([]string, error) {
	var all []string
	var cursor uint64
	for {
		keys, nextCursor, err := c.rdb.Scan(ctx, cursor, pattern, count).Result()
		if err != nil {
			return nil, err
		}
		all = append(all, keys...)
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return all, nil
}

// GetKey returns the string value of a single key.
// Returns ("", nil) if the key does not exist.
func (c *Client) GetKey(ctx context.Context, key string) (string, error) {
	val, err := c.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return val, err
}

// ActiveTaskCount returns the number of entries in the active_tasks hash (HLEN).
func (c *Client) ActiveTaskCount(ctx context.Context) (int64, error) {
	return c.rdb.HLen(ctx, "active_tasks").Result()
}

// GetAllActiveTasks returns all entries from the active_tasks hash (HGETALL).
// Values are raw JSON; callers unmarshal into TaskInfo.
func (c *Client) GetAllActiveTasks(ctx context.Context) (map[string]string, error) {
	return c.rdb.HGetAll(ctx, "active_tasks").Result()
}

// QueueDepth returns the length of a list queue (LLEN).
func (c *Client) QueueDepth(ctx context.Context, queueKey string) (int64, error) {
	return c.rdb.LLen(ctx, queueKey).Result()
}

// GetCounters returns values for the given counter keys (MGET).
// Returns nil elements for keys that don't exist yet.
func (c *Client) GetCounters(ctx context.Context, keys ...string) ([]any, error) {
	return c.rdb.MGet(ctx, keys...).Result()
}

// Info returns the Redis INFO output for the given section (e.g. "memory").
func (c *Client) Info(ctx context.Context, section string) (string, error) {
	return c.rdb.Info(ctx, section).Result()
}

// PersistMasterTokenStats writes master agent token counts to thread-level
// fields and global counters in a single pipeline.
func (c *Client) PersistMasterTokenStats(ctx context.Context, threadID string, stats TokenStats) error {
	pipe := c.rdb.Pipeline()
	PersistMasterTokenStats(ctx, pipe, threadID, stats)
	_, err := pipe.Exec(ctx)
	return err
}
