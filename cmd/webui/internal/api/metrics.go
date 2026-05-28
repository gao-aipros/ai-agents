package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/noodle05/ai-agents/tasklib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// newMetricsHandler creates a Prometheus HTTP handler for GET /metrics.
// The registry and collector are created once per handler invocation.
func newMetricsHandler(sysOps tasklib.SystemOps, workers tasklib.WorkerRegistry, scanner tasklib.ThreadScanner) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(newRedisCollector(sysOps, workers, scanner))
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

// redisCollector reads current metric values from Redis on each scrape.
// Histogram data is in-memory and resets on restart (per design doc).
type redisCollector struct {
	sysOps  tasklib.SystemOps
	scanner tasklib.ThreadScanner
	workers tasklib.WorkerRegistry

	tasksTotal    *prometheus.Desc
	threadsActive *prometheus.Desc
	threadsStuck  *prometheus.Desc
	workersOnline *prometheus.Desc
	queueDepth    *prometheus.Desc
	tasksRunning  *prometheus.Desc
	tasksPending  *prometheus.Desc
	taskDuration  *prometheus.Desc
	queueWait     *prometheus.Desc
}

func newRedisCollector(sysOps tasklib.SystemOps, workers tasklib.WorkerRegistry, scanner tasklib.ThreadScanner) *redisCollector {
	labels := func(name, help string, variableLabels ...string) *prometheus.Desc {
		return prometheus.NewDesc("ai_agents_"+name, help, variableLabels, nil)
	}
	return &redisCollector{
		sysOps:        sysOps,
		scanner:       scanner,
		workers:       workers,
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

	// ── Counters from Redis atomic keys ──
	counterKeys := []string{"stats:task_done", "stats:task_failed", "stats:task_cancelled"}
	statuses := []string{"done", "failed", "cancelled"}
	vals, err := c.sysOps.GetCounters(ctx, counterKeys...)
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
	if n, err := c.sysOps.ActiveTaskCount(ctx); err == nil {
		ch <- prometheus.MustNewConstMetric(c.tasksRunning, prometheus.GaugeValue, float64(n))
	}

	var pending int64
	queueKeys, err := c.sysOps.ScanKeys(ctx, "tasks:queue:*", 100)
	if err != nil {
		slog.Warn("metrics: ScanKeys failed", "error", err)
	}
	for _, key := range queueKeys {
		dep, err := c.sysOps.QueueDepth(ctx, key)
		if err == nil {
			pending += dep
		}
	}
	ch <- prometheus.MustNewConstMetric(c.tasksPending, prometheus.GaugeValue, float64(pending))

	// ── Threads ──
	active, stuck := countActiveStuckThreads(ctx, c.scanner, tasklib.DefaultStuckThreshold)
	ch <- prometheus.MustNewConstMetric(c.threadsActive, prometheus.GaugeValue, float64(active))
	ch <- prometheus.MustNewConstMetric(c.threadsStuck, prometheus.GaugeValue, float64(stuck))

	// ── Worker online count ──
	if stats, err := c.workers.GetWorkerStats(ctx); err == nil {
		for wt, ws := range stats {
			ch <- prometheus.MustNewConstMetric(c.workersOnline, prometheus.GaugeValue, float64(ws.Online), wt)
		}
	} else {
		slog.Warn("metrics: GetWorkerStats failed", "error", err)
	}

	// ── Queue depth by worker type ──
	for _, key := range queueKeys {
		parts := strings.SplitN(key, ":", 3)
		if len(parts) < 3 {
			continue
		}
		workerName := parts[2]
		dep, err := c.sysOps.QueueDepth(ctx, key)
		if err != nil {
			dep = 0
		}
		ch <- prometheus.MustNewConstMetric(c.queueDepth, prometheus.GaugeValue, float64(dep), workerName)
	}
}

// countActiveStuckThreads scans thread state keys for active and stuck counts.
func countActiveStuckThreads(ctx context.Context, scanner tasklib.ThreadScanner, stuckThreshold time.Duration) (active, stuck int) {
	cutoff := time.Now().Add(-stuckThreshold)
	threads, err := scanner.Scan(ctx, func(ts tasklib.ThreadState) bool {
		return ts.Status != "complete" && ts.Status != "error" && ts.Status != "cancelled"
	})
	if err != nil {
		return
	}
	for _, ts := range threads {
		active++
		if ts.UpdatedAt != "" {
			if t, err := time.Parse("2006-01-02T15:04:05Z", ts.UpdatedAt); err == nil && t.Before(cutoff) {
				stuck++
			}
		}
	}
	return
}
