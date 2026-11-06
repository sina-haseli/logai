# logai

An autonomous bug-fixing pipeline. `logai` ingests application errors from
OpenSearch/Kibana (polling **and** webhook), uses an LLM (Claude Sonnet 5.5 via
[OpenRouter](https://openrouter.ai) by default) to triage, localize, and fix them,
and opens a Merge Request on a self-hosted GitLab instance — optionally gated
behind human review.

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
        │  Pipeline worker  │  concurrency 2, 20-min timeout, 3 retries
        └─────────┬─────────┘
                  ▼
   ① triage ─▶ ② localize ─▶ ③ fix ─▶ ④ gitlab
    (LLM)        (LLM)        (LLM)    (branch+commit+MR)
      │            │            │
   stop if      stop if      needs file
   should_fix   confidence   from GitLab
   == false     == low
   or risk=low
```

## Prerequisites

- Go 1.22+
- Redis (for the Asynq job queue)
- Network access to: your LLM provider (OpenRouter or avalai), your self-hosted
  GitLab, and OpenSearch
- An OpenRouter (or avalai) API key, a GitLab personal/project access token
  (`api` scope), and OpenSearch credentials

> SQLite is embedded via `modernc.org/sqlite` (pure Go) — **no CGO, no sqlite3 CLI** required.

## Environment variables

| Name | Required | Description | Example |
|------|----------|-------------|---------|
| `LLM_PROVIDER` | — | `openrouter` (default), `avalai`, or `custom` | `openrouter` |
| `LLM_API_KEY` | ✅ | API key for the provider | `sk-or-v1-...` |
| `LLM_BASE_URL` | — | Chat-completions base URL (provider default; required for `custom`) | `https://openrouter.ai/api/v1` |
| `LLM_MODEL` | — | Model used for every stage (provider default, see below) | `anthropic/claude-sonnet-5.5` |
| `LLM_REASONING_EFFORT` | — | `low` / `medium` / `high` / `xhigh` / `none` (default `medium`) | `medium` |
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
| `OPENSEARCH_LEVEL_FIELD` | — | Field holding the log level (default `level`) | `log.level` |
| `OPENSEARCH_LEVEL_VALUE` | — | Level value to match, exact term (default `ERROR`) | `error` |
| `OPENSEARCH_MESSAGE_FIELD` | — | Error message field (default `message`) | `message` |
| `OPENSEARCH_STACK_TRACE_FIELD` | — | Stack trace field (default `stack_trace`) | `error.stack_trace` |
| `OPENSEARCH_SERVICE_FIELD` | — | Service name field (default `service`) | `service.name` |
| `OPENSEARCH_TIMESTAMP_FIELD` | — | Timestamp field (default `@timestamp`) | `@timestamp` |
| `REDIS_URL` | — | Redis connection URI (default `redis://localhost:6379`) | `redis://localhost:6379` |
| `PORT` | — | HTTP port (default `3000`) | `3000` |
| `HUMAN_APPROVAL_REQUIRED` | — | Open MRs as Draft when `true` (default `true`) | `true` |
| `LOG_LEVEL` | — | `debug` / `info` / `warn` / `error` (default `info`) | `info` |
| `DB_PATH` | — | SQLite file path (default `logai.db`; `/app/data/logai.db` in Docker) | `logai.db` |
| `WEBHOOK_SECRET` | recommended | Shared secret required on `POST /webhook/error` (see below) | `openssl rand -hex 32` |

All required fields are validated on startup. Missing fields are reported in a
single message and the process exits with code `1`.

### LLM providers

logai talks to the model through the OpenAI-compatible `POST {LLM_BASE_URL}/chat/completions`
API, so switching providers is a config change only:

| `LLM_PROVIDER` | Default base URL | Default model |
|---|---|---|
| `openrouter` | `https://openrouter.ai/api/v1` | `anthropic/claude-sonnet-5.5` |
| `avalai` | `https://api.avalai.ir/v1` | `claude-sonnet-5-5` (check the name in your avalai panel) |
| `custom` | — (set `LLM_BASE_URL`) | — (set `LLM_MODEL`) |

Reasoning: on OpenRouter, `LLM_REASONING_EFFORT` is sent as
`reasoning: {effort, exclude: true}` (reasoning text is not returned, only the
answer). Other providers get the OpenAI-style `reasoning_effort`. Sonnet 5.5
always reasons to some degree, and reasoning tokens count toward each stage's
`max_tokens` budget (triage/localize 8k, fix 32k). Each call's token usage and
OpenRouter cost (`cost_usd`) are logged per stage.

### OpenSearch field mapping

Field names accept dotted paths and match both nested objects and flat dotted
keys. For Elastic Common Schema (ECS) logs use:

```env
OPENSEARCH_LEVEL_FIELD=log.level
OPENSEARCH_LEVEL_VALUE=error
OPENSEARCH_STACK_TRACE_FIELD=error.stack_trace
OPENSEARCH_SERVICE_FIELD=service.name
```

The level filter is an exact `term` query, so point it at a `keyword` field
(e.g. `level.keyword` if `level` is analyzed text). The first poll runs at
startup; polls never overlap.

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
docker run --rm -p 3000:3000 --env-file .env -v logai-data:/app/data logai
```

The image is a multi-stage build (`golang:1.22-alpine` → `alpine:3.19`), runs as a
non-root user, and exposes port `3000`. The SQLite DB lives in the `/app/data`
volume so it survives container restarts.

## Webhook example

Send an error directly without waiting for the OpenSearch poll:

```bash
curl -X POST http://localhost:3000/webhook/error \
  -H 'Content-Type: application/json' \
  -H "X-Logai-Token: $WEBHOOK_SECRET" \
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

**Authentication.** Every accepted webhook can end in an MR on your repository,
so set `WEBHOOK_SECRET` in any shared environment. Callers then send it as
`X-Logai-Token: <secret>` or `Authorization: Bearer <secret>`, and anything else
gets `401`. When the secret is unset the endpoint is open and logai logs a
warning at startup. Bodies over 1 MiB are rejected with `413`.

## REST API

| Method & path | Description |
|---------------|-------------|
| `GET /health` | `{ "status": "ok", "db": "ok", "redis": "ok", "asynq_queued": N }` (`503` + `degraded` if DB or Redis is down) |
| `GET /errors?status=&limit=50&offset=0` | Paginated list of errors (`limit` max 500) |
| `GET /errors/{id}` | Error detail + all jobs + MR (if any) |
| `GET /jobs?status=&limit=50&offset=0` | Paginated list of pipeline jobs |
| `POST /retry/{errorId}` | Reset error to `new` and re-enqueue (`409` while it is `processing`) |
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

If a pipeline run fails (LLM/GitLab error, low-confidence stop, etc.) the
error lands in `failed` or `skipped`. To run it again:

```bash
curl -X POST http://localhost:3000/retry/<error-id>
# => 202 Accepted  {"error_id":"<error-id>","status":"re-enqueued"}
```

This resets the error's status to `new` and enqueues a fresh pipeline task. Each
run records its own `jobs` rows (with the attempt number) so you can inspect
every attempt via `GET /errors/{id}`.

**Automatic retries.** Asynq retries a failed run up to 3 times. While a retry
is pending the error's status is `new`; it becomes `failed` only once retries
are used up. Failures that would repeat exactly the same way are not retried:
file not found in GitLab, model output cut off at `max_tokens`, or a model
refusal. Transient LLM errors (429, 5xx, upstream-provider errors, network) are
first retried inside the client with exponential backoff that honors
`retry-after`.

## Pipeline stages

1. **Triage** — the model classifies risk and decides `should_fix`. Stops if
   `should_fix == false` or `risk_level == "low"` (status → `skipped`).
2. **Localize** — the model points to the offending source file/line from the stack
   trace. Stops if `confidence == "low"` (status → `skipped`).
3. **Fix** — the target file is fetched from GitLab, then the model returns the full
   fixed file (up to 32k output tokens, reasoning included). Stack-trace paths are usually absolute
   container paths (`/app/src/pkg/x.go`), so logai tries successively shorter
   suffixes (`app/src/pkg/x.go`, `src/pkg/x.go`, `pkg/x.go`, …) until one exists
   in the repo, and commits to that path. Stops with an error if no change is
   produced.
4. **GitLab** — creates `logai/<id8>-<unix>` branch, commits the fix, and opens
   an MR (Draft when approval is required). Status → `fixed`.

## Development

```bash
make build    # compile to bin/logai
make test     # go test ./...
make lint     # golangci-lint run
make tidy     # go mod tidy
```
