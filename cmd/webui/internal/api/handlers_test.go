package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"

	"github.com/noodle05/ai-agents/cmd/webui/internal/request"
	"github.com/noodle05/ai-agents/cmd/webui/internal/templates"
	"github.com/noodle05/ai-agents/tasklib"
)

// ── test infrastructure ───────────────────────────────────────────────────

type testHarness struct {
	Router       chi.Router
	Handler      *request.Handler
	Client       *tasklib.Client
	rdb          *redis.Client
	Renderer     *templates.Renderer
	MR           *miniredis.Miniredis
	WorkspaceDir string
	SessionsDir  string
	Notify       chan string
	cleanup      func()
}

func (th *testHarness) Cleanup() {
	th.cleanup()
}

func newTestRouter(t *testing.T, mwCfg MiddlewareConfig) *testHarness {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	client := tasklib.NewClient(rdb)
	services := tasklib.NewServices(rdb)

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
		ClaudePath: "/bin/true",
		Paths: &request.PathsConfig{
			WorkspaceDir:      workspaceDir,
			ClaudeSessionsDir: sessionsDir,
		},
		RequestTimeout: 30 * time.Second,
		MaxConcurrent:  5,
		ShutdownGrace:  5 * time.Second,
		OutputFormat:   "text",
		TestNotify:     notify,
	}

	handler := request.New(services.Threads, services.Requests, services.History, services.SysOps, cfg)
	bgCtx, bgCancel := context.WithCancel(context.Background())
	renderer, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New: %v", err)
	}
	var accessLog atomic.Pointer[slog.Logger]
	newAccessLogger := func() *slog.Logger { return nil }

	// Fill in defaults for MiddlewareConfig fields that aren't set.
	if mwCfg.RequestsLimiter == nil {
		mwCfg.RequestsLimiter = NewRateLimiter(10, time.Minute)
	}
	if mwCfg.ThreadsLimiter == nil {
		mwCfg.ThreadsLimiter = NewRateLimiter(30, time.Minute)
	}
	if mwCfg.DefaultLimiter == nil {
		mwCfg.DefaultLimiter = NewRateLimiter(60, time.Minute)
	}
	if mwCfg.Paths == nil {
		mwCfg.Paths = &request.PathsConfig{
			WorkspaceDir:      workspaceDir,
			ClaudeSessionsDir: sessionsDir,
		}
	}

	router := NewRouter(services, handler, renderer, bgCtx, &accessLog, newAccessLogger, mwCfg)

	return &testHarness{
		Router:       router,
		Handler:      handler,
		Client:       client,
		rdb:          rdb,
		Renderer:     renderer,
		MR:           mr,
		WorkspaceDir: workspaceDir,
		SessionsDir:  sessionsDir,
		Notify:       notify,
		cleanup: func() {
			bgCancel() // stop rate limiter cleanup goroutines
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
	th := newTestRouter(t, MiddlewareConfig{})
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
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()
	th.Client.CreateThread(ctx, "stats-thread", "repo/test", "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/stats", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	readJSON(w, &resp)
	if _, ok := resp["tasks_enqueued_ever"]; !ok {
		t.Error("stats missing tasks_enqueued_ever")
	}
	if _, ok := resp["queue_depths"]; !ok {
		t.Error("stats missing queue_depths")
	}
}

// ── workers ────────────────────────────────────────────────────────────────

func TestHandleListWorkers(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/workers", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHandleGetWorker(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/workers/claude", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHandleGetWorker_Unknown(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/workers/nonexistent", nil)
	th.Router.ServeHTTP(w, r)

	// Unknown workers return 200 with zero stats (dynamic discovery — any name is valid)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

// ── threads ────────────────────────────────────────────────────────────────

func TestHandleCreateThread(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
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
	th := newTestRouter(t, MiddlewareConfig{})
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
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	th.Client.CreateThread(context.Background(), "dup-thread", "", "")

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
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	th.Client.CreateThread(context.Background(), "list-thread-1", "", "")
	th.Client.CreateThread(context.Background(), "list-thread-2", "", "")

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
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	th.Client.CreateThread(context.Background(), "detail-thread", "repo/test", "")

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
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads/nonexistent", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleThreadHistory(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()
	th.Client.CreateThread(ctx, "hist-thread", "", "")
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
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()
	th.Client.CreateThread(ctx, "tail-thread", "", "")
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
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	th.Client.CreateThread(context.Background(), "ws-thread", "", "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/threads/ws-thread/workspace", nil)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleDeleteWorkspace_Confirm(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	th.Client.CreateThread(context.Background(), "ws-delete-thread", "", "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/threads/ws-delete-thread/workspace?confirm=true", nil)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestHandleKeepThread(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	th.Client.CreateThread(context.Background(), "keep-thread", "", "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/threads/keep-thread/keep", nil)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHandleResetSession(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()
	th.Client.CreateThread(ctx, "reset-thread", "", "")
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

func TestHandleDeleteThread_NoConfirm(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	th.Client.CreateThread(context.Background(), "del-noconfirm", "", "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/threads/del-noconfirm", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleDeleteThread_NotFound(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/threads/nonexistent?confirm=true", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestHandleDeleteThread_InvalidID(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/threads/invalid:thread?confirm=true", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleDeleteThread_Success(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()
	threadID := "delete-success"
	th.Client.CreateThread(ctx, threadID, "repo/test", "")
	th.Client.SetThreadSessionID(ctx, threadID, "session-to-delete")
	th.Client.SetThreadComplete(ctx, threadID)
	th.Client.AppendMessage(ctx, threadID, tasklib.Message{
		Role: "user", Type: "request", Content: "hello",
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/threads/"+threadID+"?confirm=true", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	// Thread should be gone from Redis
	exists, err := th.Client.ThreadExists(ctx, threadID)
	if err != nil {
		t.Fatalf("ThreadExists after delete: %v", err)
	}
	if exists {
		t.Error("thread should not exist in Redis after delete")
	}
}

func TestHandleDeleteThread_LockHeld(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	threadID := "delete-locked"
	th.Client.CreateThread(context.Background(), threadID, "", "")

	// Hold a request lock — delete should be rejected
	ok, err := th.Client.AcquireRequestLock(context.Background(), threadID, "req-1", tasklib.LockTTL)
	if err != nil {
		t.Fatalf("AcquireRequestLock: %v", err)
	}
	if !ok {
		t.Fatal("should have acquired lock")
	}
	defer th.Client.ReleaseRequestLock(context.Background(), threadID)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/threads/"+threadID+"?confirm=true", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleDeleteThread_ThreadLockHeld(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	threadID := "delete-thread-locked"
	th.Client.CreateThread(context.Background(), threadID, "", "")

	// Hold a thread lock (as Enqueue does) — delete should be rejected
	ok, err := th.Client.LockThread(context.Background(), threadID, "task-1", tasklib.LockTTL)
	if err != nil {
		t.Fatalf("LockThread: %v", err)
	}
	if !ok {
		t.Fatal("should have acquired thread lock")
	}
	defer th.Client.UnlockThread(context.Background(), threadID)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/threads/"+threadID+"?confirm=true", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusConflict, w.Body.String())
	}
}

// ── tasks ──────────────────────────────────────────────────────────────────

func TestHandleListTasks(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/tasks", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHandleGetTask_NotFound(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/tasks/nonexistent-task", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (for nonexistent task)", w.Code, http.StatusNotFound)
	}
}

func TestHandleGetTaskResult_NotFound(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
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
	th := newTestRouter(t, MiddlewareConfig{})
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
	th := newTestRouter(t, MiddlewareConfig{})
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
	th := newTestRouter(t, MiddlewareConfig{})
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
	th := newTestRouter(t, MiddlewareConfig{})
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
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	th.setSlowFakeClaude(t)

	ctx := context.Background()
	th.Client.CreateThread(ctx, "cancel-thread", "", "")

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
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	th.Client.CreateThread(context.Background(), "idle-thread", "", "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/threads/idle-thread/cancel", nil)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// ── HTMX content negotiation ───────────────────────────────────────────────

func TestHandleSubmitRequest_HTMXFormEncoded(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	th.setFakeClaude(t, []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"Done"}`,
	}, 0)

	// Simulate HTMX form submission (application/x-www-form-urlencoded)
	body := strings.NewReader("request=Say+hello&repo=test/repo&thread_id=")
	r := httptest.NewRequest("POST", "/api/requests", body)
	r.Header.Set("HX-Request", "true")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-CSRF-Token", th.Renderer.CSRFToken)
	w := httptest.NewRecorder()
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	// Response should be HTML partial (the request-submitted template)
	bodyStr := w.Body.String()
	if !strings.Contains(bodyStr, "Request submitted") && !strings.Contains(bodyStr, "View thread") {
		t.Errorf("expected HTML partial, got: %s", bodyStr)
	}

	// Wait for subprocess
	waitForNotification(th.Notify, 10*time.Second)
}

func TestHandleGetThread_HTMXReturnsPartial(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()
	th.Client.CreateThread(ctx, "htmx-thread", "repo/test", "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads/htmx-thread", nil)
	r.Header.Set("HX-Request", "true")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	bodyStr := w.Body.String()
	// Should contain the state panel HTML, not JSON
	if !strings.Contains(bodyStr, "state-panel") {
		t.Errorf("expected state-panel in HTML partial, got: %s", bodyStr)
	}
	if strings.HasPrefix(bodyStr, "{") {
		t.Error("expected HTML partial, got JSON")
	}
}

func TestHandleGetThread_NonHTMXReturnsJSON(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()
	th.Client.CreateThread(ctx, "json-thread", "repo/test", "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads/json-thread", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	bodyStr := w.Body.String()
	if !strings.HasPrefix(bodyStr, "{") {
		t.Error("expected JSON response for non-HTMX request")
	}

	var resp map[string]interface{}
	readJSON(w, &resp)
	if _, ok := resp["thread"]; !ok {
		t.Error("JSON response missing thread field")
	}
}

func TestHandleGetThread_InvalidID(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	// Use an ID with a colon, which is rejected by ValidThreadID but
	// doesn't get normalized away by the router like ".." does.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads/invalid:thread", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleGetThread_InvalidID_HTMX(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	// Use a colon in the ID — it's rejected by ValidThreadID but doesn't
	// create a new path segment (unlike "/").
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads/invalid:id", nil)
	r.Header.Set("HX-Request", "true")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestPageThreadDetail_InvalidID(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/threads/invalid:thread", nil)
	th.Router.ServeHTTP(w, r)

	// Page should return 200 (it's a page route, not an API route).
	// The ValidThreadID check passes nil Thread data to the template,
	// which renders the page shell successfully.
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	if w.Body.Len() == 0 {
		t.Error("page should have a body")
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

// ── auth (endpoint-level) ─────────────────────────────────────────────────

func TestAuthRequired_NoAuth(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{AuthKey: "test-secret"})
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/health", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestAuthRequired_ValidAuth(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{AuthKey: "test-secret"})
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/health", nil)
	r.Header.Set("Authorization", "Bearer test-secret")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestAuthRequired_ValidQueryParamAuth(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{AuthKey: "test-secret"})
	defer th.Cleanup()

	// Query param auth must survive sanitizeQueryMiddleware stripping api_key
	// from the URL before authMiddleware reads it.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/health?api_key=test-secret", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}
}

// ── history poll (incremental message loading) ─────────────────────────────

func TestHandleThreadHistory_Poll(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()
	th.Client.CreateThread(ctx, "poll-thread", "", "")
	for i := 0; i < 3; i++ {
		th.Client.AppendMessage(ctx, "poll-thread", tasklib.Message{
			Role: "user", Type: "request", Content: fmt.Sprintf("msg %d", i),
			Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		})
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads/poll-thread/history?offset=0&poll=1", nil)
	r.Header.Set("HX-Request", "true")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	bodyStr := w.Body.String()
	// Poll response should contain the OOB-targeted details and a replacement poller
	if !strings.Contains(bodyStr, "history-poller") {
		t.Errorf("expected history-poller in poll response, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "thread-timeline") {
		t.Errorf("expected thread-timeline OOB target in poll response, got: %s", bodyStr)
	}
}

func TestHandleThreadHistory_PollRunning(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()
	th.Client.CreateThread(ctx, "poll-running-thread", "", "")
	th.Client.AppendMessage(ctx, "poll-running-thread", tasklib.Message{
		Role: "user", Type: "request", Content: "hello",
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})
	// Acquire request lock so IsRequestRunning returns true
	th.Client.AcquireRequestLock(ctx, "poll-running-thread", "req-1", tasklib.LockTTL)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads/poll-running-thread/history?offset=0&poll=1", nil)
	r.Header.Set("HX-Request", "true")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	bodyStr := w.Body.String()
	// When running, the poll response should include a history-poller with hx-get for the next poll
	if !strings.Contains(bodyStr, "history-poller") {
		t.Errorf("expected history-poller when running, got: %s", bodyStr)
	}
}

func TestHandleThreadHistory_PollPreservesMsgCSSClasses(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()
	th.Client.CreateThread(ctx, "poll-css-thread", "", "")
	th.Client.AppendMessage(ctx, "poll-css-thread", tasklib.Message{
		Role: "master", Type: "response", Content: "styled response",
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})
	th.Client.AcquireRequestLock(ctx, "poll-css-thread", "req-1", tasklib.LockTTL)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads/poll-css-thread/history?offset=0&poll=1", nil)
	r.Header.Set("HX-Request", "true")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	bodyStr := w.Body.String()

	// OOB swap must be on a wrapper container, not on <details> directly.
	// HTMX strips the wrapper for non-outerHTML swap styles, so <details>
	// must be children of the OOB container to retain their CSS classes.
	if strings.Contains(bodyStr, "<details class=\"msg") && strings.Contains(bodyStr, "hx-swap-oob") {
		// If <details> itself has hx-swap-oob, the wrapper gets stripped
		// and .msg CSS classes are lost.
		if strings.Contains(bodyStr, "class=\"msg") {
			beforeDetails := strings.Index(bodyStr, "class=\"msg")
			afterDetails := strings.Index(bodyStr[beforeDetails:], "hx-swap-oob")
			if afterDetails > 0 && afterDetails < 200 {
				t.Errorf("hx-swap-oob must be on wrapper container, not on <details> directly. CSS classes will be stripped by HTMX.\nGot: %s", bodyStr)
			}
		}
	}

	// The <details> element must have the full CSS class with role and type.
	if !strings.Contains(bodyStr, "class=\"msg role-master type-response") {
		t.Errorf("expected <details> with class=\"msg role-master type-response\", got: %s", bodyStr)
	}

	// The OOB container must target thread-timeline.
	if !strings.Contains(bodyStr, "beforeend:#thread-timeline") {
		t.Errorf("expected OOB container targeting beforeend:#thread-timeline, got: %s", bodyStr)
	}
}

// TestHandleGetThread_StatePollDoesNotReplaceFollowupForm verifies that
// the thread-state-oob poll response for a complete thread does not OOB-swap
// the followup-section. Swapping it on every poll would wipe any text the
// user is typing into the follow-up form.
func TestHandleGetThread_StatePollDoesNotReplaceFollowupForm(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()
	th.Client.CreateThread(ctx, "state-nofollowup", "repo/test", "")
	th.Client.SetThreadComplete(ctx, "state-nofollowup")
	th.Client.UpdateThread(ctx, "state-nofollowup", map[string]string{"status": "complete"})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads/state-nofollowup", nil)
	r.Header.Set("HX-Request", "true")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	bodyStr := w.Body.String()
	if !strings.Contains(bodyStr, "Response ready") {
		t.Errorf("expected 'Response ready' banner for complete thread, got: %s", bodyStr)
	}
	if strings.Contains(bodyStr, `id="followup-section"`) {
		t.Errorf("state poll must not OOB-swap #followup-section (it would wipe user input on every poll), got: %s", bodyStr)
	}
}

// TestHandleThreadHistory_PollCompleteInjectsFollowupForm verifies that
// when the history poll detects the thread is no longer running (transition
// from running to complete), it injects the follow-up form via OOB swap.
// This fires exactly once because the history poller stops after completion.
func TestHandleThreadHistory_PollCompleteInjectsFollowupForm(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()
	th.Client.CreateThread(ctx, "hist-complete-followup", "", "")
	th.Client.AppendMessage(ctx, "hist-complete-followup", tasklib.Message{
		Role: "user", Type: "request", Content: "hello",
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})
	th.Client.SetThreadComplete(ctx, "hist-complete-followup")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads/hist-complete-followup/history?offset=0&poll=1", nil)
	r.Header.Set("HX-Request", "true")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	bodyStr := w.Body.String()
	if !strings.Contains(bodyStr, `id="followup-section"`) {
		t.Errorf("history poll should inject #followup-section when thread completes, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `name="thread_id" value="hist-complete-followup"`) {
		t.Errorf("expected followup form with thread_id=hist-complete-followup, got: %s", bodyStr)
	}
}

// ── follow-up request clears complete flag ─────────────────────────────────

func TestHandleSubmitRequest_FollowUpClearsComplete(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	th.setFakeClaude(t, []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"Done"}`,
	}, 0)

	ctx := context.Background()
	// Create a thread that has already completed
	th.Client.CreateThread(ctx, "followup-thread", "", "")
	th.Client.SetThreadComplete(ctx, "followup-thread")
	th.Client.UpdateThread(ctx, "followup-thread", map[string]string{"status": "complete"})

	// Verify it's complete before
	complete, _ := th.Client.IsThreadComplete(ctx, "followup-thread")
	if !complete {
		t.Fatal("thread should be complete before follow-up")
	}

	// Submit follow-up via HTMX form
	body := strings.NewReader("request=Follow-up+question&thread_id=followup-thread&from_thread=true")
	r := httptest.NewRequest("POST", "/api/requests", body)
	r.Header.Set("HX-Request", "true")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-CSRF-Token", th.Renderer.CSRFToken)
	w := httptest.NewRecorder()
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	// Should show follow-up confirmation
	bodyStr := w.Body.String()
	if !strings.Contains(bodyStr, "Follow-up submitted") {
		t.Errorf("expected 'Follow-up submitted', got: %s", bodyStr)
	}
	// Should include OOB swap to re-trigger history fetch with polling
	if !strings.Contains(bodyStr, "thread-history") || !strings.Contains(bodyStr, "hx-swap-oob") {
		t.Errorf("expected thread-history OOB re-trigger in reply-confirmed, got: %s", bodyStr)
	}

	// Complete flag must be cleared
	complete, _ = th.Client.IsThreadComplete(ctx, "followup-thread")
	if complete {
		t.Error("thread should NOT be complete after follow-up submission")
	}

	// Status must be updated to running
	thread, err := th.Client.GetThread(ctx, "followup-thread")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thread.Status != "running" {
		t.Errorf("thread status = %q, want %q", thread.Status, "running")
	}

	// Wait for subprocess
	waitForNotification(th.Notify, 10*time.Second)
}

func TestHandleSubmitRequest_FollowUpHTMXReplyConfirmed(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	th.setFakeClaude(t, []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"Done"}`,
	}, 0)

	// Submit a new request from the thread detail page (from_thread=true)
	body := strings.NewReader("request=Do+something&thread_id=reply-thread&from_thread=true")
	r := httptest.NewRequest("POST", "/api/requests", body)
	r.Header.Set("HX-Request", "true")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-CSRF-Token", th.Renderer.CSRFToken)
	w := httptest.NewRecorder()
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	bodyStr := w.Body.String()
	if !strings.Contains(bodyStr, "Follow-up submitted") {
		t.Errorf("expected reply-confirmed partial, got: %s", bodyStr)
	}
	// The reply-confirmed template links back to the thread detail page
	if !strings.Contains(bodyStr, "/threads/reply-thread") {
		t.Errorf("expected link back to thread, got: %s", bodyStr)
	}

	waitForNotification(th.Notify, 10*time.Second)
}

// ── /api/metrics tests ─────────────────────────────────────────────────────

func TestMetricsEndpoint_Returns200(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	rdb := th.rdb
	rdb.Set(context.Background(), "stats:task_done", "10", 0)
	rdb.Set(context.Background(), "stats:task_failed", "2", 0)
	rdb.Set(context.Background(), "stats:task_cancelled", "1", 0)
	rdb.HSet(context.Background(), "active_tasks", "task-1", `{"status":"running"}`)

	// Set up heartbeat and queue keys so metrics have workers to report
	rdb.LPush(context.Background(), tasklib.QueueKey("claude"), "t1")
	rdb.SetEx(context.Background(), tasklib.HeartbeatKey("claude"), `{"worker_name":"claude","hostname":"h1","last_heartbeat_at":"2026-01-01T00:00:00Z"}`, 30*time.Second)

	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	th.Router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body=%s)", w.Code, w.Body.String())
	}

	body := w.Body.String()
	expectedMetrics := []string{
		"ai_agents_tasks_total",
		"ai_agents_threads_active",
		"ai_agents_threads_stuck",
		"ai_agents_workers_online",
		"ai_agents_queue_depth",
		"ai_agents_tasks_running",
		"ai_agents_tasks_pending",
	}
	for _, name := range expectedMetrics {
		if !strings.Contains(body, name) {
			t.Errorf("expected metric %q in output", name)
		}
	}
	if !strings.Contains(body, "# HELP ai_agents_tasks_total") {
		t.Error("missing HELP line for ai_agents_tasks_total")
	}
	if !strings.Contains(body, "# TYPE ai_agents_tasks_total counter") {
		t.Error("missing TYPE line for ai_agents_tasks_total")
	}
}

func TestMetricsEndpoint_StatusLabels(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	rdb := th.rdb
	rdb.Set(context.Background(), "stats:task_done", "5", 0)
	rdb.Set(context.Background(), "stats:task_failed", "3", 0)
	rdb.Set(context.Background(), "stats:task_cancelled", "2", 0)

	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	th.Router.ServeHTTP(w, req)

	body := w.Body.String()
	for _, st := range []string{"done", "failed", "cancelled"} {
		if !strings.Contains(body, `status="`+st+`"`) {
			t.Errorf("expected status label %q in output", st)
		}
	}
}

func TestMetricsEndpoint_QueueDepthLabels(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	rdb := th.rdb
	rdb.LPush(context.Background(), tasklib.QueueKey("claude"), "task-json-1")
	rdb.LPush(context.Background(), tasklib.QueueKey("copilot"), "task-json-2")

	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	th.Router.ServeHTTP(w, req)

	body := w.Body.String()
	for _, wt := range []string{"claude", "copilot"} {
		if !strings.Contains(body, `worker_name="`+wt+`"`) {
			t.Errorf("expected worker_name label %q in output", wt)
		}
	}
}

// ── parent-child thread tests ────────────────────────────────────────────

func TestHandleCreateThread_WithParentThreadID(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	// Create parent thread first — validation now requires it to exist.
	th.Client.CreateThread(context.Background(), "parent", "", "")

	body := strings.NewReader(`{"thread_id":"child","repo":"owner/repo","parent_thread_id":"parent"}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/threads", body)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp map[string]interface{}
	readJSON(w, &resp)
	if resp["parent_thread_id"] != "parent" {
		t.Errorf("parent_thread_id = %q, want %q", resp["parent_thread_id"], "parent")
	}

	// Verify parent stored in Redis
	state, _ := th.rdb.HGetAll(context.Background(), tasklib.ThreadStateKey("child")).Result()
	if state["parent_thread_id"] != "parent" {
		t.Errorf("Redis parent_thread_id = %q, want %q", state["parent_thread_id"], "parent")
	}
}

func TestHandleGetThread_ReturnsChildren(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()
	th.Client.CreateThread(ctx, "parent", "", "")
	th.Client.CreateThread(ctx, "child-1", "", "parent")
	th.Client.CreateThread(ctx, "child-2", "", "parent")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads/parent", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	readJSON(w, &resp)
	children, ok := resp["children"].([]interface{})
	if !ok {
		t.Fatal("response missing children field")
	}
	if len(children) != 2 {
		t.Errorf("got %d children, want 2", len(children))
	}
}

func TestBuildThreadTree(t *testing.T) {
	threads := []*tasklib.Thread{
		{ThreadID: "root", ParentThreadID: ""},
		{ThreadID: "child-1", ParentThreadID: "root"},
		{ThreadID: "child-2", ParentThreadID: "root"},
		{ThreadID: "orphan", ParentThreadID: "missing"},
	}

	children := buildThreadTree(threads)
	if len(children["root"]) != 2 {
		t.Errorf("root children = %d, want 2", len(children["root"]))
	}
	if len(children["missing"]) != 1 {
		t.Errorf("missing children = %d, want 1", len(children["missing"]))
	}
	if _, ok := children[""]; ok {
		t.Error("empty parent should not have entries")
	}
}

func TestFilterRootThreads(t *testing.T) {
	threads := []*tasklib.Thread{
		{ThreadID: "root", ParentThreadID: ""},
		{ThreadID: "child", ParentThreadID: "root"},
		{ThreadID: "root-2", ParentThreadID: ""},
		{ThreadID: "orphan", ParentThreadID: "missing"},
	}

	known := make(map[string]bool)
	for _, th := range threads {
		known[th.ThreadID] = true
	}

	roots := filterRootThreads(threads)
	if len(roots) != 3 {
		t.Errorf("got %d root threads, want 3 (root, root-2, orphan)", len(roots))
	}
	for _, r := range roots {
		if r.ParentThreadID != "" && known[r.ParentThreadID] {
			t.Errorf("root thread %q has valid ParentThreadID = %q but was not included in set", r.ThreadID, r.ParentThreadID)
		}
	}
}

func TestFilterChildren(t *testing.T) {
	threads := []*tasklib.Thread{
		{ThreadID: "c1", ParentThreadID: "p"},
		{ThreadID: "c2", ParentThreadID: "p"},
		{ThreadID: "c3", ParentThreadID: "other"},
	}

	children := filterChildren(threads, "p")
	if len(children) != 2 {
		t.Errorf("got %d children, want 2", len(children))
	}
	for _, c := range children {
		if c.ParentThreadID != "p" {
			t.Errorf("child %q has ParentThreadID = %q", c.ThreadID, c.ParentThreadID)
		}
	}
}

// ── cascade delete tests ────────────────────────────────────────────────

func TestHandleDeleteThread_Cascade(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()

	// Create parent + 2 children + 1 unrelated thread
	th.Client.CreateThread(ctx, "del-parent", "", "")
	th.Client.CreateThread(ctx, "del-child-1", "", "del-parent")
	th.Client.CreateThread(ctx, "del-child-2", "", "del-parent")
	th.Client.CreateThread(ctx, "del-unrelated", "", "") // should survive

	// Set session IDs on all 3
	th.Client.SetThreadSessionID(ctx, "del-parent", "sess-parent")
	th.Client.SetThreadSessionID(ctx, "del-child-1", "sess-child-1")
	th.Client.SetThreadSessionID(ctx, "del-child-2", "sess-child-2")

	// Create workspace directories for all 3
	for _, id := range []string{"del-parent", "del-child-1", "del-child-2"} {
		wp := filepath.Join(th.WorkspaceDir, id)
		if err := os.MkdirAll(wp, 0755); err != nil {
			t.Fatalf("MkdirAll workspace %s: %v", wp, err)
		}
	}

	// Create session files for all 3
	projectDir := filepath.Join(th.SessionsDir, "projects", "-workspace-")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("MkdirAll projects: %v", err)
	}
	for _, sid := range []string{"sess-parent", "sess-child-1", "sess-child-2"} {
		sf := filepath.Join(projectDir, sid+".json")
		if err := os.WriteFile(sf, []byte("{}"), 0644); err != nil {
			t.Fatalf("WriteFile session %s: %v", sf, err)
		}
	}

	// Delete parent — cascade should clean up children too
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/threads/del-parent?confirm=true", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify all 3 threads' Redis keys are gone
	for _, id := range []string{"del-parent", "del-child-1", "del-child-2"} {
		exists, err := th.Client.ThreadExists(ctx, id)
		if err != nil {
			t.Fatalf("ThreadExists(%s): %v", id, err)
		}
		if exists {
			t.Errorf("thread %s should not exist in Redis after cascade delete", id)
		}
	}

	// Verify workspace dirs are gone
	for _, id := range []string{"del-parent", "del-child-1", "del-child-2"} {
		wp := filepath.Join(th.WorkspaceDir, id)
		if _, err := os.Stat(wp); err == nil {
			t.Errorf("workspace dir %s should be removed", wp)
		}
	}

	// Verify session files are gone
	for _, sid := range []string{"sess-parent", "sess-child-1", "sess-child-2"} {
		sf := filepath.Join(projectDir, sid+".json")
		if _, err := os.Stat(sf); err == nil {
			t.Errorf("session file %s should be removed", sf)
		}
	}

	// Verify unrelated thread survived
	exists, err := th.Client.ThreadExists(ctx, "del-unrelated")
	if err != nil {
		t.Fatalf("ThreadExists(unrelated): %v", err)
	}
	if !exists {
		t.Error("unrelated thread should still exist after cascade delete")
	}
}

func TestHandleDeleteThread_CascadeChildHasActiveTask(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()

	// Create parent + child
	th.Client.CreateThread(ctx, "active-parent", "", "")
	th.Client.CreateThread(ctx, "active-child", "", "active-parent")

	// Enqueue an active (pending) task on the child
	_, err := th.Client.Enqueue(ctx, "claude", "active-child", "do work")
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}

	// Delete parent — should be rejected because child has active task
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/threads/active-parent?confirm=true", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusConflict, w.Body.String())
	}

	// Verify parent still exists (was not deleted)
	exists, err := th.Client.ThreadExists(ctx, "active-parent")
	if err != nil {
		t.Fatalf("ThreadExists: %v", err)
	}
	if !exists {
		t.Error("parent should still exist after rejected delete")
	}

	// Verify child still exists
	exists, err = th.Client.ThreadExists(ctx, "active-child")
	if err != nil {
		t.Fatalf("ThreadExists: %v", err)
	}
	if !exists {
		t.Error("child should still exist after rejected delete")
	}
}

func TestHandleDeleteThread_CascadeMoreThan50Tasks(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()

	// Create parent + child
	th.Client.CreateThread(ctx, "bulk-parent", "", "")
	th.Client.CreateThread(ctx, "bulk-child", "", "bulk-parent")

	// Create 60 tasks on the child (exceeds old default limit of 50)
	// All but the last are "done" (inactive)
	for i := 0; i < 59; i++ {
		task, err := th.Client.Enqueue(ctx, "claude", "bulk-child", fmt.Sprintf("task %d", i))
		if err != nil {
			t.Fatalf("Enqueue %d failed: %v", i, err)
		}
		// Mark task as done
		th.rdb.Set(ctx, tasklib.TaskKey(task.TaskID, "status"), "done", 0)
	}

	// The 60th task is "running" (active) — should be detected even though it's past position 50
	task, err := th.Client.Enqueue(ctx, "claude", "bulk-child", "active task")
	if err != nil {
		t.Fatalf("Enqueue active task failed: %v", err)
	}
	th.rdb.Set(ctx, tasklib.TaskKey(task.TaskID, "status"), "running", 0)
	// Release the thread lock so IsThreadLocked doesn't catch this first —
	// we want ListTasks to detect the "running" task at position 60.
	th.Client.UnlockThread(ctx, "bulk-child")

	// Delete parent — should be rejected because child has active task at position 60
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/threads/bulk-parent?confirm=true", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}

	// Verify parent still exists
	exists, _ := th.Client.ThreadExists(ctx, "bulk-parent")
	if !exists {
		t.Error("parent should still exist after rejected delete")
	}
}

func TestHandleDeleteThread_DeepCascade(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()

	// Three-level tree: grandparent → parent → child
	th.Client.CreateThread(ctx, "deep-grand", "", "")
	th.Client.CreateThread(ctx, "deep-parent", "", "deep-grand")
	th.Client.CreateThread(ctx, "deep-child", "", "deep-parent")

	// Set session IDs on all 3
	th.Client.SetThreadSessionID(ctx, "deep-grand", "sess-grand")
	th.Client.SetThreadSessionID(ctx, "deep-parent", "sess-parent-2")
	th.Client.SetThreadSessionID(ctx, "deep-child", "sess-child-3")

	// Create workspace directories for all 3
	for _, id := range []string{"deep-grand", "deep-parent", "deep-child"} {
		wp := filepath.Join(th.WorkspaceDir, id)
		if err := os.MkdirAll(wp, 0755); err != nil {
			t.Fatalf("MkdirAll workspace %s: %v", wp, err)
		}
	}

	// Create session files for all 3
	projectDir := filepath.Join(th.SessionsDir, "projects", "-workspace-")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("MkdirAll projects: %v", err)
	}
	for _, sid := range []string{"sess-grand", "sess-parent-2", "sess-child-3"} {
		sf := filepath.Join(projectDir, sid+".json")
		if err := os.WriteFile(sf, []byte("{}"), 0644); err != nil {
			t.Fatalf("WriteFile session %s: %v", sf, err)
		}
	}

	// Delete grandparent — cascade should reach parent and child
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/threads/deep-grand?confirm=true", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify all 3 threads' Redis keys are gone
	for _, id := range []string{"deep-grand", "deep-parent", "deep-child"} {
		exists, err := th.Client.ThreadExists(ctx, id)
		if err != nil {
			t.Fatalf("ThreadExists(%s): %v", id, err)
		}
		if exists {
			t.Errorf("thread %s should not exist in Redis after cascade delete", id)
		}
	}

	// Verify workspace dirs are gone for all 3 levels
	for _, id := range []string{"deep-grand", "deep-parent", "deep-child"} {
		wp := filepath.Join(th.WorkspaceDir, id)
		if _, err := os.Stat(wp); err == nil {
			t.Errorf("workspace dir %s should be removed", wp)
		}
	}

	// Verify session files are gone for all 3 levels
	for _, sid := range []string{"sess-grand", "sess-parent-2", "sess-child-3"} {
		sf := filepath.Join(projectDir, sid+".json")
		if _, err := os.Stat(sf); err == nil {
			t.Errorf("session file %s should be removed", sf)
		}
	}
}

func TestHandleDeleteThread_NoChildren(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()
	threadID := "delete-no-kids"
	th.Client.CreateThread(ctx, threadID, "repo/test", "")
	th.Client.SetThreadSessionID(ctx, threadID, "session-to-delete")
	th.Client.SetThreadComplete(ctx, threadID)
	th.Client.AppendMessage(ctx, threadID, tasklib.Message{
		Role: "user", Type: "request", Content: "hello",
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/threads/"+threadID+"?confirm=true", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	// Thread should be gone from Redis
	exists, err := th.Client.ThreadExists(ctx, threadID)
	if err != nil {
		t.Fatalf("ThreadExists after delete: %v", err)
	}
	if exists {
		t.Error("thread should not exist in Redis after delete")
	}
}

// ── parent validation tests ──────────────────────────────────────────────

func TestHandleCreateThread_ParentNotFound(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	body := strings.NewReader(`{"thread_id":"orphan-child","parent_thread_id":"nonexistent"}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/threads", body)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleCreateThread_SelfParent(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	body := strings.NewReader(`{"thread_id":"self-thread","parent_thread_id":"self-thread"}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/threads", body)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleCreateThread_ValidParent(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	// Create parent first
	th.Client.CreateThread(context.Background(), "valid-parent", "", "")

	body := strings.NewReader(`{"thread_id":"valid-child","parent_thread_id":"valid-parent"}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/threads", body)
	r.Header.Set("Content-Type", "application/json")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp map[string]interface{}
	readJSON(w, &resp)
	if resp["parent_thread_id"] != "valid-parent" {
		t.Errorf("parent_thread_id = %q, want %q", resp["parent_thread_id"], "valid-parent")
	}
}

// ── global tokens ─────────────────────────────────────────────────────────

func seedTokenData(t *testing.T, rdb *redis.Client) {
	t.Helper()
	ctx := context.Background()

	rdb.HSet(ctx, tasklib.StatsTotalKey(), map[string]interface{}{
		"input_tokens":  int64(50000),
		"output_tokens": int64(12000),
		"cache_read":    int64(3000),
		"cache_write":   int64(1000),
		"reasoning":     int64(0),
		"task_count":    int64(197),
	})
	rdb.HSet(ctx, tasklib.StatsAgentKey("master"), map[string]interface{}{
		"input_tokens":  int64(5000),
		"output_tokens": int64(2000),
		"cache_read":    int64(500),
		"cache_write":   int64(200),
		"reasoning":     int64(0),
		"task_count":    int64(50),
	})
	rdb.HSet(ctx, tasklib.StatsAgentKey("claude"), map[string]interface{}{
		"input_tokens":  int64(45000),
		"output_tokens": int64(10000),
		"cache_read":    int64(2500),
		"cache_write":   int64(800),
		"reasoning":     int64(0),
		"task_count":    int64(147),
	})
}

func TestHandleGlobalTokens_JSON(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	seedTokenData(t, th.rdb)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/tokens", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	readJSON(w, &resp)

	total, ok := resp["total"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing total field")
	}
	if total["input_tokens"] != float64(50000) {
		t.Errorf("total input_tokens = %v, want 50000", total["input_tokens"])
	}
	if total["output_tokens"] != float64(12000) {
		t.Errorf("total output_tokens = %v, want 12000", total["output_tokens"])
	}

	if taskCount, ok := resp["task_count"].(float64); !ok || taskCount != 197 {
		t.Errorf("task_count = %v, want 197", resp["task_count"])
	}

	workers, ok := resp["workers"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing workers field")
	}
	if _, ok := workers["master"]; !ok {
		t.Error("workers missing master")
	}
	if _, ok := workers["claude"]; !ok {
		t.Error("workers missing claude")
	}

	// Only master and claude should be present (codex/copilot/opencode have no data)
	if len(workers) != 2 {
		t.Errorf("workers count = %d, want 2 (master + claude only)", len(workers))
	}
}

func TestHandleGlobalTokens_HTMX(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	seedTokenData(t, th.rdb)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/tokens", nil)
	r.Header.Set("HX-Request", "true")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	bodyStr := w.Body.String()
	if !strings.Contains(bodyStr, "50K") {
		t.Errorf("expected formatted total input (50K) in HTML partial, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "12K") {
		t.Errorf("expected formatted total output (12K) in HTML partial, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "197 tasks") {
		t.Errorf("expected task count in HTML partial, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "master") {
		t.Errorf("expected master row in HTML partial, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "claude") {
		t.Errorf("expected claude row in HTML partial, got: %s", bodyStr)
	}
	// token-stats template wraps content in card
	if !strings.Contains(bodyStr, "token-stats") {
		t.Errorf("expected token-stats CSS class, got: %s", bodyStr)
	}
}

func TestHandleGlobalTokens_Empty(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/tokens", nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	readJSON(w, &resp)

	total, ok := resp["total"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing total field")
	}
	if len(total) != 0 {
		t.Errorf("total should be empty map, got %v", total)
	}

	if taskCount, ok := resp["task_count"].(float64); !ok || taskCount != 0 {
		t.Errorf("task_count = %v, want 0", resp["task_count"])
	}

	workers, ok := resp["workers"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing workers field")
	}
	if len(workers) != 0 {
		t.Errorf("workers should be empty, got %v", workers)
	}
}

func TestHandleGlobalTokens_Empty_HTMX(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/tokens", nil)
	r.Header.Set("HX-Request", "true")
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}
	// Empty response should not render the token-stats card div.
	if strings.Contains(w.Body.String(), "card token-stats") {
		t.Errorf("expected no token-stats card in empty HTMX response, got: %s", w.Body.String())
	}
}

func TestHandleGetThread_IncludesTokens(t *testing.T) {
	th := newTestRouter(t, MiddlewareConfig{})
	defer th.Cleanup()

	ctx := context.Background()
	threadID := "tokens-thread"
	th.Client.CreateThread(ctx, threadID, "repo/test", "")

	// Add master token stats to the thread state directly
	th.rdb.HSet(ctx, tasklib.ThreadStateKey(threadID), map[string]interface{}{
		"master_input_tokens":        int64(1000),
		"master_output_tokens":       int64(500),
		"master_cache_read_tokens":   int64(100),
		"master_cache_write_tokens":  int64(50),
		"master_reasoning_tokens":    int64(0),
	})

	// Enqueue a task with token data
	task, err := th.Client.Enqueue(ctx, "claude", threadID, "test instruction")
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}
	th.rdb.Set(ctx, tasklib.TaskKey(task.TaskID, "status"), "done", 0)
	th.rdb.Set(ctx, tasklib.TaskKey(task.TaskID, "input_tokens"), int64(300), 0)
	th.rdb.Set(ctx, tasklib.TaskKey(task.TaskID, "output_tokens"), int64(200), 0)
	th.rdb.Set(ctx, tasklib.TaskKey(task.TaskID, "cache_read_tokens"), int64(50), 0)

	// Unlock thread so task is visible
	th.Client.UnlockThread(ctx, threadID)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/threads/"+threadID, nil)
	th.Router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	readJSON(w, &resp)

	tokens, ok := resp["tokens"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing tokens field")
	}

	master, ok := tokens["master"].(map[string]interface{})
	if !ok {
		t.Fatal("tokens missing master field")
	}
	if master["input_tokens"] != float64(1000) {
		t.Errorf("master InputTokens = %v, want 1000", master["input_tokens"])
	}
	if master["output_tokens"] != float64(500) {
		t.Errorf("master OutputTokens = %v, want 500", master["output_tokens"])
	}

	workers, ok := tokens["workers"].(map[string]interface{})
	if !ok {
		t.Fatal("tokens missing workers field")
	}
	claude, ok := workers["claude"].(map[string]interface{})
	if !ok {
		t.Fatal("workers missing claude")
	}
	if claude["input_tokens"] != float64(300) {
		t.Errorf("claude InputTokens = %v, want 300", claude["input_tokens"])
	}
	if claude["output_tokens"] != float64(200) {
		t.Errorf("claude OutputTokens = %v, want 200", claude["output_tokens"])
	}

	tokenRows, ok := resp["token_rows"].([]interface{})
	if !ok {
		t.Fatal("response missing token_rows field")
	}
	if len(tokenRows) != 2 {
		t.Errorf("token_rows count = %d, want 2 (master + claude)", len(tokenRows))
	}
}
