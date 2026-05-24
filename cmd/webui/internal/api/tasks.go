package api

import (
	"net/http"
	"strconv"

	"github.com/noodle05/ai-agents/cmd/webui/internal/templates"
	"github.com/noodle05/ai-agents/tasklib"
)

type tasksResource struct {
	client   *tasklib.Client
	renderer *templates.Renderer
}

// GET /api/tasks
func (tr *tasksResource) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	worker := q.Get("worker")
	status := q.Get("status")
	threadID := q.Get("thread_id")
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if limit <= 0 {
		limit = 50
	}

	sortBy := q.Get("sort_by")
	sortDir := q.Get("sort_dir")

	tasks, err := tr.client.ListTasks(r.Context(), worker, status, threadID, limit, offset, sortBy, sortDir)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	if IsHTMX(r) {
		Partial(w, tr.renderer, "task-table", map[string]interface{}{
			"Tasks":    tasks,
			"HasTokens": tasklib.HasAnyTaskTokens(tasks),
			"SortBy":   sortBy,
			"SortDir":  sortDir,
			"Worker":   worker,
			"Status":   status,
			"ThreadID": threadID,
		})
	} else {
		Respond(w, r, http.StatusOK, tasks)
	}
}

// GET /api/tasks/{task_id}
func (tr *tasksResource) get(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")
	task, err := tr.client.GetTask(r.Context(), taskID)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	if task.Status == "" {
		Error(w, http.StatusNotFound, "task not found")
		return
	}
	Respond(w, r, http.StatusOK, task)
}

// GET /api/tasks/{task_id}/result
func (tr *tasksResource) result(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))

	task, err := tr.client.GetTask(r.Context(), taskID)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	if task.Status == "" {
		Error(w, http.StatusNotFound, "task not found")
		return
	}

	result, err := tr.client.GetTaskResult(r.Context(), taskID, tail)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	Respond(w, r, http.StatusOK, map[string]string{"task_id": taskID, "result": result})
}
