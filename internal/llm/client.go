// Package llm is a small client for OpenAI-compatible chat-completions APIs
// (OpenRouter, avalai, or any other gateway speaking the same protocol).
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Supported providers. "custom" means any OpenAI-compatible endpoint.
const (
	ProviderOpenRouter = "openrouter"
	ProviderAvalAI     = "avalai"
	ProviderCustom     = "custom"
)

// ProviderDefaults returns the default base URL and model for a provider.
func ProviderDefaults(provider string) (baseURL, model string, ok bool) {
	switch provider {
	case ProviderOpenRouter:
		return "https://openrouter.ai/api/v1", "anthropic/claude-sonnet-5.5", true
	case ProviderAvalAI:
		return "https://api.avalai.ir/v1", "claude-sonnet-5-5", true
	case ProviderCustom:
		return "", "", true
	}
	return "", "", false
}

const (
	// httpTimeout bounds one HTTP attempt. The fix stage can emit a whole
	// source file plus reasoning, which takes minutes, not seconds.
	httpTimeout  = 8 * time.Minute
	bodyExcerpts = 512

	defaultMaxRetries = 3
	baseBackoff       = 2 * time.Second
	maxBackoff        = 30 * time.Second
)

// ErrTruncated is returned when the model hit max_tokens before finishing.
// The partial output is never valid structured JSON, so callers should not parse it.
var ErrTruncated = errors.New("llm: response truncated (finish_reason=length)")

// ErrRefused is returned when the model or provider declined the request.
var ErrRefused = errors.New("llm: model refused the request")

// Options configures a Client.
type Options struct {
	Provider string // openrouter | avalai | custom
	BaseURL  string // e.g. https://openrouter.ai/api/v1
	APIKey   string
	Model    string // e.g. anthropic/claude-sonnet-5.5
	// ReasoningEffort is passed to the provider when non-empty
	// (low | medium | high | xhigh).
	ReasoningEffort string
	Logger          *slog.Logger
}

// Client is a reusable chat-completions client.
type Client struct {
	provider        string
	baseURL         string
	apiKey          string
	model           string
	reasoningEffort string

	HTTPClient *http.Client
	// MaxRetries is how many times a retryable failure (429, 5xx, network
	// error) is retried before giving up.
	MaxRetries int
	// sleep is swappable for tests.
	sleep  func(ctx context.Context, d time.Duration) error
	logger *slog.Logger
}

// New constructs a Client.
func New(o Options) *Client {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Client{
		provider:        o.Provider,
		baseURL:         strings.TrimRight(o.BaseURL, "/"),
		apiKey:          o.APIKey,
		model:           o.Model,
		reasoningEffort: o.ReasoningEffort,
		HTTPClient:      &http.Client{Timeout: httpTimeout},
		MaxRetries:      defaultMaxRetries,
		sleep:           sleepCtx,
		logger:          o.Logger,
	}
}

// Model returns the configured model id.
func (c *Client) Model() string { return c.model }

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type requestBody struct {
	Model     string    `json:"model"`
	Messages  []message `json:"messages"`
	MaxTokens int       `json:"max_tokens"`
	// OpenRouter's unified reasoning control. Reasoning text is excluded from
	// the response: the pipeline only consumes the final answer.
	Reasoning *reasoningParam `json:"reasoning,omitempty"`
	// OpenAI-style reasoning control, used by other gateways.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type reasoningParam struct {
	Effort  string `json:"effort,omitempty"`
	Exclude bool   `json:"exclude"`
}

type apiError struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

type responseBody struct {
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
			Refusal string          `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int     `json:"prompt_tokens"`
		CompletionTokens int     `json:"completion_tokens"`
		Cost             float64 `json:"cost"`
	} `json:"usage"`
	// OpenRouter can report an upstream failure inside a 200 response.
	Error *apiError `json:"error"`
}

// statusError is a failed response from the API.
type statusError struct {
	Status     int
	Body       string
	RetryAfter time.Duration
}

func (e *statusError) Error() string {
	return fmt.Sprintf("llm: unexpected status %d: %s", e.Status, e.Body)
}

// retryable reports whether a status is worth retrying: timeouts, rate limits,
// and transient upstream/provider errors.
func retryable(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	}
	return false
}

// Complete sends a system + user prompt and returns the assistant text.
// stage is used only for structured logging. maxTokens caps the response
// length (including any reasoning tokens); size it to the largest output the
// stage can produce. Retryable failures are retried with exponential backoff.
func (c *Client) Complete(ctx context.Context, stage, systemPrompt, userPrompt string, maxTokens int) (string, error) {
	body := requestBody{
		Model:     c.model,
		MaxTokens: maxTokens,
		Messages: []message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}
	if c.provider == ProviderOpenRouter {
		body.Reasoning = &reasoningParam{Effort: c.reasoningEffort, Exclude: true}
	} else {
		body.ReasoningEffort = c.reasoningEffort
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("llm: marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			wait := backoff(attempt, lastErr)
			c.logger.Warn("llm retry", "stage", stage, "attempt", attempt, "wait_ms", wait.Milliseconds(), "err", lastErr)
			if err := c.sleep(ctx, wait); err != nil {
				return "", fmt.Errorf("llm: %w (last error: %v)", err, lastErr)
			}
		}

		text, err := c.once(ctx, stage, payload)
		if err == nil {
			return text, nil
		}
		lastErr = err

		var se *statusError
		switch {
		case ctx.Err() != nil:
			return "", err
		case errors.As(err, &se) && !retryable(se.Status):
			return "", err
		case errors.Is(err, ErrTruncated), errors.Is(err, ErrRefused):
			// Deterministic outcomes: retrying the same request won't help.
			return "", err
		}
	}
	return "", fmt.Errorf("llm: giving up after %d retries: %w", c.MaxRetries, lastErr)
}

// once performs a single HTTP attempt.
func (c *Client) once(ctx context.Context, stage string, payload []byte) (string, error) {
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("llm: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if c.provider == ProviderOpenRouter {
		// Optional attribution headers shown in the OpenRouter dashboard.
		req.Header.Set("X-Title", "logai")
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: do request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("llm: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", &statusError{
			Status:     resp.StatusCode,
			Body:       excerpt(string(raw)),
			RetryAfter: parseRetryAfter(resp.Header.Get("retry-after")),
		}
	}

	var parsed responseBody
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("llm: unmarshal response: %w (got: %s)", err, excerpt(string(raw)))
	}
	if parsed.Error != nil {
		return "", &statusError{Status: errorCode(parsed.Error.Code), Body: excerpt(parsed.Error.Message)}
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("llm: response has no choices: %s", excerpt(string(raw)))
	}

	choice := parsed.Choices[0]
	text := contentText(choice.Message.Content)

	c.logger.Info("llm completion",
		"stage", stage,
		"provider", c.provider,
		"model", c.model,
		"finish_reason", choice.FinishReason,
		"duration_ms", time.Since(start).Milliseconds(),
		"prompt_tokens", parsed.Usage.PromptTokens,
		"completion_tokens", parsed.Usage.CompletionTokens,
		"cost_usd", parsed.Usage.Cost,
	)

	switch {
	case choice.FinishReason == "length":
		return "", ErrTruncated
	case choice.FinishReason == "content_filter", choice.Message.Refusal != "":
		return "", fmt.Errorf("%w: %s", ErrRefused, choice.Message.Refusal)
	case choice.FinishReason == "error":
		return "", &statusError{Status: http.StatusBadGateway, Body: "provider reported finish_reason=error"}
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("llm: empty content in response")
	}
	return text, nil
}

// contentText accepts either a plain string or an array of content parts.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// errorCode turns an in-body error code (number or string) into an HTTP-ish
// status so retry classification works. Unknown codes count as 502.
func errorCode(raw json.RawMessage) int {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
		return n
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return http.StatusBadGateway
}

// backoff returns the wait before the given retry attempt (1-based),
// honoring a server-provided retry-after when present.
func backoff(attempt int, lastErr error) time.Duration {
	var se *statusError
	if errors.As(lastErr, &se) && se.RetryAfter > 0 {
		if se.RetryAfter > maxBackoff {
			return maxBackoff
		}
		return se.RetryAfter
	}
	d := baseBackoff << (attempt - 1)
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func excerpt(s string) string {
	if len(s) > bodyExcerpts {
		return s[:bodyExcerpts]
	}
	return s
}

// ParseJSON strips optional markdown code fences and unmarshals the model's
// response into v. Use this for all structured-output stages.
func ParseJSON(raw string, v any) error {
	cleaned := StripFences(raw)
	if err := json.Unmarshal([]byte(cleaned), v); err != nil {
		return fmt.Errorf("llm: parse json: %w (got: %s)", err, excerpt(cleaned))
	}
	return nil
}

// StripFences removes ```json ... ``` or ``` ... ``` wrappers if present and
// trims surrounding whitespace, returning the inner payload. If the model
// added prose around a bare JSON object, the outermost {...} is extracted.
func StripFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		// Drop the opening fence line (``` or ```json).
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		} else {
			s = strings.TrimPrefix(s, "```")
		}
		// Drop the trailing fence.
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		return strings.TrimSpace(s)
	}
	if !strings.HasPrefix(s, "{") {
		start, end := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
		if start >= 0 && end > start {
			return s[start : end+1]
		}
	}
	return s
}
