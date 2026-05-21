package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ── admin auth middleware tests ───────────────────────────────────────────

func TestAdminAuthMiddleware_NoKeySet(t *testing.T) {
	handler := adminAuthMiddleware("")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/admin/log-access", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestAdminAuthMiddleware_MissingAuth(t *testing.T) {
	handler := adminAuthMiddleware("admin-secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/admin/log-access", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	if !strings.Contains(w.Body.String(), "missing admin API key") {
		t.Errorf("body should say 'missing admin API key', got %q", w.Body.String())
	}
}

func TestAdminAuthMiddleware_ValidBearer(t *testing.T) {
	handler := adminAuthMiddleware("admin-secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/admin/log-access", nil)
	r.Header.Set("Authorization", "Bearer admin-secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestAdminAuthMiddleware_InvalidBearer(t *testing.T) {
	handler := adminAuthMiddleware("admin-secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/admin/log-access", nil)
	r.Header.Set("Authorization", "Bearer wrong-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	if !strings.Contains(w.Body.String(), "invalid admin API key") {
		t.Errorf("body should say 'invalid admin API key', got %q", w.Body.String())
	}
}

func TestAdminAuthMiddleware_WrongScheme(t *testing.T) {
	handler := adminAuthMiddleware("admin-secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/admin/log-access", nil)
	r.Header.Set("Authorization", "Basic admin-secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// ── admin resource tests ──────────────────────────────────────────────────

type adminTestHarness struct {
	accessLog       *atomic.Pointer[slog.Logger]
	newAccessLogger func() *slog.Logger
	logBuf          *bytes.Buffer
	admin           *adminResource
}

func newAdminTestHarness() *adminTestHarness {
	var accessLog atomic.Pointer[slog.Logger]
	var logBuf bytes.Buffer

	newAccessLogger := func() *slog.Logger {
		h := slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})
		return slog.New(h)
	}

	return &adminTestHarness{
		accessLog:       &accessLog,
		newAccessLogger: newAccessLogger,
		logBuf:          &logBuf,
		admin:           &adminResource{accessLog: &accessLog, newAccessLogger: newAccessLogger},
	}
}

func (h *adminTestHarness) Do(method, path, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	h.admin.logAccessHandler(w, r)
	return w
}

func (h *adminTestHarness) GetJSON(w *httptest.ResponseRecorder) logAccessResponse {
	var resp logAccessResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		panic("failed to unmarshal response: " + w.Body.String())
	}
	return resp
}

func TestAdminGetLogAccess_InitiallyDisabled(t *testing.T) {
	h := newAdminTestHarness()
	w := h.Do("GET", "/api/admin/log-access", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	resp := h.GetJSON(w)
	if resp.Enabled {
		t.Error("expected enabled=false initially")
	}
}

func TestAdminGetLogAccess_AfterEnable(t *testing.T) {
	h := newAdminTestHarness()
	h.accessLog.Store(h.newAccessLogger())

	w := h.Do("GET", "/api/admin/log-access", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	resp := h.GetJSON(w)
	if !resp.Enabled {
		t.Error("expected enabled=true after Store")
	}
}

func TestAdminPutLogAccess_Enable(t *testing.T) {
	h := newAdminTestHarness()
	w := h.Do("PUT", "/api/admin/log-access", `{"enabled":true}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}
	resp := h.GetJSON(w)
	if !resp.Enabled {
		t.Error("expected enabled=true")
	}
	if h.accessLog.Load() == nil {
		t.Error("access logger should not be nil after enable")
	}
}

func TestAdminPutLogAccess_Disable(t *testing.T) {
	h := newAdminTestHarness()
	h.accessLog.Store(h.newAccessLogger()) // start enabled

	w := h.Do("PUT", "/api/admin/log-access", `{"enabled":false}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}
	resp := h.GetJSON(w)
	if resp.Enabled {
		t.Error("expected enabled=false")
	}
	if h.accessLog.Load() != nil {
		t.Error("access logger should be nil after disable")
	}
}

func TestAdminPutLogAccess_IdempotentEnable(t *testing.T) {
	h := newAdminTestHarness()
	h.accessLog.Store(h.newAccessLogger()) // already enabled

	w := h.Do("PUT", "/api/admin/log-access", `{"enabled":true}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}
	resp := h.GetJSON(w)
	if !resp.Enabled {
		t.Error("expected enabled=true for idempotent request")
	}
}

func TestAdminPutLogAccess_IdempotentDisable(t *testing.T) {
	h := newAdminTestHarness()
	// already disabled (nil)

	w := h.Do("PUT", "/api/admin/log-access", `{"enabled":false}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}
	resp := h.GetJSON(w)
	if resp.Enabled {
		t.Error("expected enabled=false for idempotent request")
	}
}

func TestAdminPutLogAccess_EnableCreatesWorkingLogger(t *testing.T) {
	h := newAdminTestHarness()

	w := h.Do("PUT", "/api/admin/log-access", `{"enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("enable failed: %d", w.Code)
	}

	logger := h.accessLog.Load()
	if logger == nil {
		t.Fatal("logger is nil after enable")
	}
	logger.Info("test", "key", "value")

	output := h.logBuf.String()
	if !strings.Contains(output, `"key":"value"`) {
		t.Errorf("logger didn't produce output: %s", output)
	}
}

func TestAdminPutLogAccess_MissingEnabled(t *testing.T) {
	h := newAdminTestHarness()
	w := h.Do("PUT", "/api/admin/log-access", `{}`)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestAdminPutLogAccess_ExtraFields(t *testing.T) {
	h := newAdminTestHarness()
	w := h.Do("PUT", "/api/admin/log-access", `{"enabled":true,"extra":"field"}`)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestAdminPutLogAccess_NonBoolean(t *testing.T) {
	h := newAdminTestHarness()
	w := h.Do("PUT", "/api/admin/log-access", `{"enabled":"yes"}`)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestAdminPutLogAccess_InvalidJSON(t *testing.T) {
	h := newAdminTestHarness()
	w := h.Do("PUT", "/api/admin/log-access", `not-json`)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestAdminPutLogAccess_TrailingGarbage(t *testing.T) {
	h := newAdminTestHarness()
	w := h.Do("PUT", "/api/admin/log-access", `{"enabled":true}garbage`)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestAdminPutLogAccess_MissingContentType(t *testing.T) {
	h := newAdminTestHarness()
	r := httptest.NewRequest("PUT", "/api/admin/log-access", strings.NewReader(`{"enabled":true}`))
	// Don't set Content-Type
	w := httptest.NewRecorder()
	h.admin.logAccessHandler(w, r)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnsupportedMediaType)
	}
}

func TestAdminMethodNotAllowed(t *testing.T) {
	h := newAdminTestHarness()
	w := h.Do("POST", "/api/admin/log-access", `{}`)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}
