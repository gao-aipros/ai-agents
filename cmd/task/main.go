package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/noodle05/ai-agents/tasklib"
)

var (
	redisHost    string
	redisPort    int
	redisDB      int
	workspaceDir string
)

// Flag variables — all commands share these.
var (
	enqueueWorker      string
	enqueueThread      string
	enqueueInstruction string
	enqueueGroup       string
	statusID           string
	resultID           string
	resultTail         int
	listWorker         string
	listStatus         string
	listThread         string
	listLimit          int
	listVerbose       bool
	whyThread         string
	waitID             string
	waitTimeout        int
	requeueWorker      string
	requeueOlderThan   int
	cancelID           string
	unlockThread       string
	tcID               string
	tcRepo             string
	thID               string
	thTail             int
	tsID               string
	tuID               string
	tuStatus           string
	tuDesign           string
	tuPR               int
	tclID              string
	gwThread           string
	gwGroup            string
	gwTimeout          int
)

var getClient = func() *tasklib.Client {
	rdb := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", redisHost, redisPort),
		DB:   redisDB,
	})
	return tasklib.NewClient(rdb)
}

var die = func(msg string) {
	fmt.Fprintf(os.Stderr, "ERROR: %s\n", msg)
	os.Exit(1)
}

func init() {
	redisHost = envDefault("REDIS_HOST", "redis")
	redisPort = envIntDefault("REDIS_PORT", 6379)
	redisDB = envIntDefault("COMPAT_TEST_DB", 0)
	workspaceDir = envDefault("WORKSPACE_DIR", "/workspace")
}

func main() {
	root := &cobra.Command{Use: "task"}

	// ── task enqueue ────────────────────────────────────────────────────
	enqueueCmd := &cobra.Command{
		Use:  "enqueue",
		RunE: cmdEnqueue,
	}
	enqueueCmd.Flags().StringVar(&enqueueWorker, "worker", "", "")
	enqueueCmd.Flags().StringVar(&enqueueThread, "thread", "", "")
	enqueueCmd.Flags().StringVar(&enqueueInstruction, "instruction", "", "")
	enqueueCmd.Flags().StringVar(&enqueueGroup, "group", "", "")
	enqueueCmd.MarkFlagRequired("worker")
	enqueueCmd.MarkFlagRequired("thread")
	enqueueCmd.MarkFlagRequired("instruction")
	root.AddCommand(enqueueCmd)

	// ── task status ─────────────────────────────────────────────────────
	statusCmd := &cobra.Command{
		Use:  "status",
		RunE: cmdStatus,
	}
	statusCmd.Flags().StringVar(&statusID, "id", "", "")
	statusCmd.MarkFlagRequired("id")
	root.AddCommand(statusCmd)

	// ── task result ─────────────────────────────────────────────────────
	resultCmd := &cobra.Command{
		Use:  "result",
		RunE: cmdResult,
	}
	resultCmd.Flags().StringVar(&resultID, "id", "", "")
	resultCmd.Flags().IntVar(&resultTail, "tail", -1, "")
	resultCmd.MarkFlagRequired("id")
	root.AddCommand(resultCmd)

	// ── task list ───────────────────────────────────────────────────────
	listCmd := &cobra.Command{
		Use:  "list",
		RunE: cmdList,
	}
	listCmd.Flags().StringVar(&listWorker, "worker", "", "")
	listCmd.Flags().StringVar(&listStatus, "status", "", "")
	listCmd.Flags().StringVar(&listThread, "thread", "", "")
	listCmd.Flags().IntVar(&listLimit, "limit", 50, "")
	listCmd.Flags().BoolVar(&listVerbose, "verbose", false, "")
	root.AddCommand(listCmd)

	// ── task wait ───────────────────────────────────────────────────────
	waitCmd := &cobra.Command{
		Use:  "wait",
		RunE: cmdWait,
	}
	waitCmd.Flags().StringVar(&waitID, "id", "", "")
	waitCmd.Flags().IntVar(&waitTimeout, "timeout", 300, "")
	waitCmd.MarkFlagRequired("id")
	root.AddCommand(waitCmd)

	// ── task requeue-stale ──────────────────────────────────────────────
	requeueCmd := &cobra.Command{
		Use:  "requeue-stale",
		RunE: cmdRequeueStale,
	}
	requeueCmd.Flags().StringVar(&requeueWorker, "worker", "", "")
	requeueCmd.Flags().IntVar(&requeueOlderThan, "older-than", 600, "")
	root.AddCommand(requeueCmd)

	// ── task cancel ─────────────────────────────────────────────────────
	cancelCmd := &cobra.Command{
		Use:  "cancel",
		RunE: cmdCancel,
	}
	cancelCmd.Flags().StringVar(&cancelID, "id", "", "")
	cancelCmd.MarkFlagRequired("id")
	root.AddCommand(cancelCmd)

	// ── task unlock ─────────────────────────────────────────────────────
	unlockCmd := &cobra.Command{
		Use:  "unlock",
		RunE: cmdUnlock,
	}
	unlockCmd.Flags().StringVar(&unlockThread, "thread", "", "")
	unlockCmd.MarkFlagRequired("thread")
	root.AddCommand(unlockCmd)

	// ── thread-create ───────────────────────────────────────────────────
	threadCreateCmd := &cobra.Command{
		Use:  "thread-create",
		RunE: cmdThreadCreate,
	}
	threadCreateCmd.Flags().StringVar(&tcID, "id", "", "")
	threadCreateCmd.Flags().StringVar(&tcRepo, "repo", "", "")
	threadCreateCmd.MarkFlagRequired("id")
	root.AddCommand(threadCreateCmd)

	// ── thread-history ──────────────────────────────────────────────────
	threadHistoryCmd := &cobra.Command{
		Use:  "thread-history",
		RunE: cmdThreadHistory,
	}
	threadHistoryCmd.Flags().StringVar(&thID, "id", "", "")
	threadHistoryCmd.Flags().IntVar(&thTail, "tail", -1, "")
	threadHistoryCmd.MarkFlagRequired("id")
	root.AddCommand(threadHistoryCmd)

	// ── thread-state ────────────────────────────────────────────────────
	threadStateCmd := &cobra.Command{
		Use:  "thread-state",
		RunE: cmdThreadState,
	}
	threadStateCmd.Flags().StringVar(&tsID, "id", "", "")
	threadStateCmd.MarkFlagRequired("id")
	root.AddCommand(threadStateCmd)

	// ── thread-update ───────────────────────────────────────────────────
	threadUpdateCmd := &cobra.Command{
		Use:  "thread-update",
		RunE: cmdThreadUpdate,
	}
	threadUpdateCmd.Flags().StringVar(&tuID, "id", "", "")
	threadUpdateCmd.Flags().StringVar(&tuStatus, "status", "", "")
	threadUpdateCmd.Flags().StringVar(&tuDesign, "design", "", "")
	threadUpdateCmd.Flags().IntVar(&tuPR, "pr", -1, "")
	threadUpdateCmd.MarkFlagRequired("id")
	threadUpdateCmd.MarkFlagRequired("status")
	root.AddCommand(threadUpdateCmd)

	// ── thread-list ─────────────────────────────────────────────────────
	threadListCmd := &cobra.Command{
		Use:  "thread-list",
		RunE: cmdThreadList,
	}
	root.AddCommand(threadListCmd)

	// ── thread-cleanup ──────────────────────────────────────────────────
	threadCleanupCmd := &cobra.Command{
		Use:  "thread-cleanup",
		RunE: cmdThreadCleanup,
	}
	threadCleanupCmd.Flags().StringVar(&tclID, "id", "", "")
	threadCleanupCmd.MarkFlagRequired("id")
	root.AddCommand(threadCleanupCmd)

	// ── task group-wait ──────────────────────────────────────────────────
	groupWaitCmd := &cobra.Command{
		Use:  "group-wait",
		RunE: cmdGroupWait,
	}
	groupWaitCmd.Flags().StringVar(&gwThread, "thread", "", "")
	groupWaitCmd.Flags().StringVar(&gwGroup, "group", "", "")
	groupWaitCmd.Flags().IntVar(&gwTimeout, "timeout", 600, "")
	groupWaitCmd.MarkFlagRequired("thread")
	groupWaitCmd.MarkFlagRequired("group")
	root.AddCommand(groupWaitCmd)

	// ── task why ─────────────────────────────────────────────────────────
	whyCmd := &cobra.Command{
		Use:  "why",
		Short: "Diagnose a thread — why is it stuck or what happened?",
		RunE: cmdWhy,
	}
	whyCmd.Flags().StringVar(&whyThread, "thread", "", "")
	whyCmd.MarkFlagRequired("thread")
	root.AddCommand(whyCmd)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// ── command handlers (each matches task.py output exactly) ──────────────

func cmdEnqueue(cmd *cobra.Command, args []string) error {
	c := getClient()
	ctx := context.Background()

	if enqueueGroup != "" {
		// Validate group label before any API call
		if strings.ContainsAny(enqueueGroup, ":\t\n\r ") {
			die(fmt.Sprintf("invalid group label %q: must not contain ':' or whitespace", enqueueGroup))
		}
		task, err := c.EnqueueGroup(ctx, enqueueWorker, enqueueThread, enqueueGroup, enqueueInstruction)
		if err != nil {
			die(err.Error())
		}
		data, _ := json.Marshal(map[string]string{"task_id": task.TaskID})
		fmt.Println(string(data))
		return nil
	}

	task, err := c.Enqueue(ctx, enqueueWorker, enqueueThread, enqueueInstruction)
	if err != nil {
		die(err.Error())
	}
	data, _ := json.Marshal(map[string]string{"task_id": task.TaskID})
	fmt.Println(string(data))
	return nil
}

func cmdStatus(cmd *cobra.Command, args []string) error {
	c := getClient()
	ctx := context.Background()

	task, err := c.GetTask(ctx, statusID)
	if err != nil {
		die(err.Error())
	}
	if task.Status == "" {
		die(fmt.Sprintf("task %s not found", statusID))
	}
	data, _ := json.MarshalIndent(task, "", "  ")
	fmt.Println(string(data))
	return nil
}

func cmdResult(cmd *cobra.Command, args []string) error {
	c := getClient()
	ctx := context.Background()

	result, err := c.RDB().Get(ctx, tasklib.TaskKey(resultID, "result")).Result()
	if err == redis.Nil {
		result = ""
	} else if err != nil {
		die(err.Error())
	}

	// cmdResult was invoked without --tail (cobra default -1) → no tail flag set.
	// task.py uses default=None and checks `if args.tail is not None`.
	// We use -1 to signal "not set" (since 0 is a valid tail value meaning "empty").
	tailFlag := cmd.Flags().Changed("tail")
	if tailFlag {
		if resultTail == 0 {
			result = ""
		} else if resultTail > 0 {
			lines := strings.Split(result, "\n")
			if len(lines) > resultTail {
				result = strings.Join(lines[len(lines)-resultTail:], "\n")
			}
		}
		// resultTail < 0 means invalid input; treat as full
	}
	fmt.Println(result)
	return nil
}

func cmdList(cmd *cobra.Command, args []string) error {
	c := getClient()
	ctx := context.Background()

	rdb := c.RDB()
	tasks := make(map[string]map[string]interface{})

	// Collect from active_tasks hash
	active, _ := rdb.HGetAll(ctx, "active_tasks").Result()
	for taskID, raw := range active {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			entry = map[string]interface{}{"status": "unknown"}
		}
		tasks[taskID] = entry
	}

	// Scan task:*:status keys
	var cursor uint64
	for {
		keys, nextCursor, err := rdb.Scan(ctx, cursor, "task:*:status", 100).Result()
		if err != nil {
			break
		}
		for _, key := range keys {
			parts := strings.SplitN(key, ":", 3)
			if len(parts) >= 2 {
				taskID := parts[1]
				if _, exists := tasks[taskID]; !exists {
					tasks[taskID] = map[string]interface{}{}
				}
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	// Sort task IDs
	sortedIDs := make([]string, 0, len(tasks))
	for id := range tasks {
		sortedIDs = append(sortedIDs, id)
	}
	sort.Strings(sortedIDs)

	// Replicate task.py exact behavior: limit check BEFORE filter
	limit := listLimit
	if limit <= 0 {
		limit = 50
	}
	var rows []map[string]interface{}
	for _, taskID := range sortedIDs {
		if len(rows) >= limit {
			break
		}
		entry := tasks[taskID]
		entry["task_id"] = taskID

		// Populate missing fields from Redis
		if _, ok := entry["status"]; !ok {
			val, _ := rdb.Get(ctx, tasklib.TaskKey(taskID, "status")).Result()
			if val == "" {
				val = "unknown"
			}
			entry["status"] = val
		}
		if _, ok := entry["worker"]; !ok {
			val, _ := rdb.Get(ctx, tasklib.TaskKey(taskID, "worker")).Result()
			if val == "" {
				val = "-"
			}
			entry["worker"] = val
		}
		if _, ok := entry["thread_id"]; !ok {
			val, _ := rdb.Get(ctx, tasklib.TaskKey(taskID, "thread_id")).Result()
			if val == "" {
				val = "-"
			}
			entry["thread_id"] = val
		}
		if _, ok := entry["started_at"]; !ok {
			val, _ := rdb.Get(ctx, tasklib.TaskKey(taskID, "started_at")).Result()
			if val == "" {
				val, _ = rdb.Get(ctx, tasklib.TaskKey(taskID, "enqueued_at")).Result()
			}
			if val == "" {
				val = "-"
			}
			entry["started_at"] = val
		}

		// Apply filters (after populating, matching task.py order)
		if listWorker != "" && entry["worker"] != listWorker {
			continue
		}
		if listStatus != "" && entry["status"] != listStatus {
			continue
		}
		if listThread != "" && entry["thread_id"] != listThread {
			continue
		}

		rows = append(rows, entry)
	}

	if len(rows) == 0 {
		fmt.Println("(no tasks)")
		return nil
	}

	if listVerbose {
		// Verbose: show enriched fields
		for _, entry := range rows {
			taskID := fmt.Sprintf("%v", entry["task_id"])
			task, err := c.GetTask(ctx, taskID)
			if err != nil {
				data, _ := json.MarshalIndent(entry, "", "  ")
				fmt.Println(string(data))
				continue
			}
			data, _ := json.MarshalIndent(task, "", "  ")
			fmt.Println(string(data))
			fmt.Println("---")
		}
		return nil
	}

	header := fmt.Sprintf("%-38s %-12s %-10s %-20s %-20s", "TASK ID", "STATUS", "WORKER", "THREAD", "STARTED")
	fmt.Println(header)
	fmt.Println(strings.Repeat("-", len(header)))
	for _, entry := range rows {
		tid := fmt.Sprintf("%v", entry["task_id"])
		if len(tid) > 36 {
			tid = tid[:36]
		}
		status := fmt.Sprintf("%v", entry["status"])
		worker := fmt.Sprintf("%v", entry["worker"])
		thread := fmt.Sprintf("%v", entry["thread_id"])
		if len(thread) > 18 {
			thread = thread[:18]
		}
		started := fmt.Sprintf("%v", entry["started_at"])
		fmt.Printf("%-38s %-12s %-10s %-20s %-20s\n", tid, status, worker, thread, started)
	}
	return nil
}

func cmdWait(cmd *cobra.Command, args []string) error {
	c := getClient()
	ctx := context.Background()

	// Read thread_id from the task so the lock can be released on timeout
	// (matching task.py's finally block that reads thread_id from Redis).
	threadID, _ := c.RDB().Get(ctx, tasklib.TaskKey(waitID, "thread_id")).Result()

	task, err := c.WaitTask(ctx, waitID, threadID, time.Duration(waitTimeout)*time.Second)
	if err != nil {
		die(err.Error())
	}

	info := map[string]interface{}{
		"task_id": waitID,
		"status":  task.Status,
		"worker":  task.Worker,
		"thread_id": task.ThreadID,
		"exit_code": task.ExitCode,
		"enqueued_at": task.EnqueuedAt,
		"completed_at": task.CompletedAt,
	}
	data, _ := json.Marshal(info)
	fmt.Println(string(data))
	return nil
}

func cmdRequeueStale(cmd *cobra.Command, args []string) error {
	c := getClient()
	ctx := context.Background()

	workers := tasklib.WorkerTypes
	if requeueWorker != "" {
		workers = []string{requeueWorker}
	}

	for _, worker := range workers {
		processingKey := tasklib.ProcessingKey(worker)
		queueKey := tasklib.QueueKey(worker)

		items, err := c.RDB().LRange(ctx, processingKey, 0, -1).Result()
		if err != nil {
			die(err.Error())
		}

		for _, itemJSON := range items {
			var task struct {
				TaskID   string `json:"task_id"`
				ThreadID string `json:"thread_id"`
			}
			if err := json.Unmarshal([]byte(itemJSON), &task); err != nil {
				c.RDB().LRem(ctx, processingKey, 0, itemJSON)
				continue
			}

			status, _ := c.RDB().Get(ctx, tasklib.TaskKey(task.TaskID, "status")).Result()
			lastStartedAt, _ := c.RDB().Get(ctx, tasklib.TaskKey(task.TaskID, "last_started_at")).Result()

			requeue := false
			oldStatus := status

			if status == "" || status == "pending" {
				requeue = true
			} else if status == "running" && lastStartedAt != "" {
				started, parseErr := time.Parse("2006-01-02T15:04:05Z", lastStartedAt)
				if parseErr == nil && time.Since(started) > time.Duration(requeueOlderThan)*time.Second {
					requeue = true
				}
			} else if status == "done" || status == "failed" || status == "cancelled" {
				c.RDB().LRem(ctx, processingKey, 0, itemJSON)
				continue
			}

			if requeue {
				c.RDB().LPush(ctx, queueKey, itemJSON)
				c.RDB().LRem(ctx, processingKey, 0, itemJSON)
				c.RDB().Set(ctx, tasklib.TaskKey(task.TaskID, "status"), "pending", tasklib.TTLTask)
				pipe := c.RDB().Pipeline()
				pipe.Incr(ctx, tasklib.TaskKey(task.TaskID, "retry_count"))
				pipe.Expire(ctx, tasklib.TaskKey(task.TaskID, "retry_count"), tasklib.TTLTask)
				pipe.Exec(ctx)

				displayStatus := oldStatus
				if displayStatus == "" {
					displayStatus = "missing"
				}
				fmt.Printf("Requeued: %s (worker=%s, was status=%s)\n", task.TaskID, worker, displayStatus)
			}
		}
	}
	return nil
}

func cmdCancel(cmd *cobra.Command, args []string) error {
	c := getClient()
	ctx := context.Background()

	if err := c.CancelTask(ctx, cancelID, "user"); err != nil {
		die(err.Error())
	}
	fmt.Printf("Cancel flag set for task %s\n", cancelID)
	return nil
}

func cmdUnlock(cmd *cobra.Command, args []string) error {
	c := getClient()
	ctx := context.Background()

	key := tasklib.ThreadLockKey(unlockThread)
	deleted, err := c.RDB().Del(ctx, key).Result()
	if err != nil {
		die(err.Error())
	}
	if deleted > 0 {
		fmt.Printf("Lock released for thread '%s'\n", unlockThread)
	} else {
		fmt.Printf("No lock found for thread '%s'\n", unlockThread)
	}
	return nil
}

func cmdThreadCreate(cmd *cobra.Command, args []string) error {
	c := getClient()
	ctx := context.Background()

	_, err := c.CreateThread(ctx, tcID, tcRepo)
	if err != nil {
		die(err.Error())
	}
	fmt.Printf("Thread '%s' created\n", tcID)
	return nil
}

func cmdThreadHistory(cmd *cobra.Command, args []string) error {
	c := getClient()
	ctx := context.Background()

	key := tasklib.ThreadMessagesKey(thID)
	var msgs []string
	var err error

	tailFlag := cmd.Flags().Changed("tail")
	if tailFlag {
		if thTail == 0 {
			fmt.Println("(no messages)")
			return nil
		}
		msgs, err = c.RDB().LRange(ctx, key, int64(-thTail), -1).Result()
	} else {
		msgs, err = c.RDB().LRange(ctx, key, 0, -1).Result()
	}
	if err != nil {
		die(err.Error())
	}
	if len(msgs) == 0 {
		fmt.Println("(no messages)")
		return nil
	}

	for _, msg := range msgs {
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(msg), &data); err != nil {
			fmt.Println(msg)
			fmt.Println("---")
			continue
		}
		fmt.Printf("[%v] %v\n", data["role"], data["timestamp"])
		fmt.Printf("%v\n", data["content"])
		fmt.Println("---")
	}
	return nil
}

func cmdThreadState(cmd *cobra.Command, args []string) error {
	c := getClient()
	ctx := context.Background()

	thread, err := c.GetThread(ctx, tsID)
	if err != nil {
		die(err.Error())
	}

	data, _ := json.MarshalIndent(thread, "", "  ")
	fmt.Println(string(data))

	// Show recent events
	events, err := c.GetThreadEvents(ctx, tsID, 20)
	if err == nil && len(events) > 0 {
		fmt.Println("\nRecent events:")
		for _, ev := range events {
			fmt.Printf("  [%s] %s", ev.Timestamp, ev.Type)
			if ev.TaskID != "" {
				fmt.Printf(" task=%s", ev.TaskID)
			}
			if ev.WorkerType != "" {
				fmt.Printf(" worker=%s", ev.WorkerType)
			}
			fmt.Println()
		}
	}

	// Task summary for this thread
	tasks, err := c.ListTasks(ctx, "", "", tsID, 200, 0)
	if err == nil && len(tasks) > 0 {
		counts := make(map[string]int)
		for _, t := range tasks {
			counts[t.Status]++
		}
		fmt.Println("\nTask summary:")
		for status, count := range counts {
			fmt.Printf("  %s: %d\n", status, count)
		}
	}

	return nil
}

func cmdThreadUpdate(cmd *cobra.Command, args []string) error {
	c := getClient()
	ctx := context.Background()

	fields := tasklib.ParseThreadUpdateFields(tuStatus, tuDesign, "")
	if tuPR >= 0 {
		fields["gh_pr_number"] = fmt.Sprintf("%d", tuPR)
	}
	if err := c.UpdateThread(ctx, tuID, fields); err != nil {
		die(err.Error())
	}
	fmt.Printf("Thread '%s' updated\n", tuID)
	return nil
}

func cmdThreadList(cmd *cobra.Command, args []string) error {
	c := getClient()
	ctx := context.Background()

	threads, err := c.ListThreads(ctx)
	if err != nil {
		die(err.Error())
	}
	if len(threads) == 0 {
		fmt.Println("(no threads)")
		return nil
	}

	// Sort by updated_at descending (match task.py)
	sort.Slice(threads, func(i, j int) bool {
		return threads[i].UpdatedAt > threads[j].UpdatedAt
	})

	header := fmt.Sprintf("%-30s %-16s %-20s %-20s %-6s", "THREAD ID", "STATUS", "UPDATED", "REPO", "PR")
	fmt.Println(header)
	fmt.Println(strings.Repeat("-", len(header)))
	for _, t := range threads {
		fmt.Printf("%-30s %-16s %-20s %-20s %-6s\n",
			t.ThreadID, t.Status, t.UpdatedAt, t.GHRepo, t.GHPRNumber)
	}
	return nil
}

func cmdThreadCleanup(cmd *cobra.Command, args []string) error {
	threadID := tclID

	// Validate path: reject traversal attempts
	if strings.Contains(threadID, "..") || strings.Contains(threadID, "/") {
		die(fmt.Sprintf("Invalid thread ID: %s", threadID))
	}

	workspacePath := filepath.Join(workspaceDir, threadID)

	info, err := os.Stat(workspacePath)
	if os.IsNotExist(err) {
		fmt.Printf("Nothing to clean up: %s does not exist\n", workspacePath)
		return nil
	}
	if err != nil {
		die(fmt.Sprintf("Cannot access %s: %v", workspacePath, err))
	}
	if !info.IsDir() {
		die(fmt.Sprintf("Cannot delete %s: not a directory", workspacePath))
	}

	if err := os.RemoveAll(workspacePath); err != nil {
		if os.IsPermission(err) {
			die(fmt.Sprintf("Cannot delete %s: %v", workspacePath, err))
		}
		die(fmt.Sprintf("Cannot delete %s: %v", workspacePath, err))
	}
	fmt.Printf("Deleted %s\n", workspacePath)
	return nil
}

func cmdGroupWait(cmd *cobra.Command, args []string) error {
	c := getClient()
	ctx := context.Background()

	result, err := c.GroupWait(ctx, gwThread, gwGroup, time.Duration(gwTimeout)*time.Second)
	if err != nil {
		die(err.Error())
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(data))

	switch result.Status {
	case "complete":
		return nil
	default:
		die(fmt.Sprintf("group %q: %s", result.Label, result.Status))
		return nil
	}
}

// ── task why ──────────────────────────────────────────────────────────────

func cmdWhy(cmd *cobra.Command, args []string) error {
	c := getClient()
	ctx := context.Background()

	d, err := c.GetThreadDiagnostics(ctx, whyThread)
	if err != nil {
		die(err.Error())
	}

	fmt.Printf("Thread: %s\n", d.ThreadID)
	fmt.Printf("Status: %s", d.Status)
	if d.UpdatedAt != "" {
		fmt.Printf(" (updated %s)", d.UpdatedAt)
	}
	fmt.Println()
	if d.CorrelationID != "" {
		fmt.Printf("Correlation ID: %s\n", d.CorrelationID)
	}

	// Lock state
	if d.Lock != nil {
		fmt.Printf("\nLock:\n")
		fmt.Printf("  Holder task: %s\n", d.Lock.HolderTask)
		if d.Lock.LockedAt != "" {
			fmt.Printf("  Locked at:   %s", d.Lock.LockedAt)
			if d.Lock.HeldSeconds > 0 {
				fmt.Printf(" (%ds ago)", d.Lock.HeldSeconds)
			}
			fmt.Println()
		}
	} else {
		fmt.Println("\nLock: none")
	}

	// Task counts
	fmt.Println("\nTask summary:")
	for status, count := range d.TaskCounts {
		fmt.Printf("  %s: %d\n", status, count)
	}
	if len(d.TaskCounts) == 0 {
		fmt.Println("  (no tasks)")
	}

	// Last error
	if d.LastError != "" {
		fmt.Println("\nLast error:")
		fmt.Println(d.LastError)
	}

	// Stuck tasks
	if len(d.StuckTasks) > 0 {
		fmt.Println("\nStuck tasks (running > 30 min):")
		for _, t := range d.StuckTasks {
			fmt.Printf("  %s — worker=%s, started=%s, stale=%dm\n",
				t.TaskID, t.Worker, t.StartedAt, t.StaleMinutes)
		}
	}

	// Recent events
	if len(d.RecentEvents) > 0 {
		fmt.Println("\nRecent events (last 20):")
		for _, ev := range d.RecentEvents {
			fmt.Printf("  [%s] %s", ev.Timestamp, ev.Type)
			if ev.TaskID != "" {
				fmt.Printf(" task=%s", ev.TaskID)
			}
			if ev.WorkerType != "" {
				fmt.Printf(" worker=%s", ev.WorkerType)
			}
			fmt.Println()
		}
	}

	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────

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
