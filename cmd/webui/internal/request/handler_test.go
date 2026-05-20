package request

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/noodle05/ai-agents/tasklib"
)

// ── fake claude script helpers ─────────────────────────────────────────────

// writeFakeClaude creates a shell script that echoes the given lines to stdout
// and exits with the given code. Lines are plain text (no JSON wrapping).
func writeFakeClaude(dir string, lines []string, exitCode int) string {
	path := filepath.Join(dir, "fake-claude")
	var script strings.Builder
	script.WriteString("#!/bin/bash\n")
	for _, line := range lines {
		script.WriteString("echo '")
		script.WriteString(strings.ReplaceAll(line, "'", "'\\''"))
		script.WriteString("'\n")
	}
	fmt.Fprintf(&script, "exit %d\n", exitCode)
	os.WriteFile(path, []byte(script.String()), 0755)
	return path
}

// writeFakeClaudeWithStderr creates a shell script that echoes lines to stdout,
// writes stderrLines to stderr, and exits with the given code.
func writeFakeClaudeWithStderr(dir string, lines []string, stderrLines []string, exitCode int) string {
	path := filepath.Join(dir, "fake-claude-stderr")
	var script strings.Builder
	script.WriteString("#!/bin/bash\n")
	for _, line := range lines {
		script.WriteString("echo '")
		script.WriteString(strings.ReplaceAll(line, "'", "'\\''"))
		script.WriteString("'\n")
	}
	for _, line := range stderrLines {
		script.WriteString("echo '")
		script.WriteString(strings.ReplaceAll(line, "'", "'\\''"))
		script.WriteString("' >&2\n")
	}
	fmt.Fprintf(&script, "exit %d\n", exitCode)
	os.WriteFile(path, []byte(script.String()), 0755)
	return path
}

// waitForNotification waits for a notification on the given channel or times out.
func waitForNotification(ch <-chan string, timeout time.Duration) (string, bool) {
	select {
	case threadID := <-ch:
		return threadID, true
	case <-time.After(timeout):
		return "", false
	}
}

// ── handler integration tests (miniredis + fake claude) ───────────────────

func newTestHandler(t *testing.T) (*Handler, *miniredis.Miniredis, chan string) {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	client := tasklib.NewClient(rdb)

	workspaceDir, err := os.MkdirTemp("", "webui-test-workspace-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(workspaceDir) })

	sessionsDir, err := os.MkdirTemp("", "webui-test-sessions-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(sessionsDir) })

	notify := make(chan string, 10)
	cfg := Config{
		ClaudePath:        "",
		ClaudeSessionsDir: sessionsDir,
		RequestTimeout:    30 * time.Second,
		MaxConcurrent:     5,
		ShutdownGrace:     5 * time.Second,
		WorkspaceDir:      workspaceDir,
		OutputFormat:      "text",
		TestNotify:        notify,
	}
	handler := New(client, cfg)
	return handler, mr, notify
}

func TestSubmit_Success(t *testing.T) {
	handler, _, notify := newTestHandler(t)

	// Plain text mode: fake claude emits plain text lines, exits 0.
	// Each line becomes a "plan" message; full stdout becomes the response.
	lines := []string{
		"Let me plan this out.",
		"Running command to check things.",
		"Task completed successfully",
	}
	handler.cfg.ClaudePath = writeFakeClaude(handler.cfg.WorkspaceDir, lines, 0)

	ctx := context.Background()
	result, err := handler.Submit(ctx, "test-thread", "Do something", "owner/repo")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	if result.ThreadID != "test-thread" {
		t.Errorf("ThreadID = %q, want %q", result.ThreadID, "test-thread")
	}
	if result.Status != "submitted" {
		t.Errorf("Status = %q, want %q", result.Status, "submitted")
	}
	if result.RequestID == "" {
		t.Error("RequestID should not be empty")
	}

	threadID, ok := waitForNotification(notify, 5*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess to complete")
	}
	if threadID != "test-thread" {
		t.Errorf("notification threadID = %q, want %q", threadID, "test-thread")
	}

	// Verify Redis state
	if _, err := handler.client.GetThread(ctx, "test-thread"); err != nil {
		t.Fatalf("thread should exist: %v", err)
	}

	complete, err := handler.client.IsThreadComplete(ctx, "test-thread")
	if err != nil {
		t.Fatalf("IsThreadComplete: %v", err)
	}
	if !complete {
		t.Error("thread should be marked complete")
	}

	msgs, err := handler.client.GetThreadHistory(ctx, "test-thread", 0, 0)
	if err != nil {
		t.Fatalf("GetThreadHistory: %v", err)
	}
	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(msgs))
	}
	if msgs[0].Type != "request" {
		t.Errorf("first message type = %q, want %q", msgs[0].Type, "request")
	}
	if msgs[0].Role != "user" {
		t.Errorf("first message role = %q, want %q", msgs[0].Role, "user")
	}

	// Last message should be the response (full stdout concatenated)
	last := msgs[len(msgs)-1]
	if last.Type != "response" {
		t.Errorf("last message type = %q, want %q", last.Type, "response")
	}
	if last.Role != "master" {
		t.Errorf("last message role = %q, want %q", last.Role, "master")
	}
	if !strings.Contains(last.Content, "Task completed successfully") {
		t.Errorf("last message content should contain final line, got: %q", last.Content)
	}

	// Each line should be a "plan" message
	var planCount int
	for _, m := range msgs {
		if m.Type == "plan" {
			planCount++
		}
	}
	if planCount != len(lines) {
		t.Errorf("expected %d plan messages, got %d", len(lines), planCount)
	}

	// Verify lock is released
	running, err := handler.client.IsRequestRunning(ctx, "test-thread")
	if err != nil {
		t.Fatalf("IsRequestRunning: %v", err)
	}
	if running {
		t.Error("request lock should be released after completion")
	}
}

func TestSubmit_ErrorResult(t *testing.T) {
	handler, _, notify := newTestHandler(t)

	// Non-zero exit with stderr → error message.
	handler.cfg.ClaudePath = writeFakeClaudeWithStderr(
		handler.cfg.WorkspaceDir,
		nil,
		[]string{"Permission denied"},
		1,
	)

	ctx := context.Background()
	_, err := handler.Submit(ctx, "err-thread", "Do something dangerous", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	_, ok := waitForNotification(notify, 5*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess to complete")
	}

	complete, err := handler.client.IsThreadComplete(ctx, "err-thread")
	if err != nil {
		t.Fatalf("IsThreadComplete: %v", err)
	}
	if !complete {
		t.Error("thread should be marked complete after error")
	}

	msgs, _ := handler.client.GetThreadHistory(ctx, "err-thread", 0, 0)
	if len(msgs) < 2 {
		t.Fatalf("expected user + error messages, got %d", len(msgs))
	}
	last := msgs[len(msgs)-1]
	if last.Type != "error" {
		t.Errorf("last message type = %q, want %q", last.Type, "error")
	}
	if last.Role != "master" {
		t.Errorf("last message role = %q, want %q", last.Role, "master")
	}
}

func TestSubmit_ThreadBusy(t *testing.T) {
	handler, mr, _ := newTestHandler(t)

	ctx := context.Background()
	acquired, err := handler.client.AcquireRequestLock(ctx, "busy-thread", "other-request", tasklib.LockTTL)
	if err != nil {
		t.Fatalf("AcquireRequestLock: %v", err)
	}
	if !acquired {
		t.Fatal("expected lock to be acquired")
	}
	_ = mr

	handler.cfg.ClaudePath = writeFakeClaude(handler.cfg.WorkspaceDir, []string{"ok"}, 0)

	_, err = handler.Submit(ctx, "busy-thread", "Another request", "")
	if err == nil {
		t.Fatal("expected error when thread is busy")
	}
	if err != ErrThreadBusy {
		t.Errorf("error = %v, want ErrThreadBusy", err)
	}
}

func TestSubmit_ConcurrencyLimit(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	handler.cfg.MaxConcurrent = 1
	handler.sem = make(chan struct{}, 1)
	handler.cfg.ClaudePath = "/bin/true"

	handler.sem <- struct{}{}

	ctx := context.Background()
	_, err := handler.Submit(ctx, "limit-thread", "Request", "")
	if err != ErrConcurrencyLimit {
		t.Errorf("error = %v, want ErrConcurrencyLimit", err)
	}
}

func TestSubmit_CreatesThread(t *testing.T) {
	handler, _, notify := newTestHandler(t)

	handler.cfg.ClaudePath = writeFakeClaude(handler.cfg.WorkspaceDir, []string{"done"}, 0)

	ctx := context.Background()
	_, err := handler.Submit(ctx, "new-thread", "Create me a thread", "owner/repo")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	_, ok := waitForNotification(notify, 5*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess")
	}

	thread, err := handler.client.GetThread(ctx, "new-thread")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.Status != "complete" {
		t.Errorf("thread status = %q, want %q", thread.Status, "complete")
	}
	if thread.GHRepo != "owner/repo" {
		t.Errorf("thread repo = %q, want %q", thread.GHRepo, "owner/repo")
	}
}

func TestSubmit_StoresSessionID(t *testing.T) {
	handler, _, notify := newTestHandler(t)

	handler.cfg.ClaudePath = writeFakeClaude(handler.cfg.WorkspaceDir, []string{"ok"}, 0)

	ctx := context.Background()
	_, err := handler.Submit(ctx, "session-thread", "First request", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	_, ok := waitForNotification(notify, 5*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess")
	}

	sessionID, err := handler.client.GetThreadSessionID(ctx, "session-thread")
	if err != nil {
		t.Fatalf("GetThreadSessionID: %v", err)
	}
	if sessionID == "" {
		t.Error("session ID should be stored for new thread")
	}
	if !strings.Contains(sessionID, "-") {
		t.Errorf("session ID should look like a UUID, got %q", sessionID)
	}
}

func TestCancel_RemovesRegistration(t *testing.T) {
	handler, _, notify := newTestHandler(t)

	handler.cfg.ClaudePath = filepath.Join(handler.cfg.WorkspaceDir, "fake-slow")
	os.WriteFile(handler.cfg.ClaudePath, []byte("#!/bin/bash\necho 'started'\nexec sleep 30\n"), 0755)

	ctx := context.Background()
	_, err := handler.Submit(ctx, "cancel-reg-thread", "Slow request", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	handler.mu.Lock()
	_, exists := handler.cancels["cancel-reg-thread"]
	handler.mu.Unlock()
	if !exists {
		t.Error("cancel func should be registered right after Submit")
	}

	if err := handler.Cancel("cancel-reg-thread"); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}
	_, ok := waitForNotification(notify, 5*time.Second)
	if !ok {
		t.Fatal("timeout waiting for cancellation cleanup")
	}

	handler.mu.Lock()
	_, stillExists := handler.cancels["cancel-reg-thread"]
	handler.mu.Unlock()
	if stillExists {
		t.Error("cancel func should be removed after completion")
	}
	_ = ctx
}

func TestCancel_MidFlight(t *testing.T) {
	handler, _, notify := newTestHandler(t)

	script := `#!/bin/bash
echo 'started'
exec sleep 30
`
	handler.cfg.ClaudePath = filepath.Join(handler.cfg.WorkspaceDir, "fake-claude")
	os.WriteFile(handler.cfg.ClaudePath, []byte(script), 0755)

	ctx := context.Background()
	_, err := handler.Submit(ctx, "midflight-thread", "Long request", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	time.Sleep(300 * time.Millisecond)

	if err := handler.Cancel("midflight-thread"); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}

	_, ok := waitForNotification(notify, 10*time.Second)
	if !ok {
		t.Fatal("timeout waiting for cancellation cleanup")
	}

	handler.mu.Lock()
	_, exists := handler.cancels["midflight-thread"]
	handler.mu.Unlock()
	if exists {
		t.Error("cancel func should be removed after completion")
	}

	msgs, _ := handler.client.GetThreadHistory(ctx, "midflight-thread", 0, 0)
	if len(msgs) < 2 {
		t.Fatalf("expected user + error messages, got %d messages", len(msgs))
	}
	last := msgs[len(msgs)-1]
	if last.Type != "error" {
		t.Errorf("last message type = %q, want %q", last.Type, "error")
	}
}

func TestCancel_NoRunningRequest(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	err := handler.Cancel("nonexistent")
	if err != ErrNoRunningRequest {
		t.Errorf("error = %v, want ErrNoRunningRequest", err)
	}
}

func TestShutdown_WaitsForCompletion(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	handler.cfg.ClaudePath = writeFakeClaude(handler.cfg.WorkspaceDir, []string{"done"}, 0)
	handler.cfg.ShutdownGrace = 10 * time.Second

	ctx := context.Background()
	_, err := handler.Submit(ctx, "shutdown-thread", "Quick request", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := handler.Shutdown(shutdownCtx); err != nil {
		t.Errorf("Shutdown failed: %v", err)
	}

	if handler.ActiveRequests() != 0 {
		t.Errorf("ActiveRequests = %d, want 0", handler.ActiveRequests())
	}
}

func TestActiveRequests(t *testing.T) {
	handler, _, _ := newTestHandler(t)
	handler.cfg.MaxConcurrent = 3
	handler.sem = make(chan struct{}, 3)

	if n := handler.ActiveRequests(); n != 0 {
		t.Errorf("ActiveRequests = %d, want 0", n)
	}

	handler.sem <- struct{}{}
	handler.sem <- struct{}{}
	if n := handler.ActiveRequests(); n != 2 {
		t.Errorf("ActiveRequests = %d, want 2", n)
	}
}

func TestRequestError(t *testing.T) {
	if ErrThreadBusy.Error() != "Thread is already processing a request" {
		t.Errorf("Error() = %q", ErrThreadBusy.Error())
	}
	if ErrThreadBusy.Status != 409 {
		t.Errorf("Status = %d, want 409", ErrThreadBusy.Status)
	}

	if ErrConcurrencyLimit.Status != 503 {
		t.Errorf("Status = %d, want 503", ErrConcurrencyLimit.Status)
	}
	if ErrNoRunningRequest.Status != 404 {
		t.Errorf("Status = %d, want 404", ErrNoRunningRequest.Status)
	}
}

func TestSubmit_EmptyOutput(t *testing.T) {
	handler, _, notify := newTestHandler(t)

	// Exit 0 with no stdout → error (no output produced)
	handler.cfg.ClaudePath = writeFakeClaude(handler.cfg.WorkspaceDir, nil, 0)

	ctx := context.Background()
	_, err := handler.Submit(ctx, "empty-thread", "Do something", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	_, ok := waitForNotification(notify, 5*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess")
	}

	msgs, err := handler.client.GetThreadHistory(ctx, "empty-thread", 0, 0)
	if err != nil {
		t.Fatalf("GetThreadHistory: %v", err)
	}

	last := msgs[len(msgs)-1]
	if last.Type != "error" {
		t.Errorf("last message type = %q, want error (empty output)", last.Type)
	}
	if !strings.Contains(last.Content, "exited without producing output") {
		t.Errorf("error should mention no output, got: %q", last.Content)
	}
}

func TestStderrCapturedOnError(t *testing.T) {
	handler, _, notify := newTestHandler(t)

	// Non-zero exit with stderr → error message should contain stderr.
	handler.cfg.ClaudePath = writeFakeClaudeWithStderr(
		handler.cfg.WorkspaceDir,
		[]string{"Working on it..."},
		[]string{"Error: DeepSeek API returned 500 Internal Server Error", "event=api_error status=500"},
		1,
	)

	ctx := context.Background()
	_, err := handler.Submit(ctx, "stderr-err-thread", "Do something", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	_, ok := waitForNotification(notify, 5*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess to complete")
	}

	msgs, err := handler.client.GetThreadHistory(ctx, "stderr-err-thread", 0, 0)
	if err != nil {
		t.Fatalf("GetThreadHistory: %v", err)
	}

	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(msgs))
	}

	last := msgs[len(msgs)-1]
	if last.Type != "error" {
		t.Fatalf("last message type = %q, want error", last.Type)
	}

	if !strings.Contains(last.Content, "DeepSeek API returned 500 Internal Server Error") {
		t.Errorf("error message should contain stderr output, got: %q", last.Content)
	}
}

func TestSubmit_ThreadBusyNoOrphanedMessage(t *testing.T) {
	handler, _, notify := newTestHandler(t)

	ctx := context.Background()

	acquired, err := handler.client.AcquireRequestLock(ctx, "orphan-thread", "existing-request", tasklib.LockTTL)
	if err != nil {
		t.Fatalf("AcquireRequestLock: %v", err)
	}
	if !acquired {
		t.Fatal("expected lock to be acquired")
	}

	handler.cfg.ClaudePath = writeFakeClaude(handler.cfg.WorkspaceDir, []string{"ok"}, 0)

	_, err = handler.Submit(ctx, "orphan-thread", "This should fail", "")
	if err != ErrThreadBusy {
		t.Fatalf("expected ErrThreadBusy, got %v", err)
	}

	msgs, err := handler.client.GetThreadHistory(ctx, "orphan-thread", 0, 0)
	if err != nil {
		t.Fatalf("GetThreadHistory: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages (no orphaned user request), got %d", len(msgs))
	}

	if err := handler.client.ReleaseRequestLock(ctx, "orphan-thread"); err != nil {
		t.Fatalf("ReleaseRequestLock: %v", err)
	}

	_, err = handler.Submit(ctx, "orphan-thread", "This should fail", "")
	if err != nil {
		t.Fatalf("retry Submit failed: %v", err)
	}

	_, ok := waitForNotification(notify, 5*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess after retry")
	}

	msgs, err = handler.client.GetThreadHistory(ctx, "orphan-thread", 0, 0)
	if err != nil {
		t.Fatalf("GetThreadHistory after retry: %v", err)
	}
	requestCount := 0
	for _, m := range msgs {
		if m.Type == "request" {
			requestCount++
		}
	}
	if requestCount != 1 {
		t.Errorf("expected exactly 1 user request message after retry, got %d (total messages: %d)",
			requestCount, len(msgs))
	}
}

func TestMustUUID(t *testing.T) {
	id := mustUUID()
	if id == "" {
		t.Error("mustUUID should not return empty string")
	}
	if len(id) != 36 {
		t.Errorf("UUID length = %d, want 36", len(id))
	}
}

// TestLargeLineHandling verifies that large stdout lines (> 64KB) are handled
// correctly in plain text mode. See issue #102.
func TestLargeLineHandling(t *testing.T) {
	handler, _, notify := newTestHandler(t)

	script := filepath.Join(handler.cfg.WorkspaceDir, "fake-large-claude")
	scriptContent := `#!/usr/bin/env python3
import sys

# Emit a large line (> 64KB) of plain text
big_text = "X" * (70 * 1024)
print(big_text)
print("Done with large output")
sys.exit(0)
`
	if err := os.WriteFile(script, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("WriteFile fake-large-claude: %v", err)
	}
	handler.cfg.ClaudePath = script

	ctx := context.Background()
	_, err := handler.Submit(ctx, "large-line-thread", "Do something big", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	_, ok := waitForNotification(notify, 10*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess with large line")
	}

	complete, err := handler.client.IsThreadComplete(ctx, "large-line-thread")
	if err != nil {
		t.Fatalf("IsThreadComplete: %v", err)
	}
	if !complete {
		t.Error("thread should be marked complete after large-line output")
	}

	msgs, err := handler.client.GetThreadHistory(ctx, "large-line-thread", 0, 0)
	if err != nil {
		t.Fatalf("GetThreadHistory: %v", err)
	}

	for _, m := range msgs {
		if strings.Contains(m.Content, "exited without producing output") {
			t.Errorf("should not contain false error, got: %s", m.Content)
		}
	}

	last := msgs[len(msgs)-1]
	if last.Type == "error" {
		t.Errorf("last message should not be error, got: %s", last.Content)
	}
	if last.Role != "master" {
		t.Errorf("last message role = %q, want master", last.Role)
	}
}

// TestLargeStderrHandling verifies that large stderr lines (> 64KB) are handled
// in the stderr collector goroutine. See issue #102.
func TestLargeStderrHandling(t *testing.T) {
	handler, _, notify := newTestHandler(t)

	script := filepath.Join(handler.cfg.WorkspaceDir, "fake-large-stderr")
	scriptContent := `#!/usr/bin/env python3
import sys

# Write a large line (> 64KB) to stderr
big_stderr = "X" * (70 * 1024)
sys.stderr.write(big_stderr + "\n")
sys.stderr.flush()

# Emit normal stdout and exit successfully
print("Done with large stderr")
sys.exit(0)
`
	if err := os.WriteFile(script, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("WriteFile fake-large-stderr: %v", err)
	}
	handler.cfg.ClaudePath = script

	ctx := context.Background()
	_, err := handler.Submit(ctx, "large-stderr-thread", "Do something", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	_, ok := waitForNotification(notify, 10*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess with large stderr")
	}

	complete, err := handler.client.IsThreadComplete(ctx, "large-stderr-thread")
	if err != nil {
		t.Fatalf("IsThreadComplete: %v", err)
	}
	if !complete {
		t.Error("thread should be marked complete after large-stderr output")
	}

	msgs, err := handler.client.GetThreadHistory(ctx, "large-stderr-thread", 0, 0)
	if err != nil {
		t.Fatalf("GetThreadHistory: %v", err)
	}

	for _, m := range msgs {
		if strings.Contains(m.Content, "exited without producing output") {
			t.Errorf("should not contain false error, got: %s", m.Content)
		}
	}

	last := msgs[len(msgs)-1]
	if last.Type == "error" {
		t.Errorf("last message should not be error, got: %s", last.Content)
	}
}

func TestDefaultConfig_OutputFormat(t *testing.T) {
	// Default is "text" when env var is unset.
	t.Setenv("CLAUDE_OUTPUT_FORMAT", "")
	cfg := DefaultConfig()
	if cfg.OutputFormat != "text" {
		t.Errorf("default OutputFormat = %q, want %q", cfg.OutputFormat, "text")
	}

	// Explicit stream-json.
	t.Setenv("CLAUDE_OUTPUT_FORMAT", "stream-json")
	cfg = DefaultConfig()
	if cfg.OutputFormat != "stream-json" {
		t.Errorf("OutputFormat = %q, want %q", cfg.OutputFormat, "stream-json")
	}

	// Invalid value is passed through (validated at use site).
	t.Setenv("CLAUDE_OUTPUT_FORMAT", "bogus")
	cfg = DefaultConfig()
	if cfg.OutputFormat != "bogus" {
		t.Errorf("OutputFormat = %q, want %q", cfg.OutputFormat, "bogus")
	}
}


func TestSubmit_StderrWithSuccess(t *testing.T) {
	handler, _, notify := newTestHandler(t)

	// Exit 0 with warning on stderr → should be marked complete, not error.
	handler.cfg.ClaudePath = writeFakeClaudeWithStderr(
		handler.cfg.WorkspaceDir,
		[]string{"Task completed successfully"},
		[]string{"Warning: deprecation notice"},
		0,
	)

	ctx := context.Background()
	_, err := handler.Submit(ctx, "stderr-warn-thread", "Do something", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	_, ok := waitForNotification(notify, 5*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess")
	}

	complete, err := handler.client.IsThreadComplete(ctx, "stderr-warn-thread")
	if err != nil {
		t.Fatalf("IsThreadComplete: %v", err)
	}
	if !complete {
		t.Error("thread should be marked complete when exit=0 with stderr warnings")
	}

	msgs, err := handler.client.GetThreadHistory(ctx, "stderr-warn-thread", 0, 0)
	if err != nil {
		t.Fatalf("GetThreadHistory: %v", err)
	}

	last := msgs[len(msgs)-1]
	if last.Type == "error" {
		t.Errorf("last message should not be error when exit=0, got: %q", last.Content)
	}
	if last.Type != "response" {
		t.Errorf("last message type = %q, want response", last.Type)
	}
}


// ── stream-json mode regression tests ─────────────────────────────────────

func newTestHandlerStreamJSON(t *testing.T) (*Handler, *miniredis.Miniredis, chan string) {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	client := tasklib.NewClient(rdb)

	workspaceDir, err := os.MkdirTemp("", "webui-test-workspace-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(workspaceDir) })

	sessionsDir, err := os.MkdirTemp("", "webui-test-sessions-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(sessionsDir) })

	notify := make(chan string, 10)
	cfg := Config{
		ClaudePath:        "",
		ClaudeSessionsDir: sessionsDir,
		RequestTimeout:    30 * time.Second,
		MaxConcurrent:     5,
		ShutdownGrace:     5 * time.Second,
		WorkspaceDir:      workspaceDir,
		OutputFormat:      "stream-json",
		TestNotify:        notify,
	}
	handler := New(client, cfg)
	return handler, mr, notify
}

func TestStreamJSON_Success(t *testing.T) {
	handler, _, notify := newTestHandlerStreamJSON(t)

	lines := []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Let me plan this out."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Running command:"},{"type":"tool_use","text":""}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"Task completed successfully"}`,
	}
	handler.cfg.ClaudePath = writeFakeClaude(handler.cfg.WorkspaceDir, lines, 0)

	ctx := context.Background()
	_, err := handler.Submit(ctx, "json-thread", "Do something", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	_, ok := waitForNotification(notify, 5*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess")
	}

	complete, err := handler.client.IsThreadComplete(ctx, "json-thread")
	if err != nil {
		t.Fatalf("IsThreadComplete: %v", err)
	}
	if !complete {
		t.Error("thread should be marked complete")
	}

	msgs, err := handler.client.GetThreadHistory(ctx, "json-thread", 0, 0)
	if err != nil {
		t.Fatalf("GetThreadHistory: %v", err)
	}

	var hasPlan, hasToolCall bool
	for _, m := range msgs {
		if m.Type == "plan" {
			hasPlan = true
		}
		if m.Type == "tool_call" {
			hasToolCall = true
		}
	}
	if !hasPlan {
		t.Error("expected a 'plan' message from assistant text-only output")
	}
	if !hasToolCall {
		t.Error("expected a 'tool_call' message from assistant tool_use output")
	}

	last := msgs[len(msgs)-1]
	if last.Type != "response" {
		t.Errorf("last message type = %q, want %q", last.Type, "response")
	}
}

func TestStreamJSON_ErrorResult(t *testing.T) {
	handler, _, notify := newTestHandlerStreamJSON(t)

	lines := []string{
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Permission denied"}`,
	}
	handler.cfg.ClaudePath = writeFakeClaude(handler.cfg.WorkspaceDir, lines, 1)

	ctx := context.Background()
	_, err := handler.Submit(ctx, "json-err-thread", "Do something dangerous", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	_, ok := waitForNotification(notify, 5*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess")
	}

	complete, err := handler.client.IsThreadComplete(ctx, "json-err-thread")
	if err != nil {
		t.Fatalf("IsThreadComplete: %v", err)
	}
	if !complete {
		t.Error("thread should be marked complete after stream-json error result")
	}

	msgs, _ := handler.client.GetThreadHistory(ctx, "json-err-thread", 0, 0)
	last := msgs[len(msgs)-1]
	if last.Type != "error" {
		t.Errorf("last message type = %q, want %q", last.Type, "error")
	}
}

func TestStreamJSON_Dedup(t *testing.T) {
	handler, _, notify := newTestHandlerStreamJSON(t)

	lines := []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"The bug was on line 42, fixed it."}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"The bug was on line 42, fixed it."}`,
	}
	handler.cfg.ClaudePath = writeFakeClaude(handler.cfg.WorkspaceDir, lines, 0)

	ctx := context.Background()
	_, err := handler.Submit(ctx, "json-dedup-thread", "Fix the bug", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	_, ok := waitForNotification(notify, 5*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess")
	}

	msgs, err := handler.client.GetThreadHistory(ctx, "json-dedup-thread", 0, 0)
	if err != nil {
		t.Fatalf("GetThreadHistory: %v", err)
	}

	complete, err := handler.client.IsThreadComplete(ctx, "json-dedup-thread")
	if err != nil {
		t.Fatalf("IsThreadComplete: %v", err)
	}
	if !complete {
		t.Error("thread should be marked complete after stream-json dedup")
	}

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (user + plan), got %d", len(msgs))
	}
	if msgs[1].Type != "plan" {
		t.Errorf("msg[1] type = %q, want plan (response should be dedup'd)", msgs[1].Type)
	}
}

func TestStreamJSON_StderrOnCrash(t *testing.T) {
	handler, _, notify := newTestHandlerStreamJSON(t)

	script := filepath.Join(handler.cfg.WorkspaceDir, "fake-json-crash")
	scriptContent := `#!/bin/bash
echo '{"type":"system","subtype":"init"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Working on it..."}]}}'
echo 'Error: DeepSeek API returned 500 Internal Server Error' >&2
echo 'event=api_error status=500 retry=false' >&2
exit 1
`
	os.WriteFile(script, []byte(scriptContent), 0755)
	handler.cfg.ClaudePath = script

	ctx := context.Background()
	_, err := handler.Submit(ctx, "json-crash-thread", "Do something", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	_, ok := waitForNotification(notify, 5*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess")
	}

	// Thread must be marked complete even on crash (BLOCKER: thread stuck forever).
	complete, err := handler.client.IsThreadComplete(ctx, "json-crash-thread")
	if err != nil {
		t.Fatalf("IsThreadComplete: %v", err)
	}
	if !complete {
		t.Error("thread should be marked complete after stream-json crash")
	}

	msgs, err := handler.client.GetThreadHistory(ctx, "json-crash-thread", 0, 0)
	if err != nil {
		t.Fatalf("GetThreadHistory: %v", err)
	}

	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(msgs))
	}

	// Last message should be an error containing the stderr output.
	last := msgs[len(msgs)-1]
	if last.Type != "error" {
		t.Errorf("last message type = %q, want error", last.Type)
	}
	if !strings.Contains(last.Content, "DeepSeek API returned 500 Internal Server Error") {
		t.Errorf("error message should contain stderr output, got: %q", last.Content)
	}
}
