package tasklib

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Thread represents a thread entity.
type Thread struct {
	ThreadID       string `json:"thread_id"`
	Status         string `json:"status,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
	GHRepo         string `json:"gh_repo,omitempty"`
	GHPRNumber     string `json:"gh_pr_number,omitempty"`
	LastDesign     string `json:"last_design,omitempty"`
	CorrelationID  string `json:"correlation_id,omitempty"`
	ParentThreadID string `json:"parent_thread_id,omitempty"`
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
func (c *Client) CreateThread(ctx context.Context, threadID, repo, parentThreadID string) (*Thread, error) {
	correlationID, err := NewUUID()
	if err != nil {
		return nil, fmt.Errorf("generate correlation_id: %w", err)
	}

	now := ts()
	mapping := map[string]interface{}{
		"status":         "initiated",
		"created_at":     now,
		"updated_at":     now,
		"correlation_id": correlationID,
	}
	if repo != "" {
		mapping["gh_repo"] = repo
	}
	if parentThreadID != "" {
		mapping["parent_thread_id"] = parentThreadID
	}

	key := ThreadStateKey(threadID)
	if err := c.rdb.HSet(ctx, key, mapping).Err(); err != nil {
		return nil, fmt.Errorf("thread create: %w", err)
	}
	c.rdb.Expire(ctx, key, TTLThread)

	return &Thread{
		ThreadID:       threadID,
		Status:         "initiated",
		CreatedAt:      now,
		GHRepo:         repo,
		CorrelationID:  correlationID,
		ParentThreadID: parentThreadID,
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
		ThreadID:       threadID,
		Status:         state["status"],
		CreatedAt:      state["created_at"],
		UpdatedAt:      state["updated_at"],
		GHRepo:         state["gh_repo"],
		GHPRNumber:     state["gh_pr_number"],
		LastDesign:     state["last_design"],
		CorrelationID:  state["correlation_id"],
		ParentThreadID: state["parent_thread_id"],
	}, nil
}

// ListThreads retrieves all threads by scanning for thread:*:current_state keys.
// sortBy and sortDir control ordering. Default sort: status priority
// (error > running > complete), secondary by thread_id ASC.
// Supported sortBy: thread_id, status, repo, pr, updated_at.
func (c *Client) ListThreads(ctx context.Context, sortBy, sortDir string) ([]*Thread, error) {
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
				ThreadID:      threadID,
				Status:        stateVal(state, "status", "unknown"),
				CreatedAt:     stateVal(state, "created_at", "-"),
				UpdatedAt:     stateVal(state, "updated_at", "-"),
				GHRepo:        stateVal(state, "gh_repo", "-"),
				GHPRNumber:    stateVal(state, "gh_pr_number", "-"),
				CorrelationID:  stateVal(state, "correlation_id", ""),
				ParentThreadID: stateVal(state, "parent_thread_id", ""),
			})
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	// Sort by status priority by default, or by specified column.
	threadStatusOrder := map[string]int{
		"error":      0,
		"running":    1,
		"cancelled":  2,
		"complete":   3,
		"initiated":  4,
		"reviewing":  5,
		"unknown":    6,
	}

	sort.Slice(threads, func(i, j int) bool {
		sortDir = strings.ToLower(sortDir)
		asc := sortDir != "desc"
		switch sortBy {
		case "status":
			oi := threadStatusOrder[threads[i].Status]
			oj := threadStatusOrder[threads[j].Status]
			if oi != oj {
				return asc == (oi < oj)
			}
			return asc == (threads[i].ThreadID < threads[j].ThreadID)
		case "repo":
			if threads[i].GHRepo != threads[j].GHRepo {
				return asc == (threads[i].GHRepo < threads[j].GHRepo)
			}
			return asc == (threads[i].ThreadID < threads[j].ThreadID)
		case "pr":
			prI, errI := ParsePRNumber(threads[i].GHPRNumber)
			prJ, errJ := ParsePRNumber(threads[j].GHPRNumber)
			if errI == nil && errJ == nil {
				if prI != prJ {
					return asc == (prI < prJ)
				}
			} else if errI == nil {
				// i has a valid PR, j doesn't — valid PRs come first in ASC
				return asc
			} else if errJ == nil {
				return !asc
			} else if threads[i].GHPRNumber != threads[j].GHPRNumber {
				return asc == (threads[i].GHPRNumber < threads[j].GHPRNumber)
			}
			return asc == (threads[i].ThreadID < threads[j].ThreadID)
		case "updated_at":
			if threads[i].UpdatedAt != threads[j].UpdatedAt {
				return asc == (threads[i].UpdatedAt < threads[j].UpdatedAt)
			}
			return asc == (threads[i].ThreadID < threads[j].ThreadID)
		case "thread_id":
			return asc == (threads[i].ThreadID < threads[j].ThreadID)
		default:
			// Default: status priority (error > running > complete)
			oi := threadStatusOrder[threads[i].Status]
			oj := threadStatusOrder[threads[j].Status]
			if oi != oj {
				return asc == (oi < oj)
			}
			return asc == (threads[i].ThreadID < threads[j].ThreadID)
		}
	})

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
	return parseMessages(msgs), nil
}

// GetThreadHistoryTailForWorker retrieves the last N messages from thread
// history that are addressed to a specific worker. Messages without a
// "worker" metadata field pass through for backward compatibility.
// Scans backwards through the list in batches so the worker always sees
// its own messages regardless of how many other-worker messages are
// interleaved.
func (c *Client) GetThreadHistoryTailForWorker(ctx context.Context, threadID string, tail int, worker string) ([]Message, error) {
	if tail <= 0 {
		return nil, nil
	}
	key := ThreadMessagesKey(threadID)

	totalLen, err := c.rdb.LLen(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if totalLen == 0 {
		return nil, nil
	}

	const batchSize = int64(100)
	var collected []Message

	// Scan backwards in batches from the end of the list.
	for end := totalLen - 1; end >= 0; end -= batchSize {
		start := end - batchSize + 1
		if start < 0 {
			start = 0
		}
		batch, err := c.rdb.LRange(ctx, key, start, end).Result()
		if err != nil {
			return nil, err
		}

		var filtered []Message
		for _, m := range parseMessages(batch) {
			msgWorker := m.Metadata["worker"]
			if msgWorker == "" || msgWorker == worker {
				filtered = append(filtered, m)
			}
		}

		// Prepend: older batch before newer (already collected)
		collected = append(filtered, collected...)

		if len(collected) >= tail {
			break
		}
	}

	// Return only the last `tail` matching messages
	if len(collected) > tail {
		collected = collected[len(collected)-tail:]
	}
	return collected, nil
}

// parseMessages unmarshals raw JSON strings into Message structs.
// Corrupt entries are replaced with an "unknown"-role placeholder.
func parseMessages(raw []string) []Message {
	result := make([]Message, 0, len(raw))
	for _, s := range raw {
		var m Message
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			m = Message{Content: s, Role: "unknown"}
		}
		result = append(result, m)
	}
	return result
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
		case "parent_thread_id":
			mapping["parent_thread_id"] = v
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
	// Read holder before DEL for the lock_released event
	holder, _ := c.rdb.Get(ctx, ThreadLockKey(threadID)).Result()
	err := c.rdb.Del(ctx, ThreadLockKey(threadID), ThreadLockedAtKey(threadID)).Err()
	// Best-effort event: lock_released
	c.PushThreadEvent(ctx, threadID, &Event{
		Type: EventLockReleased,
		Detail: LockDetail{HolderTaskID: holder},
	})
	return err
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

// DiscoverDescendants returns the set of all descendant thread IDs for the
// given thread ID. The target thread itself is NOT included in the result.
// Descendants are threads whose ParentThreadID equals the target or any of
// its descendants, transitively. Uses BFS to handle cycles safely.
func (c *Client) DiscoverDescendants(ctx context.Context, threadID string) (map[string]bool, error) {
	threads, err := c.ListThreads(ctx, "", "")
	if err != nil {
		return nil, err
	}

	// Build parent → children index
	children := make(map[string][]string)
	for _, t := range threads {
		if t.ParentThreadID != "" {
			children[t.ParentThreadID] = append(children[t.ParentThreadID], t.ThreadID)
		}
	}

	// BFS walk to collect all descendants. Use a separate visited set so
	// the root thread is excluded from the result but still stops cycles.
	visited := map[string]bool{threadID: true}
	result := make(map[string]bool)
	queue := []string{threadID}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range children[parent] {
			if !visited[child] {
				visited[child] = true
				result[child] = true
				queue = append(queue, child)
			}
		}
	}
	return result, nil
}

// DeleteThread removes all Redis keys for a thread and all its descendants.
// Best-effort: attempts all DEL operations and returns an error with the
// count of failed threads if any individual deletion fails.
func (c *Client) DeleteThread(ctx context.Context, threadID string) error {
	descendants, err := c.DiscoverDescendants(ctx, threadID)
	if err != nil {
		return fmt.Errorf("discover descendants: %w", err)
	}

	// Collect all thread IDs: descendants + the target itself
	allIDs := make([]string, 0, len(descendants)+1)
	for id := range descendants {
		allIDs = append(allIDs, id)
	}
	allIDs = append(allIDs, threadID)

	// Delete keys for each thread. Best-effort: attempt all DELs, collect errors.
	var errs []error
	for _, id := range allIDs {
		if err := c.deleteSingleThreadKeys(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("delete thread %s: %w", id, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("partial deletion: %d/%d threads failed (first: %w)",
			len(errs), len(allIDs), errs[0])
	}
	return nil
}

// deleteSingleThreadKeys deletes the 9 Redis keys for a single thread.
func (c *Client) deleteSingleThreadKeys(ctx context.Context, threadID string) error {
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
	pipe.Expire(ctx, ThreadCompleteKey(threadID), ttl)
	pipe.Expire(ctx, ThreadRunningKey(threadID), ttl)
	pipe.Expire(ctx, ThreadSessionIDKey(threadID), ttl)
	pipe.Expire(ctx, ThreadLastActivityKey(threadID), ttl)
	// Skip ThreadLockKey: extending lock TTL on keep can make a held lock permanent
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

// ThreadDiagnostics aggregates diagnostic information for a thread.
// TaskCounts reflects at most the most recent 200 tasks. If a thread has more
// than 200 tasks, older tasks are not reflected in counts or LastError.
type ThreadDiagnostics struct {
	ThreadID       string          `json:"thread_id"`
	Status         string          `json:"status"`
	UpdatedAt      string          `json:"updated_at"`
	CorrelationID  string          `json:"correlation_id"`
	LastError      string          `json:"last_error,omitempty"`
	Lock           *LockInfo       `json:"lock,omitempty"`
	TaskCounts     map[string]int  `json:"task_counts"`
	RecentEvents   []Event         `json:"recent_events"`
	StuckTasks     []StuckTaskInfo `json:"stuck_tasks,omitempty"`
	Warnings       []string        `json:"warnings,omitempty"`
}

// LockInfo describes a thread lock.
type LockInfo struct {
	HolderTask  string `json:"holder_task"`
	LockedAt    string `json:"locked_at,omitempty"`
	HeldSeconds int64  `json:"held_seconds,omitempty"`
}

// StuckTaskInfo describes a task that has been running too long.
type StuckTaskInfo struct {
	TaskID       string `json:"task_id"`
	Worker       string `json:"worker"`
	StartedAt    string `json:"started_at"`
	StaleMinutes int64  `json:"stale_minutes"`
}

// GetThreadDiagnostics collects diagnostic information for a thread.
func (c *Client) GetThreadDiagnostics(ctx context.Context, threadID string) (*ThreadDiagnostics, error) {
	thread, err := c.GetThread(ctx, threadID)
	if err != nil {
		return nil, err
	}

	d := &ThreadDiagnostics{
		ThreadID:      threadID,
		Status:        thread.Status,
		UpdatedAt:     thread.UpdatedAt,
		CorrelationID: thread.CorrelationID,
		TaskCounts:    make(map[string]int),
	}

	// Lock state
	holder, err := c.rdb.Get(ctx, ThreadLockKey(threadID)).Result()
	if err == nil && holder != "" {
		d.Lock = &LockInfo{HolderTask: holder}
		lockedAt, err := c.rdb.Get(ctx, ThreadLockedAtKey(threadID)).Result()
		if err == nil && lockedAt != "" {
			d.Lock.LockedAt = lockedAt
			if t, err := time.Parse("2006-01-02T15:04:05Z", lockedAt); err == nil {
				d.Lock.HeldSeconds = int64(time.Since(t).Seconds())
			}
		}
	}

	// Task counts by status + find last error
	tasks, err := c.ListTasks(ctx, "", "", threadID, 200, 0, "", "")
	if err != nil {
		d.Warnings = append(d.Warnings, "failed to list tasks: "+err.Error())
	} else {
		for _, t := range tasks {
			d.TaskCounts[t.Status]++
			if t.Status == "failed" && d.LastError == "" {
				if t.ErrorMessage != "" {
					d.LastError = t.ErrorMessage
				} else if t.Result != "" {
					d.LastError = t.Result
				}
			}
		}
	}

	// Recent events
	events, _ := c.GetThreadEvents(ctx, threadID, 20)
	d.RecentEvents = events
	if d.RecentEvents == nil {
		d.RecentEvents = []Event{}
	}

	// Stuck tasks (running > 30 min)
	for _, t := range tasks {
		if t.Status == "running" && t.StartedAt != "" {
			started, err := time.Parse("2006-01-02T15:04:05Z", t.StartedAt)
			if err == nil && time.Since(started) > 30*time.Minute {
				d.StuckTasks = append(d.StuckTasks, StuckTaskInfo{
					TaskID:       t.TaskID,
					Worker:       t.Worker,
					StartedAt:    t.StartedAt,
					StaleMinutes: int64(time.Since(started).Minutes()),
				})
			}
		}
	}

	return d, nil
}

// ParsePRNumber converts a string PR number to int (for CLI usage).
func ParsePRNumber(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}
