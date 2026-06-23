package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/yourorg/logai/internal/models"
)

// ErrNotFound is returned when a row lookup yields no rows.
var ErrNotFound = errors.New("not found")

// ErrDuplicateFingerprint is returned when inserting an error whose fingerprint
// already exists (deduplication).
var ErrDuplicateFingerprint = errors.New("duplicate fingerprint")

// --- errors table ---

// InsertError inserts a new error row. If the fingerprint already exists it
// returns ErrDuplicateFingerprint so callers can skip silently.
func (d *DB) InsertError(ctx context.Context, e models.Error) error {
	const q = `
INSERT INTO errors (id, fingerprint, source, raw_json, message, stack_trace, service, status)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := d.sql.ExecContext(ctx, q,
		e.ID, e.Fingerprint, e.Source, e.RawJSON, e.Message, e.StackTrace, e.Service, models.StatusNew)
	if err != nil {
		// modernc returns a constraint error string for UNIQUE violations.
		if isUniqueViolation(err) {
			return ErrDuplicateFingerprint
		}
		return fmt.Errorf("db: insert error: %w", err)
	}
	return nil
}

// FingerprintExists reports whether an error with the fingerprint is stored.
func (d *DB) FingerprintExists(ctx context.Context, fingerprint string) (bool, error) {
	const q = `SELECT 1 FROM errors WHERE fingerprint = ? LIMIT 1`
	var x int
	err := d.sql.QueryRowContext(ctx, q, fingerprint).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("db: fingerprint exists: %w", err)
	}
	return true, nil
}

// GetError loads a single error by id.
func (d *DB) GetError(ctx context.Context, id string) (models.Error, error) {
	const q = `
SELECT id, fingerprint, source, raw_json, message, stack_trace,
       COALESCE(service,''), status, COALESCE(risk_level,''),
       COALESCE(risk_reason,''), COALESCE(affected_service,''),
       created_at, updated_at
FROM errors WHERE id = ?`
	var e models.Error
	err := d.sql.QueryRowContext(ctx, q, id).Scan(
		&e.ID, &e.Fingerprint, &e.Source, &e.RawJSON, &e.Message, &e.StackTrace,
		&e.Service, &e.Status, &e.RiskLevel, &e.RiskReason, &e.AffectedService,
		&e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return models.Error{}, ErrNotFound
	}
	if err != nil {
		return models.Error{}, fmt.Errorf("db: get error: %w", err)
	}
	return e, nil
}

// ListErrors returns errors filtered by optional status, paginated.
func (d *DB) ListErrors(ctx context.Context, status string, limit, offset int) ([]models.Error, error) {
	q := `
SELECT id, fingerprint, source, raw_json, message, stack_trace,
       COALESCE(service,''), status, COALESCE(risk_level,''),
       COALESCE(risk_reason,''), COALESCE(affected_service,''),
       created_at, updated_at
FROM errors`
	args := []any{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list errors: %w", err)
	}
	defer rows.Close()

	var out []models.Error
	for rows.Next() {
		var e models.Error
		if err := rows.Scan(
			&e.ID, &e.Fingerprint, &e.Source, &e.RawJSON, &e.Message, &e.StackTrace,
			&e.Service, &e.Status, &e.RiskLevel, &e.RiskReason, &e.AffectedService,
			&e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: list errors scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpdateErrorStatus sets the status (and updated_at) for an error.
func (d *DB) UpdateErrorStatus(ctx context.Context, id, status string) error {
	const q = `UPDATE errors SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`
	if _, err := d.sql.ExecContext(ctx, q, status, id); err != nil {
		return fmt.Errorf("db: update error status: %w", err)
	}
	return nil
}

// UpdateErrorTriage records triage outputs on the error row.
func (d *DB) UpdateErrorTriage(ctx context.Context, id, riskLevel, riskReason, affectedService string) error {
	const q = `
UPDATE errors
SET risk_level = ?, risk_reason = ?, affected_service = ?, updated_at = CURRENT_TIMESTAMP
WHERE id = ?`
	if _, err := d.sql.ExecContext(ctx, q, riskLevel, riskReason, affectedService, id); err != nil {
		return fmt.Errorf("db: update error triage: %w", err)
	}
	return nil
}

// --- jobs table ---

// InsertJob creates a job row in the given (usually running) state and returns it.
func (d *DB) InsertJob(ctx context.Context, j models.Job) error {
	const q = `
INSERT INTO jobs (id, error_id, stage, status, result_json, error_message, attempt)
VALUES (?, ?, ?, ?, ?, ?, ?)`
	_, err := d.sql.ExecContext(ctx, q,
		j.ID, j.ErrorID, j.Stage, j.Status, j.ResultJSON, j.ErrorMessage, j.Attempt)
	if err != nil {
		return fmt.Errorf("db: insert job: %w", err)
	}
	return nil
}

// UpdateJob updates status, result and error message for a job.
func (d *DB) UpdateJob(ctx context.Context, id, status, resultJSON, errorMessage string) error {
	const q = `
UPDATE jobs
SET status = ?, result_json = ?, error_message = ?, updated_at = CURRENT_TIMESTAMP
WHERE id = ?`
	if _, err := d.sql.ExecContext(ctx, q, status, resultJSON, errorMessage, id); err != nil {
		return fmt.Errorf("db: update job: %w", err)
	}
	return nil
}

// ListJobsByError returns all jobs for an error, oldest first.
func (d *DB) ListJobsByError(ctx context.Context, errorID string) ([]models.Job, error) {
	const q = `
SELECT id, error_id, stage, status, COALESCE(result_json,''),
       COALESCE(error_message,''), attempt, created_at, updated_at
FROM jobs WHERE error_id = ? ORDER BY created_at ASC`
	rows, err := d.sql.QueryContext(ctx, q, errorID)
	if err != nil {
		return nil, fmt.Errorf("db: list jobs by error: %w", err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

// ListJobs returns jobs filtered by optional status, paginated.
func (d *DB) ListJobs(ctx context.Context, status string, limit, offset int) ([]models.Job, error) {
	q := `
SELECT id, error_id, stage, status, COALESCE(result_json,''),
       COALESCE(error_message,''), attempt, created_at, updated_at
FROM jobs`
	args := []any{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list jobs: %w", err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

func scanJobs(rows *sql.Rows) ([]models.Job, error) {
	var out []models.Job
	for rows.Next() {
		var j models.Job
		if err := rows.Scan(
			&j.ID, &j.ErrorID, &j.Stage, &j.Status, &j.ResultJSON,
			&j.ErrorMessage, &j.Attempt, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: scan job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// --- merge_requests table ---

// InsertMergeRequest persists a created MR.
func (d *DB) InsertMergeRequest(ctx context.Context, m models.MergeRequest) error {
	const q = `
INSERT INTO merge_requests (id, error_id, gitlab_mr_iid, gitlab_mr_url, branch_name, is_draft)
VALUES (?, ?, ?, ?, ?, ?)`
	draft := 0
	if m.IsDraft {
		draft = 1
	}
	_, err := d.sql.ExecContext(ctx, q,
		m.ID, m.ErrorID, m.GitLabIID, m.GitLabURL, m.BranchName, draft)
	if err != nil {
		return fmt.Errorf("db: insert merge request: %w", err)
	}
	return nil
}

// GetMergeRequestByError returns the MR for an error, or ErrNotFound.
func (d *DB) GetMergeRequestByError(ctx context.Context, errorID string) (models.MergeRequest, error) {
	const q = `
SELECT id, error_id, COALESCE(gitlab_mr_iid,0), COALESCE(gitlab_mr_url,''),
       branch_name, is_draft, created_at
FROM merge_requests WHERE error_id = ? ORDER BY created_at DESC LIMIT 1`
	var m models.MergeRequest
	var draft int
	err := d.sql.QueryRowContext(ctx, q, errorID).Scan(
		&m.ID, &m.ErrorID, &m.GitLabIID, &m.GitLabURL, &m.BranchName, &draft, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return models.MergeRequest{}, ErrNotFound
	}
	if err != nil {
		return models.MergeRequest{}, fmt.Errorf("db: get merge request: %w", err)
	}
	m.IsDraft = draft == 1
	return m, nil
}

// ListMergeRequests returns all MRs newest first.
func (d *DB) ListMergeRequests(ctx context.Context) ([]models.MergeRequest, error) {
	const q = `
SELECT id, error_id, COALESCE(gitlab_mr_iid,0), COALESCE(gitlab_mr_url,''),
       branch_name, is_draft, created_at
FROM merge_requests ORDER BY created_at DESC`
	rows, err := d.sql.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list merge requests: %w", err)
	}
	defer rows.Close()

	var out []models.MergeRequest
	for rows.Next() {
		var m models.MergeRequest
		var draft int
		if err := rows.Scan(
			&m.ID, &m.ErrorID, &m.GitLabIID, &m.GitLabURL, &m.BranchName, &draft, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan merge request: %w", err)
		}
		m.IsDraft = draft == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

// isUniqueViolation detects SQLite UNIQUE constraint failures from modernc driver.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}
