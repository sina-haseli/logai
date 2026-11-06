package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, provider string, h http.HandlerFunc) (*Client, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New(Options{Provider: provider, BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", ReasoningEffort: "low"})
	c.sleep = func(context.Context, time.Duration) error { return nil } // no real backoff in tests
	return c, &calls
}

func reply(w http.ResponseWriter, content any, finish string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": finish,
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "cost": 0.0001},
	})
}

func TestOpenRouterRequestShape(t *testing.T) {
	c, _ := newTestClient(t, ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing bearer auth")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "test-model" || body["max_tokens"].(float64) != 4242 {
			t.Errorf("unexpected body: %v", body)
		}
		msgs := body["messages"].([]any)
		if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["role"] != "user" {
			t.Errorf("expected system+user messages: %v", msgs)
		}
		reasoning, _ := body["reasoning"].(map[string]any)
		if reasoning["effort"] != "low" || reasoning["exclude"] != true {
			t.Errorf("expected OpenRouter reasoning param, got %v", body["reasoning"])
		}
		if _, ok := body["reasoning_effort"]; ok {
			t.Errorf("reasoning_effort must not be sent to OpenRouter")
		}
		if _, ok := body["temperature"]; ok {
			t.Errorf("temperature must not be sent (unsupported by Sonnet 5.5)")
		}
		reply(w, `{"ok":true}`, "stop")
	})

	got, err := c.Complete(context.Background(), "test", "sys", "user", 4242)
	if err != nil || got != `{"ok":true}` {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestOtherProvidersUseReasoningEffort(t *testing.T) {
	c, _ := newTestClient(t, ProviderAvalAI, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["reasoning_effort"] != "low" {
			t.Errorf("expected reasoning_effort=low, got %v", body["reasoning_effort"])
		}
		if _, ok := body["reasoning"]; ok {
			t.Errorf("OpenRouter-only reasoning param sent to avalai")
		}
		reply(w, "ok", "stop")
	})
	if _, err := c.Complete(context.Background(), "t", "s", "u", 10); err != nil {
		t.Fatal(err)
	}
}

func TestContentAsParts(t *testing.T) {
	c, _ := newTestClient(t, ProviderCustom, func(w http.ResponseWriter, r *http.Request) {
		reply(w, []map[string]string{{"type": "text", "text": "hello "}, {"type": "text", "text": "world"}}, "stop")
	})
	got, err := c.Complete(context.Background(), "t", "s", "u", 10)
	if err != nil || got != "hello world" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestRetriesRateLimitThenSucceeds(t *testing.T) {
	var n int32
	c, calls := newTestClient(t, ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		reply(w, "done", "stop")
	})
	got, err := c.Complete(context.Background(), "t", "", "hi", 100)
	if err != nil || got != "done" {
		t.Fatalf("got %q, %v", got, err)
	}
	if *calls != 3 {
		t.Fatalf("expected 3 calls (2 retries), got %d", *calls)
	}
}

func TestRetriesInBodyUpstreamError(t *testing.T) {
	// OpenRouter can return HTTP 200 with an error object when the upstream fails.
	var n int32
	c, calls := newTestClient(t, ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			_, _ = w.Write([]byte(`{"error":{"code":502,"message":"upstream provider error"}}`))
			return
		}
		reply(w, "ok", "stop")
	})
	if _, err := c.Complete(context.Background(), "t", "", "hi", 10); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatalf("expected a retry after in-body 502, got %d calls", *calls)
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	c, calls := newTestClient(t, ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired) // OpenRouter: out of credits
		_, _ = w.Write([]byte(`{"error":{"code":402,"message":"Insufficient credits"}}`))
	})
	_, err := c.Complete(context.Background(), "t", "", "hi", 10)
	var se *statusError
	if !errors.As(err, &se) || se.Status != http.StatusPaymentRequired {
		t.Fatalf("expected 402 statusError, got %v", err)
	}
	if *calls != 1 {
		t.Fatalf("expected no retries, got %d calls", *calls)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	c, calls := newTestClient(t, ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	c.MaxRetries = 2
	if _, err := c.Complete(context.Background(), "t", "", "hi", 10); err == nil {
		t.Fatal("expected error")
	}
	if *calls != 3 {
		t.Fatalf("expected 1 + 2 retries = 3 calls, got %d", *calls)
	}
}

func TestTruncatedIsNotRetried(t *testing.T) {
	c, calls := newTestClient(t, ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) {
		reply(w, `{"fixed_code":"package ma`, "length")
	})
	if _, err := c.Complete(context.Background(), "fix", "", "hi", 10); !errors.Is(err, ErrTruncated) {
		t.Fatalf("expected ErrTruncated, got %v", err)
	}
	if *calls != 1 {
		t.Fatalf("truncation must not be retried, got %d calls", *calls)
	}
}

func TestRefusal(t *testing.T) {
	c, _ := newTestClient(t, ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, "content_filter")
	})
	if _, err := c.Complete(context.Background(), "x", "", "hi", 10); !errors.Is(err, ErrRefused) {
		t.Fatalf("expected ErrRefused, got %v", err)
	}
}

func TestProviderDefaults(t *testing.T) {
	url, model, ok := ProviderDefaults(ProviderOpenRouter)
	if !ok || url != "https://openrouter.ai/api/v1" || model != "anthropic/claude-sonnet-5.5" {
		t.Fatalf("openrouter defaults: %s %s %v", url, model, ok)
	}
	if _, _, ok := ProviderDefaults("anthropic"); ok {
		t.Fatal("unknown provider should be rejected")
	}
}

func TestBackoffHonorsRetryAfter(t *testing.T) {
	if got := backoff(1, &statusError{Status: 429, RetryAfter: 7 * time.Second}); got != 7*time.Second {
		t.Fatalf("got %v", got)
	}
	if got := backoff(10, errors.New("network")); got != maxBackoff {
		t.Fatalf("expected cap %v, got %v", maxBackoff, got)
	}
}

func TestStripFences(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                          `{"a":1}`,
		"```json\n{\"a\":1}\n```":          `{"a":1}`,
		"Here is the result:\n{\"a\":1}\n": `{"a":1}`,
	}
	for in, want := range cases {
		if got := StripFences(in); got != want {
			t.Errorf("StripFences(%q) = %q, want %q", in, got, want)
		}
	}
}
