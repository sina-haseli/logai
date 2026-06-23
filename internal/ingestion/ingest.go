package ingestion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"github.com/yourorg/logai/internal/db"
	"github.com/yourorg/logai/internal/models"
	"github.com/yourorg/logai/internal/pipeline"
)

// Ingestor persists incoming errors (with dedup) and enqueues pipeline tasks.
// It is shared by every ingestion source.
type Ingestor struct {
	DB    *db.DB
	Asynq *asynq.Client
	Log   *slog.Logger
}

// NewIngestor constructs an Ingestor.
func NewIngestor(database *db.DB, client *asynq.Client, logger *slog.Logger) *Ingestor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Ingestor{DB: database, Asynq: client, Log: logger}
}

// ErrDuplicate signals the incoming error was already ingested (skipped).
var ErrDuplicate = errors.New("duplicate error")

// Ingest fingerprints, deduplicates, persists, and enqueues an incoming error.
// Returns the new error id, or ErrDuplicate if the fingerprint already exists.
func (i *Ingestor) Ingest(ctx context.Context, source string, in models.IncomingError) (string, error) {
	fingerprint := Fingerprint(in.Message, in.StackTrace)

	exists, err := i.DB.FingerprintExists(ctx, fingerprint)
	if err != nil {
		return "", fmt.Errorf("ingest: check fingerprint: %w", err)
	}
	if exists {
		return "", ErrDuplicate
	}

	raw, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("ingest: marshal raw: %w", err)
	}

	id := uuid.NewString()
	e := models.Error{
		ID:          id,
		Fingerprint: fingerprint,
		Source:      source,
		RawJSON:     string(raw),
		Message:     in.Message,
		StackTrace:  in.StackTrace,
		Service:     in.Service,
		Status:      models.StatusNew,
	}

	if err := i.DB.InsertError(ctx, e); err != nil {
		// Lost a race against a concurrent insert of the same fingerprint.
		if errors.Is(err, db.ErrDuplicateFingerprint) {
			return "", ErrDuplicate
		}
		return "", fmt.Errorf("ingest: insert error: %w", err)
	}

	task, err := pipeline.NewProcessTask(id)
	if err != nil {
		return "", fmt.Errorf("ingest: build task: %w", err)
	}
	if _, err := i.Asynq.EnqueueContext(ctx, task, asynq.MaxRetry(3)); err != nil {
		return "", fmt.Errorf("ingest: enqueue task: %w", err)
	}

	i.Log.Info("error ingested", "error_id", id, "source", source, "service", in.Service)
	return id, nil
}

// Fingerprint computes SHA256 of lowercased(message) + the first 3 non-empty
// lines of the stack trace.
func Fingerprint(message, stackTrace string) string {
	var lines []string
	for _, ln := range strings.Split(stackTrace, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			lines = append(lines, t)
			if len(lines) == 3 {
				break
			}
		}
	}
	seed := strings.ToLower(message) + strings.Join(lines, "\n")
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}
