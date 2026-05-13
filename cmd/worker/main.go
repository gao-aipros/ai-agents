package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/noodle05/ai-agents/tasklib"
)

// errAgentTimeout is returned by execCommand when the agent subprocess exceeds
// its deadline. Exported as a sentinel so tests can simulate timeouts without
// creating real context deadlines.
var errAgentTimeout = errors.New("agent timed out")

// execCommand runs an agent subprocess and returns its output. Replaced in
// tests to mock subprocess execution.
var execCommand = func(ctx context.Context, args []string, dir string) (stdout, stderr string, exitCode int, err error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", "", -1, errAgentTimeout
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return outBuf.String(), errBuf.String(), exitErr.ExitCode(), nil
		}
		return outBuf.String(), errBuf.String(), -1, runErr
	}
	return outBuf.String(), errBuf.String(), 0, nil
}

func main() {
	if len(os.Args) < 2 {
		die("usage: worker <claude|copilot|opencode>")
	}
	workerType := os.Args[1]
	if !validWorker(workerType) {
		die("invalid worker type: " + workerType)
	}

	log := &logger{worker: workerType}

	redisHost := envDefault("REDIS_HOST", "redis")
	redisPort := envIntDefault("REDIS_PORT", 6379)
	agentCmd := os.Getenv("AGENT_CMD")
	if agentCmd == "" {
		die("AGENT_CMD not set")
	}
	taskTimeout := envIntDefault("TASK_TIMEOUT", 1800)
	historyWindow := envIntDefault("HISTORY_WINDOW", 10)
	workspaceDir := envDefault("WORKSPACE_DIR", "/workspace")
	homeDir := envDefault("HOME", "/home/agent")

	rdb := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", redisHost, redisPort),
	})
	client := tasklib.NewClient(rdb)

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "worker"
	}

	queueKey := tasklib.QueueKey(workerType)
	processingKey := tasklib.ProcessingKey(workerType)

	var running atomic.Bool
	running.Store(true)

	// Heartbeat goroutine
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for running.Load() {
			<-ticker.C
			if !running.Load() {
				return
			}
			if err := client.UpdateWorkerHeartbeat(context.Background(), workerType, hostname); err != nil {
				log.log("warn", "heartbeat failed", "error", err.Error())
			}
		}
	}()

	// Signal handler — stops the main loop but lets the current task finish.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.log("info", "received signal", "signal", int(sig.(syscall.Signal)))
		running.Store(false)
	}()

	log.log("info", "worker started", "queue", queueKey, "agent_cmd", agentCmd)

	for running.Load() {
		result, err := rdb.BLMove(context.Background(), queueKey, processingKey, "RIGHT", "LEFT", 5*time.Second).Result()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			if strings.Contains(err.Error(), "connection") ||
				strings.Contains(err.Error(), "EOF") ||
				strings.Contains(err.Error(), "broken pipe") {
				log.log("warn", "redis connection lost, reconnecting")
				time.Sleep(1 * time.Second)
			}
			if !running.Load() {
				break
			}
			continue
		}

		processOneTask(log, client, rdb, result, workerType, agentCmd,
			taskTimeout, historyWindow, workspaceDir, homeDir, processingKey, hostname)
	}

	log.log("info", "worker shutting down")
}

// ── task processing ───────────────────────────────────────────────────────────

func processOneTask(
	log *logger,
	client *tasklib.Client,
	rdb *redis.Client,
	taskJSON, workerType, agentCmd string,
	defaultTimeout, defaultHistoryWindow int,
	workspaceDir, homeDir, processingKey, hostname string,
) {
	var taskPayload struct {
		TaskID        string `json:"task_id"`
		ThreadID      string `json:"thread_id"`
		Instruction   string `json:"instruction"`
		HistoryWindow int    `json:"history_window"`
		Timeout       int    `json:"timeout"`
	}
	if err := json.Unmarshal([]byte(taskJSON), &taskPayload); err != nil {
		log.log("warn", "malformed task payload, removing from processing")
		rdb.LRem(context.Background(), processingKey, 0, taskJSON)
		return
	}

	taskID := taskPayload.TaskID
	threadID := taskPayload.ThreadID
	instruction := taskPayload.Instruction
	startedAt := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	log.log("info", "task dequeued", "task_id", taskID, "thread_id", threadID)

	// Register in active_tasks
	client.SetActiveTask(context.Background(), taskID, tasklib.TaskInfo{
		Status:     "running",
		Worker:     workerType,
		ThreadID:   threadID,
		StartedAt:  startedAt,
		WorkerHost: hostname,
	})

	// Initialize per-task keys
	pipe := rdb.Pipeline()
	pipe.Set(context.Background(), tasklib.TaskKey(taskID, "status"), "running", tasklib.TTLTask)
	pipe.Set(context.Background(), tasklib.TaskKey(taskID, "worker"), workerType, tasklib.TTLTask)
	pipe.Set(context.Background(), tasklib.TaskKey(taskID, "thread_id"), threadID, tasklib.TTLTask)
	pipe.Set(context.Background(), tasklib.TaskKey(taskID, "description"), instruction, tasklib.TTLTask)
	pipe.Set(context.Background(), tasklib.TaskKey(taskID, "created_at"), startedAt, tasklib.TTLTask)
	pipe.Exec(context.Background())

	// Build prompt with thread context
	window := defaultHistoryWindow
	if taskPayload.HistoryWindow > 0 {
		window = taskPayload.HistoryWindow
	}

	// Read thread history
	msgs, _ := client.GetThreadHistoryTail(context.Background(), threadID, window)
	var contextBuilder strings.Builder
	if len(msgs) > 0 {
		contextBuilder.WriteString("## Thread History (recent)\n\n")
		for _, msg := range msgs {
			role := msg.Role
			if role == "" {
				role = "unknown"
			}
			fmt.Fprintf(&contextBuilder, "[%s]: %s\n\n", role, msg.Content)
		}
	}

	// Read thread state
	thread, _ := client.GetThread(context.Background(), threadID)
	if thread != nil {
		contextBuilder.WriteString("\n## Current State\n")
		fmt.Fprintf(&contextBuilder, "status: %s\n", thread.Status)
		if thread.LastDesign != "" {
			fmt.Fprintf(&contextBuilder, "design: %s\n", thread.LastDesign)
		}
		if thread.GHRepo != "" {
			fmt.Fprintf(&contextBuilder, "repo: %s\n", thread.GHRepo)
		}
		if thread.GHPRNumber != "" && thread.GHPRNumber != "0" {
			fmt.Fprintf(&contextBuilder, "PR: #%s\n", thread.GHPRNumber)
		}
	}

	contextStr := contextBuilder.String()
	fullPrompt := fmt.Sprintf("%s\n## Task\n%s", contextStr, instruction)

	// Ensure workspace
	workspace := filepath.Join(workspaceDir, threadID)
	os.MkdirAll(workspace, 0755)

	// Copy agent instructions from home to workspace
	for _, filename := range []string{"AGENTS.md", "CLAUDE.md"} {
		src := filepath.Join(homeDir, filename)
		if _, err := os.Stat(src); err == nil {
			copyFile(src, filepath.Join(workspace, filename))
		}
	}

	// Check cancel flag before starting subprocess
	if cancelFlag, _ := rdb.Get(context.Background(), tasklib.TaskKey(taskID, "cancel")).Result(); cancelFlag != "" {
		log.log("info", "task cancelled before start", "task_id", taskID, "thread_id", threadID)

		cancelledAt := time.Now().UTC().Format("2006-01-02T15:04:05Z")

		pipe := rdb.Pipeline()
		pipe.Set(context.Background(), tasklib.TaskKey(taskID, "status"), "cancelled", tasklib.TTLTask)
		pipe.Set(context.Background(), tasklib.TaskKey(taskID, "result"), "Cancelled by master", tasklib.TTLTask)
		pipe.Set(context.Background(), tasklib.TaskKey(taskID, "exit_code"), "-1", tasklib.TTLTask)
		pipe.Set(context.Background(), tasklib.TaskKey(taskID, "completed_at"), cancelledAt, tasklib.TTLTask)
		pipe.Exec(context.Background())

		cancelMsg, _ := json.Marshal(map[string]interface{}{
			"role":      workerType,
			"content":   fmt.Sprintf("[cancelled] Task %s was cancelled by master", taskID),
			"timestamp": cancelledAt,
			"metadata":  map[string]string{"task_id": taskID},
		})
		rdb.RPush(context.Background(), tasklib.ThreadMessagesKey(threadID), string(cancelMsg))
		rdb.Expire(context.Background(), tasklib.ThreadMessagesKey(threadID), tasklib.TTLThread)

		rdb.LRem(context.Background(), processingKey, 0, taskJSON)
		client.RemoveActiveTask(context.Background(), taskID)
		return
	}

	// Execute agent
	timeout := defaultTimeout
	if taskPayload.Timeout > 0 {
		timeout = taskPayload.Timeout
	}

	args := append(strings.Fields(agentCmd), fullPrompt)

	log.log("info", "starting agent", "task_id", taskID, "thread_id", threadID,
		"cmd", agentCmd, "timeout", timeout)

	taskCtx, taskCancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer taskCancel()

	stdout, stderr, exitCode, runErr := execCommand(taskCtx, args, workspace)

	var result string
	var status string

	if runErr != nil {
		if errors.Is(runErr, errAgentTimeout) || taskCtx.Err() == context.DeadlineExceeded {
			exitCode = -1
			result = fmt.Sprintf("Task timed out after %ds", timeout)
			status = "failed"
			log.log("warn", "agent timed out", "task_id", taskID, "thread_id", threadID, "timeout", timeout)
		} else {
			exitCode = -1
		}
	}

	// Build result if not already set (timeout sets it above)
	if status == "" {
		result = stdout
		if stderr != "" {
			result += "\n[stderr]\n" + stderr
		}
		if exitCode != 0 {
			result = fmt.Sprintf("[FAILED exit=%d]\n%s", exitCode, result)
			status = "failed"
		} else {
			status = "done"
		}
		log.log("info", "agent finished", "task_id", taskID, "thread_id", threadID,
			"exit_code", exitCode, "status", status)
	}

	// Store results — compute a fresh timestamp after agent completion.
	completedAt := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	pipe = rdb.Pipeline()
	pipe.Set(context.Background(), tasklib.TaskKey(taskID, "result"), result, tasklib.TTLTask)
	pipe.Set(context.Background(), tasklib.TaskKey(taskID, "exit_code"), fmt.Sprintf("%d", exitCode), tasklib.TTLTask)
	pipe.Set(context.Background(), tasklib.TaskKey(taskID, "completed_at"), completedAt, tasklib.TTLTask)
	pipe.Set(context.Background(), tasklib.TaskKey(taskID, "status"), status, tasklib.TTLTask)
	pipe.Exec(context.Background())

	// Append result to thread history (cap at 10k chars)
	cappedResult := result
	if len(cappedResult) > 10000 {
		cappedResult = cappedResult[:10000]
	}
	resultMsg, _ := json.Marshal(map[string]interface{}{
		"role":      workerType,
		"content":   cappedResult,
		"timestamp": completedAt,
		"metadata":  map[string]string{"task_id": taskID},
	})
	rdb.RPush(context.Background(), tasklib.ThreadMessagesKey(threadID), string(resultMsg))
	rdb.Expire(context.Background(), tasklib.ThreadMessagesKey(threadID), tasklib.TTLThread)

	// Update thread state (metadata only — never status)
	rdb.HSet(context.Background(), tasklib.ThreadStateKey(threadID), map[string]interface{}{
		"last_updated_by": workerType,
		"last_task_id":    taskID,
		"updated_at":      completedAt,
	})
	rdb.Expire(context.Background(), tasklib.ThreadStateKey(threadID), tasklib.TTLThread)

	// Cleanup
	rdb.LRem(context.Background(), processingKey, 0, taskJSON)
	client.RemoveActiveTask(context.Background(), taskID)
	log.log("info", "task complete", "task_id", taskID, "thread_id", threadID, "status", status)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func validWorker(w string) bool {
	for _, v := range tasklib.WorkerTypes {
		if w == v {
			return true
		}
	}
	return false
}

func die(msg string) {
	fmt.Fprintf(os.Stderr, "ERROR: %s\n", msg)
	os.Exit(1)
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		fmt.Sscanf(v, "%d", &n)
		return n
	}
	return def
}


func copyFile(src, dst string) {
	s, err := os.Open(src)
	if err != nil {
		return
	}
	defer s.Close()
	d, err := os.Create(dst)
	if err != nil {
		return
	}
	defer d.Close()
	io.Copy(d, s)
}

// ── structured JSON-line logging ──────────────────────────────────────────────

type logger struct {
	worker string
}

func (l *logger) log(level, msg string, kv ...interface{}) {
	entry := map[string]interface{}{
		"level":  level,
		"msg":    msg,
		"worker": l.worker,
	}
	for i := 0; i < len(kv)-1; i += 2 {
		entry[fmt.Sprint(kv[i])] = kv[i+1]
	}
	data, _ := json.Marshal(entry)
	fmt.Println(string(data))
}
