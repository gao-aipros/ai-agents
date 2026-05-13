package api

import (
	"log"
	"net/http"
	"time"

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
			"detail": "redis unavailable",
		})
		return
	}

	workers, err := sr.client.GetWorkerStats(r.Context())
	if err != nil {
		log.Printf("[webui] health GetWorkerStats error: %v", err)
	}

	Respond(w, r, http.StatusOK, map[string]interface{}{
		"redis":              "ok",
		"workers":            workers,
		"active_concurrent":  sr.handler.ActiveRequests(),
	})
}

// GET /api/stats
func (sr *systemResource) stats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	tasks, err := sr.client.ListTasks(ctx, "", "", "", 0, 0)
	if err != nil {
		log.Printf("[webui] stats list tasks error: %v", err)
		Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	var total, done, failed, cancelled, running, pending int
	var totalDuration time.Duration
	var completedCount int

	for _, t := range tasks {
		total++
		switch t.Status {
		case "done":
			done++
			if t.CreatedAt != "" && t.CompletedAt != "" {
				if start, sErr := time.Parse("2006-01-02T15:04:05Z", t.CreatedAt); sErr == nil {
					if end, eErr := time.Parse("2006-01-02T15:04:05Z", t.CompletedAt); eErr == nil {
						totalDuration += end.Sub(start)
						completedCount++
					}
				}
			}
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

	successRate := 0.0
	if done+failed > 0 {
		successRate = float64(done) / float64(done+failed) * 100
	}

	avgDuration := 0.0
	if completedCount > 0 {
		avgDuration = totalDuration.Seconds() / float64(completedCount)
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
		"success_rate":     successRate,
		"avg_duration_sec": avgDuration,
		"queue_depths":     queueDepths,
		"active_requests":  sr.handler.ActiveRequests(),
	})
}
