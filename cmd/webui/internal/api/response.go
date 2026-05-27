package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/noodle05/ai-agents/cmd/webui/internal/templates"
)

// JSON writes a JSON response with the given status code and value.
func JSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// Error writes a JSON error response.
func Error(w http.ResponseWriter, status int, msg string) {
	JSON(w, status, map[string]string{"error": msg})
}

// IsHTMX returns true if the request has the HX-Request header set.
func IsHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// Page writes a full HTML page response using the renderer.
// contentTemplate names the page template (e.g. "page-dashboard") whose output
// is injected into base.html via {{.PageContent}}.
// Renders into a buffer first so a partial failure doesn't produce a corrupt 200.
func Page(w http.ResponseWriter, r *templates.Renderer, contentTemplate string, vm templates.ViewModel) {
	var buf bytes.Buffer
	if err := r.Page(&buf, contentTemplate, vm); err != nil {
		slog.Warn(fmt.Sprintf("[webui] template page error: %v", err))
		Error(w, http.StatusInternalServerError, "internal server error")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

// Partial writes an HTML partial response using the renderer.
// Renders into a buffer first so a partial failure doesn't produce a corrupt 200.
func Partial(w http.ResponseWriter, r *templates.Renderer, name string, data interface{}) {
	var buf bytes.Buffer
	if err := r.Partial(&buf, name, data); err != nil {
		slog.Warn(fmt.Sprintf("[webui] template partial %s error: %v", name, err))
		Error(w, http.StatusInternalServerError, "internal server error")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

// Respond writes a JSON response. Callers that need HTML partials for HTMX
// requests should check IsHTMX and call Partial directly.
func Respond(w http.ResponseWriter, r *http.Request, status int, v interface{}) {
	JSON(w, status, v)
}
