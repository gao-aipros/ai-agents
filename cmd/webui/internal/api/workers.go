package api

import (
	"log"
	"net/http"

	"github.com/noodle05/ai-agents/tasklib"
)

type workersResource struct {
	client *tasklib.Client
}

// GET /api/workers
func (wr *workersResource) list(w http.ResponseWriter, r *http.Request) {
	workers, err := wr.client.GetWorkerStats(r.Context())
	if err != nil {
		log.Printf("[webui] worker stats error: %v", err)
		Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	Respond(w, r, http.StatusOK, workers)
}

// GET /api/workers/{worker_type}
func (wr *workersResource) get(w http.ResponseWriter, r *http.Request) {
	workerType := r.PathValue("worker_type")
	info, err := wr.client.GetWorkerInfo(r.Context(), workerType)
	if err != nil {
		Error(w, http.StatusNotFound, err.Error())
		return
	}
	Respond(w, r, http.StatusOK, info)
}
