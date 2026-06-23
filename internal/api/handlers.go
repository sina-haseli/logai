package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/hibiken/asynq"

	"github.com/yourorg/logai/internal/db"
	"github.com/yourorg/logai/internal/models"
	"github.com/yourorg/logai/internal/pipeline"
)

// API holds dependencies for the REST handlers.
type API struct {
	DB        *db.DB
	Asynq     *asynq.Client
	Inspector *asynq.Inspector
	Log       *slog.Logger
}

// New constructs an API.
func New(database *db.DB, client *asynq.Client, inspector *asynq.Inspector, logger *slog.Logger) *API {
	if logger == nil {
		logger = slog.Default()
	}
	return &API{DB: database, Asynq: client, Inspector: inspector, Log: logger}
}

// RegisterRoutes mounts all REST routes onto the router.
func (a *API) RegisterRoutes(r chi.Router) {
	r.Get("/health", a.health)
	r.Get("/errors", a.listErrors)
	r.Get("/errors/{id}", a.getError)
	r.Get("/jobs", a.listJobs)
	r.Post("/retry/{errorId}", a.retry)
	r.Get("/mrs", a.listMRs)
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"status": "ok"}

	dbStatus := "ok"
	if err := a.DB.Ping(r.Context()); err != nil {
		dbStatus = "error"
		resp["status"] = "degraded"
	}
	resp["db"] = dbStatus

	queued := 0
	if queues, err := a.Inspector.Queues(); err == nil {
		for _, q := range queues {
			if info, err := a.Inspector.GetQueueInfo(q); err == nil {
				queued += info.Pending + info.Active + info.Scheduled + info.Retry
			}
		}
	}
	resp["asynq_queued"] = queued

	status := http.StatusOK
	if resp["status"] != "ok" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}

func (a *API) listErrors(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit := queryInt(r, "limit", 50)
	offset := queryInt(r, "offset", 0)

	errs, err := a.DB.ListErrors(r.Context(), status, limit, offset)
	if err != nil {
		a.serverError(w, "list errors", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"errors": errs,
		"limit":  limit,
		"offset": offset,
		"count":  len(errs),
	})
}

func (a *API) getError(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	e, err := a.DB.GetError(r.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "error not found"})
			return
		}
		a.serverError(w, "get error", err)
		return
	}

	jobs, err := a.DB.ListJobsByError(r.Context(), id)
	if err != nil {
		a.serverError(w, "get error jobs", err)
		return
	}

	resp := map[string]any{"error": e, "jobs": jobs}

	mr, err := a.DB.GetMergeRequestByError(r.Context(), id)
	switch {
	case err == nil:
		resp["merge_request"] = mr
	case errors.Is(err, db.ErrNotFound):
		resp["merge_request"] = nil
	default:
		a.serverError(w, "get error mr", err)
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (a *API) listJobs(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit := queryInt(r, "limit", 50)
	offset := queryInt(r, "offset", 0)

	jobs, err := a.DB.ListJobs(r.Context(), status, limit, offset)
	if err != nil {
		a.serverError(w, "list jobs", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs":   jobs,
		"limit":  limit,
		"offset": offset,
		"count":  len(jobs),
	})
}

func (a *API) retry(w http.ResponseWriter, r *http.Request) {
	errorID := chi.URLParam(r, "errorId")

	// Confirm the error exists before resetting it.
	if _, err := a.DB.GetError(r.Context(), errorID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "error not found"})
			return
		}
		a.serverError(w, "retry get error", err)
		return
	}

	if err := a.DB.UpdateErrorStatus(r.Context(), errorID, models.StatusNew); err != nil {
		a.serverError(w, "retry reset status", err)
		return
	}

	task, err := pipeline.NewProcessTask(errorID)
	if err != nil {
		a.serverError(w, "retry build task", err)
		return
	}
	if _, err := a.Asynq.EnqueueContext(r.Context(), task, asynq.MaxRetry(3)); err != nil {
		a.serverError(w, "retry enqueue", err)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"error_id": errorID, "status": "re-enqueued"})
}

func (a *API) listMRs(w http.ResponseWriter, r *http.Request) {
	mrs, err := a.DB.ListMergeRequests(r.Context())
	if err != nil {
		a.serverError(w, "list mrs", err)
		return
	}

	// Enrich each MR with a compact view of its error.
	type mrView struct {
		models.MergeRequest
		Error *models.Error `json:"error,omitempty"`
	}
	out := make([]mrView, 0, len(mrs))
	for _, mr := range mrs {
		v := mrView{MergeRequest: mr}
		if e, err := a.DB.GetError(r.Context(), mr.ErrorID); err == nil {
			v.Error = &e
		}
		out = append(out, v)
	}

	writeJSON(w, http.StatusOK, map[string]any{"merge_requests": out, "count": len(out)})
}

// --- helpers ---

func (a *API) serverError(w http.ResponseWriter, op string, err error) {
	a.Log.Error("api error", "op", op, "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
