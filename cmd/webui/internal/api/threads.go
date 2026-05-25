package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/noodle05/ai-agents/cmd/webui/internal/request"
	"github.com/noodle05/ai-agents/cmd/webui/internal/templates"
	"github.com/noodle05/ai-agents/tasklib"
)

type threadsResource struct {
	client   *tasklib.Client
	renderer *templates.Renderer
}

// POST /api/threads
func (tr *threadsResource) create(w http.ResponseWriter, r *http.Request) {
	type body struct {
		ThreadID       string `json:"thread_id"`
		Repo           string `json:"repo"`
		ParentThreadID string `json:"parent_thread_id"`
	}

	var b body
	if r.ContentLength > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			Error(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}

	threadID := b.ThreadID
	if threadID == "" {
		threadID = generateThreadID()
	}

	if !request.ValidThreadID(threadID) {
		Error(w, http.StatusBadRequest, "invalid thread_id")
		return
	}

	exists, err := tr.client.ThreadExists(r.Context(), threadID)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	if exists {
		Error(w, http.StatusConflict, "thread already exists")
		return
	}

	thread, err := tr.client.CreateThread(r.Context(), threadID, b.Repo, b.ParentThreadID)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}

	Respond(w, r, http.StatusCreated, thread)
}

// GET /api/threads
func (tr *threadsResource) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sortBy := q.Get("sort_by")
	sortDir := q.Get("sort_dir")

	threads, err := tr.client.ListThreads(r.Context(), sortBy, sortDir)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	if IsHTMX(r) {
		children := buildThreadTree(threads)
		rootThreads := filterRootThreads(threads)
		Partial(w, tr.renderer, "thread-table", map[string]interface{}{
			"Threads":  rootThreads,
			"Children": children,
			"SortBy":   sortBy,
			"SortDir":  sortDir,
		})
	} else {
		Respond(w, r, http.StatusOK, threads)
	}
}

// GET /api/threads/{thread_id}
func (tr *threadsResource) get(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")
	if !request.ValidThreadID(threadID) {
		Error(w, http.StatusBadRequest, "invalid thread_id")
		return
	}
	exists, err := tr.client.ThreadExists(r.Context(), threadID)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	if !exists {
		Error(w, http.StatusNotFound, "thread not found")
		return
	}
	thread, err := tr.client.GetThread(r.Context(), threadID)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}

	running, err := tr.client.IsRequestRunning(r.Context(), threadID)
	if err != nil {
		slog.Warn(fmt.Sprintf("[webui] IsRequestRunning error thread=%s: %v", threadID, err))
	}
	complete, err := tr.client.IsThreadComplete(r.Context(), threadID)
	if err != nil {
		slog.Warn(fmt.Sprintf("[webui] IsThreadComplete error thread=%s: %v", threadID, err))
	}

	// Build token rows for the token usage table
	type tokenRow struct {
		Agent     string
		Input     string
		Output    string
		Cache     string
		Reasoning string
	}
	var tokenRows []tokenRow

	// Master agent tokens
	masterTokens, _ := tr.client.GetMasterTokenStats(r.Context(), threadID)
	if masterTokens.HasAny() {
		tokenRows = append(tokenRows, tokenRow{
			Agent:     "master",
			Input:     tasklib.FormatTokenCount(masterTokens.InputTokens),
			Output:    tasklib.FormatTokenCount(masterTokens.OutputTokens),
			Cache:     tasklib.FormatTokenCount(masterTokens.CacheReadTokens),
			Reasoning: tasklib.FormatTokenCount(masterTokens.ReasoningTokens),
		})
	}

	// Per-worker token aggregation from tasks
	tasks, _ := tr.client.ListTasks(r.Context(), "", "", threadID, 200, 0, "", "")
	if tasks != nil {
		type agentToks struct {
			input, output, cacheRead, cacheWrite, reasoning int64
		}
		agentMap := map[string]agentToks{
			"claude":   {},
			"codex":    {},
			"copilot":  {},
			"opencode": {},
		}
		for _, t := range tasks {
			at := agentMap[t.Worker]
			at.input += t.InputTokens
			at.output += t.OutputTokens
			at.cacheRead += t.CacheReadTokens
			at.cacheWrite += t.CacheWriteTokens
			at.reasoning += t.ReasoningTokens
			agentMap[t.Worker] = at
		}
		for _, wt := range []string{"claude", "codex", "copilot", "opencode"} {
			at := agentMap[wt]
			if at.input > 0 || at.output > 0 || at.cacheRead > 0 {
				tokenRows = append(tokenRows, tokenRow{
					Agent:     wt,
					Input:     tasklib.FormatTokenCount(at.input),
					Output:    tasklib.FormatTokenCount(at.output),
					Cache:     tasklib.FormatTokenCount(at.cacheRead),
					Reasoning: tasklib.FormatTokenCount(at.reasoning),
				})
			}
		}
	}

	// Find child threads
	children, _ := tr.client.ListThreads(r.Context(), "", "")
	children = filterChildren(children, threadID)

	if IsHTMX(r) {
		Partial(w, tr.renderer, "thread-state-oob", map[string]interface{}{
			"Thread":    thread,
			"Running":   running,
			"Complete":  complete,
			"TokenRows": tokenRows,
			"Children":  children,
		})
	} else {
		messages, err := tr.client.GetThreadHistoryTail(r.Context(), threadID, 20)
		if err != nil {
			slog.Warn(fmt.Sprintf("[webui] thread history tail error thread=%s: %v", threadID, err))
			messages = nil
		}
		Respond(w, r, http.StatusOK, map[string]interface{}{
			"thread":   thread,
			"running":  running,
			"complete": complete,
			"messages": messages,
			"children": children,
		})
	}
}

// GET /api/threads/{thread_id}/history
func (tr *threadsResource) history(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")
	q := r.URL.Query()

	tail, _ := strconv.Atoi(q.Get("tail"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))

	var messages []tasklib.Message
	var err error

	if tail > 0 {
		messages, err = tr.client.GetThreadHistoryTail(r.Context(), threadID, tail)
	} else {
		messages, err = tr.client.GetThreadHistory(r.Context(), threadID, offset, limit)
	}

	if err != nil {
		serverError(w, "internal error", err)
		return
	}

	running, _ := tr.client.IsRequestRunning(r.Context(), threadID)

	if IsHTMX(r) {
		if q.Get("poll") == "1" {
			Partial(w, tr.renderer, "thread-history-poll", map[string]interface{}{
				"Messages":   messages,
				"ThreadID":   threadID,
				"Running":    running,
				"NextOffset": offset + len(messages),
			})
		} else {
			Partial(w, tr.renderer, "thread-history", map[string]interface{}{
				"Messages": messages,
				"ThreadID": threadID,
				"Running":  running,
				"MsgCount": len(messages),
			})
		}
	} else {
		Respond(w, r, http.StatusOK, messages)
	}
}

// DELETE /api/threads/{thread_id}/workspace
func (tr *threadsResource) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")

	if !request.ValidThreadID(threadID) {
		Error(w, http.StatusBadRequest, "invalid thread_id")
		return
	}

	if r.URL.Query().Get("confirm") != "true" {
		Error(w, http.StatusBadRequest, "require ?confirm=true")
		return
	}

	tasks, err := tr.client.ListTasks(r.Context(), "", "running", threadID, 1, 0, "", "")
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	if len(tasks) > 0 {
		Error(w, http.StatusBadRequest, "thread has running tasks")
		return
	}

	ok, err := tr.client.AcquireRequestLock(r.Context(), threadID, "deleting", tasklib.LockTTL)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	if !ok {
		Error(w, http.StatusConflict, "thread is in use")
		return
	}
	defer tr.client.ReleaseRequestLock(cleanupContext(), threadID)

	wp := workspacePath(threadID)
	if err := removeWorkspace(wp); err != nil {
		slog.Warn(fmt.Sprintf("[webui] workspace delete error thread=%s dir=%s: %v", threadID, wp, err))
		serverError(w, "failed to delete workspace", err)
		return
	}

	slog.Info(fmt.Sprintf("[webui] workspace deleted thread=%s", threadID))
	Respond(w, r, http.StatusOK, map[string]string{"status": "deleted"})
}

// POST /api/threads/{thread_id}/keep
func (tr *threadsResource) keep(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")
	if !request.ValidThreadID(threadID) {
		Error(w, http.StatusBadRequest, "invalid thread_id")
		return
	}

	exists, err := tr.client.ThreadExists(r.Context(), threadID)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	if !exists {
		Error(w, http.StatusNotFound, "thread not found")
		return
	}

	if err := tr.client.SetThreadTTL(r.Context(), threadID, tasklib.TTLThread); err != nil {
		serverError(w, "internal error", err)
		return
	}

	Respond(w, r, http.StatusOK, map[string]string{"status": "kept"})
}

// DELETE /api/threads/{thread_id}
func (tr *threadsResource) deleteThread(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")

	if !request.ValidThreadID(threadID) {
		Error(w, http.StatusBadRequest, "invalid thread_id")
		return
	}

	if r.URL.Query().Get("confirm") != "true" {
		Error(w, http.StatusBadRequest, "require ?confirm=true")
		return
	}

	exists, err := tr.client.ThreadExists(r.Context(), threadID)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	if !exists {
		Error(w, http.StatusNotFound, "thread not found")
		return
	}

	// Acquire request lock BEFORE subtree discovery to close TOCTOU window.
	ok, err := tr.client.AcquireRequestLock(r.Context(), threadID, "deleting", tasklib.LockTTL)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	if !ok {
		Error(w, http.StatusConflict, "thread is in use")
		return
	}
	defer tr.client.ReleaseRequestLock(cleanupContext(), threadID)

	// Re-check ThreadLockKey after acquiring request lock to close the
	// TOCTOU window between any prior ListTasks and lock acquisition.
	locked, err := tr.client.IsThreadLocked(r.Context(), threadID)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	if locked {
		Error(w, http.StatusConflict, "thread has a pending task")
		return
	}

	// Discover subtree for cascade deletion (after lock, so no TOCTOU).
	descendants, err := tr.client.DiscoverDescendants(r.Context(), threadID)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}

	// Build flat list of all threads to delete
	allIDs := make([]string, 0, len(descendants)+1)
	for id := range descendants {
		allIDs = append(allIDs, id)
	}
	allIDs = append(allIDs, threadID)

	// Validate no active tasks in any subtree thread. Collect session IDs
	// before DeleteThread removes the Redis keys that store them.
	var sessionIDs []string
	for _, tid := range allIDs {
		tasks, err := tr.client.ListTasks(r.Context(), "", "", tid, 1000, 0, "", "")
		if err != nil {
			serverError(w, "internal error", err)
			return
		}
		for _, t := range tasks {
			switch t.Status {
			case "running", "queued", "pending":
				Error(w, http.StatusBadRequest, fmt.Sprintf("thread %s has active tasks", tid))
				return
			}
		}
		// Collect session ID for filesystem cleanup after Redis deletion
		sid, err := tr.client.GetThreadSessionID(r.Context(), tid)
		if err != nil {
			slog.Warn(fmt.Sprintf("[webui] get session id error thread=%s: %v", tid, err))
		}
		if sid != "" {
			sessionIDs = append(sessionIDs, sid)
		}
	}

	// Delete all Redis keys (cascade happens inside tasklib).
	if err := tr.client.DeleteThread(cleanupContext(), threadID); err != nil {
		slog.Warn(fmt.Sprintf("[webui] partial cascade deletion thread=%s: %v", threadID, err))
		serverError(w, fmt.Sprintf("partial deletion: %v", err), err)
		return
	}

	// Clean up workspace directories and session files for all subtree threads.
	// Best-effort: log errors and continue.
	for _, tid := range allIDs {
		wp := workspacePath(tid)
		if err := removeWorkspace(wp); err != nil {
			slog.Warn(fmt.Sprintf("[webui] workspace delete error thread=%s dir=%s: %v", tid, wp, err))
		}
	}
	for _, sid := range sessionIDs {
		removeSessionFile(sid)
	}

	slog.Info(fmt.Sprintf("[webui] thread deleted thread=%s (subtree=%d)", threadID, len(allIDs)))
	Respond(w, r, http.StatusOK, map[string]string{"status": "deleted"})
}

// POST /api/threads/{thread_id}/reset-session
func (tr *threadsResource) resetSession(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")
	if !request.ValidThreadID(threadID) {
		Error(w, http.StatusBadRequest, "invalid thread_id")
		return
	}

	sessionID, err := tr.client.GetThreadSessionID(r.Context(), threadID)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}

	if sessionID != "" {
		removeSessionFile(sessionID)
	}

	if err := tr.client.SetThreadSessionID(r.Context(), threadID, ""); err != nil {
		slog.Warn(fmt.Sprintf("[webui] clear session id error thread=%s: %v", threadID, err))
		serverError(w, "internal error", err)
		return
	}

	slog.Info(fmt.Sprintf("[webui] session reset thread=%s", threadID))
	Respond(w, r, http.StatusOK, map[string]string{"status": "session reset"})
}

// buildThreadTree groups threads by their ParentThreadID, returning a map
// of parent_thread_id -> child threads. Threads with no parent are excluded
// from the map values.
func buildThreadTree(threads []*tasklib.Thread) map[string][]*tasklib.Thread {
	children := make(map[string][]*tasklib.Thread)
	for _, t := range threads {
		if t.ParentThreadID != "" {
			children[t.ParentThreadID] = append(children[t.ParentThreadID], t)
		}
	}
	return children
}

// filterRootThreads returns threads without a parent (root-level threads).
func filterRootThreads(threads []*tasklib.Thread) []*tasklib.Thread {
	var roots []*tasklib.Thread
	for _, t := range threads {
		if t.ParentThreadID == "" {
			roots = append(roots, t)
		}
	}
	return roots
}

// filterChildren returns threads whose ParentThreadID matches the given threadID.
func filterChildren(threads []*tasklib.Thread, threadID string) []*tasklib.Thread {
	var children []*tasklib.Thread
	for _, t := range threads {
		if t.ParentThreadID == threadID {
			children = append(children, t)
		}
	}
	return children
}
