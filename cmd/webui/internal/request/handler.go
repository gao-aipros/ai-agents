package request

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/noodle05/ai-agents/cmd/webui/internal/env"
	"github.com/noodle05/ai-agents/tasklib"
)

// Config holds configuration for the request handler.
type Config struct {
	ClaudePath        string
	ClaudeSessionsDir string
	RequestTimeout    time.Duration
	MaxConcurrent     int
	ShutdownGrace     time.Duration
	WorkspaceDir      string
	// TestNotify is an optional channel that receives the thread ID when
	// a background subprocess completes. Only used in tests.
	TestNotify chan string
}

// DefaultConfig returns a Config with defaults from environment variables.
func DefaultConfig() Config {
	return Config{
		ClaudePath:        env.String("CLAUDE_PATH", "/usr/local/bin/claude"),
		ClaudeSessionsDir: env.String("CLAUDE_SESSIONS_DIR", "/home/agent/.claude"),
		RequestTimeout:    time.Duration(env.Int("REQUEST_TIMEOUT", 1800)) * time.Second,
		MaxConcurrent:     env.Int("MAX_CONCURRENT_REQUESTS", 5),
		ShutdownGrace:     time.Duration(env.Int("REQUEST_SHUTDOWN_GRACE", 60)) * time.Second,
		WorkspaceDir:      env.String("WORKSPACE_DIR", "/workspace"),
	}
}

// Handler manages claude -p subprocess lifecycles. It spawns one-shot claude
// invocations per user request, parses the stream-json output, and writes
// results to Redis via the tasklib client.
type Handler struct {
	client *tasklib.Client
	cfg    Config
	sem    chan struct{}
	logger *log.Logger

	mu      sync.Mutex
	cancels map[string]context.CancelFunc // threadID -> cancel
	wg      sync.WaitGroup
}

// New creates a new Handler.
func New(client *tasklib.Client, cfg Config) *Handler {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 5
	}
	return &Handler{
		client:  client,
		cfg:     cfg,
		sem:     make(chan struct{}, cfg.MaxConcurrent),
		logger:  log.New(os.Stderr, "[request] ", log.LstdFlags),
		cancels: make(map[string]context.CancelFunc),
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
	// Note: two concurrent Submit calls for the same new thread may both
	// see ThreadExists=false and both call CreateThread. This is harmless:
	// HSET is idempotent (last write wins, only updated_at changes).
	exists, err := h.client.ThreadExists(ctx, threadID)
	if err != nil {
		<-h.sem // release semaphore slot
		return nil, fmt.Errorf("check thread exists: %w", err)
	}
	if !exists {
		if _, err := h.client.CreateThread(ctx, threadID, repo); err != nil {
			<-h.sem
			return nil, fmt.Errorf("create thread: %w", err)
		}
	}

	// Acquire request lock (SET NX thread:<id>:running) BEFORE writing
	// the user message so a failed lock acquisition doesn't leave an
	// orphaned message that would duplicate on retry.
	acquired, err := h.client.AcquireRequestLock(ctx, threadID, requestID, tasklib.LockTTL)
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
	if err := h.client.AppendMessage(ctx, threadID, userMsg); err != nil {
		h.client.ReleaseRequestLock(ctx, threadID)
		<-h.sem
		return nil, fmt.Errorf("append user message: %w", err)
	}

	// Determine session approach: --session-id (new) or --resume (existing)
	sessionID, err := h.client.GetThreadSessionID(ctx, threadID)
	if err != nil {
		h.client.ReleaseRequestLock(ctx, threadID)
		<-h.sem
		return nil, fmt.Errorf("get session id: %w", err)
	}

	useResume := false
	if sessionID != "" {
		// Check if the session file still exists
		if sessionFileExists(h.cfg.ClaudeSessionsDir, sessionID) {
			useResume = true
		} else {
			h.logger.Printf("thread=%s session file missing for %s, generating fresh session", threadID, sessionID)
			sessionID = ""
		}
	}

	if sessionID == "" {
		sessionID = mustUUID()
		if err := h.client.SetThreadSessionID(ctx, threadID, sessionID); err != nil {
			h.client.ReleaseRequestLock(ctx, threadID)
			<-h.sem
			return nil, fmt.Errorf("set session id: %w", err)
		}
	}

	// Build claude -p command. -p consumes the next argument as the prompt,
	// so it must come after all other flags and be followed by the prompt text.
	args := []string{
		"--dangerously-skip-permissions",
		"--output-format", "stream-json", "--verbose",
	}
	if useResume {
		args = append(args, "--resume", sessionID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	args = append(args, "-p", userRequest)

	// Create a context for the subprocess lifecycle
	procCtx, cancel := context.WithTimeout(context.Background(), h.cfg.RequestTimeout)

	// Clear previous completion state and mark thread as running.
	// Must happen before the UI polls the thread state.
	if err := h.client.ClearThreadComplete(ctx, threadID); err != nil {
		h.logger.Printf("thread=%s ClearThreadComplete error: %v", threadID, err)
	}
	if err := h.client.UpdateThread(ctx, threadID, map[string]string{"status": "running"}); err != nil {
		h.logger.Printf("thread=%s UpdateThread error: %v", threadID, err)
	}

	// Register the cancel function for external cancellation
	h.mu.Lock()
	h.cancels[threadID] = cancel
	h.mu.Unlock()

	h.wg.Add(1)
	go h.runSubprocess(procCtx, cancel, threadID, requestID, args)

	// Update last activity
	h.client.UpdateThreadLastActivity(ctx, threadID)

	return &SubmitResult{
		ThreadID:  threadID,
		RequestID: requestID,
		Status:    "submitted",
	}, nil
}

// Cancel cancels a running request for the given thread. It calls cancel()
// on the subprocess context, which sends SIGTERM to claude -p. The background
// goroutine detects the cancellation and writes an error message.
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

// Shutdown gracefully stops all in-flight subprocesses. It cancels all
// running requests and waits up to cfg.ShutdownGrace for them to exit.
func (h *Handler) Shutdown(ctx context.Context) error {
	h.logger.Printf("shutting down, cancelling in-flight requests")

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
		h.logger.Printf("all in-flight requests completed")
	case <-ctx.Done():
		h.logger.Printf("shutdown timeout exceeded, some requests may still be running")
		return ctx.Err()
	}
	return nil
}

// ActiveRequests returns the number of currently running requests.
func (h *Handler) ActiveRequests() int {
	return len(h.sem)
}

// cleanupCtx returns a context with a 30s deadline for Redis cleanup
// operations. Using context.Background() in cleanup paths risks blocking
// indefinitely if Redis is unreachable, leaking the semaphore slot and
// per-thread request lock.
func cleanupCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// ── background subprocess management ──────────────────────────────────────

func (h *Handler) runSubprocess(ctx context.Context, cancel context.CancelFunc, threadID, requestID string, args []string) {
	defer h.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			h.logger.Printf("panic in subprocess goroutine thread=%s: %v", threadID, r)
			h.writeErrorMessage(ctx, threadID, fmt.Sprintf("internal error: panic in handler: %v", r))
		}
	}()

	// Cleanup: release semaphore, request lock, remove cancel registration
	defer func() {
		<-h.sem
		cleanCtx, cleanCancel := cleanupCtx()
		defer cleanCancel()
		h.client.ReleaseRequestLock(cleanCtx, threadID)
		h.client.UpdateThreadLastActivity(cleanCtx, threadID)

		h.mu.Lock()
		delete(h.cancels, threadID)
		h.mu.Unlock()

		if h.cfg.TestNotify != nil {
			h.cfg.TestNotify <- threadID
		}
	}()

	h.logger.Printf("thread=%s request=%s spawning claude -p", threadID, requestID)

	cmd := exec.CommandContext(ctx, h.cfg.ClaudePath, args...)
	cmd.Dir = filepath.Join(h.cfg.WorkspaceDir, threadID)
	if err := os.MkdirAll(cmd.Dir, 0755); err != nil {
		h.writeErrorMessage(ctx, threadID, fmt.Sprintf("failed to create workspace dir: %v", err))
		return
	}

	// Session files are stored in ~/.claude/projects/ by default.
	// The Docker volume claude_sessions is mounted at ~/.claude so
	// sessions persist across restarts — no extra env vars needed.

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		h.writeErrorMessage(ctx, threadID, fmt.Sprintf("failed to create stdout pipe: %v", err))
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		h.writeErrorMessage(ctx, threadID, fmt.Sprintf("failed to create stderr pipe: %v", err))
		return
	}

	if err := cmd.Start(); err != nil {
		h.writeErrorMessage(ctx, threadID, fmt.Sprintf("failed to start claude: %v", err))
		return
	}

	// Check for cancellation before processing output
	if h.isCancelled(ctx) {
		return
	}

	// Read stdout line by line, parse stream-json.
	// stderr is collected in a background goroutine for error reporting.
	var stderrMu sync.Mutex
	var stderrWg sync.WaitGroup
	var lastStderr strings.Builder
	stderrWg.Add(1)
	go func() {
		defer stderrWg.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			stderrMu.Lock()
			lastStderr.WriteString(scanner.Text())
			lastStderr.WriteByte('\n')
			// Keep only the last 4KB
			if lastStderr.Len() > 4096 {
				s := lastStderr.String()
				lastStderr.Reset()
				lastStderr.WriteString(s[len(s)-4096:])
			}
			stderrMu.Unlock()
		}
	}()

	completed := false
	lastWritten := "" // dedup: skip response if identical to last assistant message
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 64*1024) // 64KB line buffer

	for scanner.Scan() {
		if h.isCancelled(ctx) {
			// Process was cancelled — claude will be killed by context
			break
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg streamMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			h.logger.Printf("thread=%s unparseable stream-json line: %v", threadID, err)
			continue
		}

		switch msg.Type {
		case "system":
			// init messages — discard
			continue

		case "user":
			// tool result feedback — discard (workers write results via tasklib)
			continue

		case "assistant":
			if content := h.handleAssistantMessage(ctx, threadID, &msg); content != "" {
				lastWritten = content
			}

		case "result":
			completed = true
			if msg.IsError {
				errContent := msg.Result
				if errContent == "" {
					errContent = fmt.Sprintf("claude error: subtype=%s", msg.Subtype)
				}
				h.writeErrorMessage(ctx, threadID, errContent)
			} else if msg.Result == lastWritten {
				h.completeThread(ctx, threadID)
			} else {
				h.writeResponseMessage(ctx, threadID, msg.Result)
			}
		}
	}

	// Wait for the process to exit (or kill it if timeout/cancelled).
	// Must do this before reading lastStderr so the stderr pipe is closed
	// and the collector goroutine has consumed all output.
	if ctx.Err() != nil {
		// Give claude a 10s grace period to exit on SIGTERM
		done := make(chan struct{})
		go func() {
			cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Signal(syscall.SIGKILL)
			cmd.Wait()
		}
	} else {
		cmd.Wait()
	}

	// Wait for stderr collector to finish now that the process exited
	// and the pipe is closed.
	stderrWg.Wait()

	if !completed {
		// Subprocess exited without emitting a result message
		stderrMu.Lock()
		errContent := strings.TrimSpace(lastStderr.String())
		stderrMu.Unlock()

		// Log stderr for debugging
		if errContent != "" {
			h.logger.Printf("thread=%s claude stderr: %s", threadID, errContent)
		}

		if errContent == "" {
			errContent = "claude exited without emitting a result message"
		}

		if ctx.Err() == context.DeadlineExceeded {
			errContent = fmt.Sprintf("Master agent timed out after %s", h.cfg.RequestTimeout)
		} else if ctx.Err() == context.Canceled {
			errContent = "Request cancelled"
		}

		if !h.isCancelled(ctx) {
			h.writeErrorMessage(ctx, threadID, errContent)
		} else {
			h.writeErrorMessage(ctx, threadID, "Request cancelled")
		}
	}
}

// ── message handling ──────────────────────────────────────────────────────

// handleAssistantMessage classifies assistant output as "plan" or "tool_call"
// and writes it to thread history for live progress display.
// Returns the content written (empty string if nothing was written).
func (h *Handler) handleAssistantMessage(ctx context.Context, threadID string, msg *streamMessage) string {
	msgType := "plan"
	if msg.Message != nil && hasToolUse(msg.Message.Content) {
		msgType = "tool_call"
	}

	text := extractText(msg)
	if text == "" {
		return ""
	}

	cleanCtx, cleanCancel := cleanupCtx()
	defer cleanCancel()
	h.client.AppendMessage(cleanCtx, threadID, tasklib.Message{
		Role:      "master",
		Type:      msgType,
		Content:   text,
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})
	return text
}

func (h *Handler) writeResponseMessage(ctx context.Context, threadID, content string) {
	h.logger.Printf("thread=%s completed successfully", threadID)

	cleanCtx, cleanCancel := cleanupCtx()
	defer cleanCancel()

	h.client.SetThreadComplete(cleanCtx, threadID)

	h.client.AppendMessage(cleanCtx, threadID, tasklib.Message{
		Role:      "master",
		Type:      "response",
		Content:   content,
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})

	h.client.UpdateThread(cleanCtx, threadID, map[string]string{
		"status": "complete",
	})
}

func (h *Handler) writeErrorMessage(ctx context.Context, threadID, content string) {
	h.logger.Printf("thread=%s error: %s", threadID, content)

	cleanCtx, cleanCancel := cleanupCtx()
	defer cleanCancel()

	h.client.SetThreadComplete(cleanCtx, threadID)

	h.client.AppendMessage(cleanCtx, threadID, tasklib.Message{
		Role:      "master",
		Type:      "error",
		Content:   content,
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})

	h.client.UpdateThread(cleanCtx, threadID, map[string]string{
		"status": "error",
	})
}

// completeThread marks the thread complete without appending a message.
// Used when the final result matches the last assistant message (dedup).
func (h *Handler) completeThread(ctx context.Context, threadID string) {
	h.logger.Printf("thread=%s completed successfully (dedup)", threadID)

	cleanCtx, cleanCancel := cleanupCtx()
	defer cleanCancel()

	h.client.SetThreadComplete(cleanCtx, threadID)
	h.client.UpdateThread(cleanCtx, threadID, map[string]string{
		"status": "complete",
	})
}

func (h *Handler) isCancelled(ctx context.Context) bool {
	return ctx.Err() != nil
}

// ValidThreadID rejects thread IDs containing path traversal sequences
// or colons (which would break Redis key parsing in ListThreads).
func ValidThreadID(id string) bool {
	if id == "" {
		return false
	}
	return !strings.Contains(id, "..") && !strings.ContainsAny(id, "/\\:")
}

// ── stream-json parsing ───────────────────────────────────────────────────

// streamMessage represents a single JSON line from claude --output-format stream-json.
type streamMessage struct {
	Type    string            `json:"type"`
	Subtype string            `json:"subtype"`
	IsError bool              `json:"is_error"`
	Result  string            `json:"result"`
	Message *streamAssistant  `json:"message"`
}

type streamAssistant struct {
	Content []streamContentBlock `json:"content"`
}

type streamContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// hasToolUse returns true if any content block is a tool_use.
func hasToolUse(blocks []streamContentBlock) bool {
	for _, b := range blocks {
		if b.Type == "tool_use" {
			return true
		}
	}
	return false
}

// extractText concatenates all text blocks from an assistant message.
func extractText(msg *streamMessage) string {
	if msg.Message == nil {
		return ""
	}
	var parts []string
	for _, b := range msg.Message.Content {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// ── session file detection ────────────────────────────────────────────────

// sessionFileExists checks whether a session file for the given UUID exists
// under the Claude sessions directory.
func sessionFileExists(sessionsDir, sessionID string) bool {
	projectsDir := filepath.Join(sessionsDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return false
	}

	// Session files are named <session-id>.json inside per-project directories
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionFile := filepath.Join(projectsDir, entry.Name(), sessionID+".jsonl")
		if _, err := os.Stat(sessionFile); err == nil {
			return true
		}
	}
	return false
}

// ── error sentinels ───────────────────────────────────────────────────────

var (
	ErrThreadBusy       = &RequestError{Status: 409, Message: "Thread is already processing a request"}
	ErrConcurrencyLimit = &RequestError{Status: 503, Message: "Too many concurrent requests"}
	ErrNoRunningRequest = &RequestError{Status: 404, Message: "No running request for this thread"}
)

// RequestError is an error with an HTTP status code.
type RequestError struct {
	Status  int    `json:"-"`
	Message string `json:"error"`
}

func (e *RequestError) Error() string { return e.Message }

// ── helpers ────────────────────────────────────────────────────────────────

func mustUUID() string {
	id, err := tasklib.NewUUID()
	if err == nil {
		return id
	}
	// Fallback: crypto/rand failure is catastrophic, but produce a valid
	// UUID v4 so --session-id / --resume still work. crypto/rand failing
	// here means tasklib.NewUUID() already failed the same way — try once
	// more with a fresh read, then degrade to a deterministic suffix.
	var b [16]byte
	if _, err2 := crand.Read(b[:]); err2 == nil {
		b[6] = (b[6] & 0x0f) | 0x40
		b[8] = (b[8] & 0x3f) | 0x80
		return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1000000000000)
}
