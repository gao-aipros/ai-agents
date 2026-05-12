package tasklib

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
)

// ── embedded Lua scripts ──────────────────────────────────────────────────

//go:embed lua/push_request.lua
var pushRequestScript string

//go:embed lua/cancel_request.lua
var cancelRequestScript string

// ── request payload ───────────────────────────────────────────────────────

// PushRequestPayload is the JSON payload LPUSHed to requests:inbox.
type PushRequestPayload struct {
	RequestID string `json:"request_id"`
	ThreadID  string `json:"thread_id"`
	Repo      string `json:"repo,omitempty"`
	Request   string `json:"request"`
	Timestamp string `json:"timestamp"`
}

// ── atomic operations ─────────────────────────────────────────────────────

// PushRequestAtomic atomically writes a web UI request to:
//  1. Thread state (sets gh_repo if missing)
//  2. Thread messages (user request)
//  3. requests:inbox (the pending request payload)
//
// Uses a Lua script for atomicity. All three operations succeed or fail together.
func (c *Client) PushRequestAtomic(ctx context.Context, threadID, repo, request string) error {
	requestID, err := newUUID()
	if err != nil {
		return fmt.Errorf("generate request id: %w", err)
	}
	now := ts()

	payload := PushRequestPayload{
		RequestID: requestID,
		ThreadID:  threadID,
		Repo:      repo,
		Request:   request,
		Timestamp: now,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	msg := Message{
		Role:      "user",
		Type:      "request",
		Content:   request,
		Timestamp: now,
		Source:    "webui",
	}
	msgJSON, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	keys := []string{
		ThreadStateKey(threadID),
		ThreadMessagesKey(threadID),
		"requests:inbox",
		fmt.Sprintf("requests:inbox:pending:%s", threadID),
	}
	args := []interface{}{
		repo,
		now,
		string(msgJSON),
		string(payloadJSON),
	}

	result, err := c.rdb.Eval(ctx, pushRequestScript, keys, args...).Result()
	if err != nil {
		return fmt.Errorf("push_request lua: %w", err)
	}

	if result == "duplicate" {
		return fmt.Errorf("thread %s already has a pending request", threadID)
	}

	// Set TTL on thread state and messages (non-critical, outside atomic block)
	c.rdb.Expire(ctx, ThreadStateKey(threadID), TTLThread)
	c.rdb.Expire(ctx, ThreadMessagesKey(threadID), TTLThread)

	return nil
}

// CancelRequest atomically cancels a pending request by removing it from
// requests:inbox and requests:inbox_processing, and setting thread status to cancelled.
func (c *Client) CancelRequest(ctx context.Context, threadID string) error {
	keys := []string{
		"requests:inbox",
		"requests:inbox_processing",
		ThreadStateKey(threadID),
		fmt.Sprintf("requests:inbox:pending:%s", threadID),
	}
	args := []interface{}{threadID}

	_, err := c.rdb.Eval(ctx, cancelRequestScript, keys, args...).Result()
	return err
}
