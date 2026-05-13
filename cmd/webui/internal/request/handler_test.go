package request

import (
	"context"
	"encoding/json"
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

// ── stream-json parsing tests ─────────────────────────────────────────────

func TestHasToolUse(t *testing.T) {
	tests := []struct {
		name   string
		blocks []streamContentBlock
		want   bool
	}{
		{
			name:   "empty blocks",
			blocks: []streamContentBlock{},
			want:   false,
		},
		{
			name: "text only",
			blocks: []streamContentBlock{
				{Type: "text", Text: "hello"},
				{Type: "text", Text: "world"},
			},
			want: false,
		},
		{
			name: "contains tool_use",
			blocks: []streamContentBlock{
				{Type: "text", Text: "let me run a command"},
				{Type: "tool_use", Text: ""},
			},
			want: true,
		},
		{
			name: "single tool_use",
			blocks: []streamContentBlock{
				{Type: "tool_use", Text: ""},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasToolUse(tt.blocks); got != tt.want {
				t.Errorf("hasToolUse() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractText(t *testing.T) {
	tests := []struct {
		name string
		msg  *streamMessage
		want string
	}{
		{
			name: "nil message",
			msg:  &streamMessage{},
			want: "",
		},
		{
			name: "empty content",
			msg: &streamMessage{
				Message: &streamAssistant{Content: []streamContentBlock{}},
			},
			want: "",
		},
		{
			name: "single text block",
			msg: &streamMessage{
				Message: &streamAssistant{Content: []streamContentBlock{
					{Type: "text", Text: "Hello world"},
				}},
			},
			want: "Hello world",
		},
		{
			name: "multiple text blocks",
			msg: &streamMessage{
				Message: &streamAssistant{Content: []streamContentBlock{
					{Type: "text", Text: "First"},
					{Type: "text", Text: "Second"},
				}},
			},
			want: "First\nSecond",
		},
		{
			name: "mixed blocks skips non-text",
			msg: &streamMessage{
				Message: &streamAssistant{Content: []streamContentBlock{
					{Type: "text", Text: "Let me run:"},
					{Type: "tool_use", Text: ""},
					{Type: "text", Text: "Done"},
				}},
			},
			want: "Let me run:\nDone",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractText(tt.msg); got != tt.want {
				t.Errorf("extractText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStreamMessageUnmarshal(t *testing.T) {
	tests := []struct {
		name string
		json string
		want streamMessage
	}{
		{
			name: "system init",
			json: `{"type":"system","subtype":"init"}`,
			want: streamMessage{Type: "system", Subtype: "init"},
		},
		{
			name: "result success",
			json: `{"type":"result","subtype":"success","is_error":false,"result":"All done"}`,
			want: streamMessage{Type: "result", Subtype: "success", IsError: false, Result: "All done"},
		},
		{
			name: "result error",
			json: `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Something broke"}`,
			want: streamMessage{Type: "result", Subtype: "error_during_execution", IsError: true, Result: "Something broke"},
		},
		{
			name: "assistant with text",
			json: `{"type":"assistant","message":{"content":[{"type":"text","text":"planning output"}]}}`,
			want: streamMessage{
				Type: "assistant",
				Message: &streamAssistant{
					Content: []streamContentBlock{{Type: "text", Text: "planning output"}},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var msg streamMessage
			if err := json.Unmarshal([]byte(tt.json), &msg); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			if msg.Type != tt.want.Type {
				t.Errorf("Type = %q, want %q", msg.Type, tt.want.Type)
			}
			if msg.Subtype != tt.want.Subtype {
				t.Errorf("Subtype = %q, want %q", msg.Subtype, tt.want.Subtype)
			}
			if msg.IsError != tt.want.IsError {
				t.Errorf("IsError = %v, want %v", msg.IsError, tt.want.IsError)
			}
			if msg.Result != tt.want.Result {
				t.Errorf("Result = %q, want %q", msg.Result, tt.want.Result)
			}
		})
	}
}

// ── session file detection tests ──────────────────────────────────────────

func TestSessionFileExists(t *testing.T) {
	t.Run("no projects dir", func(t *testing.T) {
		dir := t.TempDir()
		if sessionFileExists(dir, "test-uuid") {
			t.Error("expected false when projects dir does not exist")
		}
	})

	t.Run("empty projects dir", func(t *testing.T) {
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, "projects"), 0755)
		if sessionFileExists(dir, "test-uuid") {
			t.Error("expected false when no session files exist")
		}
	})

	t.Run("session file exists", func(t *testing.T) {
		dir := t.TempDir()
		projectDir := filepath.Join(dir, "projects", "-")
		os.MkdirAll(projectDir, 0755)
		sessionFile := filepath.Join(projectDir, "abc-123.json")
		os.WriteFile(sessionFile, []byte("{}"), 0644)

		if !sessionFileExists(dir, "abc-123") {
			t.Error("expected true when session file exists")
		}
	})

	t.Run("different session id", func(t *testing.T) {
		dir := t.TempDir()
		projectDir := filepath.Join(dir, "projects", "-")
		os.MkdirAll(projectDir, 0755)
		os.WriteFile(filepath.Join(projectDir, "abc-123.json"), []byte("{}"), 0644)

		if sessionFileExists(dir, "xyz-999") {
			t.Error("expected false for different session id")
		}
	})
}

// ── fake claude script helper ─────────────────────────────────────────────

// writeFakeClaude creates a shell script that echoes the given JSON lines
// to stdout and exits with the given code.
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

	// Use os.MkdirTemp (NOT t.TempDir) so directories outlive the test
	// function. Background goroutines may still reference files here after
	// the test function returns.
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
		ClaudePath:        "", // set per-test
		ClaudeSessionsDir: sessionsDir,
		RequestTimeout:    30 * time.Second,
		MaxConcurrent:     5,
		ShutdownGrace:     5 * time.Second,
		WorkspaceDir:      workspaceDir,
		TestNotify:        notify,
	}
	handler := New(client, cfg)
	return handler, mr, notify
}

func TestSubmit_Success(t *testing.T) {
	handler, _, notify := newTestHandler(t)

	// Write a fake claude that produces a success result
	lines := []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Let me plan this out."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Running command:"},{"type":"tool_use","text":""}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"Task completed successfully"}`,
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

	// Wait for background goroutine to finish
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

	// Check completion flag
	complete, err := handler.client.IsThreadComplete(ctx, "test-thread")
	if err != nil {
		t.Fatalf("IsThreadComplete: %v", err)
	}
	if !complete {
		t.Error("thread should be marked complete")
	}

	// Check messages: user request + 2 assistant + 1 response = 4
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

	// Last message should be the response
	last := msgs[len(msgs)-1]
	if last.Type != "response" {
		t.Errorf("last message type = %q, want %q", last.Type, "response")
	}
	if last.Role != "master" {
		t.Errorf("last message role = %q, want %q", last.Role, "master")
	}
	if last.Content != "Task completed successfully" {
		t.Errorf("last message content = %q, want %q", last.Content, "Task completed successfully")
	}

	// Check assistant messages are classified correctly
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

	lines := []string{
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Permission denied"}`,
	}
	handler.cfg.ClaudePath = writeFakeClaude(handler.cfg.WorkspaceDir, lines, 1)

	ctx := context.Background()
	_, err := handler.Submit(ctx, "err-thread", "Do something dangerous", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	_, ok := waitForNotification(notify, 5*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess to complete")
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

	// Manually acquire the request lock
	ctx := context.Background()
	acquired, err := handler.client.AcquireRequestLock(ctx, "busy-thread", "other-request", tasklib.LockTTL)
	if err != nil {
		t.Fatalf("AcquireRequestLock: %v", err)
	}
	if !acquired {
		t.Fatal("expected lock to be acquired")
	}
	_ = mr // used via client

	handler.cfg.ClaudePath = writeFakeClaude(handler.cfg.WorkspaceDir, []string{`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`}, 0)

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

	// Set max concurrent to 1 to make testing easier
	handler.cfg.MaxConcurrent = 1
	handler.sem = make(chan struct{}, 1)

	// Don't actually spawn claude — we just need a fake path
	handler.cfg.ClaudePath = "/bin/true"

	// First submit fills the semaphore slot (but it will error because
	// the goroutine can't actually run /bin/true with these args)
	// We need to prevent Submit from spawning a real subprocess.
	// Acquire the semaphore manually instead.
	handler.sem <- struct{}{}

	ctx := context.Background()
	_, err := handler.Submit(ctx, "limit-thread", "Request", "")
	if err != ErrConcurrencyLimit {
		t.Errorf("error = %v, want ErrConcurrencyLimit", err)
	}
}

func TestSubmit_CreatesThread(t *testing.T) {
	handler, _, notify := newTestHandler(t)

	lines := []string{
		`{"type":"result","subtype":"success","is_error":false,"result":"done"}`,
	}
	handler.cfg.ClaudePath = writeFakeClaude(handler.cfg.WorkspaceDir, lines, 0)

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

	lines := []string{
		`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`,
	}
	handler.cfg.ClaudePath = writeFakeClaude(handler.cfg.WorkspaceDir, lines, 0)

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

	// Write a fake claude that sleeps before exiting, so the background
	// goroutine stays alive long enough to verify cancel registration.
	// /bin/true can complete before Submit returns on fast/single-core
	// machines (ARM64 runners).
	handler.cfg.ClaudePath = filepath.Join(handler.cfg.WorkspaceDir, "fake-slow")
	os.WriteFile(handler.cfg.ClaudePath, []byte("#!/bin/bash\necho '{\"type\":\"system\",\"subtype\":\"init\"}'\nexec sleep 30\n"), 0755)

	ctx := context.Background()
	_, err := handler.Submit(ctx, "cancel-reg-thread", "Slow request", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Verify the cancel func is registered
	handler.mu.Lock()
	_, exists := handler.cancels["cancel-reg-thread"]
	handler.mu.Unlock()
	if !exists {
		t.Error("cancel func should be registered right after Submit")
	}

	// Cancel the running subprocess and wait for cleanup
	if err := handler.Cancel("cancel-reg-thread"); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}
	_, ok := waitForNotification(notify, 5*time.Second)
	if !ok {
		t.Fatal("timeout waiting for cancellation cleanup")
	}

	// Verify cleanup
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

	// Write a fake claude that writes to stdout then sleeps, so the goroutine
	// is in the stdout scan loop when we cancel.
	script := `#!/bin/bash
echo '{"type":"system","subtype":"init"}'
exec sleep 30
`
	handler.cfg.ClaudePath = filepath.Join(handler.cfg.WorkspaceDir, "fake-claude")
	os.WriteFile(handler.cfg.ClaudePath, []byte(script), 0755)

	ctx := context.Background()
	_, err := handler.Submit(ctx, "midflight-thread", "Long request", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Wait for the goroutine to spawn and start reading stdout (first echo line processed)
	time.Sleep(300 * time.Millisecond)

	// Cancel should succeed — the cancel func is registered
	if err := handler.Cancel("midflight-thread"); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}

	// Wait for the goroutine to clean up after cancellation
	_, ok := waitForNotification(notify, 10*time.Second)
	if !ok {
		t.Fatal("timeout waiting for cancellation cleanup")
	}

	// Verify the cancel func was cleaned up
	handler.mu.Lock()
	_, exists := handler.cancels["midflight-thread"]
	handler.mu.Unlock()
	if exists {
		t.Error("cancel func should be removed after completion")
	}

	// Verify an error message was written for the cancelled request
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

	lines := []string{
		`{"type":"result","subtype":"success","is_error":false,"result":"done"}`,
	}
	handler.cfg.ClaudePath = writeFakeClaude(handler.cfg.WorkspaceDir, lines, 0)
	handler.cfg.ShutdownGrace = 10 * time.Second

	ctx := context.Background()
	_, err := handler.Submit(ctx, "shutdown-thread", "Quick request", "")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Shutdown should wait for the goroutine to finish
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := handler.Shutdown(shutdownCtx); err != nil {
		t.Errorf("Shutdown failed: %v", err)
	}

	// After shutdown, the semaphore should be drained
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

func TestMustUUID(t *testing.T) {
	id := mustUUID()
	if id == "" {
		t.Error("mustUUID should not return empty string")
	}
	// UUID v4 format: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
	if len(id) != 36 {
		t.Errorf("UUID length = %d, want 36", len(id))
	}
}
