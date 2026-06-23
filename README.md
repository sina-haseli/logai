# logai

An autonomous bug-fixing pipeline. `logai` ingests application errors from
OpenSearch/Kibana (polling **and** webhook), uses Claude to triage, localize, and
fix them, and opens a Merge Request on a self-hosted GitLab instance — optionally
gated behind human review.

```
 ┌────────────┐      ┌────────────┐
 │ OpenSearch │      │  Webhook   │
 │  (poll)    │      │ POST /...  │
 └─────┬──────┘      └─────┬──────┘
       │   fingerprint + dedup │
       └──────────┬───────────┘
                  ▼
            ┌───────────┐
            │  SQLite   │  errors / jobs / merge_requests
            └─────┬─────┘
                  │ enqueue (Asynq / Redis)
                  ▼
        ┌───────────────────┐
        │  Pipeline worker  │  concurrency 2, 5-min timeout, 3 retries
        └─────────┬─────────┘
                  ▼
   ① triage ─▶ ② localize ─▶ ③ fix ─▶ ④ gitlab
   (Claude)     (Claude)     (Claude)   (branch+commit+MR)
      │            │            │
   stop if      stop if      needs file
   should_fix   confidence   from GitLab
   == false     == low
   or risk=low
```

## Prerequisites

- Go 1.22+
- Redis (for the Asynq job queue)
- Network access to: the Anthropic API, your self-hosted GitLab, and OpenSearch
- An Anthropic API key, a GitLab personal/project access token (`api` scope),
  and OpenSearch credentials

> SQLite is embedded via `modernc.org/sqlite` (pure Go) — **no CGO, no sqlite3 CLI** required.

## Environment variables

| Name | Required | Description | Example |
|------|----------|-------------|---------|
| `ANTHROPIC_API_KEY` | ✅ | Anthropic API key | `sk-ant-...` |
| `GITLAB_URL` | ✅ | Base URL of the GitLab instance | `https://gitlab.example.com` |
| `GITLAB_TOKEN` | ✅ | GitLab token with `api` scope | `glpat-...` |
| `GITLAB_PROJECT_ID` | ✅ | Numeric ID or URL-encoded path of the project | `42` |
| `GITLAB_DEFAULT_BRANCH` | — | Branch new fix branches are cut from (default `main`) | `main` |
| `GITLAB_MR_TARGET_BRANCH` | — | Target branch for the MR (default `main`) | `main` |
| `GITLAB_MR_ASSIGNEE_ID` | — | User ID to assign the MR to | `7` |
| `OPENSEARCH_URL` | ✅ | OpenSearch base URL | `https://opensearch.example.com` |
| `OPENSEARCH_USERNAME` | ✅ | OpenSearch username | `admin` |
| `OPENSEARCH_PASSWORD` | ✅ | OpenSearch password | `••••••` |
| `OPENSEARCH_INDEX` | — | Index/pattern to query (default `logs-*`) | `logs-*` |
| `OPENSEARCH_POLL_INTERVAL_SECONDS` | — | Poll cadence (default `60`) | `60` |
| `OPENSEARCH_LOOKBACK_SECONDS` | — | Time window per poll (default `120`) | `120` |
| `REDIS_URL` | — | Redis connection URI (default `redis://localhost:6379`) | `redis://localhost:6379` |
| `PORT` | — | HTTP port (default `3000`) | `3000` |
| `HUMAN_APPROVAL_REQUIRED` | — | Open MRs as Draft when `true` (default `true`) | `true` |
| `LOG_LEVEL` | — | `debug` / `info` / `warn` / `error` (default `info`) | `info` |
| `DB_PATH` | — | SQLite file path (default `logai.db`) | `logai.db` |

All required fields are validated on startup. Missing fields are reported in a
single message and the process exits with code `1`.

## Quick start

```bash
git clone <repo> logai && cd logai
cp .env.example .env          # then fill in the required values
make migrate                  # create the SQLite schema (optional; run() also migrates)
make run                      # start the service
```

The service starts the HTTP server, the Asynq worker (concurrency 2), and the
OpenSearch poller.

## Docker quick start

```bash
make docker-build
docker run --rm -p 3000:3000 --env-file .env logai
```

The image is a multi-stage build (`golang:1.22-alpine` → `alpine:3.19`), runs as a
non-root user, and exposes port `3000`.

## Webhook example

Send an error directly without waiting for the OpenSearch poll:

```bash
curl -X POST http://localhost:3000/webhook/error \
  -H 'Content-Type: application/json' \
  -d '{
    "message": "panic: runtime error: invalid memory address or nil pointer dereference",
    "stack_trace": "goroutine 1 [running]:\nmain.handler(...)\n\t/app/handler.go:42 +0x1a",
    "service": "checkout-api",
    "severity": "error",
    "timestamp": "2026-06-23T10:00:00Z"
  }'
# => 202 Accepted  {"error_id":"...."}
```

Duplicate errors (same fingerprint) are acknowledged idempotently and **not**
re-processed.

## REST API

| Method & path | Description |
|---------------|-------------|
| `GET /health` | `{ "status": "ok", "asynq_queued": N, "db": "ok" }` |
| `GET /errors?status=&limit=50&offset=0` | Paginated list of errors |
| `GET /errors/{id}` | Error detail + all jobs + MR (if any) |
| `GET /jobs?status=&limit=50` | Paginated list of pipeline jobs |
| `POST /retry/{errorId}` | Reset error to `new` and re-enqueue |
| `GET /mrs` | All merge requests, enriched with their error |
| `POST /webhook/error` | Ingest a single error |

## How `HUMAN_APPROVAL_REQUIRED` works

- **`true` (default):** the MR is created as a **Draft** (title prefixed with
  `Draft:`). A human must mark it ready and merge. Nothing is merged
  automatically.
- **`false`:** the MR is opened as a normal (non-draft) MR, ready for the
  project's own merge rules. `logai` still never clicks "merge" itself.

Either way the MR description is clearly marked as auto-generated and asks for
careful review.

## Retrying a failed error

If a pipeline run fails (Claude/GitLab error, low-confidence stop, etc.) the
error lands in `failed` or `skipped`. To run it again:

```bash
curl -X POST http://localhost:3000/retry/<error-id>
# => 202 Accepted  {"error_id":"<error-id>","status":"re-enqueued"}
```

This resets the error's status to `new` and enqueues a fresh pipeline task. Each
run records its own `jobs` rows so you can inspect every attempt via
`GET /errors/{id}`.

## Pipeline stages

1. **Triage** — Claude classifies risk and decides `should_fix`. Stops if
   `should_fix == false` or `risk_level == "low"` (status → `skipped`).
2. **Localize** — Claude points to the offending source file/line from the stack
   trace. Stops if `confidence == "low"` (status → `skipped`).
3. **Fix** — the target file is fetched from GitLab, then Claude returns the full
   fixed file. Stops with an error if no change is produced.
4. **GitLab** — creates `logai/<id8>-<unix>` branch, commits the fix, and opens
   an MR (Draft when approval is required). Status → `fixed`.

## Development

```bash
make build    # compile to bin/logai
make test     # go test ./...
make lint     # golangci-lint run
make tidy     # go mod tidy
```
