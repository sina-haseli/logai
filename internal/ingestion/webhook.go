package ingestion

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/yourorg/logai/internal/models"
)

// maxWebhookBody caps the request body; stack traces are large but not 1 MiB large.
const maxWebhookBody = 1 << 20

// WebhookHandler returns a chi-compatible handler for POST /webhook/error.
// When secret is non-empty, callers must send it via the X-Logai-Token header
// or "Authorization: Bearer <secret>"; otherwise the request is rejected 401.
func (i *Ingestor) WebhookHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret != "" && !authorized(r, secret) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
		var in models.IncomingError
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
		if strings.TrimSpace(in.Message) == "" {
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

// authorized checks the shared secret in constant time.
func authorized(r *http.Request, secret string) bool {
	got := r.Header.Get("X-Logai-Token")
	if got == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			got = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
