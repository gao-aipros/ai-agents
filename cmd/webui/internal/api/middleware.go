package api

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ── auth ──────────────────────────────────────────────────────────────────

var apiKey string

func init() {
	apiKey = os.Getenv("WEBUI_API_KEY")
	if apiKey == "" {
		log.Printf("[webui] WARNING: WEBUI_API_KEY is not set — all /api/ endpoints are open (no auth)")
	}
}

// authMiddleware validates the Authorization: Bearer header when WEBUI_API_KEY
// is configured. Both read and write endpoints require auth.
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiKey == "" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			Error(w, http.StatusUnauthorized, "missing Authorization header")
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		if token != apiKey {
			Error(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── recovery ──────────────────────────────────────────────────────────────

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[webui] panic: %v", rec)
				Error(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ── csrf ──────────────────────────────────────────────────────────────────

// csrfMiddleware validates the X-CSRF-Token header for HTMX mutation requests.
// The token is compared against the Renderer's CSRFToken (generated at startup).
func csrfMiddleware(csrfToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead ||
				r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			if !IsHTMX(r) {
				next.ServeHTTP(w, r)
				return
			}
			if r.Header.Get("X-CSRF-Token") != csrfToken {
				Error(w, http.StatusForbidden, "invalid CSRF token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ── content-type ──────────────────────────────────────────────────────────

// contentTypeMiddleware requires application/json Content-Type (or HX-Request
// header) on mutation endpoints. GET/HEAD/OPTIONS requests are passed through.
func contentTypeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GET, HEAD, OPTIONS, and DELETE have no meaningful JSON body.
		if r.Method == http.MethodGet || r.Method == http.MethodHead ||
			r.Method == http.MethodOptions || r.Method == http.MethodDelete {
			next.ServeHTTP(w, r)
			return
		}
		if IsHTMX(r) {
			next.ServeHTTP(w, r)
			return
		}
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "application/json") {
			Error(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── rate limiting ─────────────────────────────────────────────────────────

type rateLimiter struct {
	mu       sync.Mutex
	windows  map[string][]time.Time
	limit    int
	interval time.Duration
}

// defaultLimiter is a moderate rate limit applied to all mutation endpoints
// that don't have a more specific limit (cancel, keep, reset-session, workspace).
var defaultLimiter = &rateLimiter{limit: 60, interval: time.Minute, windows: make(map[string][]time.Time)}

var (
	requestsLimiter = &rateLimiter{limit: 10, interval: time.Minute, windows: make(map[string][]time.Time)}
	threadsLimiter  = &rateLimiter{limit: 30, interval: time.Minute, windows: make(map[string][]time.Time)}
)

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.interval)

	window := rl.windows[ip]
	valid := window[:0]
	for _, t := range window {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	if len(valid) >= rl.limit {
		rl.windows[ip] = valid
		return false
	}
	rl.windows[ip] = append(valid, now)
	return true
}

// cleanup periodically removes expired entries from rate limiter maps.
// Exits when ctx is cancelled.
func (rl *rateLimiter) cleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			rl.mu.Lock()
			cutoff := time.Now().Add(-rl.interval)
			for ip, window := range rl.windows {
				valid := window[:0]
				for _, t := range window {
					if t.After(cutoff) {
						valid = append(valid, t)
					}
				}
				if len(valid) == 0 {
					delete(rl.windows, ip)
				} else {
					rl.windows[ip] = valid
				}
			}
			rl.mu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

// clientIP returns the client IP for rate limiting. It reads r.RemoteAddr,
// which has already been set by chimw.RealIP middleware (runs earlier in the
// stack). RealIP extracts the real client IP from X-Forwarded-For /
// X-Real-IP headers when the request comes from a trusted proxy, and falls
// back to the direct connection IP otherwise. This avoids trusting spoofed
// headers from non-proxy clients.
func clientIP(r *http.Request) string {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip == "" {
		return r.RemoteAddr
	}
	return ip
}

func rateLimitMiddleware(rl *rateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			if !rl.allow(ip) {
				w.Header().Set("Retry-After", fmt.Sprintf("%.0f", rl.interval.Seconds()))
				Error(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ── body size limit ───────────────────────────────────────────────────────

// maxBytesMiddleware limits the request body size. Use for specific routes.
func maxBytesMiddleware(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}
