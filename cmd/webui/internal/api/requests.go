package api

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/noodle05/ai-agents/cmd/webui/internal/request"
	"github.com/noodle05/ai-agents/tasklib"
)

type requestsResource struct {
	client  *tasklib.Client
	handler *request.Handler
}

// POST /api/requests
func (rs *requestsResource) submit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ThreadID string `json:"thread_id"`
		Repo     string `json:"repo"`
		Request  string `json:"request"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.Request == "" {
		Error(w, http.StatusBadRequest, "request field is required")
		return
	}
	if len(req.Request) > 32*1024 {
		Error(w, http.StatusRequestEntityTooLarge, "request exceeds 32KB limit")
		return
	}

	threadID := req.ThreadID
	if threadID == "" {
		threadID = generateThreadID()
	}

	result, err := rs.handler.Submit(r.Context(), threadID, req.Request, req.Repo)
	if err != nil {
		if re, ok := err.(*request.RequestError); ok {
			Error(w, re.Status, re.Message)
		} else {
			log.Printf("[webui] submit error: %v", err)
			Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	Respond(w, r, http.StatusAccepted, result)
}

// POST /api/threads/{thread_id}/cancel
func (rs *requestsResource) cancel(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")

	// Cancel the subprocess context (SIGTERM → SIGKILL)
	if err := rs.handler.Cancel(threadID); err != nil {
		if re, ok := err.(*request.RequestError); ok {
			Error(w, re.Status, re.Message)
		} else {
			serverError(w, "internal error", err)
		}
		return
	}

	// Set thread status to cancelled in Redis
	if err := rs.client.CancelRequest(r.Context(), threadID); err != nil {
		log.Printf("[webui] cancel request redis error thread=%s: %v", threadID, err)
	}

	Respond(w, r, http.StatusOK, map[string]string{"status": "cancelled"})
}

// ── helpers ────────────────────────────────────────────────────────────────

func generateThreadID() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("web_%d_%x", time.Now().Unix(), time.Now().UnixNano())
	}
	const chars = "0123456789abcdefghijklmnopqrstuvwxyz"
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return fmt.Sprintf("web_%d_%s", time.Now().Unix(), string(b))
}
