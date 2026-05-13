package api

import (
	"log"
	"net/http"

	"github.com/noodle05/ai-agents/cmd/webui/internal/request"
	"github.com/noodle05/ai-agents/tasklib"
)

type systemResource struct {
	client  *tasklib.Client
	handler *request.Handler
}

// GET /api/health
func (sr *systemResource) health(w http.ResponseWriter, r *http.Request) {
	err := sr.client.Ping(r.Context())
	if err != nil {
		Respond(w, r, http.StatusServiceUnavailable, map[string]string{
			"redis":  "error",
			"detail": err.Error(),
		})
		return
	}

	workers, _ := sr.client.GetWorkerStats(r.Context())

	Respond(w, r, http.StatusOK, map[string]interface{}{
		"redis":              "ok",
		"workers":            workers,
		"active_concurrent":  sr.handler.ActiveRequests(),
	})
}

// GET /api/stats
func (sr *systemResource) stats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Aggregate task stats
	tasks, err := sr.client.ListTasks(ctx, "", "", "", 0, 0)
	if err != nil {
		log.Printf("[webui] stats list tasks error: %v", err)
		Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	var total, done, failed, cancelled, running, pending int
	for _, t := range tasks {
		total++
		switch t.Status {
		case "done":
			done++
		case "failed":
			failed++
		case "cancelled":
			cancelled++
		case "running":
			running++
		case "pending":
			pending++
		}
	}

	// Queue depths
	queueDepths := make(map[string]int64)
	for _, workerType := range tasklib.WorkerTypes {
		dep, err := sr.client.RDB().LLen(ctx, tasklib.QueueKey(workerType)).Result()
		if err != nil {
			dep = -1
		}
		queueDepths[workerType] = dep
	}

	Respond(w, r, http.StatusOK, map[string]interface{}{
		"total_tasks":      total,
		"done":             done,
		"failed":           failed,
		"cancelled":        cancelled,
		"running":          running,
		"pending":          pending,
		"queue_depths":     queueDepths,
		"active_requests":  sr.handler.ActiveRequests(),
	})
}
