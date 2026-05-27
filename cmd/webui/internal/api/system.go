package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/noodle05/ai-agents/cmd/webui/internal/request"
	"github.com/noodle05/ai-agents/tasklib"
	"github.com/redis/go-redis/v9"
)

type systemResource struct {
	rdb     *redis.Client
	workers tasklib.WorkerRegistry
	handler *request.Handler
}

// GET /api/health
func (sr *systemResource) health(w http.ResponseWriter, r *http.Request) {
	err := sr.rdb.Ping(r.Context()).Err()
	if err != nil {
		Respond(w, r, http.StatusServiceUnavailable, map[string]string{
			"redis":  "error",
			"detail": "redis unavailable",
		})
		return
	}

	workers, err := sr.workers.GetWorkerStats(r.Context())
	if err != nil {
		slog.Warn(fmt.Sprintf("[webui] health GetWorkerStats error: %v", err))
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
	rdb := sr.rdb

	// Read atomic counters via MGET
	counterKeys := []string{"stats:task_total", "stats:task_done", "stats:task_failed", "stats:task_cancelled"}
	vals, err := rdb.MGet(ctx, counterKeys...).Result()
	if err != nil {
		slog.Warn(fmt.Sprintf("[webui] stats counters error: %v", err))
		Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	toInt := func(v interface{}) (int, bool) {
		if v == nil {
			return 0, true // key not set yet, valid zero
		}
		s, ok := v.(string)
		if !ok {
			slog.Warn("stats counter: unexpected type", "type", fmt.Sprintf("%T", v), "value", v)
			return 0, false
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			slog.Warn("stats counter parse error", "value", s, "error", err)
			return 0, false
		}
		return n, true
	}
	total, totalOK := toInt(vals[0])
	done, doneOK := toInt(vals[1])
	failed, failedOK := toInt(vals[2])
	cancelled, cancelledOK := toInt(vals[3])
	countersOK := totalOK && doneOK && failedOK && cancelledOK

	// running = size of active_tasks hash
	running, err := rdb.HLen(ctx, "active_tasks").Result()
	if err != nil {
		slog.Warn(fmt.Sprintf("[webui] stats: active_tasks HLen error: %v", err))
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
	if countersOK && done+failed > 0 {
		successRate = float64(done) / float64(done+failed) * 100
	}

	resp := map[string]interface{}{
		"done":                 done,
		"failed":               failed,
		"cancelled":            cancelled,
		"running":              int(running),
		"pending":              int(pending),
		"avg_duration_sec":     nil,
		"queue_depths":         queueDepths,
		"active_requests":      sr.handler.ActiveRequests(),
	}
	if countersOK {
		resp["total_tasks"] = total
		resp["tasks_enqueued_ever"] = total
		resp["success_rate"] = successRate
	}
	Respond(w, r, http.StatusOK, resp)

}
