package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/noodle05/ai-agents/tasklib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

// ── Metrics handler (GET /metrics) ──

func metricsHandler(client *tasklib.Client) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(newRedisCollector(client))
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

// redisCollector reads current metric values from Redis on each scrape.
// Histogram data is in-memory and resets on restart (per design doc).
type redisCollector struct {
	client *tasklib.Client

	tasksTotal   *prometheus.Desc
	threadsActive  *prometheus.Desc
	threadsStuck   *prometheus.Desc
	workersOnline  *prometheus.Desc
	queueDepth     *prometheus.Desc
	tasksRunning   *prometheus.Desc
	tasksPending   *prometheus.Desc
	taskDuration   *prometheus.Desc
	queueWait      *prometheus.Desc
}

func newRedisCollector(client *tasklib.Client) *redisCollector {
	labels := func(name, help string, variableLabels ...string) *prometheus.Desc {
		return prometheus.NewDesc("ai_agents_"+name, help, variableLabels, nil)
	}
	return &redisCollector{
		client:        client,
		tasksTotal:    labels("tasks_total", "Total tasks by status.", "status"),
		threadsActive: labels("threads_active", "Non-terminal threads."),
		threadsStuck:  labels("threads_stuck", "Active threads with no recent update."),
		workersOnline: labels("workers_online", "Online worker instances by type.", "worker_type"),
		queueDepth:    labels("queue_depth", "Tasks waiting in queue per worker type.", "worker_type"),
		tasksRunning:  labels("tasks_running", "Tasks currently executing."),
		tasksPending:  labels("tasks_pending", "Tasks waiting in all queues."),
		taskDuration:  labels("task_duration_seconds", "Task execution time (in-memory, resets on restart).", "worker_type"),
		queueWait:     labels("queue_wait_seconds", "Queue wait time (in-memory, resets on restart).", "worker_type"),
	}
}

func (c *redisCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.tasksTotal
	ch <- c.threadsActive
	ch <- c.threadsStuck
	ch <- c.workersOnline
	ch <- c.queueDepth
	ch <- c.tasksRunning
	ch <- c.tasksPending
	ch <- c.taskDuration
	ch <- c.queueWait
}

func (c *redisCollector) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()
	rdb := c.client.RDB()

	// ── Counters from Redis atomic keys ──
	counterKeys := []string{"stats:task_done", "stats:task_failed", "stats:task_cancelled"}
	statuses := []string{"done", "failed", "cancelled"}
	vals, err := rdb.MGet(ctx, counterKeys...).Result()
	if err != nil {
		slog.Warn("metrics: counter MGET failed", "error", err)
	} else {
		for i, st := range statuses {
			if vals[i] == nil {
				continue
			}
			if s, ok := vals[i].(string); ok {
				if n, err := strconv.ParseFloat(s, 64); err == nil {
					ch <- prometheus.MustNewConstMetric(c.tasksTotal, prometheus.CounterValue, n, st)
				}
			}
		}
	}

	// ── Running / pending ──
	if n, err := rdb.HLen(ctx, "active_tasks").Result(); err == nil {
		ch <- prometheus.MustNewConstMetric(c.tasksRunning, prometheus.GaugeValue, float64(n))
	}

	var pending int64
	for _, wt := range tasklib.WorkerTypes {
		if dep, err := rdb.LLen(ctx, tasklib.QueueKey(wt)).Result(); err == nil {
			pending += dep
		}
	}
	ch <- prometheus.MustNewConstMetric(c.tasksPending, prometheus.GaugeValue, float64(pending))

	// ── Threads ──
	active, stuck := countActiveStuckThreads(ctx, rdb)
	ch <- prometheus.MustNewConstMetric(c.threadsActive, prometheus.GaugeValue, float64(active))
	ch <- prometheus.MustNewConstMetric(c.threadsStuck, prometheus.GaugeValue, float64(stuck))

	// ── Worker online count ──
	if stats, err := c.client.GetWorkerStats(ctx); err == nil {
		for wt, ws := range stats {
			ch <- prometheus.MustNewConstMetric(c.workersOnline, prometheus.GaugeValue, float64(ws.Online), wt)
		}
	}

	// ── Queue depth by worker type ──
	for _, wt := range tasklib.WorkerTypes {
		dep, err := rdb.LLen(ctx, tasklib.QueueKey(wt)).Result()
		if err != nil {
			dep = 0
		}
		ch <- prometheus.MustNewConstMetric(c.queueDepth, prometheus.GaugeValue, float64(dep), wt)
	}
}

// countActiveStuckThreads scans thread state keys.
func countActiveStuckThreads(ctx context.Context, rdb *redis.Client) (active, stuck int) {
	var cursor uint64
	cutoff := time.Now().Add(-30 * time.Minute)
	for {
		keys, nextCursor, err := rdb.Scan(ctx, cursor, "thread:*:current_state", 100).Result()
		if err != nil {
			return
		}
		for _, key := range keys {
			status, err := rdb.HGet(ctx, key, "status").Result()
			if err != nil {
				continue
			}
			if status != "complete" && status != "error" && status != "cancelled" {
				active++
				if updatedAt, err := rdb.HGet(ctx, key, "updated_at").Result(); err == nil && updatedAt != "" {
					if t, err := time.Parse("2006-01-02T15:04:05Z", updatedAt); err == nil && t.Before(cutoff) {
						stuck++
					}
				}
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return
}
