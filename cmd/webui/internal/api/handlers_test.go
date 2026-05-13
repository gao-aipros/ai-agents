package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"

	"github.com/noodle05/ai-agents/cmd/webui/internal/request"
	"github.com/noodle05/ai-agents/tasklib"
)

// ── test infrastructure ───────────────────────────────────────────────────

type testHarness struct {
	Router       chi.Router
	Handler      *request.Handler
	Client       *tasklib.Client
	MR           *miniredis.Miniredis
	WorkspaceDir string
	SessionsDir  string
	Notify       chan string
	cleanup      func()
}

func (th *testHarness) Cleanup() {
	th.cleanup()
}

func newTestRouter(t *testing.T) *testHarness {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	client := tasklib.NewClient(rdb)

	workspaceDir, err := os.MkdirTemp("", "webui-api-test-workspace-*")
	if err != nil {
		t.Fatalf("MkdirTemp workspace: %v", err)
	}

	sessionsDir, err := os.MkdirTemp("", "webui-api-test-sessions-*")
	if err != nil {
		t.Fatalf("MkdirTemp sessions: %v", err)
	}

	notify := make(chan string, 10)
	cfg := request.Config{
		ClaudePath:        "/bin/true",
		ClaudeSessionsDir: sessionsDir,
		RequestTimeout:    30 * time.Second,
		MaxConcurrent:     5,
		ShutdownGrace:     5 * time.Second,
		WorkspaceDir:      workspaceDir,
		TestNotify:        notify,
	}

	handler := request.New(client, cfg)
	router := NewRouter(client, handler, cfg)

	return &testHarness{
		Router:       router,
		Handler:      handler,
		Client:       client,
		MR:           mr,
		WorkspaceDir: workspaceDir,
		SessionsDir:  sessionsDir,
		Notify:       notify,
		cleanup: func() {
			os.RemoveAll(workspaceDir)
			os.RemoveAll(sessionsDir)
		},
	}
}

func (th *testHarness) setFakeClaude(t *testing.T, lines []string, exitCode int) {
	t.Helper()
	path := writeFakeClaude(th.WorkspaceDir, lines, exitCode)
	th.Handler.SetClaudePath(path)
}

func (th *testHarness) setSlowFakeClaude(t *testing.T) {
	t.Helper()
	// Use exec so the sleep process replaces bash. This ensures that when
	// CommandContext kills the process, the stdout pipe write end is closed
	// and scanner.Scan() unblocks with EOF.
	script := `#!/bin/bash
echo '{"type":"system","subtype":"init"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"working..."}]}}'
exec sleep 30
`
	path := filepath.Join(th.WorkspaceDir, "fake-claude-slow")
	os.WriteFile(path, []byte(script), 0755)
	th.Handler.SetClaudePath(path)
}

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

func waitForNotification(ch <-chan string, timeout time.Duration) (string, bool) {
	select {
	case threadID := <-ch:
		return threadID, true
	case <-time.After(timeout):
		return "", false
	}
}

func readJSON(r *httptest.ResponseRecorder, v interface{}) {
	json.NewDecoder(r.Body).Decode(v)
}

// ── health / stats ────────────────────────────────────────────────────────

func TestHandleHealth(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/health", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	readJSON(w, &resp)
	if resp["redis"] != "ok" {
		t.Errorf("redis = %q, want %q", resp["redis"], "ok")
	}
}

func TestHandleStats(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	ctx := context.Background()
	th.Client.CreateThread(ctx, "stats-thread", "repo/test")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/stats", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	readJSON(w, &resp)
	if _, ok := resp["total_tasks"]; !ok {
		t.Error("stats missing total_tasks")
	}
	if _, ok := resp["queue_depths"]; !ok {
		t.Error("stats missing queue_depths")
	}
}

// ── workers ────────────────────────────────────────────────────────────────

func TestHandleListWorkers(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/workers", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHandleGetWorker(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/workers/claude", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHandleGetWorker_Unknown(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/workers/nonexistent", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// ── threads ────────────────────────────────────────────────────────────────

func TestHandleCreateThread(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	body := strings.NewReader(`{"thread_id":"my-thread","repo":"owner/repo"}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/threads", body)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusCreated, w.Body.String())
	}

	thread, err := th.Client.GetThread(context.Background(), "my-thread")
	if err != nil {
		t.Fatalf("thread should exist: %v", err)
	}
	if thread.Status != "initiated" {
		t.Errorf("thread status = %q, want %q", thread.Status, "initiated")
	}
}

func TestHandleCreateThread_AutoID(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	body := strings.NewReader(`{}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/threads", body)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp map[string]interface{}
	readJSON(w, &resp)
	threadID, ok := resp["thread_id"].(string)
	if !ok || threadID == "" {
		t.Fatal("thread_id should be auto-generated")
	}

	_, err := th.Client.GetThread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("auto-generated thread should exist: %v", err)
	}
}

func TestHandleCreateThread_Conflict(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	th.Client.CreateThread(context.Background(), "dup-thread", "")

	body := strings.NewReader(`{"thread_id":"dup-thread"}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/threads", body)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestHandleListThreads(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	th.Client.CreateThread(context.Background(), "list-thread-1", "")
	th.Client.CreateThread(context.Background(), "list-thread-2", "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var threads []map[string]interface{}
	readJSON(w, &threads)
	if len(threads) < 2 {
		t.Errorf("got %d threads, want at least 2", len(threads))
	}
}

func TestHandleGetThread(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	th.Client.CreateThread(context.Background(), "detail-thread", "repo/test")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads/detail-thread", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	readJSON(w, &resp)
	thread, ok := resp["thread"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing thread field")
	}
	if thread["thread_id"] != "detail-thread" {
		t.Errorf("thread_id = %q", thread["thread_id"])
	}
}

func TestHandleGetThread_NotFound(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads/nonexistent", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleThreadHistory(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	ctx := context.Background()
	th.Client.CreateThread(ctx, "hist-thread", "")
	th.Client.AppendMessage(ctx, "hist-thread", tasklib.Message{
		Role: "user", Type: "request", Content: "hello",
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})
	th.Client.AppendMessage(ctx, "hist-thread", tasklib.Message{
		Role: "master", Type: "response", Content: "hi there",
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads/hist-thread/history", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var msgs []map[string]interface{}
	readJSON(w, &msgs)
	if len(msgs) != 2 {
		t.Errorf("got %d messages, want 2", len(msgs))
	}
}

func TestHandleThreadHistory_Tail(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	ctx := context.Background()
	th.Client.CreateThread(ctx, "tail-thread", "")
	for i := 0; i < 5; i++ {
		th.Client.AppendMessage(ctx, "tail-thread", tasklib.Message{
			Role: "user", Type: "request", Content: fmt.Sprintf("msg %d", i),
			Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		})
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads/tail-thread/history?tail=2", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var msgs []map[string]interface{}
	readJSON(w, &msgs)
	if len(msgs) != 2 {
		t.Errorf("got %d messages, want 2 (tail=2)", len(msgs))
	}
}

func TestHandleDeleteWorkspace_NoConfirm(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	th.Client.CreateThread(context.Background(), "ws-thread", "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/threads/ws-thread/workspace", nil)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleDeleteWorkspace_Confirm(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	th.Client.CreateThread(context.Background(), "ws-delete-thread", "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/threads/ws-delete-thread/workspace?confirm=true", nil)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestHandleKeepThread(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	th.Client.CreateThread(context.Background(), "keep-thread", "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/threads/keep-thread/keep", nil)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHandleResetSession(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	ctx := context.Background()
	th.Client.CreateThread(ctx, "reset-thread", "")
	th.Client.SetThreadSessionID(ctx, "reset-thread", "test-session-uuid")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/threads/reset-thread/reset-session", nil)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	sid, _ := th.Client.GetThreadSessionID(ctx, "reset-thread")
	if sid != "" {
		t.Errorf("session_id should be cleared, got %q", sid)
	}
}

// ── tasks ──────────────────────────────────────────────────────────────────

func TestHandleListTasks(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/tasks", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHandleGetTask_NotFound(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/tasks/nonexistent-task", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (for nonexistent task)", w.Code, http.StatusNotFound)
	}
}

func TestHandleGetTaskResult_NotFound(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/tasks/nonexistent/result", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// ── requests ───────────────────────────────────────────────────────────────

func TestHandleSubmitRequest_Success(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	th.setFakeClaude(t, []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Planning..."}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"Done"}`,
	}, 0)

	body := strings.NewReader(`{"request":"Say hello","repo":"test/repo"}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/requests", body)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusAccepted, w.Body.String())
	}

	var resp map[string]interface{}
	readJSON(w, &resp)
	threadID, ok := resp["thread_id"].(string)
	if !ok || threadID == "" {
		t.Fatal("thread_id should be returned")
	}
	if resp["status"] != "submitted" {
		t.Errorf("status = %q, want %q", resp["status"], "submitted")
	}

	// Wait for background goroutine
	tid, ok := waitForNotification(th.Notify, 10*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess to complete")
	}
	if tid != threadID {
		t.Errorf("notification threadID = %q, want %q", tid, threadID)
	}

	// Verify thread was created and completed
	thread, err := th.Client.GetThread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("thread should exist: %v", err)
	}
	if thread.Status != "complete" {
		t.Errorf("thread status = %q, want %q", thread.Status, "complete")
	}
}

func TestHandleSubmitRequest_MissingRequest(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	body := strings.NewReader(`{"request":""}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/requests", body)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSubmitRequest_InvalidJSON(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	body := strings.NewReader(`not json`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/requests", body)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSubmitRequest_ThreadBusy(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	th.setFakeClaude(t, []string{
		`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`,
	}, 0)

	// First request — should succeed
	body1 := strings.NewReader(`{"request":"First","thread_id":"busy-thread"}`)
	w1 := httptest.NewRecorder()
	r1 := httptest.NewRequest("POST", "/api/requests", body1)
	r1.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w1, r1)

	if w1.Code != http.StatusAccepted {
		t.Fatalf("first submit: status = %d (body=%s)", w1.Code, w1.Body.String())
	}

	// Second request to same thread — should get 409
	body2 := strings.NewReader(`{"request":"Second","thread_id":"busy-thread"}`)
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("POST", "/api/requests", body2)
	r2.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w2, r2)

	if w2.Code != http.StatusConflict {
		t.Errorf("second submit status = %d, want %d (body=%s)", w2.Code, http.StatusConflict, w2.Body.String())
	}

	// Wait for first request to complete
	waitForNotification(th.Notify, 10*time.Second)
}

func TestHandleCancelRequest(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	th.setSlowFakeClaude(t)

	ctx := context.Background()
	th.Client.CreateThread(ctx, "cancel-thread", "")

	body := strings.NewReader(`{"request":"Long task","thread_id":"cancel-thread"}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/requests", body)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("submit: status = %d (body=%s)", w.Code, w.Body.String())
	}

	// Give the subprocess a moment to start
	time.Sleep(200 * time.Millisecond)

	// Cancel it
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("POST", "/api/threads/cancel-thread/cancel", nil)
	r2.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w2, r2)

	if w2.Code != http.StatusOK {
		t.Errorf("cancel status = %d, want %d (body=%s)", w2.Code, http.StatusOK, w2.Body.String())
	}

	// Wait for background goroutine to detect cancellation
	tid, ok := waitForNotification(th.Notify, 10*time.Second)
	if !ok {
		t.Fatal("timeout waiting for subprocess to handle cancellation")
	}
	if tid != "cancel-thread" {
		t.Errorf("notification threadID = %q, want %q", tid, "cancel-thread")
	}
}

func TestHandleCancelRequest_NoRunning(t *testing.T) {
	th := newTestRouter(t)
	defer th.Cleanup()

	th.Client.CreateThread(context.Background(), "idle-thread", "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/threads/idle-thread/cancel", nil)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// ── auth (endpoint-level) ─────────────────────────────────────────────────

func TestAuthRequired_NoAuth(t *testing.T) {
	oldKey := apiKey
	apiKey = "test-secret"
	defer func() { apiKey = oldKey }()

	th := newTestRouter(t)
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/health", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestAuthRequired_ValidAuth(t *testing.T) {
	oldKey := apiKey
	apiKey = "test-secret"
	defer func() { apiKey = oldKey }()

	th := newTestRouter(t)
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/health", nil)
	r.Header.Set("Authorization", "Bearer test-secret")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}
}
