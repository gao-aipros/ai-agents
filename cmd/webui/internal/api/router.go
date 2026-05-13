package api

import (
	"context"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/noodle05/ai-agents/cmd/webui/internal/request"
	"github.com/noodle05/ai-agents/tasklib"
)

// NewRouter creates a chi router with all /api/ endpoints.
// shutdownCtx is cancelled on server shutdown to stop background goroutines
// (rate limiter cleanup tickers).
func NewRouter(client *tasklib.Client, handler *request.Handler, shutdownCtx context.Context) chi.Router {
	r := chi.NewRouter()

	// Middleware stack
	r.Use(chimw.Logger)
	r.Use(chimw.RealIP)
	r.Use(recoverMiddleware)
	r.Use(authMiddleware)
	r.Use(contentTypeMiddleware)

	r.Route("/api", func(r chi.Router) {
		sys := &systemResource{client: client, handler: handler}
		wrk := &workersResource{client: client}
		req := &requestsResource{client: client, handler: handler}
		thr := &threadsResource{client: client}
		tsk := &tasksResource{client: client}

		// Health / stats
		r.Get("/health", sys.health)
		r.Get("/stats", sys.stats)

		// Workers
		r.Get("/workers", wrk.list)
		r.Get("/workers/{worker_type}", wrk.get)

		// Requests — strict rate limits
		r.With(rateLimitMiddleware(requestsLimiter), maxBytesMiddleware(32*1024)).
			Post("/requests", req.submit)
		r.With(rateLimitMiddleware(defaultLimiter)).
			Post("/threads/{thread_id}/cancel", req.cancel)

		// Threads
		r.With(rateLimitMiddleware(threadsLimiter)).Post("/threads", thr.create)
		r.Get("/threads", thr.list)
		r.Get("/threads/{thread_id}", thr.get)
		r.Get("/threads/{thread_id}/history", thr.history)
		r.With(rateLimitMiddleware(defaultLimiter)).
			Delete("/threads/{thread_id}/workspace", thr.deleteWorkspace)
		r.With(rateLimitMiddleware(defaultLimiter)).
			Post("/threads/{thread_id}/keep", thr.keep)
		r.With(rateLimitMiddleware(defaultLimiter)).
			Post("/threads/{thread_id}/reset-session", thr.resetSession)

		// Tasks
		r.Get("/tasks", tsk.list)
		r.Get("/tasks/{task_id}", tsk.get)
		r.Get("/tasks/{task_id}/result", tsk.result)
	})

	// Start rate limiter cleanup goroutines (stop on shutdownCtx)
	go requestsLimiter.cleanup(shutdownCtx)
	go threadsLimiter.cleanup(shutdownCtx)
	go defaultLimiter.cleanup(shutdownCtx)

	return r
}
