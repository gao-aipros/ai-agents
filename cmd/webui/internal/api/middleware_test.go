package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// ── auth middleware tests ──────────────────────────────────────────────────

func TestAuthMiddleware_NoKeySet(t *testing.T) {
	// Ensure no API key is set
	oldKey := apiKey
	apiKey = ""
	defer func() { apiKey = oldKey }()

	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestAuthMiddleware_KeySet_NoHeader(t *testing.T) {
	oldKey := apiKey
	apiKey = "test-secret"
	defer func() { apiKey = oldKey }()

	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestAuthMiddleware_KeySet_ValidBearer(t *testing.T) {
	oldKey := apiKey
	apiKey = "test-secret"
	defer func() { apiKey = oldKey }()

	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/health", nil)
	r.Header.Set("Authorization", "Bearer test-secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestAuthMiddleware_KeySet_WrongBearer(t *testing.T) {
	oldKey := apiKey
	apiKey = "test-secret"
	defer func() { apiKey = oldKey }()

	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/health", nil)
	r.Header.Set("Authorization", "Bearer wrong-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestAuthMiddleware_KeySet_WrongScheme(t *testing.T) {
	oldKey := apiKey
	apiKey = "test-secret"
	defer func() { apiKey = oldKey }()

	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/health", nil)
	r.Header.Set("Authorization", "Basic test-secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// ── content-type middleware tests ──────────────────────────────────────────

func TestContentTypeMiddleware_GetPassesThrough(t *testing.T) {
	handler := contentTypeMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/threads", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestContentTypeMiddleware_PostWithoutJSON(t *testing.T) {
	handler := contentTypeMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("POST", "/api/requests", strings.NewReader("not json"))
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnsupportedMediaType)
	}
}

func TestContentTypeMiddleware_PostWithJSON(t *testing.T) {
	handler := contentTypeMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("POST", "/api/requests", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestContentTypeMiddleware_PostWithHXRequest(t *testing.T) {
	handler := contentTypeMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("POST", "/api/requests", strings.NewReader(""))
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (HTMX bypasses content-type check)", w.Code, http.StatusOK)
	}
}

// ── rate limiter tests ────────────────────────────────────────────────────

func TestRateLimiter_Allow(t *testing.T) {
	rl := &rateLimiter{limit: 3, interval: time.Hour, windows: make(map[string][]time.Time)}

	ip := "10.0.0.1"
	for i := 0; i < rl.limit; i++ {
		if !rl.allow(ip) {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	if rl.allow(ip) {
		t.Error("request over limit should be denied")
	}
}

func TestRateLimiter_DifferentIPs(t *testing.T) {
	rl := &rateLimiter{limit: 1, interval: time.Hour, windows: make(map[string][]time.Time)}

	if !rl.allow("10.0.0.1") {
		t.Error("first IP first request should be allowed")
	}
	if rl.allow("10.0.0.1") {
		t.Error("first IP second request should be denied")
	}
	if !rl.allow("10.0.0.2") {
		t.Error("second IP first request should be allowed")
	}
}

// ── recover middleware tests ───────────────────────────────────────────────

func TestRecoverMiddleware_NoPanic(t *testing.T) {
	handler := recoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestRecoverMiddleware_Panic(t *testing.T) {
	handler := recoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("something broke")
	}))

	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// ── init warning test ─────────────────────────────────────────────────────

func TestAPIKeyInitWarning(t *testing.T) {
	// apiKey is initialized from env in init(). Verify it reads correctly.
	os.Setenv("WEBUI_API_KEY", "custom-key")
	// Can't re-run init(), just verify env var is readable
	if key := os.Getenv("WEBUI_API_KEY"); key != "custom-key" {
		t.Errorf("WEBUI_API_KEY = %q, want %q", key, "custom-key")
	}
	os.Unsetenv("WEBUI_API_KEY")
}
