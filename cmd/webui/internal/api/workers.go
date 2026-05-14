package api

import (
	"net/http"

	"github.com/noodle05/ai-agents/cmd/webui/internal/templates"
	"github.com/noodle05/ai-agents/tasklib"
)

type workersResource struct {
	client   *tasklib.Client
	renderer *templates.Renderer
}

// GET /api/workers
func (wr *workersResource) list(w http.ResponseWriter, r *http.Request) {
	workers, err := wr.client.GetWorkerStats(r.Context())
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	if IsHTMX(r) {
		Partial(w, wr.renderer, "worker-cards", map[string]interface{}{"Workers": workers})
	} else {
		Respond(w, r, http.StatusOK, workers)
	}
}

// GET /api/workers/{worker_type}
func (wr *workersResource) get(w http.ResponseWriter, r *http.Request) {
	workerType := r.PathValue("worker_type")

	valid := false
	for _, wt := range tasklib.WorkerTypes {
		if wt == workerType {
			valid = true
			break
		}
	}
	if !valid {
		Error(w, http.StatusNotFound, "unknown worker type")
		return
	}

	info, err := wr.client.GetWorkerInfo(r.Context(), workerType)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	Respond(w, r, http.StatusOK, info)
}
