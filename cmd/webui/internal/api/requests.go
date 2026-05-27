package api

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/noodle05/ai-agents/cmd/webui/internal/request"
	"github.com/noodle05/ai-agents/cmd/webui/internal/templates"
	"github.com/noodle05/ai-agents/tasklib"
)

type requestsResource struct {
	threads           tasklib.ThreadStore
	handler           *request.Handler
	renderer          *templates.Renderer
	workspaceDir      string
	claudeSessionsDir string
}

// POST /api/requests
func (rs *requestsResource) submit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ThreadID string `json:"thread_id"`
		Repo     string `json:"repo"`
		Request  string `json:"request"`
	}
	if IsHTMX(r) {
		req.ThreadID = r.FormValue("thread_id")
		req.Repo = r.FormValue("repo")
		req.Request = r.FormValue("request")
	} else {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			Error(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
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
	} else if !request.ValidThreadID(threadID) {
		Error(w, http.StatusBadRequest, "invalid thread_id")
		return
	}

	result, err := rs.handler.Submit(r.Context(), threadID, req.Request, req.Repo)
	if err != nil {
		if re, ok := err.(*request.RequestError); ok {
			Error(w, re.Status, re.Message)
		} else {
			slog.Warn(fmt.Sprintf("[webui] submit error: %v", err))
			Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	if IsHTMX(r) {
		if r.FormValue("from_thread") == "true" {
			Partial(w, rs.renderer, "reply-confirmed", result)
		} else {
			Partial(w, rs.renderer, "request-submitted", result)
		}
	} else {
		Respond(w, r, http.StatusAccepted, result)
	}
}

// POST /api/threads/{thread_id}/cancel
func (rs *requestsResource) cancel(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")

	if err := rs.handler.Cancel(threadID); err != nil {
		if re, ok := err.(*request.RequestError); ok {
			Error(w, re.Status, re.Message)
		} else {
			serverError(w, "internal error", err)
		}
		return
	}

	if err := rs.threads.CancelRequest(r.Context(), threadID); err != nil {
		slog.Warn(fmt.Sprintf("[webui] cancel request redis error thread=%s: %v", threadID, err))
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
