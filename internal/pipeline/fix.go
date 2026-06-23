package pipeline

import (
	"context"
	"fmt"

	"github.com/yourorg/logai/internal/anthropic"
	"github.com/yourorg/logai/internal/models"
)

const fixSystemPrompt = `You are a senior software engineer performing a careful, minimal bug fix. You will receive source code and information about a bug. Apply the smallest possible fix. Respond ONLY with valid JSON — no markdown, no explanation: { "fixed_code": "entire file content with fix applied", "fix_summary": "string max 200 chars", "confidence": "high" | "medium" | "low" }`

// fixOutcome bundles the model result with the original file content so the
// gitlab stage can commit the change.
type fixOutcome struct {
	models.FixResult
	OriginalCode string
}

// runFix executes stage 3: fetch the file from GitLab, then ask Claude to fix it.
func (p *Processor) runFix(ctx context.Context, e models.Error, loc models.LocalizeResult) (fixOutcome, error) {
	jobID, err := p.startJob(ctx, e.ID, models.StageFix)
	if err != nil {
		return fixOutcome{}, err
	}

	original, err := p.GitLab.GetFileContent(ctx, loc.FilePath, p.Cfg.GitLabDefaultBranch)
	if err != nil {
		p.failJob(ctx, jobID, err)
		return fixOutcome{}, fmt.Errorf("fix: fetch file: %w", err)
	}

	userPrompt := fmt.Sprintf(
		"File path: %s\nLine number: %d\nFunction name: %s\n\nError message:\n%s\n\nStack trace:\n%s\n\nFull file content:\n%s",
		loc.FilePath, loc.LineNumber, loc.FunctionName, e.Message, e.StackTrace, original)

	raw, err := p.Anthropic.Complete(ctx, models.StageFix, fixSystemPrompt, userPrompt)
	if err != nil {
		p.failJob(ctx, jobID, err)
		return fixOutcome{}, fmt.Errorf("fix: complete: %w", err)
	}

	var result models.FixResult
	if err := anthropic.ParseJSON(raw, &result); err != nil {
		p.failJob(ctx, jobID, err)
		return fixOutcome{}, fmt.Errorf("fix: %w", err)
	}

	if result.FixedCode == "" || result.FixedCode == original {
		err := fmt.Errorf("fix: Claude returned no changes")
		p.failJob(ctx, jobID, err)
		return fixOutcome{}, err
	}

	if err := p.finishJob(ctx, jobID, result); err != nil {
		return fixOutcome{}, err
	}

	p.Logger.Info("fix done",
		"error_id", e.ID, "stage", models.StageFix, "confidence", result.Confidence)

	return fixOutcome{FixResult: result, OriginalCode: original}, nil
}
