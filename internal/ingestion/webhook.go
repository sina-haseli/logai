package ingestion

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/yourorg/logai/internal/models"
)

// WebhookHandler returns a chi-compatible handler for POST /webhook/error.
func (i *Ingestor) WebhookHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in models.IncomingError
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
		if in.Message == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
			return
		}

		id, err := i.Ingest(r.Context(), "webhook", in)
		if err != nil {
			if errors.Is(err, ErrDuplicate) {
				// Already known — acknowledge idempotently.
				writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
				return
			}
			i.Log.Error("webhook ingest failed", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to ingest error"})
			return
		}

		writeJSON(w, http.StatusAccepted, map[string]string{"error_id": id})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
