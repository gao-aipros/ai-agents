package api

import (
	"encoding/json"
	"net/http"
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

// Respond writes a response. When the HX-Request header is present, it
// returns JSON for now (HTML partial rendering is wired in Step 6).
func Respond(w http.ResponseWriter, r *http.Request, status int, v interface{}) {
	JSON(w, status, v)
}
