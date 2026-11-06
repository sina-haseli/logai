package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/yourorg/logai/internal/llm"
	"github.com/yourorg/logai/internal/models"
)

const fixSystemPrompt = `You are a senior software engineer performing a careful, minimal bug fix. You will receive source code and information about a bug. Apply the smallest possible fix. Preserve the file's existing formatting, imports, and style. Respond ONLY with valid JSON — no markdown, no explanation: { "fixed_code": "entire file content with fix applied", "fix_summary": "string max 200 chars", "confidence": "high" | "medium" | "low" }`

// fixOutcome bundles the model result with the original file content and the
// resolved repository path so the gitlab stage can commit the change.
type fixOutcome struct {
	models.FixResult
	// FilePath is the repository-relative path the file was found at. It can
	// differ from the localized path, which comes from the stack trace.
	FilePath     string `json:"file_path"`
	OriginalCode string `json:"-"`
}

// runFix executes stage 3: fetch the file from GitLab, then ask the model to fix it.
func (p *Processor) runFix(ctx context.Context, e models.Error, loc models.LocalizeResult) (fixOutcome, error) {
	jobID, err := p.startJob(ctx, e.ID, models.StageFix)
	if err != nil {
		return fixOutcome{}, err
	}

	repoPath, original, err := p.GitLab.ResolveFile(ctx, loc.FilePath, p.Cfg.GitLabDefaultBranch)
	if err != nil {
		p.failJob(ctx, jobID, err)
		return fixOutcome{}, fmt.Errorf("fix: fetch file: %w", err)
	}

	userPrompt := fmt.Sprintf(
		"File path: %s\nLine number: %d\nFunction name: %s\n\nError message:\n%s\n\nStack trace:\n%s\n\nFull file content:\n%s",
		repoPath, loc.LineNumber, loc.FunctionName, e.Message, e.StackTrace, original)

	raw, err := p.LLM.Complete(ctx, models.StageFix, fixSystemPrompt, userPrompt, fixMaxTokens)
	if err != nil {
		p.failJob(ctx, jobID, err)
		return fixOutcome{}, fmt.Errorf("fix: complete: %w", err)
	}

	var result models.FixResult
	if err := llm.ParseJSON(raw, &result); err != nil {
		p.failJob(ctx, jobID, err)
		return fixOutcome{}, fmt.Errorf("fix: %w", err)
	}

	if strings.TrimSpace(result.FixedCode) == "" || result.FixedCode == original {
		err := fmt.Errorf("fix: model returned no changes")
		p.failJob(ctx, jobID, err)
		return fixOutcome{}, err
	}

	out := fixOutcome{FixResult: result, FilePath: repoPath, OriginalCode: original}
	if err := p.finishJob(ctx, jobID, out); err != nil {
		return fixOutcome{}, err
	}

	p.Logger.Info("fix done",
		"error_id", e.ID, "stage", models.StageFix, "file_path", repoPath, "confidence", result.Confidence)

	return out, nil
}
