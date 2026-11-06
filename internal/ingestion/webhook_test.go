package ingestion

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/yourorg/logai/internal/db"
	"github.com/yourorg/logai/internal/models"
)

type fakeQueue struct {
	mu    sync.Mutex
	tasks []*asynq.Task
	err   error
}

func (f *fakeQueue) EnqueueContext(_ context.Context, t *asynq.Task, _ ...asynq.Option) (*asynq.TaskInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.tasks = append(f.tasks, t)
	return &asynq.TaskInfo{}, nil
}

func newTestIngestor(t *testing.T) (*Ingestor, *fakeQueue, *db.DB) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	q := &fakeQueue{}
	return NewIngestor(database, q, nil), q, database
}

const validBody = `{"message":"panic: boom","stack_trace":"main.go:1","service":"api"}`

func post(h http.Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhook/error", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookRequiresSecretWhenConfigured(t *testing.T) {
	ing, q, _ := newTestIngestor(t)
	h := ing.WebhookHandler("s3cret")

	if rec := post(h, validBody, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: expected 401, got %d", rec.Code)
	}
	if rec := post(h, validBody, map[string]string{"X-Logai-Token": "wrong"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: expected 401, got %d", rec.Code)
	}
	if rec := post(h, validBody, map[string]string{"Authorization": "Bearer s3cret"}); rec.Code != http.StatusAccepted {
		t.Fatalf("bearer token: expected 202, got %d %s", rec.Code, rec.Body)
	}
	if len(q.tasks) != 1 {
		t.Fatalf("expected 1 enqueued task, got %d", len(q.tasks))
	}
}

func TestWebhookDedupAndValidation(t *testing.T) {
	ing, q, _ := newTestIngestor(t)
	h := ing.WebhookHandler("")

	if rec := post(h, validBody, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("first: expected 202, got %d", rec.Code)
	}
	if rec := post(h, validBody, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "duplicate") {
		t.Fatalf("duplicate: expected 200 duplicate, got %d %s", rec.Code, rec.Body)
	}
	if len(q.tasks) != 1 {
		t.Fatalf("duplicate must not be re-enqueued; got %d tasks", len(q.tasks))
	}
	if rec := post(h, `{"stack_trace":"x"}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing message: expected 400, got %d", rec.Code)
	}
	if rec := post(h, `not json`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: expected 400, got %d", rec.Code)
	}
	big := `{"message":"` + strings.Repeat("a", maxWebhookBody+10) + `"}`
	if rec := post(h, big, nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: expected 413, got %d", rec.Code)
	}
}

func TestIngestMarksFailedWhenEnqueueFails(t *testing.T) {
	ing, q, database := newTestIngestor(t)
	q.err = errors.New("redis down")

	_, err := ing.Ingest(context.Background(), "webhook", models.IncomingError{Message: "m", StackTrace: "s"})
	if err == nil {
		t.Fatal("expected error")
	}
	errs, err := database.ListErrors(context.Background(), models.StatusFailed, 10, 0)
	if err != nil || len(errs) != 1 {
		t.Fatalf("expected the unqueued error to be marked failed, got %d (%v)", len(errs), err)
	}
}
