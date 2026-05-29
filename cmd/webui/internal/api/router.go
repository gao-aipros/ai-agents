package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
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
func NewRouter(services *tasklib.Services, handler *request.Handler, renderer *templates.Renderer, shutdownCtx context.Context, accessLog *atomic.Pointer[slog.Logger], newAccessLogger func() *slog.Logger, mwCfg MiddlewareConfig) chi.Router {
	r := chi.NewRouter()

	// Middleware stack (all middleware must be registered before any routes in chi)
	r.Use(sanitizeQueryMiddleware)
	r.Use(accessLogMiddleware(accessLog))
	r.Use(chimw.RealIP)
	r.Use(recoverMiddleware)
	r.Use(authMiddleware(mwCfg.AuthKey))
	r.Use(csrfMiddleware(renderer.CSRFToken))
	r.Use(contentTypeMiddleware)

	// Static file serving (CSS, JS)
	fileServer := http.FileServer(http.FS(static.FS))
	r.Handle("/static/*", http.StripPrefix("/static/", fileServer))

	// Page resources
	pages := &pageResource{tasks: services.Tasks, threads: services.Threads, requests: services.Requests, handler: handler, renderer: renderer, workers: services.Workers}

	// Page routes (full HTML pages)
	r.Get("/", pages.dashboard)
	r.Get("/threads", pages.threadList)
	r.Get("/threads/{thread_id}", pages.threadDetail)
	r.Get("/tasks", pages.taskList)
	r.Get("/tasks/{task_id}", pages.taskDetail)

	// Request form partial (for form reset after submission)
	r.Get("/api/requests/_form", pages.requestForm)

	r.Route("/api", func(r chi.Router) {
		sys := &systemResource{sysOps: services.SysOps, workers: services.Workers, handler: handler}
		diag := &diagnosticsResource{sysOps: services.SysOps, scanner: services.Scanner}
		evt := &eventsResource{events: services.Events}
		wrk := &workersResource{workers: services.Workers, renderer: renderer}
		req := &requestsResource{requests: services.Requests, handler: handler, renderer: renderer}
		thr := &threadsResource{threads: services.Threads, requests: services.Requests, threadHistory: services.History, tasks: services.Tasks, tokens: services.Tokens, renderer: renderer, paths: mwCfg.Paths}
		tsk := &tasksResource{tasks: services.Tasks, renderer: renderer}
		tok := &tokensResource{tokens: services.Tokens, sysOps: services.SysOps, renderer: renderer}

		// Health / stats / diagnostics / events / metrics
		r.Get("/health", sys.health)
		r.Get("/stats", sys.stats)
		r.Get("/diagnostics", diag.get)
		r.Get("/events", evt.systemEvents)
		r.Get("/metrics", newMetricsHandler(services.SysOps, services.Workers, services.Scanner).ServeHTTP)

		// Workers
		r.Get("/workers", wrk.list)
		r.Get("/workers/{worker_name}", wrk.get)
		r.Get("/workers/{worker_name}/instances", wrk.instances)

		// Requests — strict rate limits
		r.With(rateLimitMiddleware(mwCfg.RequestsLimiter), maxBytesMiddleware(32*1024)).
			Post("/requests", req.submit)
		r.With(rateLimitMiddleware(mwCfg.DefaultLimiter)).
			Post("/threads/{thread_id}/cancel", req.cancel)

		// Threads
		r.With(rateLimitMiddleware(mwCfg.ThreadsLimiter)).Post("/threads", thr.create)
		r.Get("/threads", thr.list)
		r.Get("/threads/{thread_id}", thr.get)
		r.Get("/threads/{thread_id}/history", thr.history)
		r.Get("/threads/{thread_id}/events", evt.threadEvents)
		r.With(rateLimitMiddleware(mwCfg.DefaultLimiter)).
			Delete("/threads/{thread_id}", thr.deleteThread)
		r.With(rateLimitMiddleware(mwCfg.DefaultLimiter)).
			Delete("/threads/{thread_id}/workspace", thr.deleteWorkspace)
		r.With(rateLimitMiddleware(mwCfg.DefaultLimiter)).
			Post("/threads/{thread_id}/keep", thr.keep)
		r.With(rateLimitMiddleware(mwCfg.DefaultLimiter)).
			Post("/threads/{thread_id}/reset-session", thr.resetSession)

		// Tasks
		r.Get("/tasks", tsk.list)
		r.Get("/tasks/{task_id}", tsk.get)
		r.Get("/tasks/{task_id}/result", tsk.result)

		// Token stats
		r.With(rateLimitMiddleware(mwCfg.DefaultLimiter)).Get("/tokens", tok.globalTokens)

		// Admin (gated by adminAuthMiddleware; authMiddleware skips /api/admin/ paths)
		admin := &adminResource{accessLog: accessLog, newAccessLogger: newAccessLogger}
		r.With(adminAuthMiddleware(mwCfg.AdminKey)).Get("/admin/log-access", admin.logAccessHandler)
		r.With(adminAuthMiddleware(mwCfg.AdminKey)).Put("/admin/log-access", admin.logAccessHandler)
	})

	// Start rate limiter cleanup goroutines
	go mwCfg.RequestsLimiter.cleanup(shutdownCtx)
	go mwCfg.ThreadsLimiter.cleanup(shutdownCtx)
	go mwCfg.DefaultLimiter.cleanup(shutdownCtx)

	return r
}

// ── page resource (full HTML pages) ───────────────────────────────────────

type pageResource struct {
	tasks    tasklib.TaskStore
	threads  tasklib.ThreadStore
	requests tasklib.RequestStore
	handler  *request.Handler
	renderer *templates.Renderer
	workers  tasklib.WorkerRegistry
}

// GET /
func (pr *pageResource) dashboard(w http.ResponseWriter, r *http.Request) {
	Page(w, pr.renderer, "page-dashboard", &templates.DashboardView{})
}

// GET /threads
func (pr *pageResource) threadList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sortBy := q.Get("sort_by")
	sortDir := q.Get("sort_dir")
	threads, err := pr.threads.ListThreads(r.Context(), sortBy, sortDir)
	if err != nil {
		slog.Warn(fmt.Sprintf("[webui] thread list page error: %v", err))
		threads = nil
	}
	children := buildThreadTree(threads)
	rootThreads := filterRootThreads(threads)
	Page(w, pr.renderer, "page-thread-list", &templates.ThreadListView{
		Threads:  rootThreads,
		Children: children,
		SortBy:   sortBy,
		SortDir:  sortDir,
	})
}

// GET /threads/{thread_id}
func (pr *pageResource) threadDetail(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")

	if !request.ValidThreadID(threadID) {
		Page(w, pr.renderer, "page-thread-detail", &templates.ThreadDetailView{})
		return
	}

	exists, err := pr.threads.ThreadExists(r.Context(), threadID)
	if err != nil || !exists {
		Page(w, pr.renderer, "page-thread-detail", &templates.ThreadDetailView{})
		return
	}

	thread, err := pr.threads.GetThread(r.Context(), threadID)
	if err != nil {
		slog.Warn(fmt.Sprintf("[webui] thread detail page error: %v", err))
		Page(w, pr.renderer, "page-thread-detail", &templates.ThreadDetailView{})
		return
	}

	running, _ := pr.requests.IsRequestRunning(r.Context(), threadID)
	complete, _ := pr.threads.IsThreadComplete(r.Context(), threadID)

	// Find child threads
	allThreads, _ := pr.threads.ListThreads(r.Context(), "", "")
	var children []*tasklib.Thread
	for _, t := range allThreads {
		if t.ParentThreadID == threadID {
			children = append(children, t)
		}
	}

	// Fetch tasks for this thread
	tasks, _ := pr.tasks.ListTasks(r.Context(), "", "", threadID, 500, 0, "enqueued_at", "desc")

	Page(w, pr.renderer, "page-thread-detail", &templates.ThreadDetailView{
		Thread:   thread,
		Running:  running,
		Complete: complete,
		Tasks:    tasks,
		Children: children,
	})
}

// GET /tasks
func (pr *pageResource) taskList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var workerNames []string
	if stats, err := pr.workers.GetWorkerStats(r.Context()); err == nil {
		for name := range stats {
			workerNames = append(workerNames, name)
		}
	}
	sort.Strings(workerNames)
	Page(w, pr.renderer, "page-task-list", &templates.TaskListView{
		Workers: workerNames,
		SortBy:  q.Get("sort_by"),
		SortDir: q.Get("sort_dir"),
	})
}

// GET /tasks/{task_id}
func (pr *pageResource) taskDetail(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")

	task, err := pr.tasks.GetTask(r.Context(), taskID)
	if err != nil || task.Status == "" {
		Page(w, pr.renderer, "page-task-detail", &templates.TaskDetailView{})
		return
	}

	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	tailInfo := "full"
	if tail > 0 {
		tailInfo = fmt.Sprintf("last %d lines", tail)
	}

	// Fetch result if task is done
	if task.Result == "" && (task.Status == "done" || task.Status == "failed") {
		result, err := pr.tasks.GetTaskResult(r.Context(), taskID, tail)
		if err == nil {
			task.Result = result
		}
	}

	Page(w, pr.renderer, "page-task-detail", &templates.TaskDetailView{
		Task:     task,
		TailInfo: tailInfo,
	})
}

// GET /api/requests/_form
func (pr *pageResource) requestForm(w http.ResponseWriter, r *http.Request) {
	Partial(w, pr.renderer, "request-form", nil)
}
