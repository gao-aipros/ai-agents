package request

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	// OutputFormat controls the claude -p output mode: "text" (plain -p) or
	// "stream-json" (--output-format stream-json --verbose). Default "text".
	OutputFormat string
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
		OutputFormat:      env.String("CLAUDE_OUTPUT_FORMAT", "text"),
	}
}

// Handler manages claude -p subprocess lifecycles. It spawns one-shot claude
// invocations per user request, reads stdout, and writes results to Redis via
// the tasklib client.
type Handler struct {
	client *tasklib.Client
	cfg    Config
	sem    chan struct{}
	logger *slog.Logger

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
		logger:  slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).With("component", "request"),
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
	exists, err := h.client.ThreadExists(ctx, threadID)
	if err != nil {
		<-h.sem
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
		if sessionFileExists(h.cfg.ClaudeSessionsDir, sessionID) {
			useResume = true
		} else {
			h.logger.Info(fmt.Sprintf("thread=%s session file missing for %s, generating fresh session", threadID, sessionID))
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

	// Clear previous completion state and mark thread as running.
	if err := h.client.ClearThreadComplete(ctx, threadID); err != nil {
		h.logger.Info(fmt.Sprintf("thread=%s ClearThreadComplete error: %v", threadID, err))
	}
	if err := h.client.UpdateThread(ctx, threadID, map[string]string{"status": "running"}); err != nil {
		h.logger.Info(fmt.Sprintf("thread=%s UpdateThread error: %v", threadID, err))
	}

	// Register the cancel function for external cancellation
	h.mu.Lock()
	h.cancels[threadID] = cancel
	h.mu.Unlock()

	h.wg.Add(1)
	go h.runSubprocess(procCtx, cancel, threadID, requestID, args)

	h.client.UpdateThreadLastActivity(ctx, threadID)

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

// ── background subprocess management ──────────────────────────────────────

func (h *Handler) runSubprocess(ctx context.Context, cancel context.CancelFunc, threadID, requestID string, args []string) {
	defer h.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			h.logger.Warn(fmt.Sprintf("panic in subprocess goroutine thread=%s: %v", threadID, r))
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

	h.logger.Info(fmt.Sprintf("thread=%s request=%s spawning claude -p", threadID, requestID))

	cmd := exec.CommandContext(ctx, h.cfg.ClaudePath, args...)
	cmd.Dir = filepath.Join(h.cfg.WorkspaceDir, threadID)
	if err := os.MkdirAll(cmd.Dir, 0755); err != nil {
		h.writeErrorMessage(ctx, threadID, fmt.Sprintf("failed to create workspace dir: %v", err))
		return
	}

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

	if h.isCancelled(ctx) {
		return
	}

	// Read stderr in a background goroutine for error reporting.
	var stderrMu sync.Mutex
	var stderrWg sync.WaitGroup
	var lastStderr strings.Builder
	stderrWg.Add(1)
	go func() {
		defer stderrWg.Done()
		reader := bufio.NewReader(stderr)
		for {
			line, readErr := reader.ReadString('\n')
			line = strings.TrimRight(line, "\r\n")
			stderrMu.Lock()
			lastStderr.WriteString(line)
			lastStderr.WriteByte('\n')
			if lastStderr.Len() > 4096 {
				s := lastStderr.String()
				lastStderr.Reset()
				lastStderr.WriteString(s[len(s)-4096:])
			}
			stderrMu.Unlock()
			if readErr != nil {
				break
			}
		}
	}()

	// Read stdout. In "text" mode (default) we accumulate plain text and write
	// each line as a "plan" message for real-time UI updates. In "stream-json"
	// mode we dispatch JSON messages (rollback path). The respective method
	// may write response/error messages directly for stream-json.
	var fullStdout strings.Builder
	if h.cfg.OutputFormat == "stream-json" {
		h.processStreamJSON(ctx, threadID, stdout, &fullStdout)
	} else {
		h.processPlainText(ctx, threadID, stdout, &fullStdout)
	}

	// Wait for the process to exit (or kill it if timeout/cancelled).
	var waitErr error
	if ctx.Err() != nil {
		done := make(chan struct{})
		go func() {
			waitErr = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Signal(syscall.SIGKILL)
			<-done
		}
	} else {
		waitErr = cmd.Wait()
	}

	// Wait for stderr collector to finish now that the pipe is closed.
	stderrWg.Wait()

	// For plain text mode, determine completion from exit code.
	// stream-json mode already wrote its response/error message in processStreamJSON.
	if h.cfg.OutputFormat != "stream-json" {
		if ctx.Err() == context.DeadlineExceeded {
			h.writeErrorMessage(ctx, threadID, fmt.Sprintf("Master agent timed out after %s", h.cfg.RequestTimeout))
		} else if ctx.Err() == context.Canceled {
			h.writeErrorMessage(ctx, threadID, "Request cancelled")
		} else if waitErr != nil {
			stderrMu.Lock()
			errContent := strings.TrimSpace(lastStderr.String())
			stderrMu.Unlock()
			if errContent == "" {
				errContent = fmt.Sprintf("claude exited with error: %v", waitErr)
			}
			h.logger.Info(fmt.Sprintf("thread=%s claude stderr: %s", threadID, errContent))
			h.writeErrorMessage(ctx, threadID, errContent)
		} else {
			result := strings.TrimSpace(fullStdout.String())
			if result == "" {
				h.writeErrorMessage(ctx, threadID, "claude exited without producing output")
			} else {
				h.writeResponseMessage(ctx, threadID, result)
			}
		}
	}
}

// processStreamJSON reads and dispatches stream-json lines from stdout.
// It writes plan/tool_call messages for assistant output and response/error
// messages for the result. On return, fullStdout contains the full stdout.
func (h *Handler) processStreamJSON(ctx context.Context, threadID string, stdout io.Reader, fullStdout *strings.Builder) {
	lastWritten := ""
	reader := bufio.NewReader(stdout)

	for {
		if h.isCancelled(ctx) {
			break
		}

		rawLine, readErr := reader.ReadString('\n')
		rawLine = strings.TrimRight(rawLine, "\r\n")
		line := []byte(rawLine)
		if fullStdout != nil {
			fullStdout.WriteString(rawLine)
			fullStdout.WriteByte('\n')
		}
		if len(line) == 0 {
			if readErr != nil {
				if readErr != io.EOF {
					h.logger.Info(fmt.Sprintf("thread=%s stdout reader error: %v", threadID, readErr))
				}
				break
			}
			continue
		}

		var msg streamMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			h.logger.Info(fmt.Sprintf("thread=%s unparseable stream-json line: %v", threadID, err))
			continue
		}

		switch msg.Type {
		case "system":
			continue

		case "user":
			continue

		case "assistant":
			if content := h.handleAssistantMessage(ctx, threadID, &msg); content != "" {
				lastWritten = content
			}

		case "result":
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
}

// processPlainText reads plain-text lines from stdout and accumulates them.
// Each non-empty line is written as a "plan" message for real-time UI updates.
// The caller handles the final response message after the process exits.
func (h *Handler) processPlainText(ctx context.Context, threadID string, stdout io.Reader, fullStdout *strings.Builder) {
	reader := bufio.NewReader(stdout)

	for {
		if h.isCancelled(ctx) {
			break
		}

		rawLine, readErr := reader.ReadString('\n')
		rawLine = strings.TrimRight(rawLine, "\r\n")
		if rawLine != "" {
			if fullStdout != nil {
				fullStdout.WriteString(rawLine)
				fullStdout.WriteByte('\n')
			}

			cleanCtx, cleanCancel := cleanupCtx()
			h.client.AppendMessage(cleanCtx, threadID, tasklib.Message{
				Role:      "master",
				Type:      "plan",
				Content:   rawLine,
				Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
			})
			cleanCancel()
		}
		if readErr != nil {
			if readErr != io.EOF {
				h.logger.Info(fmt.Sprintf("thread=%s stdout reader error: %v", threadID, readErr))
			}
			break
		}
	}
}

// ── message handling ──────────────────────────────────────────────────────

// handleAssistantMessage classifies assistant output as "plan" or "tool_call"
// and writes it to thread history for live progress display.
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
	h.logger.Info(fmt.Sprintf("thread=%s completed successfully", threadID))

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
	h.logger.Warn(fmt.Sprintf("thread=%s error: %s", threadID, content))

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
func (h *Handler) completeThread(ctx context.Context, threadID string) {
	h.logger.Info(fmt.Sprintf("thread=%s completed successfully (dedup)", threadID))

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

// ValidThreadID rejects thread IDs containing path traversal sequences or colons.
func ValidThreadID(id string) bool {
	if id == "" {
		return false
	}
	return !strings.Contains(id, "..") && !strings.ContainsAny(id, "/\\:")
}

// ── stream-json parsing ───────────────────────────────────────────────────

// streamMessage represents a single JSON line from claude --output-format stream-json.
type streamMessage struct {
	Type    string           `json:"type"`
	Subtype string           `json:"subtype"`
	IsError bool             `json:"is_error"`
	Result  string           `json:"result"`
	Message *streamAssistant `json:"message"`
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
	var b [16]byte
	if _, err2 := crand.Read(b[:]); err2 == nil {
		b[6] = (b[6] & 0x0f) | 0x40
		b[8] = (b[8] & 0x3f) | 0x80
		return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1000000000000)
}
