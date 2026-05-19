package tasklib

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Thread represents a thread entity.
type Thread struct {
	ThreadID      string `json:"thread_id"`
	Status        string `json:"status,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
	GHRepo        string `json:"gh_repo,omitempty"`
	GHPRNumber    string `json:"gh_pr_number,omitempty"`
	LastDesign    string `json:"last_design,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// Message is a single message in thread history.
type Message struct {
	Role      string            `json:"role"`
	Type      string            `json:"type,omitempty"`
	Content   string            `json:"content"`
	Timestamp string            `json:"timestamp"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Source    string            `json:"source,omitempty"`
}

// CreateThread initializes a new thread with status "initiated".
func (c *Client) CreateThread(ctx context.Context, threadID, repo string) (*Thread, error) {
	correlationID, err := NewUUID()
	if err != nil {
		return nil, fmt.Errorf("generate correlation_id: %w", err)
	}

	mapping := map[string]interface{}{
		"status":         "initiated",
		"updated_at":     ts(),
		"correlation_id": correlationID,
	}
	if repo != "" {
		mapping["gh_repo"] = repo
	}

	key := ThreadStateKey(threadID)
	if err := c.rdb.HSet(ctx, key, mapping).Err(); err != nil {
		return nil, fmt.Errorf("thread create: %w", err)
	}
	c.rdb.Expire(ctx, key, TTLThread)

	return &Thread{
		ThreadID:      threadID,
		Status:        "initiated",
		GHRepo:        repo,
		CorrelationID: correlationID,
	}, nil
}

// GetThread retrieves thread state.
func (c *Client) GetThread(ctx context.Context, threadID string) (*Thread, error) {
	state, err := c.rdb.HGetAll(ctx, ThreadStateKey(threadID)).Result()
	if err != nil {
		return nil, err
	}
	if len(state) == 0 {
		return nil, fmt.Errorf("thread %s not found", threadID)
	}

	return &Thread{
		ThreadID:      threadID,
		Status:        state["status"],
		UpdatedAt:     state["updated_at"],
		GHRepo:        state["gh_repo"],
		GHPRNumber:    state["gh_pr_number"],
		LastDesign:    state["last_design"],
		CorrelationID: state["correlation_id"],
	}, nil
}

// ListThreads retrieves all threads by scanning for thread:*:current_state keys.
func (c *Client) ListThreads(ctx context.Context) ([]*Thread, error) {
	var threads []*Thread
	var cursor uint64

	for {
		keys, nextCursor, err := c.rdb.Scan(ctx, cursor, "thread:*:current_state", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("scan threads: %w", err)
		}
		for _, key := range keys {
			// key is "thread:<id>:current_state" — extract thread ID (field [1])
			parts := strings.SplitN(key, ":", 3)
			if len(parts) < 2 {
				continue
			}
			threadID := parts[1]
			state, _ := c.rdb.HGetAll(ctx, key).Result()
			threads = append(threads, &Thread{
				ThreadID:   threadID,
				Status:     stateVal(state, "status", "unknown"),
				UpdatedAt:  stateVal(state, "updated_at", "-"),
				GHRepo:     stateVal(state, "gh_repo", "-"),
				GHPRNumber: stateVal(state, "gh_pr_number", "-"),
			})
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	return threads, nil
}

// GetThreadHistory retrieves messages from a thread's history list.
// offset and limit control pagination. If limit is 0, all messages are returned.
func (c *Client) GetThreadHistory(ctx context.Context, threadID string, offset, limit int) ([]Message, error) {
	key := ThreadMessagesKey(threadID)

	var msgs []string
	var err error

	if limit > 0 {
		// offset-based pagination: from offset to offset+limit-1
		end := offset + limit - 1
		msgs, err = c.rdb.LRange(ctx, key, int64(offset), int64(end)).Result()
	} else if offset > 0 {
		msgs, err = c.rdb.LRange(ctx, key, int64(offset), -1).Result()
	} else {
		msgs, err = c.rdb.LRange(ctx, key, 0, -1).Result()
	}
	if err != nil {
		return nil, err
	}

	result := make([]Message, 0, len(msgs))
	for _, raw := range msgs {
		var m Message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			m = Message{Content: raw, Role: "unknown"}
		}
		result = append(result, m)
	}
	return result, nil
}

// GetThreadHistoryTail retrieves the last N messages from thread history.
func (c *Client) GetThreadHistoryTail(ctx context.Context, threadID string, tail int) ([]Message, error) {
	if tail <= 0 {
		return nil, nil
	}
	key := ThreadMessagesKey(threadID)
	msgs, err := c.rdb.LRange(ctx, key, int64(-tail), -1).Result()
	if err != nil {
		return nil, err
	}
	result := make([]Message, 0, len(msgs))
	for _, raw := range msgs {
		var m Message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			m = Message{Content: raw, Role: "unknown"}
		}
		result = append(result, m)
	}
	return result, nil
}

// AppendMessage appends a message to thread history and refreshes TTL.
func (c *Client) AppendMessage(ctx context.Context, threadID string, msg Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	key := ThreadMessagesKey(threadID)
	if err := c.rdb.RPush(ctx, key, string(data)).Err(); err != nil {
		return err
	}
	return c.rdb.Expire(ctx, key, TTLThread).Err()
}

// UpdateThread updates thread state fields.
func (c *Client) UpdateThread(ctx context.Context, threadID string, fields map[string]string) error {
	key := ThreadStateKey(threadID)
	exists, err := c.rdb.Exists(ctx, key).Result()
	if err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("thread '%s' not found. Run 'task thread-create --id %s' first", threadID, threadID)
	}

	mapping := make(map[string]interface{}, len(fields)+1)
	mapping["updated_at"] = ts()
	for k, v := range fields {
		switch k {
		case "status":
			mapping["status"] = v
		case "design", "last_design":
			mapping["last_design"] = v
		case "pr", "pr_number", "gh_pr_number":
			mapping["gh_pr_number"] = v
		default:
			mapping[k] = v
		}
	}

	if err := c.rdb.HSet(ctx, key, mapping).Err(); err != nil {
		return err
	}
	return c.rdb.Expire(ctx, key, TTLThread).Err()
}

// LockThread acquires a thread-level lock via SET NX.
// Returns true if the lock was acquired.
func (c *Client) LockThread(ctx context.Context, threadID, taskID string, ttl time.Duration) (bool, error) {
	return c.rdb.SetNX(ctx, ThreadLockKey(threadID), taskID, ttl).Result()
}

// IsThreadLocked checks whether the thread lock key exists (a task is enqueued/pending).
func (c *Client) IsThreadLocked(ctx context.Context, threadID string) (bool, error) {
	exists, err := c.rdb.Exists(ctx, ThreadLockKey(threadID)).Result()
	return exists > 0, err
}

// UnlockThread releases a thread lock. Safe to call multiple times (DEL is idempotent).
func (c *Client) UnlockThread(ctx context.Context, threadID string) error {
	return c.rdb.Del(ctx, ThreadLockKey(threadID)).Err()
}

// SetActiveTask adds or updates an entry in the active_tasks hash.
func (c *Client) SetActiveTask(ctx context.Context, taskID string, info TaskInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return c.rdb.HSet(ctx, "active_tasks", taskID, string(data)).Err()
}

// RemoveActiveTask removes an entry from the active_tasks hash.
func (c *Client) RemoveActiveTask(ctx context.Context, taskID string) error {
	return c.rdb.HDel(ctx, "active_tasks", taskID).Err()
}

// GetActiveTasks retrieves all entries from the active_tasks hash.
func (c *Client) GetActiveTasks(ctx context.Context) (map[string]*TaskInfo, error) {
	raw, err := c.rdb.HGetAll(ctx, "active_tasks").Result()
	if err != nil {
		return nil, err
	}
	result := make(map[string]*TaskInfo, len(raw))
	for taskID, data := range raw {
		var info TaskInfo
		if err := json.Unmarshal([]byte(data), &info); err != nil {
			info = TaskInfo{Status: "unknown"}
		}
		result[taskID] = &info
	}
	return result, nil
}

// ThreadExists checks if a thread exists.
func (c *Client) ThreadExists(ctx context.Context, threadID string) (bool, error) {
	exists, err := c.rdb.Exists(ctx, ThreadStateKey(threadID)).Result()
	return exists > 0, err
}

// DeleteThread removes all Redis keys for a thread.
func (c *Client) DeleteThread(ctx context.Context, threadID string) error {
	keys := []string{
		ThreadStateKey(threadID),
		ThreadMessagesKey(threadID),
		ThreadEventsKey(threadID),
		ThreadCompleteKey(threadID),
		ThreadRunningKey(threadID),
		ThreadLockKey(threadID),
		ThreadLockedAtKey(threadID),
		ThreadSessionIDKey(threadID),
		ThreadLastActivityKey(threadID),
	}
	return c.rdb.Del(ctx, keys...).Err()
}

// SetThreadTTL sets or refreshes TTL on all thread keys.
func (c *Client) SetThreadTTL(ctx context.Context, threadID string, ttl time.Duration) error {
	pipe := c.rdb.Pipeline()
	pipe.Expire(ctx, ThreadStateKey(threadID), ttl)
	pipe.Expire(ctx, ThreadMessagesKey(threadID), ttl)
	pipe.Expire(ctx, ThreadEventsKey(threadID), ttl)
	pipe.Expire(ctx, ThreadLockedAtKey(threadID), ttl)
	_, err := pipe.Exec(ctx)
	return err
}

// ThreadMessagesLen returns the number of messages in a thread's history.
func (c *Client) ThreadMessagesLen(ctx context.Context, threadID string) (int64, error) {
	return c.rdb.LLen(ctx, ThreadMessagesKey(threadID)).Result()
}

// Helpers

func stateVal(m map[string]string, key, def string) string {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// ParseThreadUpdateFields converts CLI flags into update fields (maps design → last_design, pr → gh_pr_number).
func ParseThreadUpdateFields(status, design, pr string) map[string]string {
	fields := map[string]string{}
	if status != "" {
		fields["status"] = status
	}
	if design != "" {
		fields["last_design"] = design
	}
	if pr != "" {
		fields["gh_pr_number"] = pr
	}
	return fields
}

// ParsePRNumber converts a string PR number to int (for CLI usage).
func ParsePRNumber(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}
