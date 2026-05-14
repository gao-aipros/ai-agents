package api

import (
	"encoding/json"
	"log"
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
func Page(w http.ResponseWriter, r *templates.Renderer, data interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := r.Page(w, data); err != nil {
		log.Printf("[webui] template page error: %v", err)
	}
}

// Partial writes an HTML partial response using the renderer.
func Partial(w http.ResponseWriter, r *templates.Renderer, name string, data interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := r.Partial(w, name, data); err != nil {
		log.Printf("[webui] template partial %s error: %v", name, err)
	}
}

// Respond writes a response. When the HX-Request header is present and a
// template name is provided, it renders an HTML partial. Otherwise, JSON.
func Respond(w http.ResponseWriter, r *http.Request, status int, v interface{}) {
	JSON(w, status, v)
}
