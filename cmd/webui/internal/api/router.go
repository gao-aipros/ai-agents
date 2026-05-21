package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/noodle05/ai-agents/cmd/webui/internal/request"
	"github.com/noodle05/ai-agents/cmd/webui/internal/static"
	"github.com/noodle05/ai-agents/cmd/webui/internal/templates"
	"github.com/noodle05/ai-agents/tasklib"
)

// NewRouter creates a chi router with all /api/ endpoints and page routes.
func NewRouter(client *tasklib.Client, handler *request.Handler, renderer *templates.Renderer, shutdownCtx context.Context, accessLog *atomic.Pointer[slog.Logger], adminKey string, newAccessLogger func() *slog.Logger) chi.Router {
	r := chi.NewRouter()

	// Middleware stack
	r.Use(sanitizeQueryMiddleware)
	r.Use(accessLogMiddleware(accessLog))
	r.Use(chimw.RealIP)
	r.Use(recoverMiddleware)
	r.Use(authMiddleware)
	r.Use(csrfMiddleware(renderer.CSRFToken))
	r.Use(contentTypeMiddleware)

	// Static file serving (CSS, JS)
	fileServer := http.FileServer(http.FS(static.FS))
	r.Handle("/static/*", http.StripPrefix("/static/", fileServer))

	// Page resources
	pages := &pageResource{client: client, handler: handler, renderer: renderer}

	// Page routes (full HTML pages)
	r.Get("/", pages.dashboard)
	r.Get("/threads", pages.threadList)
	r.Get("/threads/{thread_id}", pages.threadDetail)
	r.Get("/tasks", pages.taskList)
	r.Get("/tasks/{task_id}", pages.taskDetail)

	// Request form partial (for form reset after submission)
	r.Get("/api/requests/_form", pages.requestForm)

	r.Route("/api", func(r chi.Router) {
		sys := &systemResource{client: client, handler: handler}
		diag := &diagnosticsResource{client: client}
		evt := &eventsResource{client: client}
		wrk := &workersResource{client: client, renderer: renderer}
		req := &requestsResource{client: client, handler: handler, renderer: renderer}
		thr := &threadsResource{client: client, renderer: renderer}
		tsk := &tasksResource{client: client, renderer: renderer}

		// Health / stats / diagnostics / events / metrics
		r.Get("/health", sys.health)
		r.Get("/stats", sys.stats)
		r.Get("/diagnostics", diag.get)
		r.Get("/events", evt.systemEvents)
		r.Get("/metrics", newMetricsHandler(client).ServeHTTP)

		// Workers
		r.Get("/workers", wrk.list)
		r.Get("/workers/{worker_type}", wrk.get)
		r.Get("/workers/{worker_type}/instances", wrk.instances)

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
		r.Get("/threads/{thread_id}/events", evt.threadEvents)
		r.With(rateLimitMiddleware(defaultLimiter)).
			Delete("/threads/{thread_id}", thr.deleteThread)
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

		// Admin
		admin := &adminResource{accessLog: accessLog, newAccessLogger: newAccessLogger}
		r.With(adminAuthMiddleware(adminKey)).Get("/admin/log-access", admin.logAccessHandler)
		r.With(adminAuthMiddleware(adminKey)).Put("/admin/log-access", admin.logAccessHandler)
	})

	// Start rate limiter cleanup goroutines
	go requestsLimiter.cleanup(shutdownCtx)
	go threadsLimiter.cleanup(shutdownCtx)
	go defaultLimiter.cleanup(shutdownCtx)

	return r
}

// ── page resource (full HTML pages) ───────────────────────────────────────

type pageResource struct {
	client   *tasklib.Client
	handler  *request.Handler
	renderer *templates.Renderer
}

// GET /
func (pr *pageResource) dashboard(w http.ResponseWriter, r *http.Request) {
	Page(w, pr.renderer, "page-dashboard", nil)
}

// GET /threads
func (pr *pageResource) threadList(w http.ResponseWriter, r *http.Request) {
	threads, err := pr.client.ListThreads(r.Context())
	if err != nil {
		slog.Warn(fmt.Sprintf("[webui] thread list page error: %v", err))
		threads = nil
	}
	Page(w, pr.renderer, "page-thread-list", map[string]interface{}{
		"Threads": threads,
	})
}

// GET /threads/{thread_id}
func (pr *pageResource) threadDetail(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")

	if !request.ValidThreadID(threadID) {
		Page(w, pr.renderer, "page-thread-detail", map[string]interface{}{"Thread": nil})
		return
	}

	exists, err := pr.client.ThreadExists(r.Context(), threadID)
	if err != nil || !exists {
		Page(w, pr.renderer, "page-thread-detail", map[string]interface{}{
			"Thread": nil,
		})
		return
	}

	thread, err := pr.client.GetThread(r.Context(), threadID)
	if err != nil {
		slog.Warn(fmt.Sprintf("[webui] thread detail page error: %v", err))
		Page(w, pr.renderer, "page-thread-detail", map[string]interface{}{"Thread": nil})
		return
	}

	running, _ := pr.client.IsRequestRunning(r.Context(), threadID)
	complete, _ := pr.client.IsThreadComplete(r.Context(), threadID)

	Page(w, pr.renderer, "page-thread-detail", map[string]interface{}{
		"Thread":   thread,
		"Running":  running,
		"Complete": complete,
	})
}

// GET /tasks
func (pr *pageResource) taskList(w http.ResponseWriter, r *http.Request) {
	Page(w, pr.renderer, "page-task-list", nil)
}

// GET /tasks/{task_id}
func (pr *pageResource) taskDetail(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")

	task, err := pr.client.GetTask(r.Context(), taskID)
	if err != nil || task.Status == "" {
		Page(w, pr.renderer, "page-task-detail", map[string]interface{}{
			"Task": nil,
		})
		return
	}

	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	tailInfo := "full"
	if tail > 0 {
		tailInfo = fmt.Sprintf("last %d lines", tail)
	}

	// Fetch result if task is done
	if task.Result == "" && (task.Status == "done" || task.Status == "failed") {
		result, err := pr.client.GetTaskResult(r.Context(), taskID, tail)
		if err == nil {
			task.Result = result
		}
	}

	Page(w, pr.renderer, "page-task-detail", map[string]interface{}{
		"Task":     task,
		"TailInfo": tailInfo,
	})
}

// GET /api/requests/_form
func (pr *pageResource) requestForm(w http.ResponseWriter, r *http.Request) {
	Partial(w, pr.renderer, "request-form", nil)
}
