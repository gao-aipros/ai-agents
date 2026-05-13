package request

import (
	"bufio"
	"context"
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
	requestID := mustUUID()

	// Check global concurrency limit
	select {
	case h.sem <- struct{}{}:
	default:
		return nil, ErrConcurrencyLimit
	}

	// Create thread if it doesn't exist
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

	// Write user request to thread history
	userMsg := tasklib.Message{
		Role:      "user",
		Type:      "request",
		Content:   userRequest,
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	if err := h.client.AppendMessage(ctx, threadID, userMsg); err != nil {
		<-h.sem
		return nil, fmt.Errorf("append user message: %w", err)
	}

	// Acquire request lock (SET NX thread:<id>:running)
	acquired, err := h.client.AcquireRequestLock(ctx, threadID, requestID, tasklib.LockTTL)
	if err != nil {
		<-h.sem
		return nil, fmt.Errorf("acquire request lock: %w", err)
	}
	if !acquired {
		<-h.sem
		return nil, ErrThreadBusy
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

	// Build claude -p command
	args := []string{
		"--dangerously-skip-permissions", "--bare", "-p",
		"--output-format", "stream-json", "--verbose",
	}
	if useResume {
		args = append(args, "--resume", sessionID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	args = append(args, userRequest)

	// Create a context for the subprocess lifecycle
	procCtx, cancel := context.WithTimeout(context.Background(), h.cfg.RequestTimeout)

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
		h.client.ReleaseRequestLock(context.Background(), threadID)
		h.client.UpdateThreadLastActivity(context.Background(), threadID)

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
	os.MkdirAll(cmd.Dir, 0755)

	// Set CLAUDE_CONFIG_DIR so session files land in the shared volume
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+h.cfg.ClaudeSessionsDir)

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
	if h.isCancelled(ctx, threadID) {
		return
	}

	// Read stdout line by line, parse stream-json.
	// stderr is collected in a background goroutine for error reporting.
	var stderrMu sync.Mutex
	var lastStderr strings.Builder
	go func() {
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
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 64*1024) // 64KB line buffer

	for scanner.Scan() {
		if h.isCancelled(ctx, threadID) {
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
			h.handleAssistantMessage(ctx, threadID, &msg)

		case "result":
			completed = true
			if msg.IsError {
				errContent := msg.Result
				if errContent == "" {
					errContent = fmt.Sprintf("claude error: subtype=%s", msg.Subtype)
				}
				h.writeErrorMessage(ctx, threadID, errContent)
			} else {
				h.writeResponseMessage(ctx, threadID, msg.Result)
			}
		}
	}

	if !completed {
		// Subprocess exited without emitting a result message
		stderrMu.Lock()
		errContent := lastStderr.String()
		stderrMu.Unlock()
		if errContent == "" {
			errContent = "claude exited without emitting a result message"
		}

		if ctx.Err() == context.DeadlineExceeded {
			errContent = fmt.Sprintf("Master agent timed out after %s", h.cfg.RequestTimeout)
		} else if ctx.Err() == context.Canceled {
			errContent = "Request cancelled"
		}

		if !h.isCancelled(ctx, threadID) {
			h.writeErrorMessage(ctx, threadID, errContent)
		} else {
			h.writeErrorMessage(ctx, threadID, "Request cancelled")
		}
	}

	// Wait for the process to exit (or kill it if timeout/cancelled)
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
}

// ── message handling ──────────────────────────────────────────────────────

// handleAssistantMessage classifies assistant output as "plan" or "tool_call"
// and writes it to thread history for live progress display.
func (h *Handler) handleAssistantMessage(ctx context.Context, threadID string, msg *streamMessage) {
	msgType := "plan"
	if msg.Message != nil && hasToolUse(msg.Message.Content) {
		msgType = "tool_call"
	}

	text := extractText(msg)
	if text == "" {
		return
	}

	h.client.AppendMessage(context.Background(), threadID, tasklib.Message{
		Role:      "master",
		Type:      msgType,
		Content:   text,
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})
}

func (h *Handler) writeResponseMessage(ctx context.Context, threadID, content string) {
	h.logger.Printf("thread=%s completed successfully", threadID)

	h.client.SetThreadComplete(context.Background(), threadID)

	h.client.AppendMessage(context.Background(), threadID, tasklib.Message{
		Role:      "master",
		Type:      "response",
		Content:   content,
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})

	h.client.UpdateThread(context.Background(), threadID, map[string]string{
		"status": "complete",
	})
}

func (h *Handler) writeErrorMessage(ctx context.Context, threadID, content string) {
	h.logger.Printf("thread=%s error: %s", threadID, content)

	h.client.AppendMessage(context.Background(), threadID, tasklib.Message{
		Role:      "master",
		Type:      "error",
		Content:   content,
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})

	h.client.UpdateThread(context.Background(), threadID, map[string]string{
		"status": "error",
	})
}

func (h *Handler) isCancelled(ctx context.Context, threadID string) bool {
	status, err := h.client.GetThread(ctx, threadID)
	if err != nil {
		return false
	}
	return status.Status == "cancelled" || ctx.Err() != nil
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
		sessionFile := filepath.Join(projectsDir, entry.Name(), sessionID+".json")
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
	if err != nil {
		// fallback: timestamp-based identifier
		return fmt.Sprintf("fallback_%d", time.Now().UnixNano())
	}
	return id
}
