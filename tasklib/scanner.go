package tasklib

import (
	"context"
	"strings"
)

// ThreadState is an enriched snapshot of a thread's current_state hash,
// purpose-built for scan-and-filter callers. It is intentionally narrower
// than Thread — CorrelationID and LastDesign are omitted because scan
// predicates don't need them.
type ThreadState struct {
	ThreadID       string
	Status         string
	UpdatedAt      string
	CreatedAt      string
	GHRepo         string
	GHPRNumber     string
	ParentThreadID string
}

// Scan iterates all thread:*:current_state keys, enriches each key with
// HGetAll, and collects every ThreadState for which predicate returns true.
//
// Returns nil (not empty slice) when no threads match the predicate or the
// keyspace is empty. Callers that need to distinguish "no keys" from "no
// matches" should check the error first, then len(result).
//
// Per-key HGetAll failures are silently skipped (best-effort). A SCAN
// error at the Redis level is returned immediately.
func (c *Client) Scan(ctx context.Context, predicate func(ThreadState) bool) ([]ThreadState, error) {
	var result []ThreadState
	var cursor uint64

	for {
		keys, nextCursor, err := c.rdb.Scan(ctx, cursor, "thread:*:current_state", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			// key format: "thread:<id>:current_state"
			// SplitN with limit 3 is defensive; the known pattern always produces ≥2 parts.
			parts := strings.SplitN(key, ":", 3)
			if len(parts) < 2 {
				continue
			}
			threadID := parts[1]
			fields, err := c.rdb.HGetAll(ctx, key).Result()
			if err != nil {
				continue
			}
			ts := ThreadState{
				ThreadID:       threadID,
				Status:         fields["status"],
				UpdatedAt:      fields["updated_at"],
				CreatedAt:      fields["created_at"],
				GHRepo:         fields["gh_repo"],
				GHPRNumber:     fields["gh_pr_number"],
				ParentThreadID: fields["parent_thread_id"],
			}
			if predicate(ts) {
				result = append(result, ts)
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return result, nil
}
