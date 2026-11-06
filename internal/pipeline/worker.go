package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"

	"github.com/yourorg/logai/internal/config"
	"github.com/yourorg/logai/internal/db"
	"github.com/yourorg/logai/internal/gitlab"
	"github.com/yourorg/logai/internal/llm"
	"github.com/yourorg/logai/internal/models"
)

// TaskProcessError is the Asynq task type for the full bug-fix pipeline.
const TaskProcessError = "logai:process_error"

// WorkerConcurrency is the number of pipeline tasks processed in parallel.
const WorkerConcurrency = 2

// MaxRetry is how many times Asynq retries a failed pipeline run.
const MaxRetry = 3

// pipelineTimeout bounds a single full pipeline run. The fix stage alone can
// take several minutes when the model reasons about and rewrites a large file.
const pipelineTimeout = 20 * time.Minute

// Per-stage response budgets. These cap reasoning AND answer tokens together:
// Sonnet 5.5 always reasons (adaptively), so even the small-JSON stages need
// headroom beyond the answer itself. Fix returns the entire source file inside
// JSON, so it gets the most.
const (
	triageMaxTokens   = 8000
	localizeMaxTokens = 8000
	fixMaxTokens      = 32000
)

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
	DB     *db.DB
	LLM    *llm.Client
	GitLab *gitlab.Client
	Cfg    *config.Config
	Logger *slog.Logger
}

// NewProcessor wires up a Processor.
func NewProcessor(database *db.DB, lc *llm.Client, gc *gitlab.Client, cfg *config.Config, logger *slog.Logger) *Processor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Processor{DB: database, LLM: lc, GitLab: gc, Cfg: cfg, Logger: logger}
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

	attempt := attemptNumber(ctx)
	log := p.Logger.With("error_id", e.ID, "attempt", attempt)
	log.Info("pipeline started", "service", e.Service, "status", e.Status)

	if err := p.DB.UpdateErrorStatus(ctx, e.ID, models.StatusProcessing); err != nil {
		return fmt.Errorf("pipeline: mark processing: %w", err)
	}

	if err := p.run(ctx, e, attempt, log); err != nil {
		err = classify(err)
		p.markFailed(ctx, e.ID, willRetry(ctx, err), log)
		return err
	}
	return nil
}

// run executes every stage in sequence, honoring early-exit conditions.
func (p *Processor) run(ctx context.Context, e models.Error, attempt int, log *slog.Logger) error {
	ctx = withAttempt(ctx, attempt)

	// Stage 1: triage.
	triage, err := p.runTriage(ctx, e)
	if err != nil {
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
		return fmt.Errorf("pipeline: fix: %w", err)
	}

	// Stage 4: gitlab.
	if err := p.runGitLab(ctx, e, triage, loc, fix); err != nil {
		return fmt.Errorf("pipeline: gitlab: %w", err)
	}

	log.Info("pipeline completed")
	return nil
}

// classify marks failures that will deterministically repeat as
// non-retryable, so Asynq doesn't burn retries (and LLM tokens) on them.
func classify(err error) error {
	switch {
	case errors.Is(err, asynq.SkipRetry):
		return err
	case errors.Is(err, gitlab.ErrFileNotFound),
		errors.Is(err, llm.ErrTruncated),
		errors.Is(err, llm.ErrRefused):
		return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
	}
	return err
}

// willRetry reports whether Asynq will run this task again after err.
func willRetry(ctx context.Context, err error) bool {
	if errors.Is(err, asynq.SkipRetry) {
		return false
	}
	retried, ok1 := asynq.GetRetryCount(ctx)
	maxRetry, ok2 := asynq.GetMaxRetry(ctx)
	if !ok1 || !ok2 {
		return false
	}
	return retried < maxRetry
}

// attemptNumber is the 1-based run number of the current task.
func attemptNumber(ctx context.Context) int {
	if n, ok := asynq.GetRetryCount(ctx); ok {
		return n + 1
	}
	return 1
}

type attemptKey struct{}

func withAttempt(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, attemptKey{}, n)
}

func attemptFrom(ctx context.Context) int {
	if n, ok := ctx.Value(attemptKey{}).(int); ok {
		return n
	}
	return 1
}

// markFailed records the outcome of a failed run. If Asynq will retry, the
// error goes back to "new" (queued for retry); otherwise it is "failed".
func (p *Processor) markFailed(ctx context.Context, errorID string, retrying bool, log *slog.Logger) {
	status := models.StatusFailed
	if retrying {
		status = models.StatusNew
	}
	// Use a detached context so we can still record state if ctx was cancelled.
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := p.DB.UpdateErrorStatus(bg, errorID, status); err != nil {
		log.Error("failed to record error status", "status", status, "err", err)
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
		Attempt: attemptFrom(ctx),
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
	// Best-effort; the stage error is what propagates. Detached context so a
	// timed-out stage still gets its failure recorded.
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = p.DB.UpdateJob(bg, jobID, models.JobFailed, "", cause.Error())
}
