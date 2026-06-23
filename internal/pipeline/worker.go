package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"

	"github.com/yourorg/logai/internal/anthropic"
	"github.com/yourorg/logai/internal/config"
	"github.com/yourorg/logai/internal/db"
	"github.com/yourorg/logai/internal/gitlab"
	"github.com/yourorg/logai/internal/models"
)

// TaskProcessError is the Asynq task type for the full bug-fix pipeline.
const TaskProcessError = "logai:process_error"

// WorkerConcurrency is the number of pipeline tasks processed in parallel.
const WorkerConcurrency = 2

// pipelineTimeout bounds a single full pipeline run.
const pipelineTimeout = 5 * time.Minute

// ProcessErrorPayload is the JSON payload carried by a process_error task.
type ProcessErrorPayload struct {
	ErrorID string `json:"error_id"`
}

// NewProcessTask builds an Asynq task for the given error id.
func NewProcessTask(errorID string) (*asynq.Task, error) {
	payload, err := json.Marshal(ProcessErrorPayload{ErrorID: errorID})
	if err != nil {
		return nil, fmt.Errorf("pipeline: marshal task payload: %w", err)
	}
	return asynq.NewTask(TaskProcessError, payload), nil
}

// Processor holds the dependencies shared by every pipeline stage.
type Processor struct {
	DB        *db.DB
	Anthropic *anthropic.Client
	GitLab    *gitlab.Client
	Cfg       *config.Config
	Logger    *slog.Logger
}

// NewProcessor wires up a Processor.
func NewProcessor(database *db.DB, ac *anthropic.Client, gc *gitlab.Client, cfg *config.Config, logger *slog.Logger) *Processor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Processor{DB: database, Anthropic: ac, GitLab: gc, Cfg: cfg, Logger: logger}
}

// HandleProcessError is the Asynq handler. It loads the error and runs every
// pipeline stage in sequence, honoring early-exit conditions.
func (p *Processor) HandleProcessError(ctx context.Context, t *asynq.Task) error {
	var payload ProcessErrorPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		// Non-retryable: a malformed payload will never succeed.
		return fmt.Errorf("pipeline: unmarshal payload: %v: %w", err, asynq.SkipRetry)
	}

	ctx, cancel := context.WithTimeout(ctx, pipelineTimeout)
	defer cancel()

	e, err := p.DB.GetError(ctx, payload.ErrorID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("pipeline: error %s not found: %w", payload.ErrorID, asynq.SkipRetry)
		}
		return fmt.Errorf("pipeline: load error: %w", err)
	}

	log := p.Logger.With("error_id", e.ID)
	log.Info("pipeline started", "service", e.Service, "status", e.Status)

	if err := p.DB.UpdateErrorStatus(ctx, e.ID, models.StatusProcessing); err != nil {
		return fmt.Errorf("pipeline: mark processing: %w", err)
	}

	// Stage 1: triage.
	triage, err := p.runTriage(ctx, e)
	if err != nil {
		p.markFailed(ctx, e.ID, log)
		return fmt.Errorf("pipeline: triage: %w", err)
	}
	if !triage.ShouldFix || triage.RiskLevel == "low" {
		log.Info("pipeline stopped after triage", "should_fix", triage.ShouldFix, "risk_level", triage.RiskLevel)
		if err := p.DB.UpdateErrorStatus(ctx, e.ID, models.StatusSkipped); err != nil {
			return fmt.Errorf("pipeline: mark skipped: %w", err)
		}
		return nil
	}
	// Refresh triage fields onto the in-memory error.
	e.RiskLevel = triage.RiskLevel
	e.AffectedService = triage.AffectedService

	// Stage 2: localize.
	loc, err := p.runLocalize(ctx, e, triage)
	if err != nil {
		p.markFailed(ctx, e.ID, log)
		return fmt.Errorf("pipeline: localize: %w", err)
	}
	if loc.Confidence == "low" {
		log.Info("pipeline stopped after localize", "confidence", loc.Confidence)
		if err := p.DB.UpdateErrorStatus(ctx, e.ID, models.StatusSkipped); err != nil {
			return fmt.Errorf("pipeline: mark skipped: %w", err)
		}
		return nil
	}

	// Stage 3: fix.
	fix, err := p.runFix(ctx, e, loc)
	if err != nil {
		p.markFailed(ctx, e.ID, log)
		return fmt.Errorf("pipeline: fix: %w", err)
	}

	// Stage 4: gitlab.
	if err := p.runGitLab(ctx, e, triage, loc, fix); err != nil {
		p.markFailed(ctx, e.ID, log)
		return fmt.Errorf("pipeline: gitlab: %w", err)
	}

	log.Info("pipeline completed")
	return nil
}

// markFailed flips the error to failed and logs any secondary error.
func (p *Processor) markFailed(ctx context.Context, errorID string, log *slog.Logger) {
	// Use a detached context so we can still record state if ctx was cancelled.
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.DB.UpdateErrorStatus(bg, errorID, models.StatusFailed); err != nil {
		log.Error("failed to mark error failed", "err", err)
	}
}

// startJob inserts a running job row and returns its id.
func (p *Processor) startJob(ctx context.Context, errorID, stage string) (string, error) {
	id := newID()
	j := models.Job{
		ID:      id,
		ErrorID: errorID,
		Stage:   stage,
		Status:  models.JobRunning,
		Attempt: 1,
	}
	if err := p.DB.InsertJob(ctx, j); err != nil {
		return "", fmt.Errorf("pipeline: start job %s: %w", stage, err)
	}
	return id, nil
}

// finishJob records a successful job result.
func (p *Processor) finishJob(ctx context.Context, jobID string, result any) error {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("pipeline: marshal job result: %w", err)
	}
	if err := p.DB.UpdateJob(ctx, jobID, models.JobSucceeded, string(resultJSON), ""); err != nil {
		return fmt.Errorf("pipeline: finish job: %w", err)
	}
	return nil
}

// failJob records a failed job with its error message.
func (p *Processor) failJob(ctx context.Context, jobID string, cause error) {
	// Best-effort; the stage error is what propagates.
	_ = p.DB.UpdateJob(ctx, jobID, models.JobFailed, "", cause.Error())
}
