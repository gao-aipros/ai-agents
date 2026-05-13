package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/noodle05/ai-agents/tasklib"
)

type threadsResource struct {
	client *tasklib.Client
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
		threadID = simpleUUID()
	}

	exists, err := tr.client.ThreadExists(r.Context(), threadID)
	if err != nil {
		log.Printf("[webui] thread exists check error: %v", err)
		Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if exists {
		Error(w, http.StatusConflict, "thread already exists")
		return
	}

	thread, err := tr.client.CreateThread(r.Context(), threadID, b.Repo)
	if err != nil {
		log.Printf("[webui] create thread error: %v", err)
		Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	Respond(w, r, http.StatusCreated, thread)
}

// GET /api/threads
func (tr *threadsResource) list(w http.ResponseWriter, r *http.Request) {
	threads, err := tr.client.ListThreads(r.Context())
	if err != nil {
		log.Printf("[webui] list threads error: %v", err)
		Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	Respond(w, r, http.StatusOK, threads)
}

// GET /api/threads/{thread_id}
func (tr *threadsResource) get(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")
	thread, err := tr.client.GetThread(r.Context(), threadID)
	if err != nil {
		Error(w, http.StatusNotFound, err.Error())
		return
	}

	running, _ := tr.client.IsRequestRunning(r.Context(), threadID)
	complete, _ := tr.client.IsThreadComplete(r.Context(), threadID)

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
		log.Printf("[webui] thread history error thread=%s: %v", threadID, err)
		Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	Respond(w, r, http.StatusOK, messages)
}

// DELETE /api/threads/{thread_id}/workspace
func (tr *threadsResource) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")

	if r.URL.Query().Get("confirm") != "true" {
		Error(w, http.StatusBadRequest, "require ?confirm=true")
		return
	}

	// Check for running tasks on this thread
	tasks, err := tr.client.ListTasks(r.Context(), "", "running", threadID, 1, 0)
	if err != nil {
		log.Printf("[webui] list running tasks error thread=%s: %v", threadID, err)
		Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(tasks) > 0 {
		Error(w, http.StatusBadRequest, "thread has running tasks")
		return
	}

	// Set deleting sentinel
	ok, err := tr.client.AcquireRequestLock(r.Context(), threadID, "deleting", tasklib.LockTTL)
	if err != nil {
		log.Printf("[webui] deleting sentinel error thread=%s: %v", threadID, err)
		Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		Error(w, http.StatusConflict, "thread is in use")
		return
	}
	defer tr.client.ReleaseRequestLock(r.Context(), threadID)

	wp := workspacePath(threadID)
	if err := removeWorkspace(wp); err != nil {
		log.Printf("[webui] workspace delete error thread=%s dir=%s: %v", threadID, wp, err)
		Error(w, http.StatusInternalServerError, "failed to delete workspace")
		return
	}

	log.Printf("[webui] workspace deleted thread=%s", threadID)
	Respond(w, r, http.StatusOK, map[string]string{"status": "deleted"})
}

// POST /api/threads/{thread_id}/keep
func (tr *threadsResource) keep(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")

	if err := tr.client.SetThreadTTL(r.Context(), threadID, tasklib.TTLThread); err != nil {
		log.Printf("[webui] keep thread error thread=%s: %v", threadID, err)
		Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	Respond(w, r, http.StatusOK, map[string]string{"status": "kept"})
}

// POST /api/threads/{thread_id}/reset-session
func (tr *threadsResource) resetSession(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")

	sessionID, err := tr.client.GetThreadSessionID(r.Context(), threadID)
	if err != nil {
		log.Printf("[webui] get session id error thread=%s: %v", threadID, err)
		Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	if sessionID != "" {
		removeSessionFile(sessionID)
	}

	if err := tr.client.SetThreadSessionID(r.Context(), threadID, ""); err != nil {
		log.Printf("[webui] clear session id error thread=%s: %v", threadID, err)
		Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	log.Printf("[webui] session reset thread=%s", threadID)
	Respond(w, r, http.StatusOK, map[string]string{"status": "session reset"})
}
