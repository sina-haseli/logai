package models

import "time"

// Error status values.
const (
	StatusNew        = "new"
	StatusProcessing = "processing"
	StatusSkipped    = "skipped"
	StatusFixed      = "fixed"
	StatusFailed     = "failed"
)

// Job stage values.
const (
	StageTriage   = "triage"
	StageLocalize = "localize"
	StageFix      = "fix"
	StageGitLab   = "gitlab"
)

// Job status values.
const (
	JobPending   = "pending"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
)

// IncomingError is the normalized shape produced by every ingestion source
// (OpenSearch poll or webhook) before it is persisted.
type IncomingError struct {
	Message    string `json:"message"`
	StackTrace string `json:"stack_trace"`
	Service    string `json:"service"`
	Severity   string `json:"severity"`
	Timestamp  string `json:"timestamp"`
}

// Error is a persisted error row.
type Error struct {
	ID              string    `json:"id"`
	Fingerprint     string    `json:"fingerprint"`
	Source          string    `json:"source"`
	RawJSON         string    `json:"raw_json"`
	Message         string    `json:"message"`
	StackTrace      string    `json:"stack_trace"`
	Service         string    `json:"service"`
	Status          string    `json:"status"`
	RiskLevel       string    `json:"risk_level"`
	RiskReason      string    `json:"risk_reason"`
	AffectedService string    `json:"affected_service"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Job is a persisted pipeline-stage row.
type Job struct {
	ID           string    `json:"id"`
	ErrorID      string    `json:"error_id"`
	Stage        string    `json:"stage"`
	Status       string    `json:"status"`
	ResultJSON   string    `json:"result_json"`
	ErrorMessage string    `json:"error_message"`
	Attempt      int       `json:"attempt"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// MergeRequest is a persisted MR row.
type MergeRequest struct {
	ID         string    `json:"id"`
	ErrorID    string    `json:"error_id"`
	GitLabIID  int       `json:"gitlab_mr_iid"`
	GitLabURL  string    `json:"gitlab_mr_url"`
	BranchName string    `json:"branch_name"`
	IsDraft    bool      `json:"is_draft"`
	CreatedAt  time.Time `json:"created_at"`
}

// --- Claude structured outputs ---

// TriageResult is the strict JSON shape returned by the triage stage.
type TriageResult struct {
	RiskLevel       string `json:"risk_level"`
	RiskReason      string `json:"risk_reason"`
	ShouldFix       bool   `json:"should_fix"`
	AffectedService string `json:"affected_service"`
}

// LocalizeResult is the strict JSON shape returned by the localize stage.
type LocalizeResult struct {
	FilePath     string `json:"file_path"`
	LineNumber   int    `json:"line_number"`
	FunctionName string `json:"function_name"`
	Confidence   string `json:"confidence"`
	Reasoning    string `json:"reasoning"`
}

// FixResult is the strict JSON shape returned by the fix stage.
type FixResult struct {
	FixedCode  string `json:"fixed_code"`
	FixSummary string `json:"fix_summary"`
	Confidence string `json:"confidence"`
}
