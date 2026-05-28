package api

import (
	"net/http"

	"github.com/noodle05/ai-agents/cmd/webui/internal/templates"
	"github.com/noodle05/ai-agents/tasklib"
)

type workersResource struct {
	workers  tasklib.WorkerRegistry
	renderer *templates.Renderer
}

// GET /api/workers
func (wr *workersResource) list(w http.ResponseWriter, r *http.Request) {
	workers, err := wr.workers.GetWorkerStats(r.Context())
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	if IsHTMX(r) {
		Partial(w, wr.renderer, "worker-cards", &templates.WorkerView{Workers: workers})
	} else {
		Respond(w, r, http.StatusOK, workers)
	}
}

// GET /api/workers/{worker_type}
func (wr *workersResource) get(w http.ResponseWriter, r *http.Request) {
	workerType := r.PathValue("worker_type")

	info, err := wr.workers.GetWorkerInfo(r.Context(), workerType)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	Respond(w, r, http.StatusOK, info)
}

// GET /api/workers/{worker_type}/instances
func (wr *workersResource) instances(w http.ResponseWriter, r *http.Request) {
	workerType := r.PathValue("worker_type")

	instances, err := wr.workers.GetWorkerInstances(r.Context(), workerType)
	if err != nil {
		serverError(w, "internal error", err)
		return
	}
	Respond(w, r, http.StatusOK, instances)
}
