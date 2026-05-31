package request

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/noodle05/ai-agents/tasklib"
)

// Handler manages claude -p subprocess lifecycles. It spawns one-shot claude
// invocations per user request, reads stdout, and writes results to Redis via
// the tasklib client.
type Handler struct {
	threads  tasklib.ThreadStore
	requests tasklib.RequestStore
	history  tasklib.ThreadHistory
	sysOps   tasklib.SystemOps
	cfg      Config
	sem      chan struct{}
	logger   *slog.Logger

	mu      sync.Mutex
	cancels map[string]context.CancelFunc // threadID -> cancel
	wg      sync.WaitGroup
}

// New creates a new Handler.
func New(threads tasklib.ThreadStore, requests tasklib.RequestStore, history tasklib.ThreadHistory, sysOps tasklib.SystemOps, cfg Config) *Handler {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 5
	}
	return &Handler{
		threads:  threads,
		requests: requests,
		history:  history,
		sysOps:   sysOps,
		cfg:      cfg,
		sem:      make(chan struct{}, cfg.MaxConcurrent),
		logger:   slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).With("component", "request"),
		cancels:  make(map[string]context.CancelFunc),
	}
}

// SetClaudePath sets the path to the claude binary. Used in tests to
// point at a fake shell script.
func (h *Handler) SetClaudePath(path string) {
	h.cfg.ClaudePath = path
}

// SubmitResult is returned by Submit after successfully spawning claude -p.
type SubmitResult struct {
	ThreadID  string `json:"thread_id"`
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
}

// Submit spawns claude -p as a background subprocess for the given request.
// It acquires the per-thread request lock, manages session UUIDs, and returns
// immediately. The background goroutine writes messages and the final response
// to Redis.
//
// Returns an error if the thread is already processing a request (409),
// the global concurrency limit is reached (503), or setup fails.
func (h *Handler) Submit(ctx context.Context, threadID, userRequest, repo string) (*SubmitResult, error) {
	if !ValidThreadID(threadID) {
		return nil, fmt.Errorf("invalid thread_id: %q", threadID)
	}

	requestID := mustUUID()

	// Check global concurrency limit
	select {
	case h.sem <- struct{}{}:
	default:
		return nil, ErrConcurrencyLimit
	}

	// Create thread if it doesn't exist.
	exists, err := h.threads.ThreadExists(ctx, threadID)
	if err != nil {
		<-h.sem
		return nil, fmt.Errorf("check thread exists: %w", err)
	}
	if !exists {
		if _, err := h.threads.CreateThread(ctx, threadID, repo, ""); err != nil {
			<-h.sem
			return nil, fmt.Errorf("create thread: %w", err)
		}
	}

	// Acquire request lock (SET NX thread:<id>:running) BEFORE writing
	// the user message so a failed lock acquisition doesn't leave an
	// orphaned message that would duplicate on retry.
	acquired, err := h.requests.AcquireRequestLock(ctx, threadID, requestID, tasklib.LockTTL)
	if err != nil {
		<-h.sem
		return nil, fmt.Errorf("acquire request lock: %w", err)
	}
	if !acquired {
		<-h.sem
		return nil, ErrThreadBusy
	}

	// Write user request to thread history
	userMsg := tasklib.Message{
		Role:      "user",
		Type:      "request",
		Content:   userRequest,
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	if err := h.history.AppendMessage(ctx, threadID, userMsg); err != nil {
		h.requests.ReleaseRequestLock(ctx, threadID)
		<-h.sem
		return nil, fmt.Errorf("append user message: %w", err)
	}

	// Determine session approach: --session-id (new) or --resume (existing)
	sessionID, err := h.requests.GetThreadSessionID(ctx, threadID)
	if err != nil {
		h.requests.ReleaseRequestLock(ctx, threadID)
		<-h.sem
		return nil, fmt.Errorf("get session id: %w", err)
	}

	useResume := false
	if sessionID != "" {
		if sessionFileExists(h.cfg.Paths.ClaudeSessionsDir, sessionID) {
			useResume = true
		} else {
			h.logger.Info(fmt.Sprintf("thread=%s session file missing for %s, generating fresh session", threadID, sessionID))
			sessionID = ""
		}
	}

	if sessionID == "" {
		sessionID = mustUUID()
		if err := h.requests.SetThreadSessionID(ctx, threadID, sessionID); err != nil {
			h.requests.ReleaseRequestLock(ctx, threadID)
			<-h.sem
			return nil, fmt.Errorf("set session id: %w", err)
		}
	}

	// Build claude -p command. In "text" mode (default) we use plain -p
	// with no --output-format flag. In "stream-json" mode we add
	// --output-format stream-json --verbose for backward compatibility.
	args := []string{"--dangerously-skip-permissions"}
	if h.cfg.OutputFormat == "stream-json" {
		args = append(args, "--output-format", "stream-json", "--verbose")
	}
	if useResume {
		args = append(args, "--resume", sessionID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	args = append(args, "-p", userRequest)

	// Create a context for the subprocess lifecycle
	procCtx, cancel := context.WithTimeout(context.Background(), h.cfg.RequestTimeout)

	// Clear previous completion state.
	if err := h.threads.ClearThreadComplete(ctx, threadID); err != nil {
		h.logger.Info(fmt.Sprintf("thread=%s ClearThreadComplete error: %v", threadID, err))
	}
	// Only set status to "running" if no sequential task holds the thread lock.
	// When a task is actively running on this thread, the task lifecycle
	// (WaitTask/updateThreadStatus) owns the status field.
	locked, err := h.threads.IsThreadLocked(ctx, threadID)
	if err != nil {
		h.logger.Info(fmt.Sprintf("thread=%s IsThreadLocked error: %v", threadID, err))
	}
	if !locked {
		if err := h.threads.UpdateThread(ctx, threadID, map[string]string{"status": "running"}); err != nil {
			h.logger.Info(fmt.Sprintf("thread=%s UpdateThread error: %v", threadID, err))
		}
	}

	// Register the cancel function for external cancellation
	h.mu.Lock()
	h.cancels[threadID] = cancel
	h.mu.Unlock()

	h.wg.Add(1)
	go h.runSubprocess(procCtx, cancel, threadID, requestID, args)

	h.history.UpdateThreadLastActivity(ctx, threadID)

	return &SubmitResult{
		ThreadID:  threadID,
		RequestID: requestID,
		Status:    "submitted",
	}, nil
}

// Cancel cancels a running request for the given thread.
func (h *Handler) Cancel(threadID string) error {
	h.mu.Lock()
	cancel, ok := h.cancels[threadID]
	if ok {
		delete(h.cancels, threadID)
	}
	h.mu.Unlock()

	if ok {
		cancel()
		return nil
	}
	return ErrNoRunningRequest
}

// Shutdown gracefully stops all in-flight subprocesses.
func (h *Handler) Shutdown(ctx context.Context) error {
	h.logger.Info(fmt.Sprintf("shutting down, cancelling in-flight requests"))

	h.mu.Lock()
	for threadID, cancel := range h.cancels {
		cancel()
		delete(h.cancels, threadID)
	}
	h.mu.Unlock()

	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		h.logger.Info(fmt.Sprintf("all in-flight requests completed"))
	case <-ctx.Done():
		h.logger.Info(fmt.Sprintf("shutdown timeout exceeded, some requests may still be running"))
		return ctx.Err()
	}
	return nil
}

// ActiveRequests returns the number of currently running requests.
func (h *Handler) ActiveRequests() int {
	return len(h.sem)
}

// cleanupCtx returns a context with a 30s deadline for Redis cleanup operations.
func cleanupCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

func (h *Handler) writeResponseMessage(ctx context.Context, threadID, content string) {
	h.logger.Info(fmt.Sprintf("thread=%s completed successfully", threadID))

	cleanCtx, cleanCancel := cleanupCtx()
	defer cleanCancel()

	h.threads.SetThreadComplete(cleanCtx, threadID)

	h.history.AppendMessage(cleanCtx, threadID, tasklib.Message{
		Role:      h.cfg.AgentName,
		Type:      "response",
		Content:   content,
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})

	locked, err := h.threads.IsThreadLocked(cleanCtx, threadID)
	if err != nil {
		h.logger.Info(fmt.Sprintf("thread=%s IsThreadLocked error: %v", threadID, err))
	}
	if err == nil && !locked {
		h.threads.UpdateThread(cleanCtx, threadID, map[string]string{
			"status": "complete",
		})
	}
}

func (h *Handler) writeErrorMessage(ctx context.Context, threadID, content string) {
	h.logger.Warn(fmt.Sprintf("thread=%s error: %s", threadID, content))

	cleanCtx, cleanCancel := cleanupCtx()
	defer cleanCancel()

	h.threads.SetThreadComplete(cleanCtx, threadID)

	h.history.AppendMessage(cleanCtx, threadID, tasklib.Message{
		Role:      h.cfg.AgentName,
		Type:      "error",
		Content:   content,
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})

	locked, err := h.threads.IsThreadLocked(cleanCtx, threadID)
	if err != nil {
		h.logger.Info(fmt.Sprintf("thread=%s IsThreadLocked error: %v", threadID, err))
	}
	if err == nil && !locked {
		h.threads.UpdateThread(cleanCtx, threadID, map[string]string{
			"status": "error",
		})
	}
}

// completeThread marks the thread complete without appending a message.
func (h *Handler) completeThread(ctx context.Context, threadID string) {
	h.logger.Info(fmt.Sprintf("thread=%s completed successfully (dedup)", threadID))

	cleanCtx, cleanCancel := cleanupCtx()
	defer cleanCancel()

	h.threads.SetThreadComplete(cleanCtx, threadID)
	locked, err := h.threads.IsThreadLocked(cleanCtx, threadID)
	if err != nil {
		h.logger.Info(fmt.Sprintf("thread=%s IsThreadLocked error: %v", threadID, err))
	}
	if err == nil && !locked {
		h.threads.UpdateThread(cleanCtx, threadID, map[string]string{
			"status": "complete",
		})
	}
}

func (h *Handler) isCancelled(ctx context.Context) bool {
	return ctx.Err() != nil
}

// ValidThreadID rejects thread IDs containing path traversal sequences or colons.
func ValidThreadID(id string) bool {
	if id == "" {
		return false
	}
	return !strings.Contains(id, "..") && !strings.ContainsAny(id, "/\\:")
}

func mustUUID() string {
	id, err := tasklib.NewUUID()
	if err == nil {
		return id
	}
	var b [16]byte
	if _, err2 := crand.Read(b[:]); err2 == nil {
		b[6] = (b[6] & 0x0f) | 0x40
		b[8] = (b[8] & 0x3f) | 0x80
		return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1000000000000)
}
