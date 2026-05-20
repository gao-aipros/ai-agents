package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/noodle05/ai-agents/tasklib"
)

type eventsResource struct {
	client *tasklib.Client
}

// GET /api/events?limit=50&type=worker_online
func (er *eventsResource) systemEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	eventType := r.URL.Query().Get("type")

	// Fetch a larger batch when type-filtering to improve hit rate
	fetchLimit := limit
	if eventType != "" {
		fetchLimit = limit * 3
		if fetchLimit > 1000 {
			fetchLimit = 1000
		}
	}
	events, err := er.client.GetSystemEvents(r.Context(), fetchLimit)
	if err != nil {
		slog.Warn(fmt.Sprintf("[webui] system events error: %v", err))
		Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Optional client-side filter by type
	if eventType != "" {
		filtered := make([]tasklib.Event, 0)
		for _, ev := range events {
			if ev.Type == eventType {
				filtered = append(filtered, ev)
			}
		}
		events = filtered
	}

	if events == nil {
		events = []tasklib.Event{}
	}
	Respond(w, r, http.StatusOK, events)
}

// GET /api/threads/{thread_id}/events?limit=50
func (er *eventsResource) threadEvents(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}

	events, err := er.client.GetThreadEvents(r.Context(), threadID, limit)
	if err != nil {
		slog.Warn(fmt.Sprintf("[webui] thread events error: %v", err))
		Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if events == nil {
		events = []tasklib.Event{}
	}
	Respond(w, r, http.StatusOK, events)
}
