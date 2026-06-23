package pipeline

import (
	"context"
	"fmt"

	"github.com/yourorg/logai/internal/anthropic"
	"github.com/yourorg/logai/internal/models"
)

const localizeSystemPrompt = `You are a senior software engineer. Given this stack trace, find the most likely root cause in the application's own source code — not in vendor, node_modules, or standard library frames. Respond ONLY with valid JSON — no markdown, no explanation: { "file_path": "relative/path/to/file.go", "line_number": 42, "function_name": "string", "confidence": "high" | "medium" | "low", "reasoning": "string max 150 chars" }`

// runLocalize executes stage 2: ask Claude to locate the buggy file.
func (p *Processor) runLocalize(ctx context.Context, e models.Error, triage models.TriageResult) (models.LocalizeResult, error) {
	jobID, err := p.startJob(ctx, e.ID, models.StageLocalize)
	if err != nil {
		return models.LocalizeResult{}, err
	}

	userPrompt := fmt.Sprintf(
		"Stack trace:\n%s\n\nService:\n%s\n\nAffected service (from triage):\n%s",
		e.StackTrace, e.Service, triage.AffectedService)

	raw, err := p.Anthropic.Complete(ctx, models.StageLocalize, localizeSystemPrompt, userPrompt)
	if err != nil {
		p.failJob(ctx, jobID, err)
		return models.LocalizeResult{}, fmt.Errorf("localize: complete: %w", err)
	}

	var result models.LocalizeResult
	if err := anthropic.ParseJSON(raw, &result); err != nil {
		p.failJob(ctx, jobID, err)
		return models.LocalizeResult{}, fmt.Errorf("localize: %w", err)
	}

	if result.FilePath == "" {
		err := fmt.Errorf("localize: claude returned empty file_path")
		p.failJob(ctx, jobID, err)
		return models.LocalizeResult{}, err
	}

	if err := p.finishJob(ctx, jobID, result); err != nil {
		return models.LocalizeResult{}, err
	}

	p.Logger.Info("localize done",
		"error_id", e.ID, "stage", models.StageLocalize,
		"file_path", result.FilePath, "line", result.LineNumber, "confidence", result.Confidence)

	return result, nil
}
