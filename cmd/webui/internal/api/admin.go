package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
)

// adminResource holds the mutable access logger state and factory for runtime toggling.
type adminResource struct {
	accessLog       *atomic.Pointer[slog.Logger]
	newAccessLogger func() *slog.Logger
}

// logAccessRequest is the expected JSON body for PUT /api/admin/log-access.
// Enabled is a pointer so we can distinguish missing from false.
type logAccessRequest struct {
	Enabled *bool `json:"enabled"`
}

// logAccessResponse is the JSON response for GET/PUT /api/admin/log-access.
type logAccessResponse struct {
	Enabled bool `json:"enabled"`
}

func (a *adminResource) logAccessHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.getLogAccess(w)
	case http.MethodPut:
		a.putLogAccess(w, r)
	default:
		Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *adminResource) getLogAccess(w http.ResponseWriter) {
	enabled := a.accessLog.Load() != nil
	JSON(w, http.StatusOK, logAccessResponse{Enabled: enabled})
}

func (a *adminResource) putLogAccess(w http.ResponseWriter, r *http.Request) {
	var req logAccessRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if dec.More() {
		Error(w, http.StatusBadRequest, "invalid request body: extra data after JSON object")
		return
	}
	if req.Enabled == nil {
		Error(w, http.StatusBadRequest, `missing required field "enabled"`)
		return
	}

	current := a.accessLog.Load() != nil
	if *req.Enabled == current {
		JSON(w, http.StatusOK, logAccessResponse{Enabled: current})
		return
	}

	if *req.Enabled {
		a.accessLog.Store(a.newAccessLogger())
	} else {
		a.accessLog.Store(nil)
	}
	JSON(w, http.StatusOK, logAccessResponse{Enabled: *req.Enabled})
}
