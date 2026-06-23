package pipeline

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/yourorg/logai/internal/anthropic"
	"github.com/yourorg/logai/internal/models"
)

// newID returns a fresh UUID string for job / MR rows.
func newID() string { return uuid.NewString() }

const triageSystemPrompt = `You are a senior SRE engineer. Analyze the following application error. Respond ONLY with a valid JSON object — no markdown, no explanation: { "risk_level": "critical" | "high" | "medium" | "low", "risk_reason": "string max 100 chars", "should_fix": true | false, "affected_service": "string" }`

// runTriage executes stage 1: ask Claude to classify the error.
func (p *Processor) runTriage(ctx context.Context, e models.Error) (models.TriageResult, error) {
	jobID, err := p.startJob(ctx, e.ID, models.StageTriage)
	if err != nil {
		return models.TriageResult{}, err
	}

	userPrompt := fmt.Sprintf(
		"Error message:\n%s\n\nStack trace:\n%s\n\nService:\n%s",
		e.Message, e.StackTrace, e.Service)

	raw, err := p.Anthropic.Complete(ctx, models.StageTriage, triageSystemPrompt, userPrompt)
	if err != nil {
		p.failJob(ctx, jobID, err)
		return models.TriageResult{}, fmt.Errorf("triage: complete: %w", err)
	}

	var result models.TriageResult
	if err := anthropic.ParseJSON(raw, &result); err != nil {
		p.failJob(ctx, jobID, err)
		return models.TriageResult{}, fmt.Errorf("triage: %w", err)
	}

	// Persist triage outputs on the error row.
	if err := p.DB.UpdateErrorTriage(ctx, e.ID, result.RiskLevel, result.RiskReason, result.AffectedService); err != nil {
		p.failJob(ctx, jobID, err)
		return models.TriageResult{}, fmt.Errorf("triage: %w", err)
	}

	if err := p.finishJob(ctx, jobID, result); err != nil {
		return models.TriageResult{}, err
	}

	p.Logger.Info("triage done",
		"error_id", e.ID, "stage", models.StageTriage,
		"risk_level", result.RiskLevel, "should_fix", result.ShouldFix)

	return result, nil
}
