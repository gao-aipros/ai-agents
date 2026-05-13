package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/noodle05/ai-agents/tasklib"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: worker <type>\n  type: claude, copilot, or opencode\n")
		os.Exit(1)
	}
	workerType := os.Args[1]

	valid := false
	for _, wt := range tasklib.WorkerTypes {
		if workerType == wt {
			valid = true
			break
		}
	}
	if !valid {
		fmt.Fprintf(os.Stderr, "Invalid worker type: %s (must be claude, copilot, or opencode)\n", workerType)
		os.Exit(1)
	}

	redisHost := envDefault("REDIS_HOST", "redis")
	redisPort := envIntDefault("REDIS_PORT", 6379)
	redisDB := envIntDefault("REDIS_DB", 0)
	agentCmd := envDefault("AGENT_CMD", "claude -p")
	taskTimeout := envIntDefault("TASK_TIMEOUT", 1800)
	historyWindow := envIntDefault("HISTORY_WINDOW", 10)
	workspaceDir := envDefault("WORKSPACE_DIR", "/workspace")

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = fmt.Sprintf("worker-%s-%d", workerType, os.Getpid())
	}

	rdb := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", redisHost, redisPort),
		DB:   redisDB,
	})
	client := tasklib.NewClient(rdb)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	go runHeartbeat(ctx, client, workerType, hostname)

	queueKey := tasklib.QueueKey(workerType)
	processingKey := tasklib.ProcessingKey(workerType)

	logJSON("info", "worker started", "queue", queueKey, "agent_cmd", agentCmd)

	for {
		taskJSON, err := rdb.BLMove(ctx, queueKey, processingKey, "RIGHT", "LEFT", 5*time.Second).Result()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				logJSON("info", "worker stopped")
				return
			}
			logJSON("warn", "redis connection lost, reconnecting", "error", err.Error())
			time.Sleep(1 * time.Second)
			continue
		}

		processTask(context.Background(), client, rdb, taskJSON, workerType, agentCmd, taskTimeout, historyWindow, workspaceDir)
	}
}

// workerPayload is the deserialized task from the queue.
// Optional fields use pointers to distinguish "absent" from "zero".
type workerPayload struct {
	TaskID        string `json:"task_id"`
	ThreadID      string `json:"thread_id"`
	Instruction   string `json:"instruction"`
	HistoryWindow *int   `json:"history_window,omitempty"`
	Timeout       *int   `json:"timeout,omitempty"`
}

func processTask(ctx context.Context, client *tasklib.Client, rdb *redis.Client, taskJSON, workerType, agentCmd string, taskTimeout, historyWindow int, workspaceDir string) {
	var payload workerPayload
	if err := json.Unmarshal([]byte(taskJSON), &payload); err != nil {
		logJSON("warn", "malformed task payload, skipping", "error", err.Error())
		rdb.LRem(ctx, tasklib.ProcessingKey(workerType), 0, taskJSON)
		return
	}

	taskID := payload.TaskID
	threadID := payload.ThreadID
	instruction := payload.Instruction

	logJSON("info", "task dequeued", "task_id", taskID, "thread_id", threadID)

	window := historyWindow
	if payload.HistoryWindow != nil {
		window = *payload.HistoryWindow
	}
	timeout := taskTimeout
	if payload.Timeout != nil {
		timeout = *payload.Timeout
	}

	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	// Mark task as active
	client.SetActiveTask(ctx, taskID, tasklib.TaskInfo{
		Status:    "running",
		Worker:    workerType,
		ThreadID:  threadID,
		StartedAt: now,
	})

	pipe := rdb.Pipeline()
	pipe.Set(ctx, tasklib.TaskKey(taskID, "status"), "running", tasklib.TTLTask)
	pipe.Set(ctx, tasklib.TaskKey(taskID, "worker"), workerType, tasklib.TTLTask)
	pipe.Set(ctx, tasklib.TaskKey(taskID, "thread_id"), threadID, tasklib.TTLTask)
	pipe.Set(ctx, tasklib.TaskKey(taskID, "description"), instruction, tasklib.TTLTask)
	pipe.Set(ctx, tasklib.TaskKey(taskID, "created_at"), now, tasklib.TTLTask)
	pipe.Exec(ctx)

	// Build prompt with thread context
	fullPrompt := buildPrompt(ctx, client, threadID, instruction, window)

	// Prepare workspace
	workspace := filepath.Join(workspaceDir, threadID)
	os.MkdirAll(workspace, 0755)

	home, _ := os.UserHomeDir()
	for _, filename := range []string{"AGENTS.md", "CLAUDE.md"} {
		src := filepath.Join(home, filename)
		if _, err := os.Stat(src); err == nil {
			copyFile(src, filepath.Join(workspace, filename))
		}
	}

	// Check for cancellation before starting subprocess
	if exists, _ := rdb.Exists(ctx, tasklib.TaskKey(taskID, "cancel")).Result(); exists > 0 {
		logJSON("info", "task cancelled before start", "task_id", taskID, "thread_id", threadID)
		completedAt := time.Now().UTC().Format("2006-01-02T15:04:05Z")
		p := rdb.Pipeline()
		p.Set(ctx, tasklib.TaskKey(taskID, "status"), "cancelled", tasklib.TTLTask)
		p.Set(ctx, tasklib.TaskKey(taskID, "result"), "Cancelled by master", tasklib.TTLTask)
		p.Set(ctx, tasklib.TaskKey(taskID, "exit_code"), "-1", tasklib.TTLTask)
		p.Set(ctx, tasklib.TaskKey(taskID, "completed_at"), completedAt, tasklib.TTLTask)
		p.Exec(ctx)

		msg := tasklib.Message{
			Role:      workerType,
			Content:   fmt.Sprintf("[cancelled] Task %s was cancelled by master", taskID),
			Timestamp: completedAt,
			Metadata:  map[string]string{"task_id": taskID},
		}
		client.AppendMessage(ctx, threadID, msg)

		rdb.LRem(ctx, tasklib.ProcessingKey(workerType), 0, taskJSON)
		client.RemoveActiveTask(ctx, taskID)
		return
	}

	// Execute agent subprocess
	cmdParts := strings.Fields(agentCmd)
	cmdParts = append(cmdParts, fullPrompt)

	taskCtx, taskCancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer taskCancel()

	cmd := exec.CommandContext(taskCtx, cmdParts[0], cmdParts[1:]...)
	cmd.Dir = workspace

	logJSON("info", "starting agent", "task_id", taskID, "thread_id", threadID, "cmd", agentCmd, "timeout", timeout)

	output, err := cmd.CombinedOutput()

	exitCode := 0
	status := "done"
	var result string

	if err != nil {
		if taskCtx.Err() == context.DeadlineExceeded {
			exitCode = -1
			result = fmt.Sprintf("Task timed out after %ds", timeout)
			status = "failed"
			logJSON("warn", "agent timed out", "task_id", taskID, "thread_id", threadID, "timeout", timeout)
		} else if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			result = string(output)
			if exitCode != 0 {
				result = fmt.Sprintf("[FAILED exit=%d]\n", exitCode) + result
			}
			status = "failed"
			logJSON("info", "agent finished", "task_id", taskID, "thread_id", threadID, "exit_code", exitCode, "status", status)
		} else {
			exitCode = -1
			result = fmt.Sprintf("Error: %v\n%s", err, string(output))
			status = "failed"
			logJSON("warn", "agent error", "task_id", taskID, "thread_id", threadID, "error", err.Error())
		}
	} else {
		result = string(output)
		logJSON("info", "agent finished", "task_id", taskID, "thread_id", threadID, "exit_code", exitCode, "status", status)
	}

	// Truncate result (same cap as Python worker)
	if len(result) > 10000 {
		result = result[:10000]
	}

	completedAt := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	// Store result
	p := rdb.Pipeline()
	p.Set(ctx, tasklib.TaskKey(taskID, "result"), result, tasklib.TTLTask)
	p.Set(ctx, tasklib.TaskKey(taskID, "exit_code"), fmt.Sprintf("%d", exitCode), tasklib.TTLTask)
	p.Set(ctx, tasklib.TaskKey(taskID, "completed_at"), completedAt, tasklib.TTLTask)
	p.Set(ctx, tasklib.TaskKey(taskID, "status"), status, tasklib.TTLTask)
	p.Exec(ctx)

	// Append result to thread history
	client.AppendMessage(ctx, threadID, tasklib.Message{
		Role:      workerType,
		Content:   result,
		Timestamp: completedAt,
		Metadata:  map[string]string{"task_id": taskID},
	})

	// Update thread state (best-effort)
	rdb.HSet(ctx, tasklib.ThreadStateKey(threadID), map[string]interface{}{
		"last_updated_by": workerType,
		"last_task_id":    taskID,
		"updated_at":      completedAt,
	})
	rdb.Expire(ctx, tasklib.ThreadStateKey(threadID), tasklib.TTLThread)

	// Cleanup
	rdb.LRem(ctx, tasklib.ProcessingKey(workerType), 0, taskJSON)
	client.RemoveActiveTask(ctx, taskID)

	logJSON("info", "task complete", "task_id", taskID, "thread_id", threadID, "status", status)
}

func buildPrompt(ctx context.Context, client *tasklib.Client, threadID, instruction string, window int) string {
	var b strings.Builder

	history, _ := client.GetThreadHistoryTail(ctx, threadID, window)
	if len(history) > 0 {
		b.WriteString("## Thread History (recent)\n\n")
		for _, msg := range history {
			fmt.Fprintf(&b, "[%s]: %s\n\n", msg.Role, msg.Content)
		}
	}

	state, _ := client.GetThread(ctx, threadID)
	if state != nil {
		b.WriteString("\n## Current State\n")
		fmt.Fprintf(&b, "status: %s\n", state.Status)
		if state.LastDesign != "" {
			fmt.Fprintf(&b, "design: %s\n", state.LastDesign)
		}
		if state.GHRepo != "" {
			fmt.Fprintf(&b, "repo: %s\n", state.GHRepo)
		}
		if state.GHPRNumber != "" {
			fmt.Fprintf(&b, "PR: #%s\n", state.GHPRNumber)
		}
	}

	b.WriteString("\n## Task\n")
	b.WriteString(instruction)

	return b.String()
}

func runHeartbeat(ctx context.Context, client *tasklib.Client, workerType, hostname string) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := client.UpdateWorkerHeartbeat(ctx, workerType, hostname); err != nil {
				logJSON("warn", "heartbeat failed", "error", err.Error())
			}
		}
	}
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

func logJSON(level, msg string, fields ...interface{}) {
	entry := map[string]interface{}{
		"level":  level,
		"msg":    msg,
		"worker": os.Args[1],
	}
	for i := 0; i+1 < len(fields); i += 2 {
		key, ok := fields[i].(string)
		if !ok {
			continue
		}
		entry[key] = fields[i+1]
	}
	data, _ := json.Marshal(entry)
	fmt.Println(string(data))
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
