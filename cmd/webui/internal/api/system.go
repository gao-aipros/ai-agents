package api

import (
	"fmt"
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

// GET /api/stats — reads atomic counters for O(1) performance (no task scan).
func (sr *systemResource) stats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rdb := sr.client.RDB()

	// Read atomic counters via MGET
	counterKeys := []string{"stats:task_total", "stats:task_done", "stats:task_failed", "stats:task_cancelled"}
	vals, err := rdb.MGet(ctx, counterKeys...).Result()
	if err != nil {
		log.Printf("[webui] stats counters error: %v", err)
		Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	toInt := func(v interface{}) int {
		if v == nil {
			return 0
		}
		s, ok := v.(string)
		if !ok {
			log.Printf("[webui] stats counter: unexpected type %T for value %v", v, v)
			return 0
		}
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
			log.Printf("[webui] stats counter parse error for value %q: %v", s, err)
		}
		return n
	}
	total := toInt(vals[0])
	done := toInt(vals[1])
	failed := toInt(vals[2])
	cancelled := toInt(vals[3])

	// running = size of active_tasks hash
	running, err := rdb.HLen(ctx, "active_tasks").Result()
	if err != nil {
		log.Printf("[webui] stats: active_tasks HLen error: %v", err)
		running = 0
	}

	// Queue depths + pending count
	queueDepths := make(map[string]int64)
	var pending int64
	for _, workerType := range tasklib.WorkerTypes {
		dep, err := rdb.LLen(ctx, tasklib.QueueKey(workerType)).Result()
		if err != nil {
			dep = -1
		}
		queueDepths[workerType] = dep
		pending += dep
	}

	successRate := 0.0
	if done+failed > 0 {
		successRate = float64(done) / float64(done+failed) * 100
	}

	Respond(w, r, http.StatusOK, map[string]interface{}{
		"total_tasks":     total,
		"done":            done,
		"failed":          failed,
		"cancelled":       cancelled,
		"running":         int(running),
		"pending":         int(pending),
		"success_rate":     successRate,
		"avg_duration_sec": 0, // deprecated: removed in counter-based rewrite
		"queue_depths":     queueDepths,
		"active_requests":  sr.handler.ActiveRequests(),
	})
}
