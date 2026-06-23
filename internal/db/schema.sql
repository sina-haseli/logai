CREATE TABLE IF NOT EXISTS errors (
    id TEXT PRIMARY KEY,
    fingerprint TEXT UNIQUE NOT NULL,
    source TEXT NOT NULL,
    raw_json TEXT NOT NULL,
    message TEXT NOT NULL,
    stack_trace TEXT NOT NULL,
    service TEXT,
    status TEXT NOT NULL DEFAULT 'new',
    risk_level TEXT,
    risk_reason TEXT,
    affected_service TEXT,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS jobs (
    id TEXT PRIMARY KEY,
    error_id TEXT NOT NULL REFERENCES errors(id),
    stage TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    result_json TEXT,
    error_message TEXT,
    attempt INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS merge_requests (
    id TEXT PRIMARY KEY,
    error_id TEXT NOT NULL REFERENCES errors(id),
    gitlab_mr_iid INTEGER,
    gitlab_mr_url TEXT,
    branch_name TEXT NOT NULL,
    is_draft INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_errors_status ON errors(status);
CREATE INDEX IF NOT EXISTS idx_jobs_error_id ON jobs(error_id);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_mrs_error_id ON merge_requests(error_id);
