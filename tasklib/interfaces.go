package tasklib

import (
	"context"
	"time"
)

// TaskStore defines the task lifecycle operations.
type TaskStore interface {
	Enqueue(ctx context.Context, worker, threadID, instruction string) (*Task, error)
	EnqueueGroup(ctx context.Context, worker, threadID, groupLabel, instruction string) (*Task, error)
	GetTask(ctx context.Context, taskID string) (*Task, error)
	GetTaskResult(ctx context.Context, taskID string, tail int) (string, error)
	ListTasks(ctx context.Context, worker, status, threadID string, limit, offset int, sortBy, sortDir string) ([]*Task, error)
	WaitTask(ctx context.Context, taskID, threadID string, timeout time.Duration) (*Task, error)
	GroupWait(ctx context.Context, threadID, groupLabel string, timeout time.Duration) (*GroupResult, error)
	CancelTask(ctx context.Context, taskID, cancelledBy string) error
	RequeueStale(ctx context.Context, worker string, olderThan time.Duration) ([]string, error)
}

// RequestStore defines request-lock and session-ID operations for the
// web UI request handler.
type RequestStore interface {
	AcquireRequestLock(ctx context.Context, threadID, requestID string, ttl time.Duration) (bool, error)
	ReleaseRequestLock(ctx context.Context, threadID string) error
	SetThreadSessionID(ctx context.Context, threadID, sessionID string) error
	GetThreadSessionID(ctx context.Context, threadID string) (string, error)
	IsRequestRunning(ctx context.Context, threadID string) (bool, error)
	CancelRequest(ctx context.Context, threadID string) error
}

// ThreadHistory defines message append, retrieval, and activity-timestamp
// operations.
type ThreadHistory interface {
	AppendMessage(ctx context.Context, threadID string, msg Message) error
	GetThreadHistory(ctx context.Context, threadID string, offset, limit int) ([]Message, error)
	GetThreadHistoryTail(ctx context.Context, threadID string, tail int) ([]Message, error)
	GetThreadHistoryTailForWorker(ctx context.Context, threadID string, tail int, worker string) ([]Message, error)
	ThreadMessagesLen(ctx context.Context, threadID string) (int64, error)
	UpdateThreadLastActivity(ctx context.Context, threadID string) error
	GetThreadLastActivity(ctx context.Context, threadID string) (string, error)
}

// ThreadStore defines thread lifecycle, locking, active-task tracking,
// cascade delete, and diagnostics.
type ThreadStore interface {
	// Core lifecycle
	CreateThread(ctx context.Context, threadID, repo, parentThreadID string) (*Thread, error)
	GetThread(ctx context.Context, threadID string) (*Thread, error)
	ListThreads(ctx context.Context, sortBy, sortDir string) ([]*Thread, error)
	UpdateThread(ctx context.Context, threadID string, fields map[string]string) error
	ThreadExists(ctx context.Context, threadID string) (bool, error)
	SetThreadTTL(ctx context.Context, threadID string, ttl time.Duration) error
	GetThreadDiagnostics(ctx context.Context, threadID string) (*ThreadDiagnostics, error)

	// Locking
	LockThread(ctx context.Context, threadID, taskID string, ttl time.Duration) (bool, error)
	IsThreadLocked(ctx context.Context, threadID string) (bool, error)
	UnlockThread(ctx context.Context, threadID string) error

	// Active tasks
	SetActiveTask(ctx context.Context, taskID string, info TaskInfo) error
	RemoveActiveTask(ctx context.Context, taskID string) error
	GetActiveTasks(ctx context.Context) (map[string]*TaskInfo, error)

	// Cascade delete
	DiscoverDescendants(ctx context.Context, threadID string) (map[string]bool, error)
	DeleteThreadKnown(ctx context.Context, threadID string, descendants map[string]bool) error
	DeleteThread(ctx context.Context, threadID string) error

	// Completion flags
	SetThreadComplete(ctx context.Context, threadID string) error
	ClearThreadComplete(ctx context.Context, threadID string) error
	IsThreadComplete(ctx context.Context, threadID string) (bool, error)
}

// EventBus defines event publishing and querying.
type EventBus interface {
	PushEvent(ctx context.Context, listKey string, ev *Event)
	PushThreadEvent(ctx context.Context, threadID string, ev *Event)
	PushSystemEvent(ctx context.Context, ev *Event)
	GetThreadEvents(ctx context.Context, threadID string, limit int) ([]Event, error)
	GetSystemEvents(ctx context.Context, limit int) ([]Event, error)
}

// WorkerRegistry defines worker heartbeat and stats operations.
type WorkerRegistry interface {
	UpdateWorkerHeartbeat(ctx context.Context, workerType, hostname string, data HeartbeatData) error
	GetWorkerStats(ctx context.Context) (WorkerStats, error)
	GetWorkerInfo(ctx context.Context, workerType string) (*WorkerInfo, error)
	GetWorkerInstances(ctx context.Context, workerType string) ([]WorkerInstance, error)
}

// ThreadScanner defines thread-state scanning with a caller-supplied predicate.
type ThreadScanner interface {
	Scan(ctx context.Context, predicate func(ThreadState) bool) ([]ThreadState, error)
}

// TokenLedger defines token usage tracking operations.
type TokenLedger interface {
	GetTokenStats(ctx context.Context, key string) (*TokenStats, error)
	GetTokenStatsTaskCount(ctx context.Context, key string) (int64, error)
	GetMasterTokenStats(ctx context.Context, threadID string) (TokenStats, error)
	GetTaskTokenStats(ctx context.Context, taskID string) (TokenStats, error)
}

// SystemOps defines infrastructure/observability operations that don't fit
// into the domain-specific role interfaces (TaskStore, ThreadStore, etc.).
// It replaces the raw *redis.Client escape hatch previously exposed by
// Services.RDB().
type SystemOps interface {
	// Ping checks Redis connectivity.
	Ping(ctx context.Context) error
	// ScanKeys returns keys matching pattern using SCAN.
	// count is the COUNT hint passed to each SCAN iteration (0 = Redis default of 10).
	// Callers should pass a positive count (10+) for reasonable performance.
	ScanKeys(ctx context.Context, pattern string, count int64) ([]string, error)
	GetKey(ctx context.Context, key string) (string, error)
	ActiveTaskCount(ctx context.Context) (int64, error)
	GetAllActiveTasks(ctx context.Context) (map[string]string, error)
	QueueDepth(ctx context.Context, queueKey string) (int64, error)
	GetCounters(ctx context.Context, keys ...string) ([]any, error)
	Info(ctx context.Context, section string) (string, error)
	PersistMasterTokenStats(ctx context.Context, threadID string, stats TokenStats) error
}

// Compile-time assertions: *Client satisfies all interfaces.
var (
	_ TaskStore      = (*Client)(nil)
	_ ThreadStore    = (*Client)(nil)
	_ RequestStore   = (*Client)(nil)
	_ ThreadHistory  = (*Client)(nil)
	_ EventBus       = (*Client)(nil)
	_ WorkerRegistry = (*Client)(nil)
	_ TokenLedger    = (*Client)(nil)
	_ ThreadScanner  = (*Client)(nil)
	_ SystemOps      = (*Client)(nil)
)
