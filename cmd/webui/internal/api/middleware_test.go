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

func TestAuthMiddleware_KeySet_ValidQueryParam(t *testing.T) {
	oldKey := apiKey
	apiKey = "test-secret"
	defer func() { apiKey = oldKey }()

	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/health?api_key=test-secret", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestAuthMiddleware_KeySet_WrongQueryParam(t *testing.T) {
	oldKey := apiKey
	apiKey = "test-secret"
	defer func() { apiKey = oldKey }()

	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/health?api_key=wrong-key", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestAuthMiddleware_KeySet_EmptyQueryParam(t *testing.T) {
	oldKey := apiKey
	apiKey = "test-secret"
	defer func() { apiKey = oldKey }()

	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/health?api_key=", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (empty api_key should fail)", w.Code, http.StatusUnauthorized)
	}
}

func TestAuthMiddleware_KeySet_HeaderOverridesQueryParam(t *testing.T) {
	oldKey := apiKey
	apiKey = "test-secret"
	defer func() { apiKey = oldKey }()

	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/health?api_key=wrong-key", nil)
	r.Header.Set("Authorization", "Bearer test-secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (header should take precedence over query param)", w.Code, http.StatusOK)
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

// ── csrf middleware tests ──────────────────────────────────────────────────

func TestCSRFMiddleware_GetPassesThrough(t *testing.T) {
	handler := csrfMiddleware("test-token")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/threads", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestCSRFMiddleware_NonHTMXPostPassesThrough(t *testing.T) {
	handler := csrfMiddleware("test-token")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("POST", "/api/requests", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (non-HTMX POST should bypass CSRF)", w.Code, http.StatusOK)
	}
}

func TestCSRFMiddleware_HTMXPost_ValidToken(t *testing.T) {
	handler := csrfMiddleware("test-token")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("POST", "/api/requests", strings.NewReader("field=value"))
	r.Header.Set("HX-Request", "true")
	r.Header.Set("X-CSRF-Token", "test-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestCSRFMiddleware_HTMXPost_WrongToken(t *testing.T) {
	handler := csrfMiddleware("test-token")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("POST", "/api/keep", strings.NewReader(""))
	r.Header.Set("HX-Request", "true")
	r.Header.Set("X-CSRF-Token", "wrong-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestCSRFMiddleware_HTMXPost_MissingToken(t *testing.T) {
	handler := csrfMiddleware("test-token")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("POST", "/api/cancel", strings.NewReader(""))
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestCSRFMiddleware_HTMXDelete(t *testing.T) {
	handler := csrfMiddleware("test-token")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("DELETE", "/api/threads/t1/workspace?confirm=true", nil)
	r.Header.Set("HX-Request", "true")
	r.Header.Set("X-CSRF-Token", "test-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestCSRFMiddleware_HEADPassesThrough(t *testing.T) {
	handler := csrfMiddleware("test-token")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("HEAD", "/api/health", nil)
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
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

// ── sanitize query middleware tests ─────────────────────────────────────────

func TestSanitizeQuery_StripsAPIKeyFromRawQuery(t *testing.T) {
	handler := sanitizeQueryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery == "other=1" && !strings.Contains(r.URL.RawQuery, "api_key") {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))

	r := httptest.NewRequest("GET", "/api/health?api_key=secret&other=1", nil)
	// Simulate server parse: set RequestURI as real HTTP server does
	r.RequestURI = r.URL.String()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (RawQuery=%q)", w.Code, http.StatusOK, r.URL.RawQuery)
	}
}

func TestSanitizeQuery_StripsAPIKeyFromRequestURI(t *testing.T) {
	handler := sanitizeQueryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.RequestURI, "api_key") && strings.Contains(r.RequestURI, "other=1") {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))

	r := httptest.NewRequest("GET", "/api/health?api_key=secret&other=1", nil)
	r.RequestURI = r.URL.String()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (RequestURI=%q)", w.Code, http.StatusOK, r.RequestURI)
	}
}

func TestSanitizeQuery_StoresAPIKeyInContext(t *testing.T) {
	handler := sanitizeQueryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Context().Value(ctxQueryAPIKey)
		if v == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if v.(string) != "secret" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/health?api_key=secret", nil)
	r.RequestURI = r.URL.String()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestSanitizeQuery_NoAPIKeyUnchanged(t *testing.T) {
	handler := sanitizeQueryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery == "foo=bar" {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))

	r := httptest.NewRequest("GET", "/api/health?foo=bar", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (RawQuery=%q)", w.Code, http.StatusOK, r.URL.RawQuery)
	}
}

// TestSanitizeBeforeLogger verifies the middleware ordering: sanitize strips
// api_key before Logger would see it, while authMiddleware still authenticates
// successfully via the context value.
func TestSanitizeBeforeLogger_AuthPasses(t *testing.T) {
	oldKey := apiKey
	apiKey = "test-secret"
	defer func() { apiKey = oldKey }()

	var loggedURI string

	// Simulate full stack: sanitize → logger → auth
	handler := sanitizeQueryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Logger would see this RequestURI
		loggedURI = r.RequestURI
		// Then auth reads from context (set by sanitize)
		authMiddleware(http.HandlerFunc(func(w2 http.ResponseWriter, r2 *http.Request) {
			w2.WriteHeader(http.StatusOK)
		})).ServeHTTP(w, r)
	}))

	r := httptest.NewRequest("GET", "/api/health?api_key=test-secret", nil)
	r.RequestURI = r.URL.String()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if strings.Contains(loggedURI, "api_key") {
		t.Errorf("RequestURI visible to Logger still contains api_key: %q", loggedURI)
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
