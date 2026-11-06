package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/yourorg/logai/internal/config"
	"github.com/yourorg/logai/internal/db"
	"github.com/yourorg/logai/internal/gitlab"
	"github.com/yourorg/logai/internal/llm"
	"github.com/yourorg/logai/internal/models"
)

const buggy = "package app\n\nfunc Divide(a, b int) int {\n\treturn a / b\n}\n"
const fixed = "package app\n\nfunc Divide(a, b int) int {\n\tif b == 0 {\n\t\treturn 0\n\t}\n\treturn a / b\n}\n"

// fakeLLM is an OpenAI-compatible chat-completions server that answers each
// stage based on its system prompt.
func fakeLLM(t *testing.T, triage string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var req struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Messages  []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "anthropic/claude-sonnet-5.5" {
			t.Errorf("unexpected model %q", req.Model)
		}
		var system string
		if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
			system = req.Messages[0].Content
		}

		var text string
		switch system {
		case triageSystemPrompt:
			text = triage
		case localizeSystemPrompt:
			// Absolute container path, as it appears in real stack traces.
			text = `{"file_path":"/src/app/calculator.go","line_number":4,"function_name":"Divide","confidence":"high","reasoning":"divide by b without zero check"}`
		case fixSystemPrompt:
			if req.MaxTokens < 16000 {
				t.Errorf("fix stage max_tokens too small for reasoning + a whole file: %d", req.MaxTokens)
			}
			b, _ := json.Marshal(map[string]string{"fixed_code": fixed, "fix_summary": "guard b == 0", "confidence": "high"})
			text = "```json\n" + string(b) + "\n```"
		default:
			t.Errorf("unexpected system prompt: %q", system)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": text},
				"finish_reason": "stop",
			}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// strictGitLab behaves like real GitLab: exact paths only, PUT fails on
// files that don't exist on the branch.
type strictGitLab struct {
	mu       sync.Mutex
	files    map[string]map[string]string // branch -> path -> content
	mrs      []map[string]any
	branches []string
}

func newStrictGitLab(t *testing.T) (*strictGitLab, *httptest.Server) {
	g := &strictGitLab{files: map[string]map[string]string{"main": {"app/calculator.go": buggy}}}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv
}

func (g *strictGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	const base = "/api/v4/projects/42"
	p := strings.TrimPrefix(r.URL.EscapedPath(), base)

	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/repository/files/") && strings.HasSuffix(p, "/raw"):
		fp, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(p, "/repository/files/"), "/raw"))
		c, ok := g.files[r.URL.Query().Get("ref")][fp]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(c))
	case r.Method == http.MethodPost && p == "/repository/branches":
		br, ref := r.URL.Query().Get("branch"), r.URL.Query().Get("ref")
		cp := map[string]string{}
		for k, v := range g.files[ref] {
			cp[k] = v
		}
		g.files[br] = cp
		g.branches = append(g.branches, br)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPut && strings.HasPrefix(p, "/repository/files/"):
		fp, _ := url.PathUnescape(strings.TrimPrefix(p, "/repository/files/"))
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := g.files[body["branch"]][fp]; !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"A file with this name doesn't exist"}`))
			return
		}
		g.files[body["branch"]][fp] = body["content"]
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && p == "/merge_requests":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.mrs = append(g.mrs, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"iid": len(g.mrs), "web_url": "http://gitlab/mr/1"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func setup(t *testing.T, triage string) (*Processor, *db.DB, *strictGitLab) {
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

	model := fakeLLM(t, triage)
	gl, glSrv := newStrictGitLab(t)

	cfg := &config.Config{GitLabDefaultBranch: "main", GitLabMRTarget: "main", HumanApprovalRequired: true}
	p := NewProcessor(database,
		llm.New(llm.Options{Provider: llm.ProviderOpenRouter, BaseURL: model.URL, APIKey: "k", Model: "anthropic/claude-sonnet-5.5"}),
		gitlab.New(glSrv.URL, "tok", "42", nil),
		cfg, nil)
	return p, database, gl
}

func insertError(t *testing.T, database *db.DB, id string) {
	t.Helper()
	err := database.InsertError(context.Background(), models.Error{
		ID: id, Fingerprint: "fp-" + id, Source: "webhook", RawJSON: "{}",
		Message:    "panic: runtime error: integer divide by zero",
		StackTrace: "app.Divide(0xa, 0x0)\n\t/src/app/calculator.go:4",
		Service:    "calculator",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func task(t *testing.T, id string) *asynq.Task {
	t.Helper()
	tk, err := NewProcessTask(id)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestPipelineHappyPathOpensDraftMR(t *testing.T) {
	p, database, gl := setup(t, `{"risk_level":"high","risk_reason":"crash","should_fix":true,"affected_service":"calculator"}`)
	insertError(t, database, "err-1")

	if err := p.HandleProcessError(context.Background(), task(t, "err-1")); err != nil {
		t.Fatalf("pipeline: %v", err)
	}

	e, _ := database.GetError(context.Background(), "err-1")
	if e.Status != models.StatusFixed || e.RiskLevel != "high" {
		t.Fatalf("expected fixed/high, got %s/%s", e.Status, e.RiskLevel)
	}

	jobs, _ := database.ListJobsByError(context.Background(), "err-1")
	if len(jobs) != 4 {
		t.Fatalf("expected 4 jobs, got %d", len(jobs))
	}
	for _, j := range jobs {
		if j.Status != models.JobSucceeded {
			t.Errorf("job %s: %s (%s)", j.Stage, j.Status, j.ErrorMessage)
		}
	}

	if len(gl.mrs) != 1 {
		t.Fatalf("expected 1 MR, got %d", len(gl.mrs))
	}
	if title, _ := gl.mrs[0]["title"].(string); !strings.HasPrefix(title, "Draft: ") {
		t.Errorf("MR should be draft when approval is required: %q", title)
	}
	// The fix must be committed at the resolved repo path, not the stack-trace path.
	if got := gl.files[gl.branches[0]]["app/calculator.go"]; got != fixed {
		t.Errorf("fix not committed at resolved path; got:\n%s", got)
	}

	mr, err := database.GetMergeRequestByError(context.Background(), "err-1")
	if err != nil || !mr.IsDraft || mr.GitLabIID != 1 {
		t.Fatalf("MR not persisted correctly: %+v (%v)", mr, err)
	}
}

func TestPipelineSkipsLowRisk(t *testing.T) {
	p, database, gl := setup(t, `{"risk_level":"low","risk_reason":"noise","should_fix":true,"affected_service":"calculator"}`)
	insertError(t, database, "err-2")

	if err := p.HandleProcessError(context.Background(), task(t, "err-2")); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	e, _ := database.GetError(context.Background(), "err-2")
	if e.Status != models.StatusSkipped {
		t.Fatalf("expected skipped, got %s", e.Status)
	}
	if len(gl.mrs) != 0 {
		t.Fatal("no MR should be opened for low-risk errors")
	}
}

func TestPipelineFileNotFoundIsNotRetried(t *testing.T) {
	p, database, gl := setup(t, `{"risk_level":"high","risk_reason":"crash","should_fix":true,"affected_service":"calculator"}`)
	delete(gl.files["main"], "app/calculator.go")
	insertError(t, database, "err-3")

	err := p.HandleProcessError(context.Background(), task(t, "err-3"))
	if !errors.Is(err, asynq.SkipRetry) || !errors.Is(err, gitlab.ErrFileNotFound) {
		t.Fatalf("expected SkipRetry wrapping ErrFileNotFound, got %v", err)
	}
	e, _ := database.GetError(context.Background(), "err-3")
	if e.Status != models.StatusFailed {
		t.Fatalf("expected failed, got %s", e.Status)
	}
}

func TestPipelineUnknownErrorIsNotRetried(t *testing.T) {
	p, _, _ := setup(t, `{}`)
	if err := p.HandleProcessError(context.Background(), task(t, "missing")); !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("expected SkipRetry for unknown error id, got %v", err)
	}
}
