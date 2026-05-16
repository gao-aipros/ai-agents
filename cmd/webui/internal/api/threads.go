package api

import (
	"encoding/json"
	"log"
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
		ThreadID string `json:"thread_id"`
		Repo     string `json:"repo"`
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

	thread, err := tr.client.CreateThread(r.Context(), threadID, b.Repo)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}

	Respond(w, r, http.StatusCreated, thread)
}

// GET /api/threads
func (tr *threadsResource) list(w http.ResponseWriter, r *http.Request) {
	threads, err := tr.client.ListThreads(r.Context())
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	if IsHTMX(r) {
		Partial(w, tr.renderer, "thread-table", map[string]interface{}{"Threads": threads})
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
		log.Printf("[webui] IsRequestRunning error thread=%s: %v", threadID, err)
	}
	complete, err := tr.client.IsThreadComplete(r.Context(), threadID)
	if err != nil {
		log.Printf("[webui] IsThreadComplete error thread=%s: %v", threadID, err)
	}

	if IsHTMX(r) {
		Partial(w, tr.renderer, "thread-state-oob", map[string]interface{}{
			"Thread":   thread,
			"Running":  running,
			"Complete": complete,
		})
	} else {
		messages, err := tr.client.GetThreadHistoryTail(r.Context(), threadID, 20)
		if err != nil {
			log.Printf("[webui] thread history tail error thread=%s: %v", threadID, err)
			messages = nil
		}
		Respond(w, r, http.StatusOK, map[string]interface{}{
			"thread":   thread,
			"running":  running,
			"complete": complete,
			"messages": messages,
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

	tasks, err := tr.client.ListTasks(r.Context(), "", "running", threadID, 1, 0)
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
		log.Printf("[webui] workspace delete error thread=%s dir=%s: %v", threadID, wp, err)
		serverError(w, "failed to delete workspace", err)
		return
	}

	log.Printf("[webui] workspace deleted thread=%s", threadID)
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

	tasks, err := tr.client.ListTasks(r.Context(), "", "", threadID, 0, 0)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	for _, t := range tasks {
		switch t.Status {
		case "running", "queued", "pending":
			Error(w, http.StatusBadRequest, "thread has active tasks")
			return
		}
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

	// Re-check ThreadLockKey after acquiring request lock to close the
	// TOCTOU window between ListTasks and AcquireRequestLock. If a task
	// was enqueued in between, ThreadLockKey will be held by Enqueue.
	locked, err := tr.client.IsThreadLocked(r.Context(), threadID)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	if locked {
		Error(w, http.StatusConflict, "thread has a pending task")
		return
	}

	// Read session ID before deleting Redis keys, since DeleteThread
	// removes the key that GetThreadSessionID reads.
	sessionID, err := tr.client.GetThreadSessionID(r.Context(), threadID)
	if err != nil {
		log.Printf("[webui] get session id error thread=%s: %v", threadID, err)
	}

	// Delete thread-level Redis keys first, so if it fails files remain intact.
	if err := tr.client.DeleteThread(cleanupContext(), threadID); err != nil {
		log.Printf("[webui] delete thread keys error thread=%s: %v", threadID, err)
		serverError(w, "failed to delete thread", err)
		return
	}

	// Delete workspace files if they exist.
	wp := workspacePath(threadID)
	if err := removeWorkspace(wp); err != nil {
		log.Printf("[webui] workspace delete error thread=%s dir=%s: %v", threadID, wp, err)
	}

	// Delete session file.
	if sessionID != "" {
		removeSessionFile(sessionID)
	}

	log.Printf("[webui] thread deleted thread=%s", threadID)
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
		log.Printf("[webui] clear session id error thread=%s: %v", threadID, err)
		serverError(w, "internal error", err)
		return
	}

	log.Printf("[webui] session reset thread=%s", threadID)
	Respond(w, r, http.StatusOK, map[string]string{"status": "session reset"})
}
