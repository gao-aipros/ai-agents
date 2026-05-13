package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/noodle05/ai-agents/cmd/webui/internal/env"
	"github.com/noodle05/ai-agents/cmd/webui/internal/request"
	"github.com/noodle05/ai-agents/tasklib"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[webui] ")

	cfg := request.DefaultConfig()
	port := env.String("WEBUI_PORT", "8000")

	// Redis connection
	redisHost := env.String("REDIS_HOST", "redis")
	redisPort := env.String("REDIS_PORT", "6379")
	rdb := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%s", redisHost, redisPort),
	})

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("redis connection failed: %v", err)
	}
	log.Printf("connected to redis at %s:%s", redisHost, redisPort)

	client := tasklib.NewClient(rdb)
	handler := request.New(client, cfg)

	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		workers, err := client.GetWorkerStats(context.Background())
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"redis":  "error",
				"detail": err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"redis":        "ok",
			"workers":      workers,
			"active_concurrent": handler.ActiveRequests(),
		})
	})

	// Worker stats
	mux.HandleFunc("GET /api/workers", func(w http.ResponseWriter, r *http.Request) {
		workers, err := client.GetWorkerStats(context.Background())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, workers)
	})

	// Submit request
	mux.HandleFunc("POST /api/requests", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
			return
		}

		var req struct {
			ThreadID string `json:"thread_id"`
			Repo     string `json:"repo"`
			Request  string `json:"request"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}

		if req.Request == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request field is required"})
			return
		}
		if len(req.Request) > 32*1024 {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request exceeds 32KB limit"})
			return
		}

		// Auto-generate thread_id if omitted
		threadID := req.ThreadID
		if threadID == "" {
			threadID = generateThreadID()
		}

		result, err := handler.Submit(r.Context(), threadID, req.Request, req.Repo)
		if err != nil {
			if re, ok := err.(*request.RequestError); ok {
				writeJSON(w, re.Status, re)
			} else {
				log.Printf("submit error: %v", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			}
			return
		}

		writeJSON(w, http.StatusAccepted, result)
	})

	// Cancel request
	mux.HandleFunc("POST /api/threads/{thread_id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		threadID := r.PathValue("thread_id")
		if err := handler.Cancel(threadID); err != nil {
			if re, ok := err.(*request.RequestError); ok {
				writeJSON(w, re.Status, re)
			} else {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
	})

	// Thread list (placeholder — expanded in Step 5)
	mux.HandleFunc("GET /api/threads", func(w http.ResponseWriter, r *http.Request) {
		threads, err := client.ListThreads(context.Background())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, threads)
	})

	// Thread detail
	mux.HandleFunc("GET /api/threads/{thread_id}", func(w http.ResponseWriter, r *http.Request) {
		threadID := r.PathValue("thread_id")
		thread, err := client.GetThread(context.Background(), threadID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, thread)
	})

	// Thread history
	mux.HandleFunc("GET /api/threads/{thread_id}/history", func(w http.ResponseWriter, r *http.Request) {
		threadID := r.PathValue("thread_id")
		msgs, err := client.GetThreadHistory(context.Background(), threadID, 0, 0)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, msgs)
	})

	// Task list (placeholder — expanded in Step 5)
	mux.HandleFunc("GET /api/tasks", func(w http.ResponseWriter, r *http.Request) {
		tasks, err := client.ListTasks(context.Background(), "", "", "", 50, 0)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, tasks)
	})

	// Root — placeholder redirect
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"service": "ai-agents webui",
			"version": "0.1.0",
		})
	})

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: withLogging(mux),
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		log.Printf("received signal %v, shutting down", sig)

		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
		defer cancel()

		// Cancel all in-flight claude subprocesses
		handler.Shutdown(shutdownCtx)

		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("listening on :%s (max concurrent requests: %d)", port, cfg.MaxConcurrent)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
	log.Printf("server stopped")
}

// ── helpers ────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func generateThreadID() string {
	b := make([]byte, 10)
	rand.Read(b)
	const chars = "0123456789abcdefghijklmnopqrstuvwxyz"
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return fmt.Sprintf("web_%d_%s", time.Now().Unix(), string(b))
}
