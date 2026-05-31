package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
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
	eventsLimit       int
	eventsType        string
	whyThread         string
	waitID             string
	waitTimeout        int
	requeueWorker      string
	requeueOlderThan   int
	cancelID           string
	unlockThread       string
	tcID               string
	tcRepo             string
	tcParent           string
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

var getServices = func() *tasklib.Services {
	rdb := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", redisHost, redisPort),
		DB:   redisDB,
	})
	return tasklib.NewServices(rdb)
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
	waitTimeout = envIntDefault("TASK_WAIT_TIMEOUT", 2100)
	gwTimeout = envIntDefault("TASK_WAIT_TIMEOUT", 2100)
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
	waitCmd.Flags().IntVar(&waitTimeout, "timeout", waitTimeout, "Timeout in seconds (env: TASK_WAIT_TIMEOUT)")
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
	threadCreateCmd.Flags().StringVar(&tcParent, "parent", "", "")
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
	groupWaitCmd.Flags().IntVar(&gwTimeout, "timeout", gwTimeout, "Timeout in seconds (env: TASK_WAIT_TIMEOUT)")
	groupWaitCmd.MarkFlagRequired("thread")
	groupWaitCmd.MarkFlagRequired("group")
	root.AddCommand(groupWaitCmd)

	// ── task events ──────────────────────────────────────────────────────
	eventsCmd := &cobra.Command{
		Use:   "events",
		Short: "Show recent system-wide events",
		RunE:  cmdEvents,
	}
	eventsCmd.Flags().IntVar(&eventsLimit, "limit", 50, "")
	eventsCmd.Flags().StringVar(&eventsType, "type", "", "")
	root.AddCommand(eventsCmd)

	// ── task workers ────────────────────────────────────────────────────────
	workersCmd := &cobra.Command{
		Use:   "workers",
		Short: "List online workers discovered from heartbeats",
		RunE:  cmdWorkers,
	}
	root.AddCommand(workersCmd)

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
	s := getServices()
	ctx := context.Background()

	if enqueueGroup != "" {
		// Validate group label before any API call
		if strings.ContainsAny(enqueueGroup, ":\t\n\r ") {
			die(fmt.Sprintf("invalid group label %q: must not contain ':' or whitespace", enqueueGroup))
		}
		task, err := s.Tasks.EnqueueGroup(ctx, enqueueWorker, enqueueThread, enqueueGroup, enqueueInstruction)
		if err != nil {
			die(err.Error())
		}
		data, _ := json.Marshal(map[string]string{"task_id": task.TaskID})
		fmt.Println(string(data))
		return nil
	}

	task, err := s.Tasks.Enqueue(ctx, enqueueWorker, enqueueThread, enqueueInstruction)
	if err != nil {
		die(err.Error())
	}
	data, _ := json.Marshal(map[string]string{"task_id": task.TaskID})
	fmt.Println(string(data))
	return nil
}

func cmdStatus(cmd *cobra.Command, args []string) error {
	s := getServices()
	ctx := context.Background()

	task, err := s.Tasks.GetTask(ctx, statusID)
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
	s := getServices()
	ctx := context.Background()

	tail := resultTail
	if !cmd.Flags().Changed("tail") {
		tail = -1 // -1 means full result (matching task.py default=None)
	}
	result, err := s.Tasks.GetTaskResult(ctx, resultID, tail)
	if err != nil {
		die(err.Error())
	}
	fmt.Println(result)
	return nil
}

func cmdList(cmd *cobra.Command, args []string) error {
	s := getServices()
	ctx := context.Background()

	// Fetch all tasks via the interface. Use a large limit so the CLI can
	// apply its own limit-before-filter semantics (matching task.py).
	allTasks, err := s.Tasks.ListTasks(ctx, "", "", "", 10000, 0, "task_id", "asc")
	if err != nil {
		die(err.Error())
	}

	// Replicate task.py exact behavior: limit check BEFORE filter
	limit := listLimit
	if limit <= 0 {
		limit = 50
	}
	var rows []*tasklib.Task
	for _, task := range allTasks {
		if len(rows) >= limit {
			break
		}
		// Apply filters (limit BEFORE filter, matching task.py order)
		if listWorker != "" && task.Worker != listWorker {
			continue
		}
		if listStatus != "" && task.Status != listStatus {
			continue
		}
		if listThread != "" && task.ThreadID != listThread {
			continue
		}
		rows = append(rows, task)
	}

	if len(rows) == 0 {
		fmt.Println("(no tasks)")
		return nil
	}

	if listVerbose {
		for _, task := range rows {
			fullTask, err := s.Tasks.GetTask(ctx, task.TaskID)
			if err != nil {
				data, _ := json.MarshalIndent(task, "", "  ")
				fmt.Println(string(data))
				continue
			}
			data, _ := json.MarshalIndent(fullTask, "", "  ")
			fmt.Println(string(data))
			fmt.Println("---")
		}
		return nil
	}

	header := fmt.Sprintf("%-38s %-12s %-10s %-20s %-20s", "TASK ID", "STATUS", "WORKER", "THREAD", "STARTED")
	fmt.Println(header)
	fmt.Println(strings.Repeat("-", len(header)))
	for _, task := range rows {
		tid := task.TaskID
		if len(tid) > 36 {
			tid = tid[:36]
		}
		status := task.Status
		worker := task.Worker
		thread := task.ThreadID
		if len(thread) > 18 {
			thread = thread[:18]
		}
		started := task.StartedAt
		fmt.Printf("%-38s %-12s %-10s %-20s %-20s\n", tid, status, worker, thread, started)
	}
	return nil
}

func cmdWait(cmd *cobra.Command, args []string) error {
	s := getServices()
	ctx := context.Background()

	// Read thread_id from the task so the lock can be released on timeout
	// (matching task.py's finally block that reads thread_id from Redis).
	task, _ := s.Tasks.GetTask(ctx, waitID)
	threadID := ""
	if task != nil {
		threadID = task.ThreadID
	}

	task, err := s.Tasks.WaitTask(ctx, waitID, threadID, time.Duration(waitTimeout)*time.Second)
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
	s := getServices()
	ctx := context.Background()

	var workers []string
	if requeueWorker != "" {
		workers = []string{requeueWorker}
	} else {
		// Discover online workers from heartbeat keys
		keys, err := s.SysOps.ScanKeys(ctx, "worker:*:heartbeat", 100)
		if err != nil {
			die("failed to scan heartbeat keys: " + err.Error())
		}
		for _, key := range keys {
			workerName := tasklib.ParseHeartbeatWorkerName(key)
			if workerName != "" {
				workers = append(workers, workerName)
			}
		}
		if len(workers) == 0 {
			fmt.Println("No workers online")
			return nil
		}
	}

	olderThan := time.Duration(requeueOlderThan) * time.Second

	var errs []error
	for _, worker := range workers {
		requeued, err := s.Tasks.RequeueStale(ctx, worker, olderThan)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			errs = append(errs, err)
			continue
		}
		for _, taskID := range requeued {
			fmt.Printf("Requeued: %s (worker=%s)\n", taskID, worker)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d worker(s) had errors during requeue-stale", len(errs))
	}
	return nil
}

func cmdCancel(cmd *cobra.Command, args []string) error {
	s := getServices()
	ctx := context.Background()

	if err := s.Tasks.CancelTask(ctx, cancelID, "user"); err != nil {
		die(err.Error())
	}
	fmt.Printf("Cancel flag set for task %s\n", cancelID)
	return nil
}

func cmdUnlock(cmd *cobra.Command, args []string) error {
	s := getServices()
	ctx := context.Background()

	// Check if lock exists before unlocking (preserves "no lock found" message)
	locked, err := s.Threads.IsThreadLocked(ctx, unlockThread)
	if err != nil {
		die(err.Error())
	}
	if !locked {
		fmt.Printf("No lock found for thread '%s'\n", unlockThread)
		return nil
	}
	if err := s.Threads.UnlockThread(ctx, unlockThread); err != nil {
		die(err.Error())
	}
	fmt.Printf("Lock released for thread '%s'\n", unlockThread)
	return nil
}

func cmdThreadCreate(cmd *cobra.Command, args []string) error {
	s := getServices()
	ctx := context.Background()

	parent := tcParent
	if !cmd.Flags().Changed("parent") {
		if v := os.Getenv("THREAD"); v != "" {
			parent = v
		}
	}

	_, err := s.Threads.CreateThread(ctx, tcID, tcRepo, parent)
	if err != nil {
		die(err.Error())
	}
	fmt.Printf("Thread '%s' created\n", tcID)
	return nil
}

func cmdThreadHistory(cmd *cobra.Command, args []string) error {
	s := getServices()
	ctx := context.Background()

	var msgs []tasklib.Message
	var err error

	tailFlag := cmd.Flags().Changed("tail")
	if tailFlag {
		if thTail == 0 {
			fmt.Println("(no messages)")
			return nil
		}
		msgs, err = s.History.GetThreadHistoryTail(ctx, thID, thTail)
	} else {
		msgs, err = s.History.GetThreadHistory(ctx, thID, 0, 0)
	}
	if err != nil {
		die(err.Error())
	}
	if len(msgs) == 0 {
		fmt.Println("(no messages)")
		return nil
	}

	for _, m := range msgs {
		fmt.Printf("[%s] %s\n", m.Role, m.Timestamp)
		fmt.Printf("%s\n", m.Content)
		fmt.Println("---")
	}
	return nil
}

func cmdThreadState(cmd *cobra.Command, args []string) error {
	s := getServices()
	ctx := context.Background()

	thread, err := s.Threads.GetThread(ctx, tsID)
	if err != nil {
		die(err.Error())
	}

	data, _ := json.MarshalIndent(thread, "", "  ")
	fmt.Println(string(data))

	// Show recent events
	events, err := s.Events.GetThreadEvents(ctx, tsID, 20)
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
	tasks, err := s.Tasks.ListTasks(ctx, "", "", tsID, 200, 0, "", "")
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
	s := getServices()
	ctx := context.Background()

	fields := tasklib.ParseThreadUpdateFields(tuStatus, tuDesign, "")
	if tuPR >= 0 {
		fields["gh_pr_number"] = fmt.Sprintf("%d", tuPR)
	}
	if err := s.Threads.UpdateThread(ctx, tuID, fields); err != nil {
		die(err.Error())
	}
	fmt.Printf("Thread '%s' updated\n", tuID)
	return nil
}

func cmdThreadList(cmd *cobra.Command, args []string) error {
	s := getServices()
	ctx := context.Background()

	threads, err := s.Threads.ListThreads(ctx, "", "")
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
	s := getServices()
	ctx := context.Background()

	result, err := s.Tasks.GroupWait(ctx, gwThread, gwGroup, time.Duration(gwTimeout)*time.Second)
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

// ── task events ────────────────────────────────────────────────────────────

func cmdEvents(cmd *cobra.Command, args []string) error {
	s := getServices()
	ctx := context.Background()

	limit := eventsLimit
	if limit <= 0 {
		limit = 50
	}

	// Fetch a larger batch when type-filtering to compensate for client-side
	// filter (matching the GET /api/events endpoint pattern).
	fetchLimit := limit
	if eventsType != "" {
		fetchLimit = limit * 3
		if fetchLimit > 1000 {
			fetchLimit = 1000
		}
	}

	events, err := s.Events.GetSystemEvents(ctx, fetchLimit)
	if err != nil {
		die(err.Error())
	}

	// Client-side type filter when --type is set
	if eventsType != "" && len(events) > 0 {
		filtered := make([]tasklib.Event, 0)
		for _, ev := range events {
			if ev.Type == eventsType {
				filtered = append(filtered, ev)
			}
		}
		events = filtered
		// Trim back to user-requested limit
		if len(events) > limit {
			events = events[:limit]
		}
	}

	if len(events) == 0 {
		fmt.Println("(no events)")
		return nil
	}

	for _, ev := range events {
		data, _ := json.MarshalIndent(ev, "", "  ")
		fmt.Println(string(data))
		fmt.Println("---")
	}
	return nil
}

// ── task workers ──────────────────────────────────────────────────────────

func cmdWorkers(cmd *cobra.Command, args []string) error {
	s := getServices()
	ctx := context.Background()

	stats, err := s.Workers.GetWorkerStats(ctx)
	if err != nil {
		die(err.Error())
	}

	if len(stats) == 0 {
		fmt.Println("No workers online")
		return nil
	}

	// Collect worker instances for agent_type + hostname + role detail
	type row struct {
		name      string
		agentType string
		role      string
		hostname  string
		tasks     int
		uptime    int64
		online    bool
	}

	var rows []row
	for workerName, info := range stats {
		instances, _ := s.Workers.GetWorkerInstances(ctx, workerName)
		for _, inst := range instances {
			rows = append(rows, row{
				name:      inst.WorkerName,
				agentType: inst.AgentType,
				role:      inst.Role,
				hostname:  inst.Hostname,
				tasks:     inst.TasksProcessed,
				uptime:    inst.UptimeSeconds,
				online:    inst.Online,
			})
		}
		if len(instances) == 0 {
			rows = append(rows, row{
				name:   workerName,
				role:   info.Role,
				online: info.Online > 0,
				tasks:  info.TotalActive,
			})
		}
	}

	header := fmt.Sprintf("%-20s %-12s %-16s %-20s %-8s %-10s %-8s", "WORKER NAME", "AGENT TYPE", "ROLE", "HOSTNAME", "TASKS", "UPTIME", "STATUS")
	fmt.Println(header)
	fmt.Println(strings.Repeat("-", len(header)))
	for _, r := range rows {
		status := "offline"
		if r.online {
			status = "online"
		}
		uptimeStr := "-"
		if r.uptime > 0 {
			d := time.Duration(r.uptime) * time.Second
			switch {
			case d < time.Hour:
				uptimeStr = fmt.Sprintf("%dm", int(d.Minutes()))
			case d < 24*time.Hour:
				h := int(d.Hours())
				m := int(d.Minutes()) % 60
				uptimeStr = fmt.Sprintf("%dh %dm", h, m)
			default:
				days := int(d.Hours() / 24)
				h := int(d.Hours()) % 24
				uptimeStr = fmt.Sprintf("%dd %dh", days, h)
			}
		}
		at := r.agentType
		if at == "" {
			at = "-"
		}
		rl := r.role
		if rl == "" {
			rl = "-"
		}
		fmt.Printf("%-20s %-12s %-16s %-20s %-8s %-10s %s\n",
			r.name, at, rl, r.hostname, fmt.Sprintf("%d", r.tasks), uptimeStr, status)
	}
	return nil
}

// ── task why ──────────────────────────────────────────────────────────────

func cmdWhy(cmd *cobra.Command, args []string) error {
	s := getServices()
	ctx := context.Background()

	d, err := s.Threads.GetThreadDiagnostics(ctx, whyThread)
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
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
